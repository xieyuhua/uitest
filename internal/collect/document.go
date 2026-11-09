package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"uitest-server/internal/browser"
	"uitest-server/internal/model"
)

// 本文件处理「页面文档（Document）」的补录：
//
//   - CDP 只在整页导航时才会发 Document 请求；先开始录制、之后才打开页面，
//     或者 SPA 前端路由改地址（history.pushState / #/xxx），都不会再有新的 Document；
//   - 插件录制（MV3）只能包 fetch / XHR，页面本身的 HTML 要靠 content script 上报；
//   - 所以这里提供「手动添加页面文档」和「在内置浏览器里打开页面（可带前置动作）并补录」
//     两条路，补录的条目带 manual 标记，和实时抓到的条目一样能勾选、建任务、识别字段。
//
// 注意：地址里的 #fragment 必须原样保留——hash 路由站点（如 /#/vip_member）剥掉
// fragment 就退化成首页 "/"，既看不出录的是哪一页，也会让两页互相误判成同一页。

// DocumentInput 「添加页面文档」/「打开页面补录」的入参。
type DocumentInput struct {
	URL    string
	Method string
	Status int
	Note   string
	// Body 页面 HTML / JSON 快照（可选，最多 256KB）
	Body string
}

// DocumentAction 「打开页面 → 执行动作 → 抓页面文档」里的一个前置动作。
//
// 支持的动作（action 字段）：
//   - open      打开一个地址（value 或 selector 填地址）
//   - type      在 selector 里输入 value（触发 input/change，兼容 React/Vue 受控组件）
//   - click     点击 selector
//   - select    把下拉框 selector 设成 value
//   - wait      等待 wait_ms 毫秒（默认 1000，最多 60000）
//   - wait_for  等 selector 出现（最多 timeout_ms，默认 8000）
//   - scroll    滚动到页面底部
//   - press     在 selector（不填则当前焦点）上按键，value 默认 Enter
type DocumentAction struct {
	Action    string `json:"action"`
	Selector  string `json:"selector"`
	Value     string `json:"value"`
	WaitMS    int    `json:"wait_ms"`
	TimeoutMS int    `json:"timeout_ms"`
	Note      string `json:"note"`
}

// docInfo 当前页面的信息：地址、状态码、content-type、大小、HTML。
// 用于「打开页面补录」和「初始页面文档自动补录」——CDP 没抓到 Document 时兜底。
const docInfoJS = `(function(){var e=(performance.getEntriesByType?performance.getEntriesByType("navigation"):[])[0]||null;var d=document.documentElement?document.documentElement.outerHTML:"";return {url:String(location.href),status:(e&&e.responseStatus)||0,mime:(e&&e.contentType)||"text/html",bytes:(e&&e.transferSize)||d.length,html:d.slice(0,262144),len:d.length};})()`

type docInfo struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
	Mime   string `json:"mime"`
	Bytes  int    `json:"bytes"`
	HTML   string `json:"html"`
	Len    int    `json:"len"`
}

/* ------------------------------------------------------------ 动作执行的 JS */

// actionFillJS 输入内容：走原生 value setter + input/change 事件，
// 这样 React / Vue 的受控组件也会更新状态（直接 el.value= 常常被框架覆盖回去）。
const actionFillJS = `(function(sel,val){var el=sel?document.querySelector(sel):null;if(!el)return "no-el";try{el.focus();}catch(e){}var done=false;try{var proto=null;if(window.HTMLTextAreaElement&&el instanceof HTMLTextAreaElement){proto=HTMLTextAreaElement.prototype;}else if(window.HTMLInputElement){proto=HTMLInputElement.prototype;}if(proto){var d=Object.getOwnPropertyDescriptor(proto,"value");if(d&&d.set){d.set.call(el,val);done=true;}}}catch(e){}if(!done){el.value=val;}try{el.dispatchEvent(new Event("input",{bubbles:true}));el.dispatchEvent(new Event("change",{bubbles:true}));}catch(e){}return "ok";})(%s,%s)`

// actionClickJS 点击元素（先滚到可视区，再走原生 click）。
const actionClickJS = `(function(sel){var el=sel?document.querySelector(sel):null;if(!el)return "no-el";try{el.scrollIntoView({block:"center"});}catch(e){}try{el.focus&&el.focus();}catch(e){}el.click();return "ok";})(%s)`

// actionSelectJS 选择下拉框的值。
const actionSelectJS = `(function(sel,val){var el=sel?document.querySelector(sel):null;if(!el)return "no-el";el.value=val;try{el.dispatchEvent(new Event("input",{bubbles:true}));el.dispatchEvent(new Event("change",{bubbles:true}));}catch(e){}return "ok";})(%s,%s)`

