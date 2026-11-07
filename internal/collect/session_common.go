package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"uitest-server/internal/model"
)

// applySessionRefresh 登录态就绪后，把浏览器里的最新会话值写进公共参数。
//
// 这是"登录之后 token 变了，抓取时要用新 token"的核心：
//   - 从浏览器实时请求头（resolveSession 已经嗅探好）里取 Authorization / *token* / *auth*；
//   - 按配置从 Cookie、localStorage / sessionStorage 里取指定字段；
//   - 写进 sess.params 后，参数绑定（Bindings）会在每次请求时用最新值覆盖目标位置；
//   - 没有显式绑定 token 时自动补一条"参数 token → 取到 token 的那个请求头"，
//     保证浏览器里嗅到的旧头不会把新 token 盖回去。
//
// 返回实际更新的项（"参数:token" / "绑定:Authorization"），用于运行日志和试抓过程展示。
func (h *Harvester) applySessionRefresh(ctx context.Context, runID string, task *model.HarvestTask,
	sess *resolvedSession) []string {

	if task == nil || sess == nil || task.Common == nil || !task.Common.Refresh.Active() {
		return nil
	}
	rf := task.Common.Refresh.Effective()
	if sess.params == nil {
		sess.params = map[string]string{}
	}
	var changed []string

	// 1) 请求头里的 token
	if hdr, raw := findSessionToken(*sess, rf.TokenHeader); raw != "" {
		tok, scheme := splitAuthScheme(raw)
		if tok != "" {
			sess.params[rf.TokenParam] = tok
			changed = append(changed, "参数:"+rf.TokenParam)
			if !sessHasHeaderBinding(*sess, rf.TokenParam, hdr) {
				sess.bindings = append(sess.bindings, model.HarvestParamBinding{
					Param: rf.TokenParam, Target: "header", Name: hdr, Template: schemeTemplate(scheme, rf.TokenParam),
				})
				changed = append(changed, "绑定:"+hdr)
			}
		}
	}

	// 2) Cookie → 公共参数
	if len(rf.Cookies) > 0 {
		byName := map[string]string{}
		for _, list := range sess.cookies {
			for _, c := range list {
				if c == nil || c.Name == "" {
					continue
				}
				if _, exists := byName[c.Name]; !exists {
					byName[c.Name] = c.Value
				}
			}
		}
		keys := make([]string, 0, len(rf.Cookies))
		for name := range rf.Cookies {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			param := strings.TrimSpace(rf.Cookies[name])
			if param == "" {
				continue
			}
			if v, ok := byName[name]; ok && v != "" {
				sess.params[param] = v
				changed = append(changed, "参数:"+param)
			}
		}
	}

	// 3) 浏览器存储 → 公共参数（localStorage:key / sessionStorage:key）
	if len(rf.Storage) > 0 && h.br != nil {
		keys := make([]string, 0, len(rf.Storage))
		for k := range rf.Storage {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		values := h.readBrowserStorage(ctx, task, keys)
		for _, key := range keys {
			param := strings.TrimSpace(rf.Storage[key])
			if param == "" {
				continue
			}
			if v := strings.TrimSpace(values[key]); v != "" {
				sess.params[param] = v
				changed = append(changed, "参数:"+param)
			}
		}
	}

	changed = dedupeStrings(changed)
	if len(changed) > 0 {
		h.logf(runID, "info", "登录后刷新公共参数：%s", strings.Join(changed, "、"))
	}
	return changed
}

// findSessionToken 从浏览器实时请求头里找 token。
// want 非空时只看这个头；否则按 Authorization → token/auth 类头的顺序找。
func findSessionToken(sess resolvedSession, want string) (header, value string) {
	if len(sess.headers) == 0 {
		if sess.token != "" {
			return orDefaultStr(sess.tokenHeader, "Authorization"), sess.tokenPrefix + sess.token
		}
		return "", ""
	}
	if strings.TrimSpace(want) != "" {
		for k, v := range sess.headers {
			if strings.EqualFold(strings.TrimSpace(k), strings.TrimSpace(want)) && strings.TrimSpace(v) != "" {
				return k, v
			}
		}
		return "", ""
	}
	priority := []string{"authorization", "blade-auth", "x-access-token", "x-auth-token", "access-token", "x-token", "token"}
	for _, name := range priority {
		for k, v := range sess.headers {
			if strings.EqualFold(strings.TrimSpace(k), name) && strings.TrimSpace(v) != "" {
				return k, v
			}
		}
	}
	// 兜底：任意 token / auth 类头
	keys := make([]string, 0, len(sess.headers))
	for k := range sess.headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if isTokenHeaderName(k) && strings.TrimSpace(sess.headers[k]) != "" {
			return k, sess.headers[k]
		}
	}
	return "", ""
}

