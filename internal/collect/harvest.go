package collect

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"uitest-server/internal/browser"
	"uitest-server/internal/bus"
	"uitest-server/internal/model"
	"uitest-server/internal/store"
)

// 本文件是"怎么抓"：一次采集执行的完整流程。
//
// 礼貌采集（默认值都偏保守，可在任务里改，但有下限）：
//   - 相邻请求之间至少 MinIntervalMS，并叠加 JitterPercent 随机抖动
//   - 单任务同一时刻只有一个执行，绝不并发打对方
//   - 429 / 503 按 Retry-After 等待，重试次数用尽就停
//   - 401 / 403 视为会话失效或明确拒绝：立即停手并提示怎么处理
//   - 连续失败达到阈值 → 本次运行熔断，任务自动暂停

// Harvester 采集执行器。
type Harvester struct {
	store   *store.DB
	br      *browser.Manager
	bus     *bus.Bus
	dataDir string

	mu       sync.Mutex
	running  map[string]string             // taskID → runID
	cancels  map[string]context.CancelFunc // runID → cancel
	cookieMu sync.Mutex
	cookies  map[string]cachedCookies // 来源站点 → 缓存的浏览器 Cookie

	// 礼貌采集的节流状态：每个任务记最近一次请求时间
	reqMu   sync.Mutex
	lastReq map[string]time.Time
}

type cachedCookies struct {
	list    []*http.Cookie
	fetched time.Time
}

// NewHarvester 创建执行器。
func NewHarvester(db *store.DB, br *browser.Manager, b *bus.Bus, dataDir string) *Harvester {
	return &Harvester{
		store:   db,
		br:      br,
		bus:     b,
		dataDir: dataDir,
		running: map[string]string{},
		cancels: map[string]context.CancelFunc{},
		cookies: map[string]cachedCookies{},
		lastReq: map[string]time.Time{},
	}
}

// Running 任务是否正在执行。
func (h *Harvester) Running(taskID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.running[taskID]
	return ok
}

