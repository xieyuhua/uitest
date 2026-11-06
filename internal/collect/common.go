package collect

import (
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"uitest-server/internal/model"
)

// CommonSuggest 从「录制结果」或「粘贴导入」汇总出来的公共参数候选。
//
// 只做建议，不会覆盖用户已经手填的值（MergeCommon 负责合并）。
type CommonSuggest struct {
	Params   map[string]string           `json:"params,omitempty"`
	Headers  map[string]string           `json:"headers,omitempty"`
	Cookies  map[string]string           `json:"cookies,omitempty"`
	Bindings []model.HarvestParamBinding `json:"bindings,omitempty"`
	Notes    []string                    `json:"notes,omitempty"`
}

// pagingParamNames 分页/翻页类参数一律不升级成公共参数：
// 一旦被固定成公共值，接口自己的翻页就会被覆盖掉。
var pagingParamNames = map[string]bool{
	"page": true, "pages": true, "pageno": true, "pagenum": true, "pageindex": true,
	"pagesize": true, "page_size": true, "per_page": true, "perpage": true,
	"offset": true, "limit": true, "cursor": true, "next": true, "next_token": true,
}

// isPagingParam 名字里带 page / 命中分页参数名单。
func isPagingParam(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if pagingParamNames[n] {
		return true
	}
	return strings.Contains(n, "page")
}

// isTokenHeaderName 像不像"登录 token"请求头（Cookie 里不放它）。
func isTokenHeaderName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" || strings.Contains(n, "csrf") || strings.Contains(n, "xsrf") {
		return false
	}
	switch n {
	case "authorization", "blade-auth", "token", "x-token", "x-auth-token", "x-access-token", "access-token":
		return true
	}
	return strings.Contains(n, "token") || strings.Contains(n, "auth")
}

// splitAuthScheme 把 "Bearer abc" 拆成 ("abc", "Bearer")；没有前缀时 scheme 为空。
func splitAuthScheme(v string) (token, scheme string) {
	t := strings.TrimSpace(v)
	for _, s := range []string{"Bearer", "Basic", "Token"} {
		if len(t) > len(s)+1 && strings.EqualFold(t[:len(s)+1], s+" ") {
			return strings.TrimSpace(t[len(s)+1:]), s
		}
	}
	return t, ""
}

// schemeTemplate 生成 "Bearer {{token}}" 这种模板；没有前缀就只留 {{token}}。
func schemeTemplate(scheme, param string) string {
	tpl := "{{" + param + "}}"
	if strings.TrimSpace(scheme) != "" {
		return strings.TrimSpace(scheme) + " " + tpl
	}
	return tpl
}

// SuggestCommonFromCaptured 从选中的录制请求里汇总"所有接口都一样"的参数。
//
// 规则（保守，宁可少提也不错提）：
//   - query / JSON body / 表单 body 里**每个接口都出现且值相同**的字段 → 公共参数；
//   - 分页类参数（page / offset / limit / cursor …）永不提升；
//   - 请求头同样要求"每个接口都有且值相同"，跳过浏览器/传输层头；
//   - Authorization / *token* / *auth* 这类头改成 {{token}} 占位并生成参数绑定，登录后自动填；
//   - Cookie 头拆成公共 Cookie（只是默认值，前置登录后的浏览器 Cookie 优先）。
func SuggestCommonFromCaptured(entries []model.CapturedRequest) CommonSuggest {
	sug := CommonSuggest{}
	if len(entries) == 0 {
		return sug
	}
	var paramMaps, headerMaps, cookieMaps []map[string]string
	for _, e := range entries {
		paramMaps = append(paramMaps, capturedParamMap(e))
		headerMaps = append(headerMaps, capturedHeaderMap(e))
		cookieMaps = append(cookieMaps, capturedCookieMap(e))
	}
	params := commonStringValues(paramMaps)
	for k := range params {
		if isPagingParam(k) {
			delete(params, k)
		}
	}
	if len(params) > 0 {
		sug.Params = params
		sug.Notes = append(sug.Notes,
			"汇总了 "+itoa(len(params))+" 个所有接口都一致的 query/body 参数（分页参数已排除）")
	}

	headers := commonStringValues(headerMaps)
	bindings := map[string]model.HarvestParamBinding{}
	var authFound bool
	for name, value := range headers {
		if !isTokenHeaderName(name) {
			continue
		}
		delete(headers, name)
		tok, scheme := splitAuthScheme(value)
		if strings.TrimSpace(tok) == "" {
			continue
		}
		b := model.HarvestParamBinding{
			Param:    "token",
			Target:   "header",
			Name:     name,
			Template: schemeTemplate(scheme, "token"),
		}
		bindings[b.Target+"|"+strings.ToLower(b.Name)] = b
		authFound = true
	}
	if len(headers) > 0 {
		sug.Headers = headers
		sug.Notes = append(sug.Notes,
			"汇总了 "+itoa(len(headers))+" 个所有接口都一致的请求头")
	}
	if authFound {
		sug.Bindings = sortedBindings(bindings)
		sug.Notes = append(sug.Notes,
			"鉴权头已改成 {{token}} 占位并生成参数绑定：登录后把最新 token 写进参数 token，请求头会自动替换")
	}

	cookies := commonStringValues(cookieMaps)
	if len(cookies) > 0 {
		sug.Cookies = cookies
		sug.Notes = append(sug.Notes,
			"汇总了 "+itoa(len(cookies))+" 个所有接口都一致的 Cookie（仅作默认值，前置登录后的浏览器 Cookie 优先）")
	}
	return sug
}