// sessHasHeaderBinding 是否已经有"该参数 → 该请求头"的绑定。
func sessHasHeaderBinding(sess resolvedSession, param, name string) bool {
	if sess.token != "" && strings.EqualFold(orDefaultStr(sess.tokenHeader, "Authorization"), name) {
		return true
	}
	for _, b := range sess.bindings {
		e := b.Effective()
		if e.Target == "header" && e.Param == param && strings.EqualFold(e.Name, name) {
			return true
		}
	}
	return false
}

// readBrowserStorage 在内置浏览器里批量读 localStorage / sessionStorage。
// key 形如 "localStorage:access_token"；读不到时返回空串（不报错，交给日志提示）。
func (h *Harvester) readBrowserStorage(ctx context.Context, task *model.HarvestTask, keys []string) map[string]string {
	out := map[string]string{}
	target := sessionTarget(task)
	if target == "" || h.br == nil || len(keys) == 0 {
		return out
	}
	openCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	page, err := h.br.OpenPage(openCtx, target)
	if err != nil {
		h.logf("", "warn", "读取浏览器存储失败：打不开 %s（%v）", target, err)
		return out
	}
	defer page.Close(context.Background())
	for _, key := range keys {
		store, name := splitStorageKey(key)
		expr := fmt.Sprintf("(function(){try{return %s.getItem(%q)||''}catch(e){return ''}})()", store, name)
		var raw string
		if err := page.EvalInto(openCtx, expr, &raw); err != nil {
			continue
		}
		out[key] = tokenFromValue(raw)
	}
	return out
}

// splitStorageKey 解析 "localStorage:key"；没写前缀时默认 localStorage。
func splitStorageKey(key string) (store, name string) {
	k := strings.TrimSpace(key)
	store = "localStorage"
	if i := strings.Index(k, ":"); i > 0 {
		p := strings.ToLower(strings.TrimSpace(k[:i]))
		if p == "localstorage" || p == "sessionstorage" {
			store = p
			k = strings.TrimSpace(k[i+1:])
		}
	}
	if strings.EqualFold(store, "sessionstorage") {
		store = "sessionStorage"
	} else {
		store = "localStorage"
	}
	return store, k
}

// dedupeStrings 去重并保持顺序。
func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// cookieBindingValues 当前参数绑定里要写进 Cookie 的值（name → value）。
func cookieBindingValues(sess resolvedSession) map[string]string {
	out := map[string]string{}
	if len(sess.bindings) == 0 {
		return out
	}
	vars := sessionVars(sess)
	for _, b := range sess.bindings {
		e := b.Effective()
		if e.Target != "cookie" {
			continue
		}
		if v := strings.TrimSpace(expand(e.Template, vars)); v != "" {
			out[e.Name] = v
		}
	}
	return out
}

// applyQueryBodyBindings 把 query / body 绑定的值写进这次请求（覆盖同名值）。
func applyQueryBodyBindings(target, body string, sess resolvedSession) (string, string) {
	if len(sess.bindings) == 0 {
		return target, body
	}
	vars := sessionVars(sess)
	query := url.Values{}
	bodyKV := map[string]string{}
	for _, b := range sess.bindings {
		e := b.Effective()
		v := strings.TrimSpace(expand(e.Template, vars))
		if v == "" {
			continue
		}
		switch e.Target {
		case "query":
			query.Set(e.Name, v)
		case "body":
			bodyKV[e.Name] = v
		}
	}
	if len(query) > 0 {
		if u, err := url.Parse(target); err == nil {
			q := u.Query()
			for k, vs := range query {
				q.Set(k, vs[0])
			}
			u.RawQuery = q.Encode()
			target = u.String()
		}
	}
	if len(bodyKV) == 0 {
		return target, body
	}
	if strings.TrimSpace(body) == "" {
		b, err := json.Marshal(bodyKV)
		if err == nil {
			return target, string(b)
		}
		return target, body
	}
	obj := map[string]any{}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		// 表单 / 非 JSON 请求体：不动它，避免把原报文搞坏
		return target, body
	}
	for k, v := range bodyKV {
		obj[k] = v
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return target, body
	}
	return target, string(b)
}