// Start 异步执行一次采集。trigger 为 manual 或 schedule。
func (h *Harvester) Start(taskID, trigger string) (model.HarvestRun, error) {
	task, err := h.store.GetHarvestTask(taskID)
	if err != nil {
		return model.HarvestRun{}, err
	}
	if task.EndpointCount() == 0 {
		return model.HarvestRun{}, fmt.Errorf("这条任务里没有启用任何接口")
	}
	h.mu.Lock()
	if runID, busy := h.running[taskID]; busy {
		h.mu.Unlock()
		return model.HarvestRun{}, fmt.Errorf("任务正在执行中（%s），请等它跑完", runID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run, err := h.store.CreateHarvestRun(model.HarvestRun{
		TaskID:    task.ID,
		TaskName:  task.Name,
		Status:    "queued",
		Trigger:   trigger,
		StartedAt: time.Now(),
		Sink:      describeSink(task.Sink),
	})
	if err != nil {
		h.mu.Unlock()
		cancel()
		return model.HarvestRun{}, err
	}
	h.running[taskID] = run.ID
	h.cancels[run.ID] = cancel
	h.mu.Unlock()

	go h.execute(ctx, run.ID)
	return run, nil
}

// Cancel 取消一次执行。
func (h *Harvester) Cancel(runID string) error {
	h.mu.Lock()
	cancel, ok := h.cancels[runID]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("这次运行已经结束或不存在")
	}
	cancel()
	return nil
}

func (h *Harvester) release(taskID, runID string) {
	h.mu.Lock()
	if cur, ok := h.running[taskID]; ok && cur == runID {
		delete(h.running, taskID)
	}
	if cancel, ok := h.cancels[runID]; ok {
		cancel()
		delete(h.cancels, runID)
	}
	h.mu.Unlock()
}

/* ------------------------------------------------------------------ 执行 */

func (h *Harvester) execute(ctx context.Context, runID string) {
	run, err := h.store.GetHarvestRun(runID)
	if err != nil {
		return
	}
	defer h.release(run.TaskID, runID)

	task, err := h.store.GetHarvestTask(run.TaskID)
	if err != nil {
		h.finish(runID, run.TaskID, "failed", "任务不存在，可能已被删除")
		return
	}
	task.Normalize()
	h.setStatus(runID, "running")
	h.logf(runID, "info", "开始采集「%s」：%d 个接口，触发方式=%s", task.Name, task.EndpointCount(), triggerLabel(run.Trigger))

	sink, err := OpenSink(task.Sink, h.dataDir)
	if err != nil {
		h.finish(runID, task.ID, "failed", "无法连接落库目标："+err.Error())
		return
	}
	defer sink.Close()
	_, _ = h.store.MutateHarvestRun(runID, func(r *model.HarvestRun) { r.Sink = sink.Describe() })
	h.logf(runID, "info", "写入目标：%s", sink.Describe())

	sess, err := h.resolveSession(ctx, &task)
	if err != nil {
		h.logf(runID, "warn", "会话准备失败（将用无登录态请求）：%v", err)
	} else if sess.cookieCount > 0 {
		h.logf(runID, "info", "会话已就绪：携带 %d 个 Cookie（来源：%s）", sess.cookieCount, sessionLabel(task.Session))
	}

	client := h.newClient(task.Throttle, sess)

	totalReq, totalFetched, totalWritten := 0, 0, 0
	failStreak := 0
	okCount, failCount := 0, 0
	stopAll := false
	stopReason := ""

	for _, ep := range task.Endpoints {
		if !ep.Enabled {
			continue
		}
		if stopAll || ctx.Err() != nil {
			break
		}
		h.logf(runID, "info", "▶ 采集接口 %s %s", strings.ToUpper(ep.Method), ep.URL)
		res := h.collectEndpoint(ctx, runID, &task, ep, client, sess, sink)
		totalReq += res.Pages
		totalFetched += res.Fetched
		totalWritten += res.Written
		_, _ = h.store.MutateHarvestRun(runID, func(r *model.HarvestRun) {
			r.Endpoints = append(r.Endpoints, res)
			r.Requests = totalReq
			r.Fetched = totalFetched
			r.Written = totalWritten
		})

		if res.Error != "" {
			failStreak++
			failCount++
			h.logf(runID, "error", "接口失败：%s（%s）", res.Error, res.Hint)
			if strings.Contains(res.Hint, "停止") {
				stopAll = true
				stopReason = res.Error
			}
			if failStreak >= task.Throttle.MaxFailStreak {
				stopAll = true
				stopReason = fmt.Sprintf("连续 %d 个接口失败，本次运行熔断（既保护对方服务器，也避免自己变成异常流量）", failStreak)
				h.logf(runID, "warn", "%s", stopReason)
			}
		} else {
			failStreak = 0
			okCount++
			h.logf(runID, "info", "接口完成：抓取 %d 条，写入 %d 行（%d 次请求）", res.Fetched, res.Written, res.Pages)
		}
	}

	if ctx.Err() != nil {
		h.finish(runID, task.ID, "canceled", "已手动取消")
		return
	}
	// 一个接口都没成功 = 这次运行失败（哪怕中途没有触发熔断）
	if okCount == 0 && failCount > 0 {
		if stopReason == "" {
			stopReason = fmt.Sprintf("全部 %d 个接口都失败了，请点开接口结果看具体原因", failCount)
		}
		h.finish(runID, task.ID, "failed", stopReason)
		return
	}
	msg := fmt.Sprintf("完成：%d 次请求，解析 %d 条记录，写入 %d 行", totalReq, totalFetched, totalWritten)
	if failCount > 0 {
		msg += fmt.Sprintf("；其中 %d 个接口失败", failCount)
	}
	if stopReason != "" {
		msg += "；提前停止：" + stopReason
	}
	h.finish(runID, task.ID, "passed", msg)
}

// collectEndpoint 抓一个接口（自动翻页，直到满足停止条件）。
//
// 翻页策略交给 Paginator：
//   - 它把当前页参数写进 URL 或请求体（page / offset / cursor 三种）；
//   - 它根据响应（总数 / has_more / 游标 / 短页）判断是否还有下一页；
//   - 它对"连续两页完全相同"做指纹检测，防止接口忽略分页参数时白跑 50 页；
//   - 跨页重复的行会被跳过（Duplicates 计数报给你）。
func (h *Harvester) collectEndpoint(ctx context.Context, runID string, task *model.HarvestTask, ep model.HarvestEndpoint,
	client *http.Client, sess resolvedSession, sink Sink) model.HarvestEndpointResult {

	started := time.Now()
	res := model.HarvestEndpointResult{
		EndpointID: ep.ID, Name: ep.Name, Method: strings.ToUpper(ep.Method), URL: ep.URL,
	}
	pg := NewPaginator(ep.Pagination)
	h.logf(runID, "info", "分页策略：%s", pg.Describe())

	maxPages := 1
	if pg.Enabled() {
		// 接口上填了最多页数就以它为准；没填才用任务的分页默认上限。
		// 两者都受 MaxPagesHardLimit 兜底，防止配置写错把对方打穿。
		maxPages = pg.pag.MaxPages
		if maxPages <= 0 {
			maxPages = task.Throttle.MaxPagesCap
		}
		if maxPages <= 0 || maxPages > model.MaxPagesHardLimit {
			maxPages = model.MaxPagesHardLimit
		}
	}

	var allRows []map[string]any
	seen := map[string]bool{}
	fields := ep.Fields
	autoFields := false
	duplicates := 0

	for i := 0; i < maxPages; i++ {
		if ctx.Err() != nil {
			return res
		}
		vars := map[string]string{
			"page":      pg.Current(),
			"host":      hostOf(ep.URL),
			"url":       ep.URL,
			"endpoint":  ep.Name,
			"now":       time.Now().Format("2006-01-02 15:04:05"),
			"date":      time.Now().Format("2006-01-02"),
			"datetime":  time.Now().Format("2006-01-02 15:04:05"),
			"timestamp": strconv.FormatInt(time.Now().Unix(), 10),
			"uuid":      randomUUID(),
		}
		target, body := pg.Apply(expand(ep.URL, vars), expand(ep.Body, vars))

		h.throttleWait(ctx, task.ID, task.Throttle)
		status, raw, retries, err := h.fetch(ctx, client, ep, target, body, sess, task.Throttle)
		res.Pages++
		res.Retries += retries
		res.Bytes += len(raw)
		if err != nil {
			res.Error = err.Error()
			switch {
			case status == http.StatusUnauthorized || status == http.StatusForbidden:
				res.Hint = "会话失效或接口拒绝访问：请在内置浏览器里重新登录后重新录制，或更新任务的 Cookie；已停止后续接口"
			case status == http.StatusTooManyRequests:
				res.Hint = "对方返回 429（限流）：已停止后续接口，建议把请求间隔调大或降低频率"
			case ctx.Err() != nil:
				res.Hint = "已取消"
			default:
				res.Hint = "检查 URL / 参数 / 会话，必要时先点「试抓预览」看原始响应"
			}
			return res
		}
		res.Status = status

		root, perr := ParseJSONBody(string(raw))
		if perr != nil {
			res.Error = "响应解析失败：" + perr.Error()
			res.Hint = "响应不是 JSON（可能是 HTML 登录页或被拦截），请检查 URL 与会话"
			return res
		}
		records, path, rerr := RecordsFrom(root, ep.RecordsPath)
		if rerr != nil {
			res.Error = rerr.Error()
			res.Hint = "记录路径不对：把「记录路径」改成响应里真正的数组位置（例如 data.list）"
			return res
		}
		if len(records) == 0 {
			res.StopReason = "第 " + pg.Current() + " 页没有数据，停止翻页"
			h.logf(runID, "info", "%s", res.StopReason)
			break
		}
		if pg.Enabled() && pg.LoopDetected(records) {
			res.StopReason = "连续两页数据完全相同，判定接口忽略了分页参数，已停止（请检查分页参数名/位置）"
			h.logf(runID, "warn", "%s", res.StopReason)
			break
		}
		if len(fields) == 0 {
			fields = SuggestFields(records, 5)
			autoFields = true
			h.logf(runID, "warn", "接口没有配置字段映射，已按响应结构自动识别 %d 个字段（建议保存到任务里）", len(fields))
		}
		if ep.RecordsPath == "" && path != "" && i == 0 {
			h.logf(runID, "info", "自动识别记录数组位于 %s", path)
		}
		for _, rec := range records {
			row := FlattenRow(rec, fields, vars)
			if len(row) == 0 {
				continue
			}
			fp := rowFingerprint(row)
			if seen[fp] {
				duplicates++
				continue
			}
			seen[fp] = true
			row["_fetched_at"] = time.Now().Format("2006-01-02 15:04:05")
			allRows = append(allRows, row)
		}
		res.Fetched += len(records)

		if pg.Enabled() {
			h.logf(runID, "info", "第 %s 页：%d 条（累计 %d 条）", pg.Current(), len(records), len(allRows))
			if stop, reason := pg.Advance(records, root); stop {
				res.StopReason = reason
				h.logf(runID, "info", "停止翻页：%s", reason)
				break
			}
		}
	}
	res.Duplicates = duplicates
	if duplicates > 0 {
		h.logf(runID, "warn", "跨页出现 %d 条重复记录，已自动跳过（接口分页可能重叠）", duplicates)
	}

	if len(allRows) > 0 {
		written, werr := sink.Write(allRows)
		res.Written = written
		if werr != nil {
			res.Error = "写入失败：" + werr.Error()
			res.Hint = "检查落库配置（表名 / 账号权限 / 字段类型）"
			return res
		}
	}
	if autoFields {
		res.Hint = "字段映射是自动识别的，建议在任务详情里保存字段映射"
	}
	res.DurationMS = time.Since(started).Milliseconds()
	return res
}

// fetch 发一次请求（含重试与礼貌退避）。返回状态码、响应体、重试次数、错误。
func (h *Harvester) fetch(ctx context.Context, client *http.Client, ep model.HarvestEndpoint, target, body string,
	sess resolvedSession, th model.HarvestThrottle) (int, []byte, int, error) {

	var lastStatus int
	var lastErr error
	for attempt := 0; attempt <= th.Retries; attempt++ {
		if ctx.Err() != nil {
			return lastStatus, nil, attempt, ctx.Err()
		}
		var reader io.Reader
		if strings.TrimSpace(body) != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, strings.ToUpper(ep.Method), target, reader)
		if err != nil {
			return 0, nil, attempt, fmt.Errorf("URL 不合法：%w", err)
		}
		for k, v := range ep.Headers {
			req.Header.Set(k, expand(v, sessionVars(sess)))
		}
		if req.Header.Get("Accept") == "" {
			req.Header.Set("Accept", "application/json, text/plain, */*")
		}
		if sess.userAgent != "" {
			req.Header.Set("User-Agent", sess.userAgent)
		}
		if sess.referer != "" && req.Header.Get("Referer") == "" {
			req.Header.Set("Referer", sess.referer)
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if attempt < th.Retries {
				if !sleepCtx(ctx, backoff(th.BackoffMS, attempt)) {
					return lastStatus, nil, attempt, ctx.Err()
				}
				continue
			}
			return lastStatus, nil, attempt, friendlyNetErr(lastErr)
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if attempt < th.Retries {
				if !sleepCtx(ctx, backoff(th.BackoffMS, attempt)) {
					return resp.StatusCode, nil, attempt, ctx.Err()
				}
				continue
			}
			return resp.StatusCode, nil, attempt, fmt.Errorf("读取响应失败：%w", readErr)
		}
		lastStatus = resp.StatusCode

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return resp.StatusCode, raw, attempt, nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
			wait := retryAfter(resp.Header.Get("Retry-After"))
			if wait <= 0 {
				wait = backoff(th.BackoffMS, attempt) * 3
			}
			if wait > 5*time.Minute {
				wait = 5 * time.Minute
			}
			lastErr = fmt.Errorf("对方返回 %d（限流/暂不可用）", resp.StatusCode)
			if attempt < th.Retries {
				h.logf("", "warn", "对方返回 %d，等待 %s 后重试（%d/%d）", resp.StatusCode, wait.Round(time.Second), attempt+1, th.Retries)
				if !sleepCtx(ctx, wait) {
					return resp.StatusCode, nil, attempt, ctx.Err()
				}
				continue
			}
			return resp.StatusCode, raw, attempt, lastErr
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return resp.StatusCode, raw, attempt, fmt.Errorf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
			if attempt < th.Retries {
				if !sleepCtx(ctx, backoff(th.BackoffMS, attempt)) {
					return resp.StatusCode, nil, attempt, ctx.Err()
				}
				continue
			}
			return resp.StatusCode, raw, attempt, lastErr
		default:
			return resp.StatusCode, raw, attempt, fmt.Errorf("HTTP %d %s（响应体前 200 字：%s）",
				resp.StatusCode, http.StatusText(resp.StatusCode), truncate(string(raw), 200))
		}
	}
	return lastStatus, nil, th.Retries, lastErr
}

