package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"uitest-server/internal/ai"
	"uitest-server/internal/model"
	"uitest-server/internal/store"
)

/* --------------------------------------------------------- AI 数据准备 */

type aiDatasetReq struct {
	RecordingID string `json:"recording_id"`
	Instruction string `json:"instruction"`
}

// aiDataset POST /api/ai/dataset
// 依据录制的输入字段（label / field_kind / 备注）生成一组合法测试数据。
func (s *Server) aiDataset(w http.ResponseWriter, r *http.Request) {
	var req aiDatasetReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	rec, err := s.DB.GetRecording(req.RecordingID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	fields := ai.DeriveFields(rec)
	res := ai.Generate(r.Context(), s.DB.Settings().AI, fields, req.Instruction)
	out := map[string]any{
		"fields":  res.Fields,
		"dataset": res.Dataset(),
		"notes":   res.Notes,
		"source":  res.Source,
	}
	if res.Warning != "" {
		out["warning"] = res.Warning
	}
	writeJSON(w, http.StatusOK, out)
}

/* ------------------------------------------------------------- 执行 Runs */

type createRunReq struct {
	RecordingID   string            `json:"recording_id"`
	Dataset       map[string]string `json:"dataset"`
	DatasetSource string            `json:"dataset_source"`
	DatasetFields []model.AIField   `json:"dataset_fields"`
	Instruction   string            `json:"instruction"`
	Mode          string            `json:"mode"`
	AI            *struct {
		Enabled     bool   `json:"enabled"`
		Instruction string `json:"instruction"`
	} `json:"ai"`
}

// createRun POST /api/runs
// 流程：取录制 → 没有现成数据时先用 AI/规则生成数据准备 → 建运行 → 异步执行。
func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var req createRunReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	rec, err := s.DB.GetRecording(req.RecordingID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if len(rec.Steps) == 0 {
		writeErr(w, http.StatusBadRequest, "这条录制还没有任何步骤，无法执行")
		return
	}

	dataset := req.Dataset
	source := req.DatasetSource
	fields := req.DatasetFields
	notes := map[string]string{}
	instruction := req.Instruction
	if req.AI != nil && strings.TrimSpace(req.AI.Instruction) != "" {
		instruction = req.AI.Instruction
	}
	wantAI := req.AI == nil || req.AI.Enabled
	if len(dataset) == 0 && wantAI {
		res := ai.Generate(r.Context(), s.DB.Settings().AI, ai.DeriveFields(rec), instruction)
		dataset = res.Dataset()
		source = res.Source
		fields = res.Fields
		notes = res.Notes
	}
	if len(dataset) == 0 {
		dataset = map[string]string{}
		if source == "" {
			source = "manual"
		}
	}
	if source == "" {
		source = "manual"
	}

	run, err := s.DB.CreateRun(model.Run{
		RecordingID:   rec.ID,
		RecordingName: rec.Name,
		Status:        "queued",
		Mode:          "server",
		Dataset:       dataset,
		DatasetSource: source,
		DatasetFields: fields,
		DatasetNotes:  notes,
		StartedAt:     time.Now(),
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	s.Runner.Start(run.ID)
	writeJSON(w, http.StatusOK, run)
}

// listRuns GET /api/runs?recording_id=&limit=
func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	items := s.DB.ListRuns(q.Get("recording_id"), limit)
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// getRun GET /api/runs/{id}
func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.DB.GetRun(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// cancelRun POST /api/runs/{id}/cancel
func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Runner.Cancel(id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	run, _ := s.DB.GetRun(id)
	writeJSON(w, http.StatusOK, run)
}

var uidFromUsedRe = regexp.MustCompile(`^uid=([a-zA-Z0-9_\-]+)`)

// healRun POST /api/runs/{id}/heal
// 把运行中「自愈命中」的新定位回写进录制，避免下次再靠自愈。
func (s *Server) healRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.DB.GetRun(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	updated := 0
	var details []map[string]string
	seen := map[string]bool{}
	for _, sr := range run.StepResults {
		if !sr.Healed || seen[sr.StepID] {
			continue
		}
		seen[sr.StepID] = true
		m := uidFromUsedRe.FindStringSubmatch(sr.LocatorUsed)
		if m == nil {
			continue
		}
		newKey := m[1]
		rec, err := s.DB.GetRecording(run.RecordingID)
		if err != nil {
			break
		}
		var step model.Step
		found := false
		for _, st := range rec.Steps {
			if st.ID == sr.StepID {
				step, found = st, true
				break
			}
		}
		if !found {
			continue
		}
		oldLocator := step.Locator
		if oldLocator.Strategy == "uid" && oldLocator.Value != "" {
			s.DB.MarkElementBroken(oldLocator.Value, true)
		}
		newLocator := model.Locator{Strategy: "uid", Value: newKey}
		newLocator.Fallbacks = append([]model.Locator{oldLocator}, oldLocator.Fallbacks...)
		if _, err := s.DB.UpdateStep(rec.ID, step.ID, store.StepPatch{Locator: &newLocator}); err == nil {
			updated++
			details = append(details, map[string]string{
				"step_id": step.ID,
				"index":   strconv.Itoa(step.Index),
				"from":    oldLocator.Strategy + "=" + oldLocator.Value,
				"to":      "uid=" + newKey,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated, "steps": details})
}

// runEvents GET /api/runs/{id}/events —— SSE 实时日志
func (s *Server) runEvents(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if _, err := s.DB.GetRun(runID); err != nil {
		writeStoreErr(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "当前连接不支持流式推送")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "data: {\"type\":\"ready\"}\n\n")
	flusher.Flush()

	events, cancel := s.Bus.Subscribe(runID)
	defer cancel()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			raw, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
			if ev.Type == "end" {
				return
			}
		}
	}
}

/* --------------------------------------------------------------- 元素库 */

// bindElement POST /api/elements/bind
func (s *Server) bindElement(w http.ResponseWriter, r *http.Request) {
	var e model.Element
	if err := readJSON(r, &e); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(e.Page) == "" && strings.TrimSpace(e.Selector) == "" {
		writeErr(w, http.StatusBadRequest, "page 与 selector 至少提供一个")
		return
	}
	out := s.DB.BindElement(e)
	writeJSON(w, http.StatusOK, out)
}

// listElements GET /api/elements
func (s *Server) listElements(w http.ResponseWriter, r *http.Request) {
	page := r.URL.Query().Get("page")
	items := s.DB.ListElements()
	if page != "" {
		filtered := items[:0]
		for _, e := range items {
			if strings.Contains(e.Page, page) {
				filtered = append(filtered, e)
			}
		}
		items = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// deleteElement DELETE /api/elements/{key}
func (s *Server) deleteElement(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.DeleteElement(r.PathValue("key")); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// batchDeleteRuns POST /api/runs/batch-delete
func (s *Server) batchDeleteRuns(w http.ResponseWriter, r *http.Request) {
	var req idsReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if len(req.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "没有指定要删除的运行记录")
		return
	}
	n := s.DB.DeleteRuns(req.IDs)
	recs, runs := s.DB.Counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": n, "recordings": recs, "runs": runs,
		"message": fmt.Sprintf("已删除 %d 条运行记录", n),
	})
}

// clearRuns DELETE /api/runs?all=1
func (s *Server) clearRuns(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") != "1" {
		writeErr(w, http.StatusBadRequest, "清空需要显式确认：DELETE /api/runs?all=1")
		return
	}
	n := s.DB.DeleteAllRuns()
	recs, runs := s.DB.Counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": n, "recordings": recs, "runs": runs,
		"message": fmt.Sprintf("已清空 %d 条运行记录（录制保留）", n),
	})
}

// batchDeleteElements POST /api/elements/batch-delete
func (s *Server) batchDeleteElements(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keys []string `json:"keys"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if len(req.Keys) == 0 {
		writeErr(w, http.StatusBadRequest, "没有指定要删除的元素")
		return
	}
	n := s.DB.DeleteElements(req.Keys)
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": n, "message": fmt.Sprintf("已删除 %d 个元素绑定", n),
	})
}

// clearElements DELETE /api/elements?all=1
func (s *Server) clearElements(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") != "1" {
		writeErr(w, http.StatusBadRequest, "清空需要显式确认：DELETE /api/elements?all=1")
		return
	}
	n := s.DB.DeleteAllElements()
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": n, "message": fmt.Sprintf("已清空 %d 个元素绑定", n),
	})
}