// SessionRefreshPreview 「登录后刷新公共参数」的结果摘要（敏感值已脱敏）。
type SessionRefreshPreview struct {
	// Session 登录态摘要：Cookie 个数、浏览器头个数
	Session string `json:"session"`
	// Cookies Cookie 名 → 脱敏后的值
	Cookies map[string]string `json:"cookies,omitempty"`
	// Params 刷新后的公共参数（值已脱敏）
	Params map[string]string `json:"params,omitempty"`
	// Headers 从浏览器取到的登录态请求头（值已脱敏）
	Headers map[string]string `json:"headers,omitempty"`
	// Bindings 本次生效的参数绑定（模板原样返回，通常是 {{token}} 这种占位）
	Bindings []model.HarvestParamBinding `json:"bindings,omitempty"`
	// Applied 本次实际写入的项："参数:token" / "绑定:Authorization"
	Applied []string `json:"applied,omitempty"`
	// Notes 给人看的提示（没启用刷新、没找到 token 等）
	Notes []string `json:"notes,omitempty"`
	// Source 会话来源：ui / http / import
	Source string `json:"source,omitempty"`
	// SourceText 来源的中文说明
	SourceText string `json:"source_text,omitempty"`
	// Login 来源是登录接口时，这次登录调用的过程（脱敏）
	Login *LoginTrace `json:"login,omitempty"`
}

// RefreshSessionPreview 按当前浏览器登录态解析一次会话，并执行「登录后刷新公共参数」，
// 返回脱敏后的结果。控制台「登录后刷新」按钮用它确认 token 到底换没换。
//
// 只读：不会把刷出来的 token 存进任务配置（token 会过期，每次运行都应该重新取），
// 也不会发任何业务请求。
func (h *Harvester) RefreshSessionPreview(ctx context.Context, task *model.HarvestTask) (SessionRefreshPreview, error) {
	out := SessionRefreshPreview{}
	if task == nil {
		return out, fmt.Errorf("任务不存在")
	}
	task.Normalize()
	if strings.TrimSpace(task.Session.Host) == "" {
		task.Session.Host = sessionTarget(task)
	}
	// 刚跑过前置登录的话，缓存里的旧会话必须作废，否则看到的是上一轮的值
	h.invalidateSession(task)

	sess, err := h.resolveSession(ctx, task)
	if err != nil {
		return out, err
	}
	out.Session = sessionSummary(task.Session, sess)
	out.Source = sess.source
	out.SourceText = task.Session.SourceText()
	out.Login = sess.loginTrace
	out.Applied = h.applySessionRefresh(ctx, "", task, &sess)
	refreshing := task.Common != nil && task.Common.Refresh.Active()
	if !refreshing {
		out.Notes = append(out.Notes,
			"没启用「登录后刷新公共参数」：这次只导出了浏览器会话，不会自动改公共参数（勾选后 token / Cookie 才会自动写进参数）")
	} else if len(out.Applied) == 0 {
		out.Notes = append(out.Notes,
			"刷新已启用，但这次没从浏览器里取到新的 token / Cookie / 存储值：确认已经登录，且「站点」是登录后的页面（列表页 / 首页）")
	}
	if len(sess.params) > 0 {
		out.Params = make(map[string]string, len(sess.params))
		for k, v := range sess.params {
			out.Params[k] = maskIfSensitive(k, v)
		}
	}
	if len(sess.headers) > 0 {
		out.Headers = make(map[string]string, len(sess.headers))
		for k, v := range sess.headers {
			out.Headers[k] = maskHeaderValue(k, v)
		}
	}
	if cookies := cookiePreview(sess); len(cookies) > 0 {
		out.Cookies = cookies
	}
	if len(sess.bindings) > 0 {
		out.Bindings = append([]model.HarvestParamBinding(nil), sess.bindings...)
	}
	return out, nil
}

// cookiePreview 会话里所有 Cookie 名 → 脱敏值（同名取第一个）。
func cookiePreview(sess resolvedSession) map[string]string {
	out := map[string]string{}
	for _, list := range sess.cookies {
		for _, c := range list {
			if c == nil || c.Name == "" {
				continue
			}
			if _, exists := out[c.Name]; !exists {
				out[c.Name] = maskSecret(c.Value)
			}
		}
	}
	for name, v := range sess.commonCookies {
		if _, exists := out[name]; !exists {
			out[name] = maskSecret(v)
		}
	}
	return out
}