// actionExistsJS 元素是否已经在页面上。
const actionExistsJS = `(function(sel){try{return !!document.querySelector(sel);}catch(e){return false;}})(%s)`

// actionPressJS 按键（默认 Enter）：键盘事件发给元素或当前焦点。
const actionPressJS = `(function(sel,key){var el=sel?document.querySelector(sel):document.activeElement;if(!el)return "no-el";try{el.focus&&el.focus();}catch(e){}var opts={key:key,bubbles:true,cancelable:true};try{el.dispatchEvent(new KeyboardEvent("keydown",opts));el.dispatchEvent(new KeyboardEvent("keyup",opts));}catch(e){}return "ok";})(%s,%s)`

/* ------------------------------------------------------------ 补录入口 */

// hasType 判断录制范围里有没有某个资源类型（大小写不敏感）。
func hasType(list []string, want string) bool {
	for _, t := range list {
		if strings.EqualFold(strings.TrimSpace(t), want) {
			return true
		}
	}
	return false
}

// AddDocument 往录制结果里补一条页面文档（Document）。
//
// 内置浏览器会话、插件会话都支持；重复补同一个地址会新建一条（用户可能就是想
// 记两次不同时间的页面），但同一 id 不会重复写入。
func (r *Recorder) AddDocument(id string, in DocumentInput) (model.CapturedRequest, error) {
	item, err := buildDocumentItem(in)
	if err != nil {
		return model.CapturedRequest{}, err
	}
	added, err := r.ingest(id, []model.CapturedRequest{item}, true)
	if err != nil {
		return model.CapturedRequest{}, err
	}
	if len(added) == 0 {
		return model.CapturedRequest{}, fmt.Errorf("这条页面文档没有写进录制结果（会话条数已满）")
	}
	return added[0], nil
}

// OpenPage 在内置浏览器的录制标签页里打开一个页面，并把它的页面文档补录进结果。
func (r *Recorder) OpenPage(ctx context.Context, id, rawURL string) (model.CapturedRequest, error) {
	item, _, err := r.OpenPageWithActions(ctx, id, rawURL, nil)
	return item, err
}

// OpenPageWithActions 在内置浏览器里打开页面、(可选)执行一串前置动作，再补录页面文档。
//
// 典型用法：「打开登录页 → 输入账号 → 输入密码 → 点击登录 → 等列表出现 → 抓文档」，
// 这样录下来的就是登录后那一页的真实 HTML，而不是登录页的。
// 返回补录的条目和每一步的执行日志（前端直接把日志展示给用户）。
func (r *Recorder) OpenPageWithActions(ctx context.Context, id, rawURL string, actions []DocumentAction) (model.CapturedRequest, []string, error) {
	target, err := normalizePageURL(rawURL)
	if err != nil {
		return model.CapturedRequest{}, nil, err
	}
	s, ok := r.Get(id)
	if !ok {
		return model.CapturedRequest{}, nil, fmt.Errorf("录制会话不存在或已被清理（服务重启后会清空）")
	}
	if s.Source != "browser" || s.Page == nil {
		return model.CapturedRequest{}, nil, fmt.Errorf("这个会话是浏览器插件录制的，没有内置浏览器标签页；请改用「手动添加页面文档」")
	}

	logs := make([]string, 0, len(actions)+2)
	navCtx, navCancel := context.WithTimeout(ctx, 45*time.Second)
	navErr := s.Page.Navigate(navCtx, target)
	navCancel()

	r.mu.Lock()
	if cur, ok := r.sessions[id]; ok {
		cur.PageURL = target
	}
	r.mu.Unlock()

	if navErr != nil {
		logs = append(logs, fmt.Sprintf("打开 %s 失败：%v", target, navErr))
	} else {
		r.waitPageReady(ctx, s.Page, 15*time.Second)
		logs = append(logs, "打开 "+target)
	}

	if len(actions) > 0 {
		alogs, aerr := r.runActions(ctx, s.Page, actions)
		logs = append(logs, alogs...)
		if aerr != nil {
			return model.CapturedRequest{}, logs, aerr
		}
	}

	note := "打开页面补录"
	if len(actions) > 0 {
		note = "打开页面 + 前置动作补录"
	}
	item, derr := r.ensureDocument(ctx, id, target, note)
	if derr != nil {
		if navErr != nil {
			return model.CapturedRequest{}, logs, fmt.Errorf("打开页面失败：%v", navErr)
		}
		return model.CapturedRequest{}, logs, derr
	}
	logs = append(logs, "已补录页面文档："+item.URL)
	return item, logs, nil
}

