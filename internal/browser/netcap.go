package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"time"

	"uitest-server/internal/model"
)

// 本文件是「接口录制」的浏览器侧实现：把内置浏览器标签页里的真实网络请求
// 通过 CDP 的 Network 域旁路记录下来（不改页面代码、不装代理、不动代理设置）。
//
// 记录粒度：一条请求一条记录（requestId 维度）；只有命中捕获类型的请求才取
// 响应体，避免把图片/字体这些二进制拖进来。

// NetCaptureOptions 录制参数。
type NetCaptureOptions struct {
	// Types 要抓的 CDP 资源类型（XHR / Fetch / Document / Script …）。
	// 空 = XHR + Fetch（也就是"接口"），需要抓页面文档时显式加 Document。
	Types []string
	// CaptureBodies 是否抓响应体（筛选与字段识别要用，建议开）。
	CaptureBodies bool
	// MaxBodyBytes 单条响应体保存上限（默认 256KB）。
	MaxBodyBytes int
	// MaxEntries 最多记录多少条（默认 2000，防止长时间录制把内存吃满）。
	MaxEntries int
	// PageURL 起始页面地址（记录在每条请求上，作为默认 Referer）。
	PageURL string
}

// NetCapture 一次接口录制会话。
type NetCapture struct {
	page *Page
	opts NetCaptureOptions

	mu    sync.Mutex
	items map[string]*model.CapturedRequest
	order []string
	seq   int

	cancels  []func()
	bodyQ    chan string
	bodyStop chan struct{}
	workerOK chan struct{}
	stopped  bool
	stopOnce sync.Once
	started  time.Time
	dropped  int
	// done 关闭后，所有事件协程退出（停止录制时释放 goroutine）
	done chan struct{}
}

// wantedType 这个 CDP 资源类型要不要录。默认只录 XHR/Fetch（即"接口"）。
func (c *NetCapture) wantedType(t string) bool {
	set := map[string]bool{}
	for _, x := range c.opts.Types {
		if x = strings.TrimSpace(x); x != "" {
			set[strings.ToLower(x)] = true
		}
	}
	if len(set) == 0 {
		set["xhr"] = true
		set["fetch"] = true
	}
	return set[strings.ToLower(t)]
}

// StartNetCapture 开始录制这个标签页的网络请求。
//
// 注意：调用前标签页必须已经 Page/Network 域可用（OpenPage 已 enable）。
func (p *Page) StartNetCapture(opts NetCaptureOptions) (*NetCapture, error) {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 256 << 10
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 2000
	}
	c := &NetCapture{
		page:     p,
		opts:     opts,
		items:    map[string]*model.CapturedRequest{},
		bodyQ:    make(chan string, 512),
		bodyStop: make(chan struct{}),
		workerOK: make(chan struct{}),
		started:  time.Now(),
		done:     make(chan struct{}),
	}
	// Network 域可能在其他标签页操作里被关掉，这里确保是开着的
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	_ = p.call(ctx, "Network.enable", nil, nil)
	cancel()

	go c.worker()

	c.sub("Network.requestWillBeSent", c.onRequest)
	c.sub("Network.responseReceived", c.onResponse)
	c.sub("Network.loadingFinished", c.onFinished)
	c.sub("Network.loadingFailed", c.onFailed)
	return c, nil
}

// sub 订阅一个事件并起协程处理（事件只属于本标签页）。
//
// 注意：cdp.Subscribe 的 cancel 只是解除订阅、不关闭通道（否则 Publish 会 panic），
// 所以这里额外用 done 信号让协程退出，避免每次录制都漏 4 个 goroutine。
func (c *NetCapture) sub(method string, fn func(json.RawMessage)) {
	ch, cancel := c.page.client.Subscribe(method)
	c.cancels = append(c.cancels, cancel)
	go func() {
		for {
			select {
			case <-c.done:
				return
			case ev := <-ch:
				if ev.SessionID != "" && c.page.SessionID != "" && ev.SessionID != c.page.SessionID {
					continue // 扁平化会话下的其他标签页事件，忽略
				}
				c.mu.Lock()
				stop := c.stopped
				c.mu.Unlock()
				if stop {
					return
				}
				fn(ev.Params)
			}
		}
	}()
}

