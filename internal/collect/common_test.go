package collect

import (
	"strings"
	"testing"

	"uitest-server/internal/model"
)

// capReq 造一条录制请求（URL 里带 query，PostData 是 JSON 请求体）。
func capReq(url, postData string, headers map[string]string) model.CapturedRequest {
	return model.CapturedRequest{
		Method:     "POST",
		URL:        url,
		PostData:   postData,
		ReqHeaders: headers,
	}
}

func TestSuggestCommonFromCapturedOnlySharedValues(t *testing.T) {
	entries := []model.CapturedRequest{
		capReq("https://api.example.com/a?tenant_id=1001&page=1&only=a",
			`{"tenant_id":"1001","page":1}`,
			map[string]string{
				"X-Tenant":      "demo",
				"Authorization": "Bearer abc123",
				"Cookie":        "SESSION=s1; other=o1",
				"User-Agent":    "UA/1",
			}),
		capReq("https://api.example.com/b?tenant_id=1001&page=2&only=b",
			`{"tenant_id":"1001","page":2}`,
			map[string]string{
				"X-Tenant":      "demo",
				"Authorization": "Bearer abc123",
				"Cookie":        "SESSION=s1; other=o1",
				"User-Agent":    "UA/1",
			}),
	}
	sug := SuggestCommonFromCaptured(entries)

	if sug.Params["tenant_id"] != "1001" {
		t.Errorf("tenant_id 应该是公共参数, got %v", sug.Params)
	}
	for _, k := range []string{"page", "only"} {
		if _, exists := sug.Params[k]; exists {
			t.Errorf("%q 不该升级成公共参数, got %v", k, sug.Params)
		}
	}
	if sug.Headers["X-Tenant"] != "demo" {
		t.Errorf("X-Tenant 应该是公共请求头, got %v", sug.Headers)
	}
	for _, k := range []string{"Authorization", "Cookie", "User-Agent"} {
		if _, exists := sug.Headers[k]; exists {
			t.Errorf("%q 不该留在公共请求头里, got %v", k, sug.Headers)
		}
	}
	if sug.Cookies["SESSION"] != "s1" || sug.Cookies["other"] != "o1" {
		t.Errorf("Cookie 没拆出来, got %v", sug.Cookies)
	}
	if len(sug.Bindings) != 1 {
		t.Fatalf("Authorization 应生成 1 条绑定, got %v", sug.Bindings)
	}
	b := sug.Bindings[0].Effective()
	if b.Param != "token" || b.Target != "header" || !strings.EqualFold(b.Name, "Authorization") {
		t.Errorf("绑定不对: %+v", b)
	}
	if b.Template != "Bearer {{token}}" {
		t.Errorf("模板应保留 Bearer 前缀, got %q", b.Template)
	}
	if len(sug.Notes) == 0 {
		t.Error("应该给出汇总说明")
	}
}

func TestSuggestCommonFromCapturedEmpty(t *testing.T) {
	sug := SuggestCommonFromCaptured(nil)
	if sug.Params != nil || sug.Headers != nil || sug.Cookies != nil {
		t.Errorf("空输入应返回空建议, got %+v", sug)
	}
}

func TestParseCommonImportCookieText(t *testing.T) {
	out, notes := ParseCommonImport(CommonImport{Cookie: "Cookie: SESSION=abc; token=tok1"})
	if out.Cookies["SESSION"] != "abc" || out.Cookies["token"] != "tok1" {
		t.Errorf("Cookie 解析不对: %v", out.Cookies)
	}
	if len(notes) == 0 {
		t.Error("应该有导入说明")
	}
}

func TestParseCommonImportHeadersWithAuth(t *testing.T) {
	out, _ := ParseCommonImport(CommonImport{
		Headers: "Authorization: Bearer abc123\nCookie: SESSION=abc\nX-Tenant: demo\n# 注释行",
	})
	if len(out.Bindings) != 1 {
		t.Fatalf("鉴权头应生成 1 条绑定, got %+v", out.Bindings)
	}
	b := out.Bindings[0].Effective()
	if b.Param != "token" || b.Template != "Bearer {{token}}" {
		t.Errorf("鉴权绑定不对: %+v", b)
	}
	if out.Params["token"] != "abc123" {
		t.Errorf("导入的 token 值应写进公共参数, got %v", out.Params)
	}
	if out.Cookies["SESSION"] != "abc" {
		t.Errorf("Header 里的 Cookie 应拆进 cookies, got %v", out.Cookies)
	}
	if out.Headers["X-Tenant"] != "demo" {
		t.Errorf("普通请求头应保留, got %v", out.Headers)
	}
	if _, exists := out.Headers["Authorization"]; exists {
		t.Error("Authorization 不该留在公共请求头里")
	}
}