// runActions 依次执行前置动作，返回每一步的日志（文案直接给用户看）。
func (r *Recorder) runActions(ctx context.Context, page *browser.Page, actions []DocumentAction) ([]string, error) {
	logs := make([]string, 0, len(actions))
	add := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	for i, a := range actions {
		kind := strings.ToLower(strings.TrimSpace(a.Action))
		if kind == "" {
			continue
		}
		n := i + 1
		sel := strings.TrimSpace(a.Selector)

		switch kind {
		case "open", "goto", "navigate", "url":
			raw := strings.TrimSpace(a.Value)
			if raw == "" {
				raw = sel
			}
			if raw == "" {
				return logs, fmt.Errorf("第 %d 步（打开页面）缺少地址", n)
			}
			u, err := normalizePageURL(raw)
			if err != nil {
				return logs, fmt.Errorf("第 %d 步打开页面失败：%v", n, err)
			}
			navCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			navErr := page.Navigate(navCtx, u)
			cancel()
			if navErr != nil {
				return logs, fmt.Errorf("第 %d 步打开 %s 失败：%v", n, u, navErr)
			}
			r.waitPageReady(ctx, page, 15*time.Second)
			add("%d. 打开 %s", n, u)

		case "type", "input", "fill", "set":
			if sel == "" {
				return logs, fmt.Errorf("第 %d 步（输入内容）缺少选择器", n)
			}
			res, err := page.Eval(ctx, fmt.Sprintf(actionFillJS, jsonStr(sel), jsonStr(a.Value)))
			if err != nil {
				return logs, fmt.Errorf("第 %d 步在 %s 输入失败：%v", n, sel, err)
			}
			if actionMissing(res) {
				return logs, fmt.Errorf("第 %d 步在 %s 输入失败：页面上找不到这个元素", n, sel)
			}
			add("%d. 输入 %s ← %s", n, sel, briefValue(sel, a.Value))

		case "click", "tap":
			if sel == "" {
				return logs, fmt.Errorf("第 %d 步（点击元素）缺少选择器", n)
			}
			res, err := page.Eval(ctx, fmt.Sprintf(actionClickJS, jsonStr(sel)))
			if err != nil {
				return logs, fmt.Errorf("第 %d 步点击 %s 失败：%v", n, sel, err)
			}
			if actionMissing(res) {
				return logs, fmt.Errorf("第 %d 步点击 %s 失败：页面上找不到这个元素", n, sel)
			}
			r.waitPageReady(ctx, page, 15*time.Second)
			add("%d. 点击 %s", n, sel)

		case "select", "choose":
			if sel == "" {
				return logs, fmt.Errorf("第 %d 步（选择下拉）缺少选择器", n)
			}
			res, err := page.Eval(ctx, fmt.Sprintf(actionSelectJS, jsonStr(sel), jsonStr(a.Value)))
			if err != nil {
				return logs, fmt.Errorf("第 %d 步设置 %s 失败：%v", n, sel, err)
			}
			if actionMissing(res) {
				return logs, fmt.Errorf("第 %d 步设置 %s 失败：页面上找不到这个元素", n, sel)
			}
			add("%d. 选择 %s = %s", n, sel, a.Value)

		case "wait", "sleep":
			ms := a.WaitMS
			if ms <= 0 {
				ms = 1000
			}
			if ms > 60000 {
				ms = 60000
			}
			select {
			case <-ctx.Done():
				return logs, ctx.Err()
			case <-time.After(time.Duration(ms) * time.Millisecond):
			}
			add("%d. 等待 %dms", n, ms)

		case "wait_for", "waitfor", "wait_selector":
			if sel == "" {
				return logs, fmt.Errorf("第 %d 步（等待元素）缺少选择器", n)
			}
			timeout := a.TimeoutMS
			if timeout <= 0 {
				timeout = 8000
			}
			deadline := time.Now().Add(time.Duration(timeout) * time.Millisecond)
			found := false
			for {
				res, err := page.Eval(ctx, fmt.Sprintf(actionExistsJS, jsonStr(sel)))
				if err == nil && strings.Contains(string(res), "true") {
					found = true
					break
				}
				if time.Now().After(deadline) {
					break
				}
				select {
				case <-ctx.Done():
					return logs, ctx.Err()
				case <-time.After(200 * time.Millisecond):
				}
			}
			if !found {
				return logs, fmt.Errorf("第 %d 步等待元素 %s 超时（%dms）", n, sel, timeout)
			}
			add("%d. 等到元素 %s 出现", n, sel)

		case "scroll", "scroll_bottom":
			if _, err := page.Eval(ctx, "window.scrollTo(0, document.body ? document.body.scrollHeight : 0); 'ok'"); err != nil {
				return logs, fmt.Errorf("第 %d 步滚动失败：%v", n, err)
			}
			select {
			case <-ctx.Done():
				return logs, ctx.Err()
			case <-time.After(400 * time.Millisecond):
			}
			add("%d. 滚动到底部", n)

		case "press", "key":
			key := strings.TrimSpace(a.Value)
			if key == "" {
				key = "Enter"
			}
			res, err := page.Eval(ctx, fmt.Sprintf(actionPressJS, jsonStr(sel), jsonStr(key)))
			if err != nil {
				return logs, fmt.Errorf("第 %d 步按键失败：%v", n, err)
			}
			if actionMissing(res) {
				return logs, fmt.Errorf("第 %d 步按键失败：页面上找不到 %s", n, sel)
			}
			r.waitPageReady(ctx, page, 15*time.Second)
			add("%d. 在 %s 按 %s", n, defaultStr(sel, "当前焦点"), key)

		default:
			return logs, fmt.Errorf("第 %d 步动作 %q 不支持（可用：打开页面 / 输入 / 点击 / 选择 / 等待 / 等待元素 / 滚动 / 按键）", n, a.Action)
		}
	}
	return logs, nil
}

