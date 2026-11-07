package collect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"uitest-server/internal/model"
)

func TestMaskSecretKeepsEdges(t *testing.T) {
	if got := maskSecret("short"); got != "***" {
		t.Errorf("短值应整体打码, got %q", got)
	}
	got := maskSecret("abcdefghijklmnopqrstuvwxyz")
	if strings.Contains(got, "ghijklmnopq") {
		t.Errorf("长值中间应打码, got %q", got)
	}
	if !strings.HasPrefix(got, "abcd") || !strings.HasSuffix(got, "wxyz") {
		t.Errorf("长值应保留首尾各 4 位, got %q", got)
	}
	if got := maskAuthValue("Bearer abcdefghijklmnop"); !strings.HasPrefix(got, "Bearer ") || strings.Contains(got, "cdefghijklmn") {
		t.Errorf("Bearer 值应保留前缀只打码凭据, got %q", got)
	}
}

func TestMaskCookieHeaderKeepsNames(t *testing.T) {
	got := maskCookieHeader("sid=supersecretvalue; theme=dark")
	if !strings.Contains(got, "sid=") || strings.Contains(got, "supersecretvalue") {
		t.Errorf("Cookie 名要留、值要打码, got %q", got)
	}
	if strings.Contains(got, "dark") {
		t.Errorf("短 Cookie 值也应打码, got %q", got)
	}
	// Set-Cookie 的 Path / HttpOnly 这类属性要原样保留
	sc := maskCookieHeader("sid=cookiesecret1234; Path=/; HttpOnly")
	if !strings.Contains(sc, "Path=/") || !strings.Contains(sc, "HttpOnly") || strings.Contains(sc, "cookiesecret1234") {
		t.Errorf("Set-Cookie 属性应保留, got %q", sc)
	}
}

func TestMaskRequestBodyAndParams(t *testing.T) {
	body := maskRequestBody(`{"username":"bob","password":"hunter2hunter2","nested":{"access_token":"tok_1234567890"}}`)
	if strings.Contains(body, "hunter2hunter2") || strings.Contains(body, "tok_1234567890") {
		t.Fatalf("请求体里的密码 / token 没打码: %s", body)
	}
	if !strings.Contains(body, "bob") {
		t.Errorf("普通字段不应被改, got %s", body)
	}
	form := maskRequestBody("user=bob&password=hunter2hunter2")
	if strings.Contains(form, "hunter2hunter2") || !strings.Contains(form, "user=bob") {
		t.Errorf("表单请求体应按参数名打码, got %q", form)
	}
	if got := maskIfSensitive("page", "3"); got != "3" {
		t.Errorf("非敏感参数不应打码, got %q", got)
	}
	if got := maskIfSensitive("access_token", "abcdef123456"); strings.Contains(got, "abcdef123456") {
		t.Errorf("token 参数应打码, got %q", got)
	}
}

// 会话 ID 常见写法（sid / _sid / sessionid）要打码，但普通单词不能被误伤。
func TestMaskIfSensitiveSessionID(t *testing.T) {
	for _, name := range []string{"sid", "SID", "jsessionid", "PHPSESSID", "shop_sid", "x-sid"} {
		got := maskIfSensitive(name, "abcdef1234567890")
		if strings.Contains(got, "abcdef1234567890") {
			t.Errorf("%s 是会话 ID，应打码, got %q", name, got)
		}
	}
	for _, name := range []string{"consider", "resident", "page", "tenant"} {
		got := maskIfSensitive(name, "abcdef1234567890")
		if got != "abcdef1234567890" {
			t.Errorf("%s 不是敏感参数，不该打码, got %q", name, got)
		}
	}
}

// URL 里的敏感 query 值要打码，参数名和普通参数保留。
func TestMaskURLSecretsKeepsParamNames(t *testing.T) {
	got := maskURLSecrets("https://x.com/api/orders?page=2&size=5&token=abcdef1234567890&tenant=42")
	if strings.Contains(got, "abcdef1234567890") {
		t.Errorf("URL 里的 token 没打码: %s", got)
	}
	for _, want := range []string{"token=", "page=2", "size=5", "tenant=42"} {
		if !strings.Contains(got, want) {
			t.Errorf("URL 丢了 %q: %s", want, got)
		}
	}
	if plain := maskURLSecrets("https://x.com/api/orders?page=2"); plain != "https://x.com/api/orders?page=2" {
		t.Errorf("没有敏感参数时不该改 URL: %s", plain)
	}
}