// capturedParamMap 一条录制请求的 query + body 参数（body 同名时以 body 为准）。
func capturedParamMap(r model.CapturedRequest) map[string]string {
	out := map[string]string{}
	if u, err := url.Parse(strings.TrimSpace(r.URL)); err == nil {
		for k, vs := range u.Query() {
			if len(vs) > 0 {
				out[k] = vs[0]
			}
		}
	}
	for k, v := range parseParamText(r.PostData) {
		out[k] = v
	}
	return out
}

// capturedHeaderMap 一条录制请求的业务请求头（去掉浏览器/传输层头）。
func capturedHeaderMap(r model.CapturedRequest) map[string]string {
	out := map[string]string{}
	for k, v := range r.ReqHeaders {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "" || strings.HasPrefix(lk, ":") || strings.HasPrefix(lk, "sec-") {
			continue
		}
		if skipHeaders[lk] {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	return out
}

// capturedCookieMap 一条录制请求 Cookie 头里的 name=value。
func capturedCookieMap(r model.CapturedRequest) map[string]string {
	for k, v := range r.ReqHeaders {
		if strings.EqualFold(strings.TrimSpace(k), "cookie") {
			return parseCookieText(v)
		}
	}
	return map[string]string{}
}

// commonStringValues 取「每张表都有且值完全相同」的键值。
func commonStringValues(maps []map[string]string) map[string]string {
	out := map[string]string{}
	if len(maps) == 0 {
		return out
	}
	for k, v := range maps[0] {
		ok := true
		for _, m := range maps[1:] {
			if mv, exists := m[k]; !exists || mv != v {
				ok = false
				break
			}
		}
		if ok {
			out[k] = v
		}
	}
	return out
}

// sortedBindings 把 map 里的绑定按 target / name 排序，保证输出稳定。
func sortedBindings(m map[string]model.HarvestParamBinding) []model.HarvestParamBinding {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]model.HarvestParamBinding, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

// MergeCommonIntoSuggest 把建议合并进已有公共参数：用户已经填了的项不覆盖。
func MergeCommonIntoSuggest(base *model.HarvestCommon, sug CommonSuggest) *model.HarvestCommon {
	if base == nil {
		base = &model.HarvestCommon{}
	}
	out := *base
	out.Params = cloneStringMap(base.Params)
	out.Headers = cloneStringMap(base.Headers)
	out.Cookies = cloneStringMap(base.Cookies)
	out.Bindings = append([]model.HarvestParamBinding(nil), base.Bindings...)
	if out.Params == nil {
		out.Params = map[string]string{}
	}
	if out.Headers == nil {
		out.Headers = map[string]string{}
	}
	if out.Cookies == nil {
		out.Cookies = map[string]string{}
	}
	for k, v := range sug.Params {
		if _, exists := out.Params[k]; !exists {
			out.Params[k] = v
		}
	}
	for k, v := range sug.Headers {
		if _, exists := out.Headers[k]; !exists {
			out.Headers[k] = v
		}
	}
	for k, v := range sug.Cookies {
		if _, exists := out.Cookies[k]; !exists {
			out.Cookies[k] = v
		}
	}
	for _, b := range sug.Bindings {
		be := b.Effective()
		if !be.Valid() {
			continue
		}
		dup := false
		for _, old := range out.Bindings {
			oe := old.Effective()
			if oe.Target == be.Target && strings.EqualFold(oe.Name, be.Name) && oe.Param == be.Param {
				dup = true
				break
			}
		}
		if !dup {
			out.Bindings = append(out.Bindings, be)
		}
	}
	if len(out.Params) == 0 {
		out.Params = nil
	}
	if len(out.Headers) == 0 {
		out.Headers = nil
	}
	if len(out.Cookies) == 0 {
		out.Cookies = nil
	}
	if len(out.Bindings) == 0 {
		out.Bindings = nil
	}
	return &out
}

// CommonImport 用户粘贴导入的原始文本（Cookie / Header / 参数 / cURL）。
type CommonImport struct {
	Cookie  string `json:"cookie,omitempty"`
	Headers string `json:"headers,omitempty"`
	Params  string `json:"params,omitempty"`
	Curl    string `json:"curl,omitempty"`
}

// ParseCommonImport 解析粘贴的会话参数，返回可保存的公共参数 + 说明。
//
// 识别规则：
//   - Cookie：`a=b; c=d`，也可以整段带 `Cookie:` 前缀或一行一个 name=value；
//   - Header：一行一个 `Name: Value`，其中 Cookie 行会拆进 cookies，
//     Authorization / *token* 行会生成 {{token}} 绑定，原值写进 params.token；
//   - Params：JSON 对象 / `a=1&b=2` / 一行一个 key=value；
//   - cURL：解析 -H / --header、-d / --data* 和 URL，再按上面三种规则拆开。
func ParseCommonImport(in CommonImport) (model.HarvestCommon, []string) {
	out := model.HarvestCommon{}
	var notes []string
	if strings.TrimSpace(in.Curl) != "" {
		curl := parseCurl(in.Curl)
		if curl.URL != "" {
			if u, err := url.Parse(curl.URL); err == nil && u.RawQuery != "" {
				for k, vs := range u.Query() {
					if len(vs) == 0 || isPagingParam(k) {
						continue
					}
					out.Params = putKV(out.Params, k, vs[0])
				}
				notes = append(notes, "cURL 的 URL 查询参数已作为公共参数候选（接口同名参数优先）")
			}
		}
		if in.Headers == "" {
			in.Headers = curl.Headers
		}
		if in.Params == "" {
			in.Params = curl.Data
		}
	}
	if strings.TrimSpace(in.Cookie) != "" {
		cookies := parseCookieText(in.Cookie)
		for k, v := range cookies {
			out.Cookies = putKV(out.Cookies, k, v)
		}
		if len(cookies) > 0 {
			notes = append(notes, "导入 Cookie "+itoa(len(cookies))+" 个（登录后的浏览器 Cookie 会优先）")
		}
	}
	if strings.TrimSpace(in.Headers) != "" {
		var nHeader, nCookie, nAuth int
		for _, line := range strings.Split(strings.ReplaceAll(in.Headers, "\r\n", "\n"), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "curl ")
			i := strings.Index(line, ":")
			if i <= 0 {
				continue
			}
			name := strings.TrimSpace(line[:i])
			value := strings.TrimSpace(line[i+1:])
			if name == "" || value == "" {
				continue
			}
			if strings.EqualFold(name, "cookie") {
				for k, v := range parseCookieText(value) {
					out.Cookies = putKV(out.Cookies, k, v)
					nCookie++
				}
				continue
			}
			if isTokenHeaderName(name) {
				tok, scheme := splitAuthScheme(value)
				if tok != "" {
					out.Params = putKV(out.Params, "token", tok)
					out.Bindings = appendBinding(out.Bindings, model.HarvestParamBinding{
						Param: "token", Target: "header", Name: name, Template: schemeTemplate(scheme, "token"),
					})
					nAuth++
					continue
				}
			}
			out.Headers = putKV(out.Headers, name, value)
			nHeader++
		}
		if nHeader > 0 {
			notes = append(notes, "导入请求头 "+itoa(nHeader)+" 个")
		}
		if nCookie > 0 {
			notes = append(notes, "从 Header 里拆出 Cookie "+itoa(nCookie)+" 个")
		}
		if nAuth > 0 {
			notes = append(notes, "鉴权头已转成 {{token}} 绑定（参数 token 当前是导入时的值，可在登录后刷新）")
		}
	}
	if strings.TrimSpace(in.Params) != "" {
		params := parseParamText(in.Params)
		var paging int
		for k, v := range params {
			if isPagingParam(k) {
				paging++
				continue
			}
			out.Params = putKV(out.Params, k, v)
		}
		if len(params)-paging > 0 {
			notes = append(notes, "导入 query/body 参数 "+itoa(len(params)-paging)+" 个")
		}
		if paging > 0 {
			notes = append(notes, "已跳过分页参数 "+itoa(paging)+" 个（page / offset / limit 等固化成公共参数会挡住接口自己翻页）")
		}
	}
	out.Normalize()
	return out, notes
}

// parseCurl 只做常见的 cURL 解析：URL、-H/--header、-d/--data*。
type curlParts struct {
	URL     string
	Headers string
	Data    string
}

var (
	curlHeaderRe = regexp.MustCompile(`(?:-H|--header)\s+(?:'([^']*)'|"([^"]*)")`)
	curlDataRe   = regexp.MustCompile(`(?:-d|--data|--data-raw|--data-binary|--data-urlencode)\s+(?:'([^']*)'|"([^"]*)")`)
	curlURLRe    = regexp.MustCompile(`(https?://[^\s'"]+)`)
)

func parseCurl(raw string) curlParts {
	out := curlParts{}
	out.URL = curlURLRe.FindString(raw)
	var headers []string
	for _, m := range curlHeaderRe.FindAllStringSubmatch(raw, -1) {
		v := m[1]
		if v == "" {
			v = m[2]
		}
		v = strings.TrimSpace(v)
		if v != "" {
			headers = append(headers, v)
		}
	}
	out.Headers = strings.Join(headers, "\n")
	if m := curlDataRe.FindStringSubmatch(raw); len(m) > 0 {
		out.Data = m[1]
		if out.Data == "" {
			out.Data = m[2]
		}
	}
	return out
}

// parseCookieText 解析 Cookie 文本（支持 "Cookie: a=b; c=d"、多行、纯 name=value）。
func parseCookieText(raw string) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	text := strings.ReplaceAll(raw, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\n", ";")
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "Cookie:"), "cookie:")
	for _, part := range strings.Split(text, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.Index(part, "=")
		if i <= 0 {
			continue
		}
		name := strings.TrimSpace(part[:i])
		value := strings.TrimSpace(part[i+1:])
		if name != "" && value != "" {
			out[name] = value
		}
	}
	return out
}