// waitPageReady 等页面 document.readyState 变成 complete（最多 max，超时就算了）。
func (r *Recorder) waitPageReady(ctx context.Context, page *browser.Page, max time.Duration) {
	if page == nil || max <= 0 {
		return
	}
	deadline := time.Now().Add(max)
	for {
		res, err := page.Eval(ctx, "document.readyState")
		if err == nil && strings.Contains(string(res), "complete") {
			// 再给渲染/请求一点点时间
			select {
			case <-ctx.Done():
			case <-time.After(300 * time.Millisecond):
			}
			return
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ensureDocument 确保结果里有「当前页面」这条页面文档：
// 先看 CDP 有没有抓到真货（带响应头 / 状态码），没有就用当前 DOM 兜底补录。
func (r *Recorder) ensureDocument(ctx context.Context, id, rawURL, note string) (model.CapturedRequest, error) {
	s, ok := r.Get(id)
	if !ok {
		return model.CapturedRequest{}, fmt.Errorf("录制会话不存在或已被清理（服务重启后会清空）")
	}

	target := strings.TrimSpace(rawURL)
	var info docInfo
	var evalErr error
	if s.Page != nil {
		evalCtx, evalCancel := context.WithTimeout(ctx, 15*time.Second)
		evalErr = s.Page.EvalInto(evalCtx, docInfoJS, &info)
		evalCancel()
		if evalErr == nil {
			// 动作可能把页面跳到了别处（登录后跳列表页）：以浏览器里的真实地址为准
			if cur := strings.TrimSpace(info.URL); strings.HasPrefix(strings.ToLower(cur), "http") {
				target = cur
			}
		}
	}

	// CDP 抓到的真货优先（有响应头、状态码、真实字节数）
	if target != "" {
		deadline := time.Now().Add(2500 * time.Millisecond)
		for {
			if it, ok := r.findDocument(id, target); ok {
				return it, nil
			}
			if time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return model.CapturedRequest{}, ctx.Err()
			case <-time.After(150 * time.Millisecond):
			}
		}
	}

	if evalErr != nil || s.Page == nil {
		if it, ok := r.findDocument(id, target); ok {
			return it, nil
		}
		if evalErr != nil {
			return model.CapturedRequest{}, fmt.Errorf("补录页面文档失败：读取页面失败（%v）", evalErr)
		}
		return model.CapturedRequest{}, fmt.Errorf("这个会话没有内置浏览器标签页，无法读取页面；请改用「手动添加页面文档」")
	}

	in := DocumentInput{URL: target, Method: "GET", Status: info.Status, Body: info.HTML, Note: note}
	if strings.TrimSpace(in.URL) == "" {
		in.URL = rawURL
	}
	item, err := r.AddDocument(id, in)
	if err != nil {
		if it, ok := r.findDocument(id, target); ok {
			return it, nil
		}
		return model.CapturedRequest{}, err
	}
	return item, nil
}

// findDocument 在录制结果里找 target 这条页面文档（#fragment 参与比较）。
func (r *Recorder) findDocument(id, target string) (model.CapturedRequest, bool) {
	list, ok, err := r.Entries(id)
	if err != nil || !ok {
		return model.CapturedRequest{}, false
	}
	for _, it := range list {
		if !strings.EqualFold(strings.TrimSpace(it.Type), "Document") {
			continue
		}
		if samePageURL(it.URL, target) {
			return it, true
		}
	}
	return model.CapturedRequest{}, false
}

// buildDocumentItem 把入参规整成一条 CapturedRequest（Document / page）。
func buildDocumentItem(in DocumentInput) (model.CapturedRequest, error) {
	raw, err := normalizePageURL(in.URL)
	if err != nil {
		return model.CapturedRequest{}, err
	}
	u, _ := url.Parse(raw)
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method == "" {
		method = "GET"
	}
	status := in.Status
	if status <= 0 {
		status = 200
	}
	item := model.CapturedRequest{
		ID:       newID("doc_"),
		Method:   method,
		URL:      raw,
		Host:     u.Host,
		Path:     pagePath(u),
		Query:    u.RawQuery,
		Status:   status,
		MimeType: "text/html",
		Type:     "Document",
		Kind:     "page",
		PageURL:  raw,
		Manual:   true,
		Note:     strings.TrimSpace(in.Note),
		At:       time.Now(),
	}
	body := in.Body
	if len(body) > 256<<10 {
		body = body[:256<<10]
		item.BodyTruncated = true
	}
	if body != "" {
		item.HasBody = true
		item.BodyPreview = body
		item.BodySize = len(body)
		item.Bytes = len(body)
		trimmed := strings.TrimSpace(body)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			item.MimeType = "application/json"
		}
	}
	return item, nil
}

// normalizePageURL 补协议、只留 http(s)，并**保留 #fragment**（hash 路由是页面的一部分）。
func normalizePageURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("请填写页面地址")
	}
	if !strings.Contains(v, "://") {
		host := v
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			host = host[:i]
		}
		if isLocalHost(host) {
			v = "http://" + v
		} else {
			v = "https://" + v
		}
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("页面地址不合法：%s", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("只支持 http / https 页面地址：%s", raw)
	}
	// 不删 fragment：#/vip_member 和 / 是两个页面
	return u.String(), nil
}