/* ------------------------------------------------------------------ 会话 */

type resolvedSession struct {
	cookies     map[string][]*http.Cookie // 来源站点 → cookie
	cookieCount int
	userAgent   string
	referer     string
}

func (h *Harvester) newClient(th model.HarvestThrottle, sess resolvedSession) *http.Client {
	jar, _ := cookiejar.New(nil)
	for origin, list := range sess.cookies {
		u, err := url.Parse(origin)
		if err != nil || u.Scheme == "" {
			continue
		}
		jar.SetCookies(u, list)
	}
	timeout := time.Duration(th.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 3 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Jar: jar, Timeout: timeout, Transport: transport}
}

// resolveSession 准备登录态：只复用"录这条接口时的那个真实会话"，不伪造。
func (h *Harvester) resolveSession(ctx context.Context, task *model.HarvestTask) (resolvedSession, error) {
	sess := resolvedSession{cookies: map[string][]*http.Cookie{}}
	sess.userAgent = strings.TrimSpace(task.Session.UserAgent)
	sess.referer = strings.TrimSpace(task.Session.Referer)
	if sess.referer == "" {
		sess.referer = strings.TrimSpace(task.StartURL)
	}

	switch strings.ToLower(task.Session.Mode) {
	case "", "none":
		return sess, nil

	case "cookie":
		pairs := parseCookieHeader(task.Session.Cookie)
		if len(pairs) == 0 {
			return sess, fmt.Errorf("Cookie 配置为空")
		}
		for _, ep := range task.Endpoints {
			if !ep.Enabled {
				continue
			}
			origin := originOf(ep.URL)
			if origin == "" {
				continue
			}
			if _, ok := sess.cookies[origin]; ok {
				continue
			}
			var list []*http.Cookie
			for _, kv := range pairs {
				list = append(list, &http.Cookie{Name: kv[0], Value: kv[1]})
			}
			sess.cookies[origin] = list
			sess.cookieCount = len(list)
		}
		return sess, nil

	case "browser":
		target := strings.TrimSpace(task.Session.Host)
		if target == "" {
			for _, ep := range task.Endpoints {
				if ep.Enabled {
					target = originOf(ep.URL)
					break
				}
			}
		}
		if target == "" {
			return sess, fmt.Errorf("browser 会话模式需要填写站点地址")
		}
		urls := []string{target}
		for _, ep := range task.Endpoints {
			if o := originOf(ep.URL); o != "" && o != target {
				urls = append(urls, o)
			}
		}
		list, err := h.browserCookies(ctx, target, urls)
		if err != nil {
			return sess, err
		}
		if len(list) == 0 {
			return sess, fmt.Errorf("内置浏览器里没有 %s 的登录 Cookie，请先在内置浏览器里登录一次", target)
		}
		for _, ep := range task.Endpoints {
			if o := originOf(ep.URL); o != "" {
				sess.cookies[o] = list
			}
		}
		sess.cookieCount = len(list)
		return sess, nil

	default:
		return sess, fmt.Errorf("未知的会话模式 %q", task.Session.Mode)
	}
}

