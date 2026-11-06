package collect

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"uitest-server/internal/model"
)

func refreshTask() *model.HarvestTask {
	return &model.HarvestTask{
		Common: &model.HarvestCommon{
			Refresh: &model.HarvestSessionRefresh{Enabled: true},
		},
	}
}

func TestFindSessionTokenPreference(t *testing.T) {
	sess := resolvedSession{headers: map[string]string{
		"X-Tenant":      "demo",
		"X-Token":       "from-x-token",
		"Authorization": "Bearer from-auth",
	}}
	hdr, val := findSessionToken(sess, "")
	if !strings.EqualFold(hdr, "Authorization") || val != "Bearer from-auth" {
		t.Errorf("应优先取 Authorization, got %q=%q", hdr, val)
	}
	hdr, val = findSessionToken(sess, "x-token")
	if hdr != "X-Token" || val != "from-x-token" {
		t.Errorf("指定头时应只看它（大小写不敏感）, got %q=%q", hdr, val)
	}
	if hdr, val = findSessionToken(sess, "X-Nope"); hdr != "" || val != "" {
		t.Errorf("指定头不存在时应返回空, got %q=%q", hdr, val)
	}
}

func TestFindSessionTokenFallbackToTokenField(t *testing.T) {
	sess := resolvedSession{token: "abc", tokenHeader: "Blade-Auth", tokenPrefix: "Bearer "}
	hdr, val := findSessionToken(sess, "")
	if hdr != "Blade-Auth" || val != "Bearer abc" {
		t.Errorf("没有请求头时应回落 token 字段, got %q=%q", hdr, val)
	}
	if hdr, val = findSessionToken(resolvedSession{}, ""); hdr != "" || val != "" {
		t.Errorf("空会话应返回空, got %q=%q", hdr, val)
	}
}

func TestApplySessionRefreshDisabledOrMissing(t *testing.T) {
	h := &Harvester{}
	sess := resolvedSession{headers: map[string]string{"Authorization": "Bearer x"}}
	if got := h.applySessionRefresh(context.Background(), "", &model.HarvestTask{}, &sess); got != nil {
		t.Errorf("没有 Common 不该刷新, got %v", got)
	}
	task := refreshTask()
	task.Common.Refresh = nil
	if got := h.applySessionRefresh(context.Background(), "", task, &sess); got != nil {
		t.Errorf("Refresh 为 nil 不该刷新, got %v", got)
	}
	task = refreshTask()
	task.Common.Refresh.Enabled = false
	if got := h.applySessionRefresh(context.Background(), "", task, &sess); got != nil {
		t.Errorf("没启用不该刷新, got %v", got)
	}
	if len(sess.bindings) != 0 {
		t.Errorf("不该自动加绑定, got %+v", sess.bindings)
	}
}

func TestApplySessionRefreshTokenAndAutoBinding(t *testing.T) {
	h := &Harvester{}
	task := refreshTask()
	sess := resolvedSession{headers: map[string]string{"Authorization": "Bearer NEW_TOKEN"}}
	changed := h.applySessionRefresh(context.Background(), "", task, &sess)

	if sess.params["token"] != "NEW_TOKEN" {
		t.Errorf("参数 token 应是去掉 Bearer 的新 token, got %v", sess.params)
	}
	if len(sess.bindings) != 1 {
		t.Fatalf("应自动补一条绑定, got %+v", sess.bindings)
	}
	b := sess.bindings[0].Effective()
	if b.Param != "token" || b.Target != "header" || !strings.EqualFold(b.Name, "Authorization") || b.Template != "Bearer {{token}}" {
		t.Errorf("自动绑定不对: %+v", b)
	}
	joined := strings.Join(changed, "、")
	if !strings.Contains(joined, "参数:token") || !strings.Contains(joined, "绑定:Authorization") {
		t.Errorf("变更项应包含参数和绑定, got %v", changed)
	}
}

func TestApplySessionRefreshKeepsExistingBinding(t *testing.T) {
	h := &Harvester{}
	task := refreshTask()
	sess := resolvedSession{
		headers: map[string]string{"authorization": "Bearer NEW"},
		bindings: []model.HarvestParamBinding{
			{Param: "token", Target: "header", Name: "Authorization", Template: "Bearer {{token}}"},
		},
	}
	changed := h.applySessionRefresh(context.Background(), "", task, &sess)
	if len(sess.bindings) != 1 {
		t.Errorf("已有同参同头绑定不该重复添加, got %+v", sess.bindings)
	}
	if strings.Join(changed, "、") != "参数:token" {
		t.Errorf("只该更新参数, got %v", changed)
	}
}