func TestParseCommonImportParamsFormats(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]string
	}{
		{"JSON", `{"tenant_id":"1001","n":2,"ok":true}`, map[string]string{"tenant_id": "1001", "n": "2", "ok": "true"}},
		{"query", "a=1&b=2", map[string]string{"a": "1", "b": "2"}},
		{"多行", "a=1\nb=2", map[string]string{"a": "1", "b": "2"}},
	}
	for _, c := range cases {
		out, _ := ParseCommonImport(CommonImport{Params: c.in})
		for k, v := range c.want {
			if out.Params[k] != v {
				t.Errorf("%s: params[%q]=%q, want %q（全量 %v）", c.name, k, out.Params[k], v, out.Params)
			}
		}
	}
}

func TestParseCommonImportCurl(t *testing.T) {
	curl := "curl 'https://api.example.com/api/list?tenant_id=1001&page=1' \\\n" +
		"  -H 'Accept: application/json' \\\n" +
		"  -H 'Authorization: Bearer abc123' \\\n" +
		"  --data-raw '{\"foo\":\"bar\"}'"
	out, _ := ParseCommonImport(CommonImport{Curl: curl})
	if out.Params["tenant_id"] != "1001" {
		t.Errorf("cURL URL 里的查询参数应导入, got %v", out.Params)
	}
	if _, exists := out.Params["page"]; exists {
		t.Errorf("分页参数不该导入, got %v", out.Params)
	}
	if out.Params["foo"] != "bar" {
		t.Errorf("cURL 请求体参数应导入, got %v", out.Params)
	}
	if out.Headers["Accept"] != "application/json" {
		t.Errorf("cURL 请求头应导入, got %v", out.Headers)
	}
	if len(out.Bindings) != 1 {
		t.Errorf("cURL 的 Authorization 应生成绑定, got %+v", out.Bindings)
	}
}

func TestMergeCommonIntoSuggestKeepsUserValues(t *testing.T) {
	base := &model.HarvestCommon{
		Params:  map[string]string{"tenant_id": "用户填的"},
		Headers: map[string]string{"X-Tenant": "mine"},
		Bindings: []model.HarvestParamBinding{
			{Param: "token", Target: "header", Name: "Authorization", Template: "Bearer {{token}}"},
		},
	}
	sug := CommonSuggest{
		Params:  map[string]string{"tenant_id": "建议值", "lang": "zh-CN"},
		Headers: map[string]string{"X-Tenant": "suggested", "X-New": "1"},
		Cookies: map[string]string{"SESSION": "s"},
		Bindings: []model.HarvestParamBinding{
			{Param: "token", Target: "header", Name: "authorization", Template: "Bearer {{token}}"},
			{Param: "sid", Target: "cookie", Name: "SESSION"},
		},
	}
	out := MergeCommonIntoSuggest(base, sug)
	if out.Params["tenant_id"] != "用户填的" {
		t.Errorf("用户已有的参数不该被覆盖, got %v", out.Params)
	}
	if out.Params["lang"] != "zh-CN" {
		t.Errorf("缺的参数应补上, got %v", out.Params)
	}
	if out.Headers["X-Tenant"] != "mine" || out.Headers["X-New"] != "1" {
		t.Errorf("请求头合并不对, got %v", out.Headers)
	}
	if out.Cookies["SESSION"] != "s" {
		t.Errorf("Cookie 应合并, got %v", out.Cookies)
	}
	if len(out.Bindings) != 2 {
		t.Errorf("重复绑定应去重（大小写不敏感）, got %+v", out.Bindings)
	}
	if base.Params["tenant_id"] != "用户填的" || len(base.Bindings) != 1 {
		t.Error("不应该改动传入的 base")
	}
}