// browserCookies 从内置浏览器导出 Cookie（30 分钟内复用，避免每次跑都拉一次浏览器）。
func (h *Harvester) browserCookies(ctx context.Context, host string, urls []string) ([]*http.Cookie, error) {
	h.cookieMu.Lock()
	if c, ok := h.cookies[host]; ok && time.Since(c.fetched) < 30*time.Minute {
		h.cookieMu.Unlock()
		return c.list, nil
	}
	h.cookieMu.Unlock()

	openCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	page, err := h.br.OpenPage(openCtx, "about:blank")
	if err != nil {
		return nil, fmt.Errorf("启动内置浏览器失败（%s）：%w；可改用 cookie 模式或检查 browser 配置", host, err)
	}
	defer page.Close(context.Background())
	jar, err := page.Cookies(openCtx, urls)
	if err != nil {
		return nil, fmt.Errorf("读取浏览器 Cookie 失败：%w", err)
	}
	var out []*http.Cookie
	for _, c := range jar {
		if c.Value == "" {
			continue
		}
		hc := &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Secure: c.Secure, HttpOnly: c.HTTPOnly}
		if c.Expires > 0 {
			hc.Expires = time.Unix(int64(c.Expires), 0)
		}
		out = append(out, hc)
	}
	h.cookieMu.Lock()
	h.cookies[host] = cachedCookies{list: out, fetched: time.Now()}
	h.cookieMu.Unlock()
	return out, nil
}