func TestApplySessionRefreshCustomTokenParamAndHeader(t *testing.T) {
	h := &Harvester{}
	task := refreshTask()
	task.Common.Refresh.TokenParam = "accessToken"
	task.Common.Refresh.TokenHeader = "X-Token"
	sess := resolvedSession{headers: map[string]string{
		"X-Token":       "tk1",
		"Authorization": "Bearer other",
	}}
	changed := h.applySessionRefresh(context.Background(), "", task, &sess)
	if sess.params["accessToken"] != "tk1" {
		t.Errorf("应按指定头取 token, got %v", sess.params)
	}
	if _, exists := sess.params["token"]; exists {
		t.Errorf("不该再写默认参数名, got %v", sess.params)
	}
	if len(sess.bindings) != 1 || sess.bindings[0].Effective().Template != "{{accessToken}}" {
		t.Errorf("绑定模板应引用自定义参数名, got %+v / %v", sess.bindings, changed)
	}
}

func TestApplySessionRefreshCookieMapping(t *testing.T) {
	h := &Harvester{}
	task := refreshTask()
	task.Common.Refresh.Cookies = map[string]string{"SESSION": "sid", "NOPE": "nope"}
	sess := resolvedSession{
		cookies: map[string][]*http.Cookie{
			"https://api.example.com": {{Name: "SESSION", Value: "s1", Path: "/"}},
		},
		headers: map[string]string{},
	}
	changed := h.applySessionRefresh(context.Background(), "", task, &sess)
	if sess.params["sid"] != "s1" {
		t.Errorf("Cookie 应映射成参数, got %v", sess.params)
	}
	if _, exists := sess.params["nope"]; exists {
		t.Errorf("取不到的 Cookie 不该写参数, got %v", sess.params)
	}
	if strings.Join(changed, "、") != "参数:sid" {
		t.Errorf("变更项不对, got %v", changed)
	}
}

func TestApplyQueryBodyBindings(t *testing.T) {
	sess := resolvedSession{
		params: map[string]string{"token": "T1", "tenant": "1001"},
		bindings: []model.HarvestParamBinding{
			{Param: "tenant", Target: "query", Name: "tenant_id"},
			{Param: "token", Target: "body", Name: "access_token", Template: "Bearer {{token}}"},
			{Param: "missing", Target: "query", Name: "x"},
		},
	}
	target, body := applyQueryBodyBindings("https://api.example.com/list?page=1&tenant_id=old", `{"a":1,"access_token":"old"}`, sess)
	u, err := url.Parse(target)
	if err != nil {
		t.Fatalf("URL 解析失败: %v", err)
	}
	if got := u.Query().Get("tenant_id"); got != "1001" {
		t.Errorf("query 绑定应覆盖同名值, got %q（%s）", got, target)
	}
	if u.Query().Get("page") != "1" {
		t.Errorf("原有 query 不该丢, got %s", target)
	}
	if !strings.Contains(body, `"access_token":"Bearer T1"`) || !strings.Contains(body, `"a":1`) {
		t.Errorf("body 绑定应覆盖原有字段并保留其它字段, got %s", body)
	}
	if strings.Contains(body, "missing") {
		t.Errorf("取不到值的绑定不该写进 body, got %s", body)
	}
}

func TestApplyQueryBodyBindingsNonJSONBodyUntouched(t *testing.T) {
	sess := resolvedSession{
		params:   map[string]string{"token": "T1"},
		bindings: []model.HarvestParamBinding{{Param: "token", Target: "body", Name: "access_token"}},
	}
	target, body := applyQueryBodyBindings("https://api.example.com/x", "a=1&b=2", sess)
	if body != "a=1&b=2" {
		t.Errorf("表单请求体不该被改成 JSON, got %s", body)
	}
	if target != "https://api.example.com/x" {
		t.Errorf("没有 query 绑定不该改动 URL, got %s", target)
	}
}

func TestCookieBindingValues(t *testing.T) {
	sess := resolvedSession{
		params: map[string]string{"sid": "S1"},
		bindings: []model.HarvestParamBinding{
			{Param: "sid", Target: "cookie", Name: "SESSION"},
			{Param: "sid", Target: "header", Name: "X-Sid"},
		},
	}
	got := cookieBindingValues(sess)
	if got["SESSION"] != "S1" {
		t.Errorf("cookie 绑定应展开成值, got %v", got)
	}
	if _, exists := got["X-Sid"]; exists {
		t.Errorf("header 绑定不该出现在 Cookie 里, got %v", got)
	}
	if len(cookieBindingValues(resolvedSession{})) != 0 {
		t.Error("没有绑定时应返回空 map")
	}
}