// parseParamText 解析参数文本：JSON 对象 / URL query / 一行一个 key=value。
func parseParamText(raw string) map[string]string {
	out := map[string]string{}
	text := strings.TrimSpace(raw)
	if text == "" {
		return out
	}
	if strings.HasPrefix(text, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(text), &obj); err == nil {
			for k, v := range obj {
				switch t := v.(type) {
				case string:
					out[k] = t
				case float64:
					out[k] = trimFloat(t)
				case bool:
					if t {
						out[k] = "true"
					} else {
						out[k] = "false"
					}
				case nil:
					out[k] = ""
				default:
					b, err := json.Marshal(t)
					if err == nil {
						out[k] = string(b)
					}
				}
			}
			return out
		}
	}
	if strings.Contains(text, "=") && !strings.Contains(text, "\n") {
		if vals, err := url.ParseQuery(text); err == nil {
			for k, vs := range vals {
				if len(vs) > 0 {
					out[k] = vs[0]
				}
			}
			return out
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, "=")
		if i <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if name != "" {
			out[name] = strings.TrimSpace(line[i+1:])
		}
	}
	return out
}

// trimFloat 把 1.0 这种整数浮点变成 "1"。
func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// putKV 往 map 里写一项（nil map 自动创建）。
func putKV(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[strings.TrimSpace(k)] = v
	return m
}

// appendBinding 追加绑定（同 target + name + param 去重）。
func appendBinding(list []model.HarvestParamBinding, b model.HarvestParamBinding) []model.HarvestParamBinding {
	e := b.Effective()
	if !e.Valid() {
		return list
	}
	for _, old := range list {
		oe := old.Effective()
		if oe.Target == e.Target && strings.EqualFold(oe.Name, e.Name) && oe.Param == e.Param {
			return list
		}
	}
	return append(list, e)
}

// cloneStringMap 复制 map（nil 保持 nil）。
func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// itoa 数字转字符串。
func itoa(n int) string { return strconv.Itoa(n) }

// TaskOrigins 任务涉及的站点 origin（去重，保持稳定顺序）。
func TaskOrigins(task *model.HarvestTask) []string {
	if task == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		if o := originOf(strings.TrimSpace(raw)); o != "" && !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	if h := strings.TrimSpace(task.Session.Host); h != "" {
		add(h)
	}
	add(task.StartURL)
	for _, ep := range task.Endpoints {
		add(ep.URL)
	}
	return out
}
