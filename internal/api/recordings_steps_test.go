package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"uitest-server/internal/model"
	"uitest-server/internal/store"
)

func newRecordingTestServer(t *testing.T) http.Handler {
	t.Helper()
	db, err := store.Open(t.TempDir(), 5)
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return (&Server{DB: db}).Handler()
}

func createTestRecording(t *testing.T, h http.Handler) model.Recording {
	t.Helper()
	rr := httptest.NewRecorder()
	body := `{"name":"登录流程","start_url":"https://example.com/login"}`
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/recordings", strings.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("创建录制状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	var rec model.Recording
	if err := json.Unmarshal(rr.Body.Bytes(), &rec); err != nil {
		t.Fatalf("创建录制响应不是 JSON：%v（%q）", err, rr.Body.String())
	}
	return rec
}

func getTestRecording(t *testing.T, h http.Handler, id string) model.Recording {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/recordings/"+id, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("读取录制状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	var rec model.Recording
	if err := json.Unmarshal(rr.Body.Bytes(), &rec); err != nil {
		t.Fatalf("读取录制响应不是 JSON：%v（%q）", err, rr.Body.String())
	}
	return rec
}

// 采集任务里「自己创建 UI 流程」：一次提交整段步骤，index 按顺序重排、动作统一小写。
func TestSaveStepsBatchReplacesWholeFlow(t *testing.T) {
	h := newRecordingTestServer(t)
	rec := createTestRecording(t, h)

	payload := `{"mode":"replace","steps":[
		{"action":"Navigate","value":"https://example.com/login"},
		{"action":"input","locator":{"strategy":"css","value":"#username"},"value":"admin"},
		{"action":"input","locator":{"strategy":"css","value":"#password"},"value":"{{password}}"},
		{"action":"captcha","locator":{"strategy":"css","value":"img.captcha"},"note":"4 位数字"},
		{"action":"click","locator":{"strategy":"text","value":"登录"}}
	]}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/recordings/"+rec.ID+"/steps/batch", strings.NewReader(payload)))
	if rr.Code != http.StatusOK {
		t.Fatalf("保存步骤状态码 = %d：%s", rr.Code, rr.Body.String())
	}

	got := getTestRecording(t, h, rec.ID)
	if len(got.Steps) != 5 {
		t.Fatalf("步骤数 = %d，期望 5：%s", len(got.Steps), rr.Body.String())
	}
	for i, s := range got.Steps {
		if s.Index != i+1 {
			t.Errorf("第 %d 步 index = %d，期望 %d", i+1, s.Index, i+1)
		}
		if s.ID == "" || s.RecordingID != rec.ID {
			t.Errorf("第 %d 步 ID/RecordingID 未补全：%+v", i+1, s)
		}
		if s.Annotation == "" {
			t.Errorf("第 %d 步缺少默认备注", i+1)
		}
	}
	if !strings.Contains(got.Steps[1].Annotation, "#username") {
		t.Errorf("只填定位的步骤，备注应带上定位：%q", got.Steps[1].Annotation)
	}
	if got.Steps[0].Action != "navigate" {
		t.Errorf("动作未规范化：%q", got.Steps[0].Action)
	}
	if got.StepCount != 5 {
		t.Errorf("step_count = %d，期望 5", got.StepCount)
	}

	// 第二次保存只留 2 步：旧步骤必须被整体替换掉
	payload2 := `{"steps":[{"action":"navigate","value":"https://example.com/a"},{"action":"click","locator":{"strategy":"css","value":"#go"}}]}`
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/recordings/"+rec.ID+"/steps/batch", strings.NewReader(payload2)))
	if rr.Code != http.StatusOK {
		t.Fatalf("二次保存状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	got = getTestRecording(t, h, rec.ID)
	if len(got.Steps) != 2 {
		t.Fatalf("替换后步骤数 = %d，期望 2", len(got.Steps))
	}
	if got.Steps[1].Locator.Value != "#go" {
		t.Errorf("替换后的步骤定位不对：%+v", got.Steps[1])
	}
}

// append 模式追加在末尾，不动已有步骤。
func TestSaveStepsBatchAppends(t *testing.T) {
	h := newRecordingTestServer(t)
	rec := createTestRecording(t, h)

	for _, body := range []string{
		`{"steps":[{"action":"navigate","value":"https://example.com/a"}]}`,
		`{"mode":"append","steps":[{"action":"click","locator":{"strategy":"text","value":"确定"}}]}`,
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/recordings/"+rec.ID+"/steps/batch", strings.NewReader(body)))
		if rr.Code != http.StatusOK {
			t.Fatalf("保存步骤状态码 = %d：%s", rr.Code, rr.Body.String())
		}
	}
	got := getTestRecording(t, h, rec.ID)
	if len(got.Steps) != 2 || got.Steps[1].Index != 2 {
		t.Fatalf("追加结果不对：%+v", got.Steps)
	}
}

// 不支持的动作 / 缺 URL / 缺定位都要 400，并指出第几步。
func TestSaveStepsBatchValidation(t *testing.T) {
	h := newRecordingTestServer(t)
	rec := createTestRecording(t, h)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"unsupported-action", `{"steps":[{"action":"drag"}]}`, "不支持"},
		{"navigate-without-url", `{"steps":[{"action":"navigate"}]}`, "URL"},
		{"click-without-locator", `{"steps":[{"action":"click"}]}`, "定位"},
		{"bad-mode", `{"mode":"merge","steps":[{"action":"navigate","value":"https://a"}]}`, "mode"},
		{"empty-replace", `{"steps":[]}`, "至少要"},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/recordings/"+rec.ID+"/steps/batch", strings.NewReader(c.body)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s 状态码 = %d，期望 400：%s", c.name, rr.Code, rr.Body.String())
			continue
		}
		if msg := decodeError(t, rr); !strings.Contains(msg, c.want) {
			t.Errorf("%s 错误信息 = %q，应包含 %q", c.name, msg, c.want)
		}
	}
}