/* -------------------------------------------------------------- 限速/退避 */

// throttleWait 保证同一任务的相邻请求之间至少间隔 MinIntervalMS，并叠加随机抖动。
func (h *Harvester) throttleWait(ctx context.Context, taskID string, th model.HarvestThrottle) {
	min := th.MinIntervalMS
	if min < 200 {
		min = 200
	}
	base := jitterDuration(time.Duration(min)*time.Millisecond, th.JitterPercent)

	h.reqMu.Lock()
	last := h.lastReq[taskID]
	h.reqMu.Unlock()
	if !last.IsZero() {
		if wait := base - time.Since(last); wait > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		}
	}
	h.reqMu.Lock()
	h.lastReq[taskID] = time.Now()
	h.reqMu.Unlock()
}

func backoff(baseMS, attempt int) time.Duration {
	if baseMS <= 0 {
		baseMS = 1500
	}
	d := time.Duration(baseMS) * time.Millisecond
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	if d > 2*time.Minute {
		d = 2 * time.Minute
	}
	return jitterDuration(d, 20)
}

func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func friendlyNetErr(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "context deadline exceeded"):
		return fmt.Errorf("请求超时（可在任务里调大 timeout_ms）")
	case strings.Contains(msg, "no such host"):
		return fmt.Errorf("域名解析失败：%v", err)
	case strings.Contains(msg, "connection refused"):
		return fmt.Errorf("连接被拒绝：%v", err)
	case strings.Contains(msg, "certificate"):
		return fmt.Errorf("TLS 证书校验失败：%v", err)
	default:
		return err
	}
}