func TestMergeCommonIntoSuggestNilBase(t *testing.T) {
	out := MergeCommonIntoSuggest(nil, CommonSuggest{Params: map[string]string{"a": "1"}})
	if out == nil || out.Params["a"] != "1" {
		t.Errorf("nil base 也要产出结果, got %+v", out)
	}
	if out.Headers != nil || out.Cookies != nil || out.Bindings != nil {
		t.Errorf("没内容的字段应为 nil, got %+v", out)
	}
}

func TestHarvestCommonNormalize(t *testing.T) {
	c := &model.HarvestCommon{
		Params:  map[string]string{" token ": "t", "  ": "x"},
		Headers: map[string]string{"": "y"},
		Cookies: map[string]string{" SESSION ": "s"},
		Bindings: []model.HarvestParamBinding{
			{Param: "", Target: "header", Name: "X"},
			{Param: "token", Target: "", Name: ""},
			{Param: "token", Target: "header", Name: "Authorization"},
			{Param: " token ", Target: "header", Name: "authorization"},
			{Param: "sid", Target: "cookies", Name: "SESSION"},
		},
		Refresh: &model.HarvestSessionRefresh{Enabled: true},
	}
	c.Normalize()
	if _, exists := c.Params["token"]; !exists {
		t.Errorf("参数键应去掉首尾空白, got %v", c.Params)
	}
	if len(c.Params) != 1 || len(c.Cookies) != 1 || len(c.Headers) != 0 {
		t.Errorf("空键应被清掉, got params=%v headers=%v cookies=%v", c.Params, c.Headers, c.Cookies)
	}
	// 去重键是 target + name + param：token→header:Authorization 与 token→header:token 是两条
	if len(c.Bindings) != 3 {
		t.Fatalf("无效/重复绑定应被清掉, got %+v", c.Bindings)
	}
	if c.Bindings[0].Name != "token" {
		t.Errorf("目标名留空时用参数名, got %+v", c.Bindings[0])
	}
	if c.Bindings[1].Name != "Authorization" || c.Bindings[1].Param != "token" {
		t.Errorf("大小写不同的重复绑定应只留一条, got %+v", c.Bindings[1])
	}
	if c.Bindings[2].Target != "cookie" {
		t.Errorf("cookies 应归一成 cookie, got %+v", c.Bindings[2])
	}
	if c.Refresh == nil || c.Refresh.TokenParam != "token" {
		t.Errorf("Refresh 应补默认 TokenParam, got %+v", c.Refresh)
	}

	var nilCommon *model.HarvestCommon
	nilCommon.Normalize() // 不应 panic
}

func TestIsPagingParam(t *testing.T) {
	for _, k := range []string{"page", "PageNo", "page_size", "offset", "limit", "cursor", "next_token"} {
		if !isPagingParam(k) {
			t.Errorf("%q 应判定为分页参数", k)
		}
	}
	for _, k := range []string{"tenant_id", "token", "user_id"} {
		if isPagingParam(k) {
			t.Errorf("%q 不该判定为分页参数", k)
		}
	}
}

func TestTaskOrigins(t *testing.T) {
	task := &model.HarvestTask{
		StartURL: "https://a.example.com/home?x=1",
		Session:  model.HarvestSession{Host: "https://a.example.com"},
		Endpoints: []model.HarvestEndpoint{
			{URL: "https://b.example.com/api/1"},
			{URL: "https://a.example.com/api/2"},
		},
	}
	got := TaskOrigins(task)
	if len(got) != 2 {
		t.Fatalf("应去重成 2 个 origin, got %v", got)
	}
	if TaskOrigins(nil) != nil {
		t.Error("nil task 应返回 nil")
	}
}

func TestParseCommonImportSkipsPagingParams(t *testing.T) {
	out, notes := ParseCommonImport(CommonImport{Params: "tenant_id=1001&page=2&limit=20"})
	if out.Params["tenant_id"] != "1001" {
		t.Errorf("普通参数应导入, got %v", out.Params)
	}
	for _, k := range []string{"page", "limit"} {
		if _, exists := out.Params[k]; exists {
			t.Errorf("分页参数 %q 不该导入公共参数, got %v", k, out.Params)
		}
	}
	if !strings.Contains(strings.Join(notes, "；"), "跳过分页参数") {
		t.Errorf("应提示已跳过分页参数, got %v", notes)
	}
}
