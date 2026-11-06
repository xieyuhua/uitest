package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeError(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q，期望 application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON：%v（%q）", err, rr.Body.String())
	}
	msg := body["error"]
	if msg == "" {
		t.Fatalf("缺少 error 字段：%q", rr.Body.String())
	}
	return msg
}

// 未注册的 /api 路径必须返回 JSON 404（docs/API.md 约定），而不是 net/http 的纯文本。
func TestUnknownAPIPathReturnsJSON404(t *testing.T) {
	h := (&Server{}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rr.Code)
	}
	if msg := decodeError(t, rr); !strings.Contains(msg, "/api/does-not-exist") {
		t.Errorf("错误信息未带上路径：%q", msg)
	}
}

// 路径存在、只是方法不对：仍然是 405，并带上 Allow 头。
func TestAPIMethodNotAllowedIsJSON(t *testing.T) {
	h := (&Server{}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/health", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rr.Code)
	}
	if allow := rr.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Errorf("Allow = %q，期望包含 GET", allow)
	}
	if msg := decodeError(t, rr); !strings.Contains(msg, "POST") {
		t.Errorf("错误信息未说明方法：%q", msg)
	}
}

// 兜底不能影响通配路由：{id} 必须照常写进 r.PathValue。
func TestWithAPIFallbackKeepsPathValues(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/recordings/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id")})
	})
	rr := httptest.NewRecorder()
	withAPIFallback(mux).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/recordings/rec_1", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"id":"rec_1"`) {
		t.Fatalf("通配路由被破坏：%d %q", rr.Code, rr.Body.String())
	}
}

// 已注册的接口不受兜底影响；非 /api/ 路径也保持原样。
func TestWithAPIFallbackPassesThrough(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	h := withAPIFallback(mux)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/ping", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Fatalf("已注册接口被拦截：%d %q", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/other", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("非 /api/ 路径状态码 = %d，期望 404（交还给 mux 默认处理）", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		t.Errorf("非 /api/ 路径不应被改写成 JSON：%q", ct)
	}
}
