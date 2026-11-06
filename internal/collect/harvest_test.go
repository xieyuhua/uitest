package collect

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"uitest-server/internal/model"
)

func TestExpandTemplate(t *testing.T) {
	t.Setenv("UITEST_TEST_ENV", "from-env")
	vars := map[string]string{"token": "abc123", "page": "2"}
	cases := []struct{ name, tpl, want string }{
		{"无占位符", "plain", "plain"},
		{"单个变量", "{{token}}", "abc123"},
		{"变量嵌在 URL 里", "/api?t={{token}}&p={{page}}", "/api?t=abc123&p=2"},
		{"未知变量原样保留", "{{nope}}", "{{nope}}"},
		{"环境变量", "{{env:UITEST_TEST_ENV}}", "from-env"},
	}
	for _, c := range cases {
		if got := expand(c.tpl, vars); got != c.want {
			t.Errorf("%s: expand(%q)=%q, want %q", c.name, c.tpl, got, c.want)
		}
	}
}

func TestSessionVarsBuiltinsAndParams(t *testing.T) {
	sess := resolvedSession{params: map[string]string{"token": "t1", "date": "覆盖不了"}}
	vars := sessionVars(sess)
	for _, k := range []string{"now", "date", "datetime", "timestamp", "uuid"} {
		if strings.TrimSpace(vars[k]) == "" {
			t.Errorf("内置变量 %q 为空", k)
		}
	}
	if vars["token"] != "t1" {
		t.Errorf("公共参数 token 应注入, got %q", vars["token"])
	}
	if vars["date"] == "覆盖不了" {
		t.Error("公共参数不应覆盖内置变量")
	}
}

func TestTruncateKeepsValidUTF8(t *testing.T) {
	in := "中文测试"
	got := truncate(in, 4)
	if !utf8.ValidString(got) {
		t.Fatalf("截断后不是合法 UTF-8: %q", got)
	}
	if len(got) > 4 {
		t.Errorf("截断后长度 %d 超过上限 4", len(got))
	}
	if truncate("abc", 10) != "abc" {
		t.Error("未超过上限时应原样返回")
	}
	if truncate("abc", 0) != "abc" {
		t.Error("max<=0 视为不截断")
	}
	if truncate("中文", 6) != "中文" {
		t.Error("刚好等于上限时应原样返回")
	}
}