// RunPreloginNow 立即跑一次前置登录（控制台按钮用）。
func (h *Harvester) RunPreloginNow(ctx context.Context, task *model.HarvestTask) error {
	if task == nil || task.Prelogin == nil || !task.Prelogin.Active() {
		return fmt.Errorf("这个任务没有配置前置登录（先选一个「登录流程录制」并打开开关）")
	}
	return h.runPreloginQuiet(ctx, task)
}

/* -------------------------------------------------------- 会话过程追踪 */

// SessionTrace 这次抓取用的会话是怎么拼出来的（敏感值一律脱敏）。
// 试抓 / 自检 / 「登录后刷新」都把它展示出来，方便核对：
// Cookie 从哪来、token 换成新的没有、公共参数被刷新成了什么、参数绑定怎么覆盖请求头。
type SessionTrace struct {
	Source       string                      `json:"source"`
	SourceText   string                      `json:"source_text"`
	Host         string                      `json:"host,omitempty"`
	HeaderMode   string                      `json:"header_mode"`
	CookieCount  int                         `json:"cookie_count"`
	Cookies      []string                    `json:"cookies,omitempty"`
	TokenHeader  string                      `json:"token_header,omitempty"`
	TokenPrefix  string                      `json:"token_prefix,omitempty"`
	TokenPreview string                      `json:"token_preview,omitempty"`
	TokenLength  int                         `json:"token_length,omitempty"`
	Headers      map[string]string           `json:"headers,omitempty"`
	Params       map[string]string           `json:"params,omitempty"`
	Bindings     []model.HarvestParamBinding `json:"bindings,omitempty"`
	Refresh      []string                    `json:"refresh,omitempty"`
	ScriptRan    bool                        `json:"script_ran,omitempty"`
	Notes        []string                    `json:"notes,omitempty"`
}

// buildSessionTrace 把已解析好的会话整理成可展示的追踪信息。
func buildSessionTrace(cfg model.HarvestSession, sess resolvedSession, refreshed []string, scriptRan bool) *SessionTrace {
	out := &SessionTrace{
		Source:      sess.source,
		SourceText:  cfg.SourceText(),
		Host:        strings.TrimSpace(cfg.Host),
		HeaderMode:  orDefaultStr(sess.headerMode, "all"),
		CookieCount: sess.cookieCount,
		Cookies:     cookieNameList(sess),
		Refresh:     append([]string(nil), refreshed...),
		ScriptRan:   scriptRan,
	}
	if sess.token != "" {
		out.TokenHeader = orDefaultStr(sess.tokenHeader, "Authorization")
		out.TokenPrefix = sess.tokenPrefix
		out.TokenPreview = maskSecret(sess.token)
		out.TokenLength = len(sess.token)
	}
	if len(sess.headers) > 0 {
		out.Headers = make(map[string]string, len(sess.headers))
		for k, v := range sess.headers {
			out.Headers[k] = maskHeaderValue(k, v)
		}
	}
	if len(sess.params) > 0 {
		out.Params = make(map[string]string, len(sess.params))
		for k, v := range sess.params {
			out.Params[k] = maskIfSensitive(k, v)
		}
	}
	if len(sess.bindings) > 0 {
		out.Bindings = append([]model.HarvestParamBinding(nil), sess.bindings...)
	}
	switch sess.source {
	case model.SessionSourceUI:
		if len(sess.headers) == 0 {
			out.Notes = append(out.Notes,
				"没从浏览器嗅到登录态请求头：接口里录制下来的 Authorization 可能还是旧值，把「站点」改成登录后就会请求接口的页面（列表页 / 首页）")
		}
	case model.SessionSourceHTTP:
		if sess.loginTrace != nil && sess.loginTrace.Error != "" {
			out.Notes = append(out.Notes, "登录接口失败："+sess.loginTrace.Error)
		} else {
			out.Notes = append(out.Notes, "token / Cookie 来自登录接口，不依赖浏览器；登录接口不会自动续期，凭证过期时每次运行会重新登一次")
		}
	case model.SessionSourceImport:
		out.Notes = append(out.Notes,
			"仅使用公共参数里导入的 Cookie / 参数，不会打开浏览器；凭证过期后需要重新「粘贴导入」或改用登录接口")
	}
	return out
}