// pagePath 展示 / 筛选用：路径 + #fragment（hash 路由站点一定要带，否则都显示成 "/"）。
func pagePath(u *url.URL) string {
	p := u.Path
	if p == "" {
		p = "/"
	}
	if u.Fragment != "" {
		p += "#" + u.Fragment
	}
	return p
}

// isLocalHost 本机 / 内网地址默认用 http（内网系统很少配证书）。
func isLocalHost(h string) bool {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" || h == "127.0.0.1" || h == "::1" {
		return true
	}
	if strings.HasPrefix(h, "192.168.") || strings.HasPrefix(h, "10.") || strings.HasPrefix(h, "127.") {
		return true
	}
	for i := 16; i <= 31; i++ {
		if strings.HasPrefix(h, fmt.Sprintf("172.%d.", i)) {
			return true
		}
	}
	return false
}

// samePageURL 两个页面地址是否指同一页：忽略协议大小写与路径结尾斜杠，但 **#fragment 要一致**。
func samePageURL(a, b string) bool {
	na, nb := strings.TrimSpace(a), strings.TrimSpace(b)
	if na == "" || nb == "" {
		return false
	}
	ua, ea := url.Parse(na)
	ub, eb := url.Parse(nb)
	if ea != nil || eb != nil {
		return strings.TrimRight(na, "/") == strings.TrimRight(nb, "/")
	}
	return pageKey(ua) == pageKey(ub)
}

// pageKey 归一化后的比较键（scheme/host 小写、路径去结尾斜杠、fragment 保留）。
func pageKey(u *url.URL) string {
	c := *u
	c.Scheme = strings.ToLower(c.Scheme)
	c.Host = strings.ToLower(c.Host)
	c.Path = strings.TrimRight(c.Path, "/")
	if c.Path == "" {
		c.Path = "/"
	}
	return c.String()
}

/* ------------------------------------------------------------ 小工具 */

// jsonStr 把 Go 字符串编码成 JS 字面量（处理引号 / 换行 / 中文）。
func jsonStr(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// actionMissing 动作脚本返回 "no-el"：页面上找不到元素。
func actionMissing(res json.RawMessage) bool {
	return strings.Contains(string(res), "no-el")
}

// briefValue 日志里展示输入值；密码类字段打码。
func briefValue(sel, val string) string {
	l := strings.ToLower(sel)
	if strings.Contains(l, "pass") || strings.Contains(l, "pwd") || strings.Contains(l, "secret") {
		if val == "" {
			return "（空）"
		}
		return "••••••（已隐藏）"
	}
	if val == "" {
		return "（清空）"
	}
	if len(val) > 60 {
		return val[:60] + "…"
	}
	return val
}

func defaultStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