// 试抓详情里的请求 / 响应报文必须来自「真正发出去、收回来」的那一次请求。
func TestFetchTraceRecordsRealRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "abcdef1234567890" {
			t.Errorf("服务端没收到接口配置的请求头: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Set-Cookie", "sid=cookiesecret1234; Path=/; HttpOnly")
		_, _ = w.Write([]byte(`{"total":1,"data":[{"id":1,"name":"甲"}]}`))
	}))
	defer srv.Close()

	h := NewHarvester(nil, nil, nil, t.TempDir())
	ep := model.HarvestEndpoint{
		ID: "ep1", Name: "列表", Method: "GET", Enabled: true,
		URL:     srv.URL + "/api/list?page=1&token=abcdefghijkl",
		Headers: map[string]string{"X-Api-Key": "abcdef1234567890"},
	}
	pg := NewPaginatorFor(model.HarvestPagination{Mode: "page", Param: "page", Start: 1, PageSize: 2, SizeParam: "size"}, "GET")
	target, body := pg.Apply(ep.URL, "")

	sess := resolvedSession{}
	var tr fetchTrace
	status, raw, retries, err := h.fetch(context.Background(), h.newClient(model.DefaultThrottle(), sess),
		ep, target, body, sess, model.DefaultThrottle(), &tr)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if status != 200 || tr.Status != 200 {
		t.Fatalf("状态码不对: status=%d trace=%d", status, tr.Status)
	}
	if tr.ResponseSize != len(raw) || tr.ResponseSize == 0 {
		t.Errorf("响应大小没记下来: trace=%d len=%d", tr.ResponseSize, len(raw))
	}
	if tr.Attempts != 1 {
		t.Errorf("首次成功应记 1 次尝试, got %d", tr.Attempts)
	}

	pr := buildPreviewRequest(ep, target, body, sess, pg, &tr, retries, 12)
	if pr.Method != "GET" {
		t.Errorf("method = %q", pr.Method)
	}
	if !strings.Contains(pr.URL, "size=2") || !strings.Contains(pr.URL, "page=1") {
		t.Errorf("URL 应带上分页参数, got %q", pr.URL)
	}
	if pr.QueryParams["page"] != "1" || pr.QueryParams["size"] != "2" {
		t.Errorf("query 明细不对: %v", pr.QueryParams)
	}
	if pr.QueryParams["token"] == "abcdefghijkl" {
		t.Errorf("query 里的 token 没打码: %v", pr.QueryParams)
	}
	if pr.Headers["X-Api-Key"] == "abcdef1234567890" {
		t.Errorf("请求头里的密钥没打码: %v", pr.Headers)
	}
	if !strings.Contains(pr.Headers["X-Api-Key"], "…") {
		t.Errorf("打码应保留首尾可辨认, got %q", pr.Headers["X-Api-Key"])
	}
	if pr.DurationMS < 0 || pr.Retries != 0 {
		t.Errorf("耗时/重试记录不对: %+v", pr)
	}

	resp := buildPreviewResponse(status, &tr, raw)
	if resp.Status != 200 || resp.StatusText != "OK" {
		t.Errorf("响应状态不对: %+v", resp)
	}
	if resp.Size != len(raw) {
		t.Errorf("响应大小 = %d, want %d", resp.Size, len(raw))
	}
	if resp.ContentType == "" || !strings.Contains(resp.ContentType, "application/json") {
		t.Errorf("Content-Type 没取到: %q", resp.ContentType)
	}
	if resp.BodyPreview == "" || !strings.Contains(resp.BodyPreview, "甲") {
		t.Errorf("响应体预览为空: %q", resp.BodyPreview)
	}
	if strings.Contains(resp.Headers["Set-Cookie"], "cookiesecret1234") {
		t.Errorf("Set-Cookie 没打码: %q", resp.Headers["Set-Cookie"])
	}
}