/* ---------------------------------------------------------------- 状态维护 */

func (h *Harvester) setStatus(runID, status string) {
	_, _ = h.store.MutateHarvestRun(runID, func(r *model.HarvestRun) { r.Status = status })
	h.bus.Publish(runID, bus.Event{Type: "status", Status: status})
}

func (h *Harvester) finish(runID, taskID, status, message string) {
	now := time.Now()
	_, _ = h.store.MutateHarvestRun(runID, func(r *model.HarvestRun) {
		r.Status = status
		r.FinishedAt = now
		if status == "failed" || status == "canceled" {
			r.Error = message
		}
	})
	level := "info"
	switch status {
	case "failed":
		level = "error"
	case "canceled":
		level = "warn"
	}
	h.logf(runID, level, "%s", message)

	// 回写任务状态与下一次触发时间（先读运行结果，再进任务写锁，避免锁嵌套）
	written := 0
	if r, err := h.store.GetHarvestRun(runID); err == nil {
		written = r.Written
	}
	_, _ = h.store.MutateHarvestTask(taskID, func(t *model.HarvestTask) {
		t.LastRunAt = now
		t.LastStatus = status
		t.RunCount++
		t.LastWritten = written
		if status == "failed" {
			t.FailStreak++
			t.LastError = message
			if t.FailStreak >= 3 {
				t.Status = "paused"
				t.LastError = message + "（已连续失败 3 次，任务自动暂停）"
			}
		} else {
			t.FailStreak = 0
			t.LastError = ""
		}
		if next, ok := NextRunAt(t.Schedule, now); ok {
			t.NextRunAt = next
		} else {
			t.NextRunAt = time.Time{}
		}
	})

	h.bus.Publish(runID, bus.Event{Type: "status", Status: status})
	h.bus.Publish(runID, bus.Event{Type: "end", Status: status})
}

func (h *Harvester) logf(runID, level, format string, args ...any) {
	entry := model.LogEntry{TS: time.Now(), Level: level, Message: fmt.Sprintf(format, args...)}
	if runID == "" {
		log.Printf("[collect] %s: %s", entry.Level, entry.Message)
		return
	}
	_, _ = h.store.MutateHarvestRun(runID, func(r *model.HarvestRun) {
		r.Logs = append(r.Logs, entry)
		if len(r.Logs) > 3000 {
			r.Logs = r.Logs[len(r.Logs)-3000:]
		}
	})
	h.bus.Publish(runID, bus.Event{Type: "log", Log: &entry})
}