/* --------------------------------------------------------------- 事件处理 */

func (c *NetCapture) onRequest(raw json.RawMessage) {
	var p struct {
		RequestID   string  `json:"requestId"`
		Type        string  `json:"type"`
		Timestamp   float64 `json:"timestamp"`
		DocumentURL string  `json:"documentURL"`
		Request     struct {
			URL         string         `json:"url"`
			Method      string         `json:"method"`
			Headers     map[string]any `json:"headers"`
			PostData    string         `json:"postData"`
			HasPostData bool           `json:"hasPostData"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.RequestID == "" {
		return
	}
	if !c.wantedType(p.Type) {
		return
	}
	u, err := url.Parse(p.Request.URL)
	if err != nil || u.Scheme == "" {
		return
	}
	// data: / blob: 这类没有意义的请求丢掉
	switch u.Scheme {
	case "http", "https":
	default:
		if p.Type != "Document" {
			return
		}
	}

	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	// 重定向会复用同一个 requestId：后一次覆盖前一次即可
	item, exists := c.items[p.RequestID]
	if !exists {
		if len(c.order) >= c.opts.MaxEntries {
			c.dropped++
			c.mu.Unlock()
			return
		}
		c.seq++
		item = &model.CapturedRequest{
			ID:      p.RequestID,
			Index:   c.seq,
			At:      time.Now(),
			Type:    p.Type,
			PageURL: firstNonEmptyStr(p.DocumentURL, c.opts.PageURL),
		}
		c.items[p.RequestID] = item
		c.order = append(c.order, p.RequestID)
	}
	item.Method = p.Request.Method
	item.URL = p.Request.URL
	item.Host = u.Host
	item.Path = u.Path
	item.Query = u.RawQuery
	item.Kind = kindOf(p.Type)
	item.Type = p.Type
	if c.opts.CaptureBodies && len(p.Request.Headers) > 0 {
		item.ReqHeaders = stringifyHeaders(p.Request.Headers)
	}
	if p.Request.HasPostData && p.Request.PostData != "" {
		item.PostData = truncate(p.Request.PostData, 64<<10)
	}
	c.mu.Unlock()
}

func (c *NetCapture) onResponse(raw json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
		Type      string `json:"type"`
		Response  struct {
			URL               string         `json:"url"`
			Status            float64        `json:"status"`
			MimeType          string         `json:"mimeType"`
			Headers           map[string]any `json:"headers"`
			EncodedDataLength float64        `json:"encodedDataLength"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	c.mu.Lock()
	item, ok := c.items[p.RequestID]
	if ok {
		item.Status = int(p.Response.Status)
		item.MimeType = p.Response.MimeType
		if item.URL == "" {
			item.URL = p.Response.URL
		}
		if item.Bytes == 0 {
			item.Bytes = int(p.Response.EncodedDataLength)
		}
	}
	c.mu.Unlock()
}

func (c *NetCapture) onFinished(raw json.RawMessage) {
	var p struct {
		RequestID         string  `json:"requestId"`
		Timestamp         float64 `json:"timestamp"`
		EncodedDataLength float64 `json:"encodedDataLength"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	c.mu.Lock()
	item, ok := c.items[p.RequestID]
	if !ok {
		c.mu.Unlock()
		return
	}
	if p.EncodedDataLength > 0 {
		item.Bytes = int(p.EncodedDataLength)
	}
	if item.DurationMS == 0 {
		item.DurationMS = time.Since(item.At).Milliseconds()
	}
	c.mu.Unlock()

	if c.opts.CaptureBodies {
		select {
		case c.bodyQ <- p.RequestID:
		default: // 队列满就放弃响应体，不阻塞事件处理
		}
	}
}

func (c *NetCapture) onFailed(raw json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
		ErrorText string `json:"errorText"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	c.mu.Lock()
	if item, ok := c.items[p.RequestID]; ok {
		item.Error = p.ErrorText
	}
	c.mu.Unlock()
}

/* ------------------------------------------------------------- 响应体抓取 */

func (c *NetCapture) worker() {
	defer close(c.workerOK)
	for {
		select {
		case rid := <-c.bodyQ:
			c.fetchBody(rid)
		case <-c.bodyStop:
			for {
				select {
				case rid := <-c.bodyQ:
					c.fetchBody(rid)
				default:
					return
				}
			}
		}
	}
}

func (c *NetCapture) fetchBody(requestID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var res struct {
		Body          string `json:"body"`
		Base64Encoded bool   `json:"base64Encoded"`
	}
	if err := c.page.call(ctx, "Network.getResponseBody", map[string]any{"requestId": requestID}, &res); err != nil {
		return
	}
	body := res.Body
	if res.Base64Encoded {
		if raw, err := base64.StdEncoding.DecodeString(body); err == nil {
			body = string(raw)
		}
	}
	if body == "" {
		return
	}
	size := len(body)
	max := c.opts.MaxBodyBytes
	truncated := false
	if size > max {
		body = body[:max]
		truncated = true
	}
	c.mu.Lock()
	if item, ok := c.items[requestID]; ok {
		item.HasBody = true
		item.BodyPreview = body
		item.BodySize = size
		item.BodyTruncated = truncated
	}
	c.mu.Unlock()
}

/* ------------------------------------------------------------------ 生命周期 */

// Snapshot 取当前已录到的请求（按录制顺序）。
func (c *NetCapture) Snapshot() []model.CapturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]model.CapturedRequest, 0, len(c.order))
	for _, id := range c.order {
		if it, ok := c.items[id]; ok {
			out = append(out, *it)
		}
	}
	return out
}