func TestSetJarCookiesPriority(t *testing.T) {
	const origin = "https://api.example.com"
	read := func(sess resolvedSession) map[string]string {
		jar, _ := cookiejar.New(nil)
		setJarCookies(jar, sess)
		u, _ := url.Parse(origin + "/")
		out := map[string]string{}
		for _, c := range jar.Cookies(u) {
			out[c.Name] = c.Value
		}
		return out
	}
	base := resolvedSession{
		origins:       []string{origin},
		cookies:       map[string][]*http.Cookie{origin: {{Name: "SESSION", Value: "browser", Path: "/"}}},
		commonCookies: map[string]string{"SESSION": "common", "extra": "e"},
		params:        map[string]string{"sid": "bound"},
		bindings:      []model.HarvestParamBinding{{Param: "sid", Target: "cookie", Name: "SESSION"}},
	}
	got := read(base)
	if got["SESSION"] != "bound" {
		t.Errorf("参数绑定应覆盖浏览器 Cookie, got %v", got)
	}
	if got["extra"] != "e" {
		t.Errorf("公共 Cookie 应种进 jar, got %v", got)
	}

	base.bindings = nil
	if got = read(base); got["SESSION"] != "browser" {
		t.Errorf("浏览器 Cookie 应覆盖公共 Cookie, got %v", got)
	}

	base.cookies = nil
	if got = read(base); got["SESSION"] != "common" {
		t.Errorf("没有浏览器 Cookie 时应回落到公共 Cookie, got %v", got)
	}
}

func TestEffectiveHeadersTokenBindingWinsOverStaleBrowserHeader(t *testing.T) {
	ep := model.HarvestEndpoint{
		URL:     "https://api.example.com/x",
		Headers: map[string]string{"Authorization": "Bearer OLD_RECORDED", "X-A": "1", "Cookie": "SESSION=old"},
	}
	sess := resolvedSession{
		headerMode: "auth",
		headers:    map[string]string{"Authorization": "Bearer STALE_BROWSER"},
		params:     map[string]string{"token": "NEW_TOKEN"},
		bindings: []model.HarvestParamBinding{
			{Param: "token", Target: "header", Name: "Authorization", Template: "Bearer {{token}}"},
		},
	}
	out := effectiveHeaders(ep, sess)
	if got := out.Get("Authorization"); got != "Bearer NEW_TOKEN" {
		t.Errorf("参数绑定应覆盖浏览器里的旧 token, got %q", got)
	}
	if out.Get("X-A") != "1" {
		t.Errorf("接口自己的普通请求头应保留, got %q", out.Get("X-A"))
	}
	if out.Get("Cookie") != "" {
		t.Errorf("接口里的 Cookie 不该发出去, got %q", out.Get("Cookie"))
	}
}

func TestEffectiveHeadersCommonTemplateBeatsBrowserHeader(t *testing.T) {
	ep := model.HarvestEndpoint{URL: "https://api.example.com/x"}
	sess := resolvedSession{
		headers:       map[string]string{"X-Token": "stale"},
		commonHeaders: map[string]string{"X-Token": "{{token}}"},
		params:        map[string]string{"token": "FRESH"},
	}
	if got := effectiveHeaders(ep, sess).Get("X-Token"); got != "FRESH" {
		t.Errorf("公共头里的 {{token}} 应覆盖浏览器旧值, got %q", got)
	}

	sess.commonHeaders = map[string]string{"X-Token": "static"}
	if got := effectiveHeaders(ep, sess).Get("X-Token"); got != "stale" {
		t.Errorf("没有模板的公共头应让位给浏览器实时值, got %q", got)
	}
}

func TestSessionScaffoldCarriesCommon(t *testing.T) {
	task := &model.HarvestTask{
		StartURL: "https://api.example.com/home",
		Endpoints: []model.HarvestEndpoint{
			{URL: "https://api.example.com/api/list"},
		},
		Common: &model.HarvestCommon{
			Params:  map[string]string{"tenant_id": "1001"},
			Headers: map[string]string{"X-Tenant": "demo"},
			Cookies: map[string]string{"SESSION": "s1"},
			Bindings: []model.HarvestParamBinding{
				{Param: "token", Target: "header", Name: "Authorization"},
			},
		},
	}
	sess := sessionScaffold(task)
	if sess.params["tenant_id"] != "1001" || sess.commonHeaders["X-Tenant"] != "demo" {
		t.Errorf("公共参数 / 请求头应带进会话, got %+v / %+v", sess.params, sess.commonHeaders)
	}
	if sess.commonCookies["SESSION"] != "s1" {
		t.Errorf("公共 Cookie 应带进会话, got %v", sess.commonCookies)
	}
	if len(sess.bindings) != 1 {
		t.Errorf("绑定应带进会话, got %+v", sess.bindings)
	}
	if len(sess.origins) == 0 {
		t.Error("站点 origin 应带进会话")
	}
	if task.Common.Params["tenant_id"] != "1001" {
		t.Error("不该改动任务配置")
	}
}

func TestSplitStorageKey(t *testing.T) {
	cases := []struct{ in, store, name string }{
		{"localStorage:token", "localStorage", "token"},
		{"sessionStorage:token", "sessionStorage", "token"},
		{"token", "localStorage", "token"},
		{"  sessionStorage: a.b  ", "sessionStorage", "a.b"},
		{"other:key", "localStorage", "other:key"},
	}
	for _, c := range cases {
		store, name := splitStorageKey(c.in)
		if store != c.store || name != c.name {
			t.Errorf("splitStorageKey(%q)=(%q,%q), want (%q,%q)", c.in, store, name, c.store, c.name)
		}
	}
}
