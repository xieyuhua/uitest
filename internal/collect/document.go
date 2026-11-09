package collect

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"uitest-server/internal/model"
)

// 本文件处理「页面文档（Document）」的补录：
//
//   - CDP 只在整页导航时才会发 Document 请求；先开始录制、之后才打开页面，
//     或者 SPA 前端路由改地址（history.pushState），都不会再有新的 Document；
//   - 插件录制（MV3）只能包 fetch / XHR，页面本身的 HTML 要靠 content script 上报；
//   - 所以这里提供「手动添加页面文档」和「在内置浏览器里打开页面并补录」两条路，
//     补录的条目带 manual 标记，和实时抓到的条目一样能勾选、建任务、识别字段。

// DocumentInput 「添加页面文档」/「打开页面补录」的入参。
type DocumentInput struct {
	URL    string
	Method string
	Status int
	Note   string
	// Body 页面 HTML / JSON 快照（可选，最多 256KB）
	Body string
}

// docInfoJS 取当前页面的文档信息：地址、状态码、content-type、大小、HTML。
// 用于「打开页面补录」和「初始页面文档自动补录」——CDP 没抓到 Document 时兜底。
const docInfoJS = `(function(){var e=(performance.getEntriesByType?performance.getEntriesByType("navigation"):[])[0]||null;var d=document.documentElement?document.documentElement.outerHTML:"";return {url:String(location.href),status:(e&&e.responseStatus)||0,mime:(e&&e.contentType)||"text/html",bytes:(e&&e.transferSize)||d.length,html:d.slice(0,262144),len:d.length};})()`

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
//
// 适合：先开始录制、之后才登录/进入列表页；或者地址没变但内容变了，想补一条最新 HTML。
func (r *Recorder) OpenPage(ctx context.Context, id, rawURL string) (model.CapturedRequest, error) {
	target, err := normalizePageURL(rawURL)
	if err != nil {
		return model.CapturedRequest{}, err
	}
	s, ok := r.Get(id)
	if !ok {
		return model.CapturedRequest{}, fmt.Errorf("录制会话不存在或已被清理（服务重启后会清空）")
	}
	if s.Source != "browser" || s.Page == nil {
		return model.CapturedRequest{}, fmt.Errorf("这个会话是浏览器插件录制的，没有内置浏览器标签页；请改用「添加页面文档」")
	}
	navCtx, navCancel := context.WithTimeout(ctx, 45*time.Second)
	navErr := s.Page.Navigate(navCtx, target)
	navCancel()

	r.mu.Lock()
	if cur, ok := r.sessions[id]; ok {
		cur.PageURL = target
	}
	r.mu.Unlock()

	item, derr := r.ensureDocument(ctx, id, target, "打开页面补录")
	if derr != nil {
		if navErr != nil {
			return model.CapturedRequest{}, fmt.Errorf("打开页面失败：%v", navErr)
		}
		return model.CapturedRequest{}, derr
	}
	return item, nil
}

// ensureDocument 确保结果里有 target 这条页面文档：先等 CDP 抓到真货，
// 等不到就用当前 DOM 兜底补录（带 HTML 预览，后面识别字段用得上）。
func (r *Recorder) ensureDocument(ctx context.Context, id, rawURL, note string) (model.CapturedRequest, error) {
	target, err := normalizePageURL(rawURL)
	if err != nil {
		return model.CapturedRequest{}, err
	}
	deadline := time.Now().Add(6 * time.Second)
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
		case <-time.After(200 * time.Millisecond):
		}
	}

	s, ok := r.Get(id)
	if !ok || s.Page == nil {
		return model.CapturedRequest{}, fmt.Errorf("录制会话不存在或已被清理")
	}
	var info struct {
		URL    string `json:"url"`
		Status int    `json:"status"`
		Mime   string `json:"mime"`
		Bytes  int    `json:"bytes"`
		HTML   string `json:"html"`
		Len    int    `json:"len"`
	}
	evalCtx, evalCancel := context.WithTimeout(ctx, 10*time.Second)
	evalErr := s.Page.EvalInto(evalCtx, docInfoJS, &info)
	evalCancel()

	in := DocumentInput{URL: target, Method: "GET", Note: note}
	if evalErr == nil {
		if cur := strings.TrimSpace(info.URL); strings.HasPrefix(strings.ToLower(cur), "http") {
			in.URL = cur
		}
		in.Status = info.Status
		in.Body = info.HTML
	}
	item, err := r.AddDocument(id, in)
	if err != nil {
		if it, ok := r.findDocument(id, target); ok {
			return it, nil
		}
		if evalErr != nil {
			return model.CapturedRequest{}, fmt.Errorf("补录页面文档失败：%v（读取页面也失败：%v）", err, evalErr)
		}
		return model.CapturedRequest{}, err
	}
	return item, nil
}

// findDocument 在录制结果里找 target 这条页面文档（忽略 #fragment 与结尾斜杠）。
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
		Path:     u.Path,
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
		trimmed := strings.TrimSpace(body)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			item.MimeType = "application/json"
		}
	}
	return item, nil
}

// normalizePageURL 补协议、去 fragment、只留 http(s)，顺手挡掉明显不合法的地址。
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
	u.Fragment = ""
	return u.String(), nil
}

// isLocalHost 本机 / 内网地址默认用 http（内网系统很少配证书）。
func isLocalHost(h string) bool {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.LastIndex(h, ":"); i > 0 {
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

// samePageURL 两个页面地址是否指同一页（忽略 #fragment 与结尾斜杠）。
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
	ua.Fragment, ub.Fragment = "", ""
	return strings.EqualFold(strings.TrimRight(ua.String(), "/"), strings.TrimRight(ub.String(), "/"))
}
