package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"uitest-server/internal/store"
)

// 「粘贴导入公共参数」是纯解析接口（不碰数据库 / 浏览器），这里盯住它真的把
// Cookie / Authorization / 普通请求头拆成公共 Cookie + 参数 + 绑定。
func TestCollectCommonImportParsesHeaders(t *testing.T) {
	h := (&Server{}).Handler()
	body := `{"headers":"Cookie: SESSION=abc123; uid=42\nX-Tenant: demo\nAuthorization: Bearer abcdef1234567890"}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/collect/common/import", strings.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", rr.Code, rr.Body.String())
	}

	var res struct {
		Common struct {
			Cookies  map[string]string `json:"cookies"`
			Params   map[string]string `json:"params"`
			Headers  map[string]string `json:"headers"`
			Bindings []struct {
				Param  string `json:"param"`
				Target string `json:"target"`
				Name   string `json:"name"`
			} `json:"bindings"`
		} `json:"common"`
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("响应不是 JSON：%v（%q）", err, rr.Body.String())
	}
	if got := res.Common.Cookies["SESSION"]; got != "abc123" {
		t.Errorf("Cookie SESSION = %q，期望 abc123", got)
	}
	if got := res.Common.Params["token"]; got != "abcdef1234567890" {
		t.Errorf("Authorization 应拆成参数 token，得到 %q", got)
	}
	if got := res.Common.Headers["X-Tenant"]; got != "demo" {
		t.Errorf("普通请求头没进公共请求头，得到 %q", got)
	}
	found := false
	for _, b := range res.Common.Bindings {
		if b.Param == "token" && strings.EqualFold(b.Target, "header") && strings.EqualFold(b.Name, "Authorization") {
			found = true
		}
	}
	if !found {
		t.Errorf("鉴权头应自动生成本次绑定的绑定 token -> header:Authorization，实际 %+v", res.Common.Bindings)
	}
	if len(res.Notes) == 0 {
		t.Errorf("导入应返回说明（notes），实际为空")
	}
}

// 请求体不是合法 JSON：400 + JSON 错误，而不是 500 或空响应。
func TestCollectCommonImportRejectsBadJSON(t *testing.T) {
	h := (&Server{}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/collect/common/import", strings.NewReader("{oops")))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400：%s", rr.Code, rr.Body.String())
	}
	if msg := decodeError(t, rr); !strings.Contains(msg, "JSON") {
		t.Errorf("错误信息应说明 JSON 不合法，得到 %q", msg)
	}
}

// 任务不存在：导入 / 刷新都要 404，别静默成功。
func TestCollectCommonUnknownTask404(t *testing.T) {
	db, err := store.Open(t.TempDir(), 5)
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := (&Server{DB: db}).Handler()

	for _, path := range []string{
		"/api/collect/tasks/ht_nope/common/import",
		"/api/collect/tasks/ht_nope/common/refresh",
		"/api/collect/tasks/ht_nope/common/suggest",
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if rr.Code != http.StatusNotFound {
			t.Errorf("POST %s 状态码 = %d，期望 404：%s", path, rr.Code, rr.Body.String())
			continue
		}
		if msg := decodeError(t, rr); msg == "" {
			t.Errorf("POST %s 没带错误信息", path)
		}
	}
}