// 端到端：Preview 要带出请求、响应和过程步骤，且 Cookie 不能明文出现。
func TestPreviewIncludesDetailAndProcess(t *testing.T) {
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":2,"data":[{"id":1,"name":"甲"},{"id":2,"name":"乙"}]}`))
	}))
	defer srv.Close()

	h := NewHarvester(nil, nil, nil, t.TempDir())
	task := model.HarvestTask{
		ID: "t1", Name: "演示任务",
		Endpoints: []model.HarvestEndpoint{{
			ID: "ep1", Name: "列表", Method: "GET", Enabled: true,
			URL:         srv.URL + "/api/list?page=1",
			RecordsPath: "data",
			Pagination:  model.HarvestPagination{Mode: "page", Param: "page", Start: 1, PageSize: 1, TotalPath: "total"},
			Fields: []model.HarvestField{
				{Column: "id", Path: "id"},
				{Column: "name", Path: "name"},
			},
		}},
	}
	task.Normalize()
	// 预置 Cookie 缓存，避免试抓真的去启动内置浏览器
	key := sessionTarget(&task)
	h.cookies[key] = cachedCookies{list: []*http.Cookie{{Name: "sid", Value: "cookiesecret1234"}}, fetched: time.Now()}

	out := h.Preview(context.Background(), task, 5, "", PreviewOptions{})
	if len(out) != 1 {
		t.Fatalf("应只试抓 1 个接口, got %d", len(out))
	}
	p := out[0]
	if p.Error != "" {
		t.Fatalf("试抓报错: %s", p.Error)
	}
	if p.Warning != "" {
		t.Errorf("已拿到 Cookie 不应该提示无登录态: %s", p.Warning)
	}
	if gotCookie == "" || !strings.Contains(gotCookie, "sid=") {
		t.Fatalf("服务端没收到 Cookie: %q", gotCookie)
	}
	if p.Request == nil || p.Response == nil {
		t.Fatalf("缺少请求/响应明细")
	}
	if p.Request.Method != "GET" || !strings.Contains(p.Request.URL, "page=1") {
		t.Errorf("请求行不对: %+v", p.Request)
	}
	if ck := p.Request.Headers["Cookie"]; ck == "" || strings.Contains(ck, "cookiesecret1234") {
		t.Errorf("Cookie 头应展示但打码, got %q", ck)
	}
	if p.Response.Status != 200 || p.Response.Size == 0 {
		t.Errorf("响应明细不对: %+v", p.Response)
	}
	if len(p.Process) < 4 {
		t.Errorf("过程步骤太少: %v", p.Process)
	}
	joined := strings.Join(p.Process, "\n")
	for _, want := range []string{"会话：", "分页：", "请求：GET", "响应：200"} {
		if !strings.Contains(joined, want) {
			t.Errorf("过程里缺少 %q:\n%s", want, joined)
		}
	}
	if p.Session == "" {
		t.Errorf("缺少登录态摘要")
	}
	// 时间线：会话 → 变量 → 请求 → 响应 → 解析，顺序固定、编号从 1 开始
	if len(p.Timeline) < 5 {
		t.Fatalf("时间线步骤太少: %+v", p.Timeline)
	}
	var actions []string
	for i, st := range p.Timeline {
		if st.Index != i+1 {
			t.Errorf("时间线编号不连续: idx=%d want=%d", st.Index, i+1)
		}
		if st.Status == "" {
			t.Errorf("时间线步骤缺少状态: %+v", st)
		}
		actions = append(actions, st.Action)
	}
	seq := strings.Join(actions, ",")
	for _, want := range []string{"session", "vars", "request", "response", "parse"} {
		if !strings.Contains(seq, want) {
			t.Errorf("时间线缺少 %q: %s", want, seq)
		}
	}
	if p.Timeline[0].Action != "session" {
		t.Errorf("第一步应该是会话, got %q", p.Timeline[0].Action)
	}
	last := p.Timeline[len(p.Timeline)-1]
	if last.Action != "parse" || last.Status != "ok" {
		t.Errorf("最后一步应该是解析成功, got %+v", last)
	}
	if last.Details["本页记录"] == "" {
		t.Errorf("解析步骤缺少记录数明细: %+v", last.Details)
	}
	// 时间线里的 Cookie 也必须打码
	for _, st := range p.Timeline {
		for k, v := range st.Details {
			if strings.Contains(v, "cookiesecret1234") {
				t.Errorf("时间线 %s/%s 泄漏了 Cookie 明文: %q", st.Action, k, v)
			}
		}
		if strings.Contains(st.ValueUsed, "cookiesecret1234") {
			t.Errorf("时间线 %s 的 ValueUsed 泄漏了 Cookie 明文", st.Action)
		}
	}
	if p.Records != 2 || p.Total != 2 || p.EstimatedPages != 2 {
		t.Errorf("解析结果不对: records=%d total=%d pages=%d", p.Records, p.Total, p.EstimatedPages)
	}
	if len(p.Rows) != 2 {
		t.Errorf("预览行数 = %d, want 2", len(p.Rows))
	}
}

// 指定不存在的接口时不能静默返回空数组。
func TestPreviewUnknownEndpointReportsError(t *testing.T) {
	h := NewHarvester(nil, nil, nil, t.TempDir())
	task := model.HarvestTask{
		ID: "t1",
		Endpoints: []model.HarvestEndpoint{{
			ID: "ep1", Name: "列表", Method: "GET", Enabled: true, URL: "http://127.0.0.1:1/api",
		}},
	}
	task.Normalize()
	h.cookies[sessionTarget(&task)] = cachedCookies{list: []*http.Cookie{{Name: "sid", Value: "x"}}, fetched: time.Now()}
	out := h.Preview(context.Background(), task, 5, "ep-missing", PreviewOptions{})
	if len(out) != 1 || out[0].Error == "" {
		t.Fatalf("接口不存在时应返回明确错误, got %+v", out)
	}
}