func TestPreviewTextKeepsValidUTF8(t *testing.T) {
	in := strings.Repeat("中文", 10)
	got := previewText(in, 7)
	if !utf8.ValidString(got) {
		t.Fatalf("previewText 产生了非法 UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Error("previewText 结果里出现了替换字符")
	}
	if previewText("  abc  ", 100) != "abc" {
		t.Error("previewText 应去掉首尾空白")
	}
	if previewText("abc", 0) != "abc" {
		t.Error("max<=0 视为不截断")
	}
}

func TestEffectiveHeadersTokenOverride(t *testing.T) {
	ep := model.HarvestEndpoint{Headers: map[string]string{
		"Authorization": "Bearer recorded-old",
		"Blade-Auth":    "bearer recorded-old",
		"X-Trace":       "trace-1",
		"Cookie":        "sid=stale",
	}}
	sess := resolvedSession{
		commonHeaders: map[string]string{"X-Common": "c1", "X-Empty": "  "},
		headers:       map[string]string{"Authorization": "Bearer browser-old", "Blade-Auth": "bearer browser-old"},
		token:         "fresh",
		tokenHeader:   "Authorization",
		tokenPrefix:   "Bearer ",
	}
	got := effectiveHeaders(ep, sess)

	if v := got.Get("Authorization"); v != "Bearer fresh" {
		t.Errorf("Authorization 应换成新 token, got %q", v)
	}
	if v := got.Get("Blade-Auth"); v != "bearer fresh" {
		t.Errorf("Blade-Auth 应保留原前缀风格且不重复前缀, got %q", v)
	}
	if v := got.Get("X-Trace"); v != "trace-1" {
		t.Errorf("业务头应保留, got %q", v)
	}
	if v := got.Get("X-Common"); v != "c1" {
		t.Errorf("任务级公共头应注入, got %q", v)
	}
	if v := got.Get("Cookie"); v != "" {
		t.Errorf("录制里的 Cookie 不应发出去, got %q", v)
	}
	if v := got.Get("X-Empty"); v != "" {
		t.Errorf("空的公共头不应注入, got %q", v)
	}
}

func TestEffectiveHeadersModeOff(t *testing.T) {
	ep := model.HarvestEndpoint{Headers: map[string]string{"X-Trace": "t"}}
	sess := resolvedSession{headerMode: "off", headers: map[string]string{"Authorization": "Bearer x"}}
	got := effectiveHeaders(ep, sess)
	if got.Get("Authorization") != "" {
		t.Error("header_mode=off 时不应带浏览器里的登录态头")
	}
	if got.Get("X-Trace") != "t" {
		t.Error("接口自己的业务头应保留")
	}
}

func TestKeepScheme(t *testing.T) {
	cases := []struct{ old, want, expect string }{
		{"Bearer old", "Bearer new", "bearer new"},
		{"bearer old", "Bearer new", "bearer new"},
		{"blade old", "Bearer new", "Bearer new"},
		{"bearer old", "plain", "bearer plain"},
		{"plain old", "plain new", "plain new"},
	}
	for _, c := range cases {
		if got := keepScheme(c.old, c.want); got != c.expect {
			t.Errorf("keepScheme(%q,%q)=%q, want %q", c.old, c.want, got, c.expect)
		}
	}
	if strings.Contains(keepScheme("bearer x", "Bearer fresh"), "Bearer fresh") {
		t.Error("不应拼出重复的认证前缀")
	}
}

func TestHasHeader(t *testing.T) {
	parts := []string{"Accept: */*", "Cookie: a=1"}
	if !hasHeader(parts, "Cookie: ") {
		t.Error("应匹配到 Cookie 头")
	}
	if hasHeader(parts, "Authorization: ") {
		t.Error("不存在的头不应匹配")
	}
	if hasHeader(nil, "Cookie: ") {
		t.Error("空列表不应匹配")
	}
}

func TestOriginAndHost(t *testing.T) {
	if got := originOf(" https://a.example.com/x/y?z=1 "); got != "https://a.example.com" {
		t.Errorf("originOf=%q", got)
	}
	if got := originOf("not a url"); got != "" {
		t.Errorf("非法地址应返回空, got %q", got)
	}
	if got := hostOf("https://a.example.com:8443/x"); got != "a.example.com:8443" {
		t.Errorf("hostOf=%q", got)
	}
}

func TestSetQueryParam(t *testing.T) {
	got := setQueryParam("https://a.com/x?p=1", "p", "2")
	if !strings.Contains(got, "p=2") || strings.Contains(got, "p=1") {
		t.Errorf("setQueryParam=%q", got)
	}
	if got := setQueryParam("https://a.com/x", "q", "中文 值"); !strings.Contains(got, "q=") {
		t.Errorf("应追加参数, got %q", got)
	}
	if got := setQueryParam("::bad::", "q", "1"); got != "::bad::" {
		t.Errorf("非法 URL 应原样返回, got %q", got)
	}
}

func TestRequestHeadLineNoDuplicateCookie(t *testing.T) {
	ep := model.HarvestEndpoint{Headers: map[string]string{"X-Trace": "t"}}
	sess := resolvedSession{
		headers:   map[string]string{"Authorization": "Bearer a"},
		userAgent: "UA/1.0",
		referer:   "https://a.com/",
		cookies: map[string][]*http.Cookie{
			"https://a.com": {
				{Name: "sid", Value: "1"},
				{Name: "sid", Value: "2"},
				{Name: "lang", Value: "zh"},
			},
		},
	}
	line := requestHeadLine(ep, sess)
	if strings.Count(line, "Cookie: ") != 1 {
		t.Errorf("Cookie 头应只有一处, got %q", line)
	}
	if strings.Contains(line, "sid=1; sid=2") {
		t.Errorf("同名 Cookie 应去重, got %q", line)
	}
	if !strings.Contains(line, "User-Agent: UA/1.0") || !strings.Contains(line, "Referer: https://a.com/") {
		t.Errorf("UA / Referer 应补齐, got %q", line)
	}
}
