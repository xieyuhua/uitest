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
