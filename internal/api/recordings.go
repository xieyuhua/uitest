package api

import (
	"fmt"
	"net/http"
	"strings"

	"uitest-server/internal/model"
	"uitest-server/internal/store"
)

// listRecordings GET /api/recordings?q=&tag=
func (s *Server) listRecordings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	tag := r.URL.Query().Get("tag")
	items := s.DB.ListRecordings(q, tag)
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type createRecordingReq struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	StartURL    string   `json:"start_url"`
	Tags        []string `json:"tags"`
}

// createRecording POST /api/recordings
func (s *Server) createRecording(w http.ResponseWriter, r *http.Request) {
	var req createRecordingReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	rec, err := s.DB.CreateRecording(req.Name, req.Description, req.StartURL, req.Tags)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// getRecording GET /api/recordings/{id}
func (s *Server) getRecording(w http.ResponseWriter, r *http.Request) {
	rec, err := s.DB.GetRecording(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// patchRecording PATCH /api/recordings/{id}
func (s *Server) patchRecording(w http.ResponseWriter, r *http.Request) {
	var p store.RecordingPatch
	if err := readJSON(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	rec, err := s.DB.UpdateRecording(r.PathValue("id"), p)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// deleteRecording DELETE /api/recordings/{id}
func (s *Server) deleteRecording(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.DeleteRecording(r.PathValue("id")); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// addStep POST /api/recordings/{id}/steps
func (s *Server) addStep(w http.ResponseWriter, r *http.Request) {
	var step model.Step
	if err := readJSON(r, &step); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(step.Action) == "" {
		writeErr(w, http.StatusBadRequest, "缺少 action 字段")
		return
	}
	out, err := s.DB.AddStep(r.PathValue("id"), step)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// patchStep PATCH /api/recordings/{id}/steps/{sid}
func (s *Server) patchStep(w http.ResponseWriter, r *http.Request) {
	var p store.StepPatch
	if err := readJSON(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	out, err := s.DB.UpdateStep(r.PathValue("id"), r.PathValue("sid"), p)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteStep DELETE /api/recordings/{id}/steps/{sid}
func (s *Server) deleteStep(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.DeleteStep(r.PathValue("id"), r.PathValue("sid")); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// reorderSteps POST /api/recordings/{id}/reorder
func (s *Server) reorderSteps(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StepIDs []string `json:"step_ids"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	rec, err := s.DB.Reorder(r.PathValue("id"), req.StepIDs)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

/* --------------------------------------------------- 整段写入步骤（流程编辑器） */

// 流程编辑器里能选的动作（与 internal/engine 的执行分支一一对应）。
var stepActionNames = map[string]string{
	"navigate": "打开页面", "input": "输入", "click": "点击", "select": "选择",
	"check": "勾选", "uncheck": "取消勾选", "key": "按键", "hover": "悬停",
	"wait": "等待", "upload": "上传", "captcha": "AI 验证码", "assert": "断言", "script": "脚本",
}

// 必须给出元素定位的动作（AI 验证码可以留空，执行时按图片特征自动找）。
var stepNeedsLocator = map[string]bool{
	"input": true, "click": true, "select": true, "check": true,
	"uncheck": true, "hover": true, "upload": true,
}

func stepActionList() string {
	order := []string{"navigate", "input", "click", "select", "check", "uncheck", "key",
		"hover", "wait", "upload", "captcha", "assert", "script"}
	out := make([]string, 0, len(order))
	for _, a := range order {
		out = append(out, a+"（"+stepActionNames[a]+"）")
	}
	return strings.Join(out, "、")
}

// targetHasHint 判断步骤是否带了可用的元素语义（label / 文本 / placeholder / name 等）。
func targetHasHint(t model.Target) bool {
	return strings.TrimSpace(t.Label+t.Text+t.Placeholder+t.Aria+t.Name+t.StableID+t.TestAttr+
		t.FormItem+t.Column+t.RowText) != ""
}

type saveStepsReq struct {
	// replace（默认）：整段替换；append：追加到现有步骤之后
	Mode  string       `json:"mode"`
	Steps []model.Step `json:"steps"`
}

// saveSteps POST /api/recordings/{id}/steps/batch
//
// 采集任务里的「自己创建 UI 流程」一次保存整段步骤：前端不用逐步调接口，
// 也不会出现保存到一半只写进去半步的情况（store 里是整体替换）。
func (s *Server) saveSteps(w http.ResponseWriter, r *http.Request) {
	var req saveStepsReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "replace"
	}
	if mode != "replace" && mode != "append" {
		writeErr(w, http.StatusBadRequest, "mode 只能是 replace 或 append")
		return
	}
	if mode == "replace" && len(req.Steps) == 0 {
		writeErr(w, http.StatusBadRequest, "流程至少要有一步；要清空请到 UI 自动化里删除录制")
		return
	}
	if len(req.Steps) > 200 {
		writeErr(w, http.StatusBadRequest, "一次最多保存 200 步")
		return
	}
	for i := range req.Steps {
		st := &req.Steps[i]
		n := i + 1
		st.Action = strings.ToLower(strings.TrimSpace(st.Action))
		if _, ok := stepActionNames[st.Action]; !ok {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("第 %d 步的动作「%s」不支持，可选：%s", n, st.Action, stepActionList()))
			return
		}
		if st.Action == "navigate" && strings.TrimSpace(st.Value) == "" {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("第 %d 步是「打开页面」，必须在输入值里填 URL", n))
			return
		}
		if st.Action == "wait" && strings.TrimSpace(st.Value) == "" {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("第 %d 步是「等待」，请填毫秒数或要等待出现的元素定位", n))
			return
		}
		if stepNeedsLocator[st.Action] && strings.TrimSpace(st.Locator.Value) == "" && !targetHasHint(st.Target) {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("第 %d 步是「%s」，缺少元素定位（如 css=#username 或 text=登录）", n, stepActionNames[st.Action]))
			return
		}
	}
	rec, err := s.DB.SaveSteps(r.PathValue("id"), req.Steps, mode == "append")
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// duplicateRecording POST /api/recordings/{id}/duplicate
func (s *Server) duplicateRecording(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	_ = readJSON(r, &req)
	rec, err := s.DB.DuplicateRecording(r.PathValue("id"), req.Name)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// exportRecording GET /api/recordings/{id}/export
func (s *Server) exportRecording(w http.ResponseWriter, r *http.Request) {
	rec, err := s.DB.GetRecording(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+rec.ID+`.json"`)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	writeJSONBody(w, rec)
}

// importRecording POST /api/recordings/import
func (s *Server) importRecording(w http.ResponseWriter, r *http.Request) {
	var in model.Recording
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "导入内容不是合法 JSON: "+err.Error())
		return
	}
	rec, err := s.DB.ImportRecording(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

/* ------------------------------------------------------- 批量删除 / 清空 */

type idsReq struct {
	IDs []string `json:"ids"`
}

// batchDeleteRecordings POST /api/recordings/batch-delete
func (s *Server) batchDeleteRecordings(w http.ResponseWriter, r *http.Request) {
	var req idsReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if len(req.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "没有指定要删除的录制")
		return
	}
	n := s.DB.DeleteRecordings(req.IDs)
	recs, runs := s.DB.Counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": n, "recordings": recs, "runs": runs,
		"message": fmt.Sprintf("已删除 %d 条录制（连带其运行记录）", n),
	})
}

// clearRecordings DELETE /api/recordings?all=1
//
// 必须显式带 all=1：避免脚本或误操作一键清空。
func (s *Server) clearRecordings(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") != "1" {
		writeErr(w, http.StatusBadRequest, "清空需要显式确认：DELETE /api/recordings?all=1")
		return
	}
	n := s.DB.DeleteAllRecordings()
	recs, runs := s.DB.Counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": n, "recordings": recs, "runs": runs,
		"message": fmt.Sprintf("已清空 %d 条录制（连带全部运行记录）", n),
	})
}