// Count 已录条数。
func (c *NetCapture) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.order)
}

// Stop 停止录制并返回最终结果（幂等）。
func (c *NetCapture) Stop() []model.CapturedRequest {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.stopped = true
		c.mu.Unlock()
		for _, cancel := range c.cancels {
			cancel()
		}
		// 给还在路上的响应体一点时间落地，再放事件协程退出
		time.Sleep(250 * time.Millisecond)
		close(c.done)
		close(c.bodyStop)
		select {
		case <-c.workerOK:
		case <-time.After(6 * time.Second):
		}
	})
	return c.Snapshot()
}

/* ---------------------------------------------------------------- Cookie 导出 */

// Cookie 一条浏览器 Cookie（导出给定时采集复用登录态）。
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"http_only"`
	Secure   bool    `json:"secure"`
}

// Cookies 导出与给定 URL 相关的 Cookie（用于让定时任务带着登录态访问接口）。
//
// 只读本机浏览器的 Cookie 存储，不做任何"伪造/轮换"：采集请求用的就是
// 录制时那个真实登录态。
func (p *Page) Cookies(ctx context.Context, urls []string) ([]Cookie, error) {
	if len(urls) == 0 {
		return nil, nil
	}
	var res struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := p.call(ctx, "Network.getCookies", map[string]any{"urls": urls}, &res); err != nil {
		return nil, err
	}
	return res.Cookies, nil
}

/* -------------------------------------------------------------------- 辅助 */

func kindOf(cdpType string) string {
	switch strings.ToLower(cdpType) {
	case "xhr", "fetch":
		return "api"
	case "document":
		return "page"
	case "script", "stylesheet", "image", "font", "media", "manifest", "texttrack", "websocket", "other", "prefetch", "signedexchange":
		return "static"
	default:
		return "other"
	}
}

func stringifyHeaders(in map[string]any) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		switch tv := v.(type) {
		case string:
			out[k] = tv
		case float64:
			out[k] = jsonNumber(tv)
		case bool:
			if tv {
				out[k] = "true"
			} else {
				out[k] = "false"
			}
		default:
			if b, err := json.Marshal(tv); err == nil {
				out[k] = string(b)
			}
		}
	}
	return out
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func truncate(s string, max int) string {
	if max > 0 && len(s) > max {
		return s[:max]
	}
	return s
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