func triggerLabel(t string) string {
	if t == "schedule" {
		return "定时触发"
	}
	return "手动触发"
}

func sessionLabel(s model.HarvestSession) string {
	switch strings.ToLower(s.Mode) {
	case "cookie":
		return "手工 Cookie"
	case "browser":
		return "内置浏览器登录态"
	default:
		return "无会话"
	}
}

func describeSink(cfg model.HarvestSink) string {
	switch strings.ToLower(cfg.Driver) {
	case "jsonl":
		return "JSONL 文件 " + cfg.Table + ".jsonl"
	case "json":
		return "JSON 文件 " + cfg.Table + ".json"
	case "xlsx", "excel":
		return "Excel 表格 " + cfg.Table + ".xlsx"
	case "mysql":
		return "MySQL 表 " + cfg.Table
	case "postgres", "postgresql", "pgsql":
		return "PostgreSQL 表 " + cfg.Table
	default:
		return "SQLite 表 " + cfg.Table
	}
}

/* ------------------------------------------------------------ 模板与 URL */

var tmplVarRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_@:\-]+)\s*\}\}`)

// expand 展开 {{变量}}：内置时间变量、{{page}}、{{env:NAME}} 等。
func expand(tpl string, vars map[string]string) string {
	if !strings.Contains(tpl, "{{") {
		return tpl
	}
	return tmplVarRe.ReplaceAllStringFunc(tpl, func(all string) string {
		m := tmplVarRe.FindStringSubmatch(all)
		if len(m) < 2 {
			return all
		}
		name := strings.TrimSpace(m[1])
		if v, ok := vars[name]; ok {
			return v
		}
		if strings.HasPrefix(name, "env:") {
			return os.Getenv(strings.TrimSpace(strings.TrimPrefix(name, "env:")))
		}
		return all
	})
}

func sessionVars(sess resolvedSession) map[string]string {
	now := time.Now()
	return map[string]string{
		"now":       now.Format("2006-01-02 15:04:05"),
		"date":      now.Format("2006-01-02"),
		"datetime":  now.Format("2006-01-02 15:04:05"),
		"timestamp": strconv.FormatInt(now.Unix(), 10),
		"uuid":      randomUUID(),
	}
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}

func originOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func setQueryParam(raw, name, value string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set(name, value)
	u.RawQuery = q.Encode()
	return u.String()
}

func parseCookieHeader(s string) [][2]string {
	var out [][2]string
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		out = append(out, [2]string{strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])})
	}
	return out
}

func randomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func truncate(s string, max int) string {
	if max > 0 && len(s) > max {
		return s[:max]
	}
	return s
}

/* ------------------------------------------------------------ 试抓与预览 */

// PreviewResult 试抓（不写库）结果，给"先看数据长什么样再决定落库"用。
type PreviewResult struct {
	EndpointID  string               `json:"endpoint_id"`
	Name        string               `json:"name"`
	URL         string               `json:"url"`
	Status      int                  `json:"status"`
	Records     int                  `json:"records"`
	RecordsPath string               `json:"records_path"`
	Columns     []string             `json:"columns"`
	Rows        []map[string]any     `json:"rows"`
	Fields      []model.HarvestField `json:"fields"`
	// Pagination 分页策略描述（试抓只取第一页，不会真翻页）
	Pagination string `json:"pagination,omitempty"`
	// Total / EstimatedPages 响应里能读到总数时给出"预计抓多少条 / 多少页"
	Total          int    `json:"total,omitempty"`
	EstimatedPages int    `json:"estimated_pages,omitempty"`
	Error          string `json:"error,omitempty"`
	Hint           string `json:"hint,omitempty"`
}

// Preview 试抓一次并返回解析出的前 limit 行（不写库）。
func (h *Harvester) Preview(ctx context.Context, task model.HarvestTask, limit int) []PreviewResult {
	task.Normalize()
	if limit <= 0 {
		limit = 5
	}
	sess, err := h.resolveSession(ctx, &task)
	if err != nil {
		return []PreviewResult{{Error: "会话准备失败：" + err.Error(), Hint: "先把会话（登录态）配置好再试抓"}}
	}
	client := h.newClient(task.Throttle, sess)
	var out []PreviewResult
	first := true
	for _, ep := range task.Endpoints {
		if !ep.Enabled {
			continue
		}
		sel := ep
		pg := NewPaginator(ep.Pagination)
		vars := map[string]string{
			"page":      pg.Current(),
			"host":      hostOf(ep.URL),
			"url":       ep.URL,
			"endpoint":  ep.Name,
			"now":       time.Now().Format("2006-01-02 15:04:05"),
			"date":      time.Now().Format("2006-01-02"),
			"datetime":  time.Now().Format("2006-01-02 15:04:05"),
			"timestamp": strconv.FormatInt(time.Now().Unix(), 10),
			"uuid":      randomUUID(),
		}
		if !first {
			h.throttleWait(ctx, task.ID, task.Throttle)
		}
		first = false
		target, reqBody := pg.Apply(expand(ep.URL, vars), expand(ep.Body, vars))
		status, raw, _, ferr := h.fetch(ctx, client, sel, target, reqBody, sess, task.Throttle)
		pr := PreviewResult{EndpointID: ep.ID, Name: ep.Name, URL: target, Status: status, Pagination: pg.Describe()}
		if ferr != nil {
			pr.Error = ferr.Error()
			pr.Hint = "检查 URL / 会话 / 限流状态"
			out = append(out, pr)
			continue
		}
		root, perr := ParseJSONBody(string(raw))
		if perr != nil {
			pr.Error = "响应不是 JSON：" + perr.Error()
			pr.Rows = []map[string]any{{"raw": truncate(string(raw), 500)}}
			out = append(out, pr)
			continue
		}
		records, path, rerr := RecordsFrom(root, ep.RecordsPath)
		if rerr != nil {
			pr.Error = rerr.Error()
			out = append(out, pr)
			continue
		}
		pr.RecordsPath = path
		pr.Records = len(records)
		// 用第一页的响应反推"预计还有多少页"，让你试抓时就知道会抓多少
		if total, ok := ExtractInt(root, pg.pag.TotalPath); ok && pg.pag.PageSize > 0 && total > 0 {
			pr.EstimatedPages = (total + pg.pag.PageSize - 1) / pg.pag.PageSize
			pr.Total = total
		}
		if LooksPaginatedButUnknown(pg.pag, root, records) {
			pr.Hint = "响应里 total 大于本页条数，说明还有更多数据，但没识别出翻页参数——请在任务里手工指定分页参数名"
		}
		fields := ep.Fields
		if len(fields) == 0 {
			fields = SuggestFields(records, 5)
		}
		pr.Fields = fields
		pr.Columns = Columns(fields)
		rows := make([]map[string]any, 0, limit)
		for i, rec := range records {
			if i >= limit {
				break
			}
			rows = append(rows, FlattenRow(rec, fields, vars))
		}
		pr.Rows = rows
		out = append(out, pr)
	}
	return out
}

// AnalyzeResult 响应结构分析结果。
type AnalyzeResult struct {
	RecordsPath string               `json:"records_path"`
	Records     int                  `json:"records"`
	Fields      []model.HarvestField `json:"fields"`
	Columns     []string             `json:"columns"`
	Rows        []map[string]any     `json:"rows"`
	// Pagination 从这条请求推断出的分页策略（没有分页时为 none）
	Pagination     model.HarvestPagination `json:"pagination,omitempty"`
	PaginationText string                  `json:"pagination_text,omitempty"`
	Error          string                  `json:"error,omitempty"`
}

// AnalyzeBody 分析一段响应体：给出记录路径、候选字段、前几行预览。
func AnalyzeBody(body string, recordsPath string, limit int) AnalyzeResult {
	if limit <= 0 {
		limit = 5
	}
	root, err := ParseJSONBody(body)
	if err != nil {
		return AnalyzeResult{Error: "响应不是合法 JSON：" + err.Error()}
	}
	records, path, rerr := RecordsFrom(root, recordsPath)
	if rerr != nil {
		return AnalyzeResult{Error: rerr.Error()}
	}
	fields := SuggestFields(records, 5)
	res := AnalyzeResult{RecordsPath: path, Records: len(records), Fields: fields, Columns: Columns(fields)}
	for i, rec := range records {
		if i >= limit {
			break
		}
		res.Rows = append(res.Rows, FlattenRow(rec, fields, map[string]string{}))
	}
	if res.Rows == nil {
		res.Rows = []map[string]any{}
	}
	return res
}
