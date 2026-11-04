package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"uitest-server/internal/collect"
	"uitest-server/internal/model"
)

// 本文件是「接口采集」的 HTTP 接口：录制 → 筛选 → 建任务 → 定时 → 落库 → 看运行。
// 字段与 docs/API.md 的 collect 部分严格一致。

/* ------------------------------------------------------------------ 录制 */

type startCaptureReq struct {
	StartURL      string   `json:"start_url"`
	Types         []string `json:"types"`
	CaptureBodies *bool    `json:"capture_bodies"`
	Note          string   `json:"note"`
	// Source browser（默认，服务端内置浏览器 CDP 旁路录制）| extension（浏览器插件上报）
	Source    string `json:"source"`
	UserAgent string `json:"user_agent"`
}

// startCapture POST /api/collect/captures
//
// 两种模式：
//   - 默认（source=browser）：打开内置浏览器标签页，服务端旁路录制；
//   - source=extension：只登记一个会话，由浏览器插件把请求 POST 上来
//     （插件在你自己的浏览器里录，登录态/插件/代理都不受影响）。
func (s *Server) startCapture(w http.ResponseWriter, r *http.Request) {
	var req startCaptureReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	url := strings.TrimSpace(req.StartURL)
	if url == "" && strings.ToLower(req.Source) != "extension" {
		writeErr(w, http.StatusBadRequest, "请填写要录制的页面地址（start_url）")
		return
	}
	if strings.EqualFold(req.Source, "extension") {
		info, err := s.Collect.Recorder.StartExternal(url, req.Note, strings.TrimSpace(req.UserAgent))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"capture": info,
			"hint":    "插件录制会话已就绪：在插件弹窗里开始录制，录到的请求会实时上报到这个会话",
		})
		return
	}

	bodies := true
	if req.CaptureBodies != nil {
		bodies = *req.CaptureBodies
	}
	types := req.Types
	if len(types) == 0 {
		types = []string{"XHR", "Fetch"}
	}
	info, err := s.Collect.Recorder.Start(r.Context(), url, types, bodies, req.Note)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"capture": info,
		"hint":    "内置浏览器已打开，请在这个标签页里正常操作（登录 / 点列表 / 翻页）；操作完点「停止录制」",
	})
}

type ingestRequestsReq struct {
	Items []model.CapturedRequest `json:"items"`
}

// addCaptureRequests POST /api/collect/captures/{id}/requests
// 浏览器插件把录到的请求批量上报到这里（同 id 幂等，重复上报不会重复计数）。
func (s *Server) addCaptureRequests(w http.ResponseWriter, r *http.Request) {
	var req ingestRequestsReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if len(req.Items) == 0 {
		writeErr(w, http.StatusBadRequest, "items 为空")
		return
	}
	id := r.PathValue("id")
	added, err := s.Collect.Recorder.AddRequests(id, req.Items)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	total := 0
	if entries, ok, e2 := s.Collect.Recorder.Entries(id); e2 == nil && ok {
		total = len(entries)
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "total": total})
}

type deleteRequestsReq struct {
	IDs []string `json:"ids"`
}

// deleteCaptureRequests DELETE /api/collect/captures/{id}/requests
// 在「录制结果」里删掉不想要的接口（单条或批量）。
// body: {"ids":["..."]}；也支持 ?ids=a,b,c 与 ?all=1（清空但保留会话）。
func (s *Server) deleteCaptureRequests(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req deleteRequestsReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	ids := req.IDs
	if q := r.URL.Query().Get("ids"); q != "" {
		for _, part := range strings.Split(q, ",") {
			if p := strings.TrimSpace(part); p != "" {
				ids = append(ids, p)
			}
		}
	}
	if r.URL.Query().Get("all") == "1" {
		entries, ok, err := s.Collect.Recorder.Entries(id)
		if err != nil || !ok {
			writeErr(w, http.StatusNotFound, errMsg(err, "录制会话不存在"))
			return
		}
		ids = ids[:0]
		for _, it := range entries {
			ids = append(ids, it.ID)
		}
	}
	n, err := s.Collect.Recorder.DeleteRequests(id, ids)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	total := 0
	if entries, ok, e2 := s.Collect.Recorder.Entries(id); e2 == nil && ok {
		total = len(entries)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": n, "total": total,
		"message": fmt.Sprintf("已删除 %d 条，剩余 %d 条", n, total),
	})
}

// listCaptures GET /api/collect/captures
func (s *Server) listCaptures(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": s.Collect.Recorder.List()})
}

// getCapture GET /api/collect/captures/{id}?q=&method=&type=&kind=&with_body=&json=&include=&exclude=&host=
// 返回筛选后的请求列表 + 统计维度。
func (s *Server) getCapture(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entries, ok, err := s.Collect.Recorder.Entries(id)
	if err != nil || !ok {
		writeErr(w, http.StatusNotFound, errMsg(err, "录制会话不存在"))
		return
	}
	filter, ferr := collect.ParseFilter(r.URL.Query())
	if ferr != nil {
		writeErr(w, http.StatusBadRequest, ferr.Error())
		return
	}
	kept := filter.Apply(entries)
	sess, _ := s.Collect.Recorder.Get(id)
	meta := map[string]any{}
	if sess != nil {
		meta = map[string]any{
			"id":         sess.ID,
			"source":     sess.Source,
			"page_url":   sess.PageURL,
			"started_at": sess.StartedAt,
			"stopped":    sess.Stopped,
			"note":       sess.Note,
			"user_agent": sess.UserAgent,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"capture": meta,
		"items":   kept,
		"facets":  collect.BuildFacets(entries, kept),
	})
}

// stopCapture POST /api/collect/captures/{id}/stop
func (s *Server) stopCapture(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entries, err := s.Collect.Recorder.Stop(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(entries),
		"items": entries,
	})
}

// deleteCapture DELETE /api/collect/captures/{id}
func (s *Server) deleteCapture(w http.ResponseWriter, r *http.Request) {
	if err := s.Collect.Recorder.Delete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type analyzeCaptureReq struct {
	RequestID string `json:"request_id"`
	// Body / RecordsPath 可选：不传则用录制时保存的样本响应体
	Body        string `json:"body"`
	RecordsPath string `json:"records_path"`
}

// analyzeCapture POST /api/collect/captures/{id}/analyze
// 分析一条录制请求的响应：自动找出记录数组位置、字段映射，并给出前几行预览。
func (s *Server) analyzeCapture(w http.ResponseWriter, r *http.Request) {
	var req analyzeCaptureReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	entries, ok, err := s.Collect.Recorder.Entries(r.PathValue("id"))
	if err != nil || !ok {
		writeErr(w, http.StatusNotFound, errMsg(err, "录制会话不存在"))
		return
	}
	body := req.Body
	if body == "" {
		for _, e := range entries {
			if e.ID == req.RequestID {
				body = e.BodyPreview
				if body == "" {
					writeErr(w, http.StatusBadRequest, "这条请求没有抓到响应体（录制时勾选了「抓响应体」吗？）")
					return
				}
				break
			}
		}
	}
	if body == "" {
		writeErr(w, http.StatusBadRequest, "没有可分析的响应体，请指定 request_id 或直接传 body")
		return
	}
	res := collect.AnalyzeBody(body, req.RecordsPath, 5)
	// 顺带把分页方式也推断出来（用录制到的 URL/请求体 + 这份响应）
	for _, e := range entries {
		if e.ID == req.RequestID {
			pag := collect.DetectForCaptured(e)
			res.Pagination = pag
			res.PaginationText = pag.Describe()
			break
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// analyzeBody POST /api/collect/analyze —— 直接分析一段 JSON（手工贴样本也能用）
func (s *Server) analyzeBody(w http.ResponseWriter, r *http.Request) {
	var req analyzeCaptureReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		writeErr(w, http.StatusBadRequest, "请提供 body（响应体 JSON 文本）")
		return
	}
	writeJSON(w, http.StatusOK, collect.AnalyzeBody(req.Body, req.RecordsPath, 5))
}

/* ------------------------------------------------------------------ 任务 */

type harvestTaskReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	StartURL    string `json:"start_url"`

	// 两种建任务方式：直接给 endpoints，或给"录制会话 + 勾选的请求 id"由服务端生成
	CaptureID  string                  `json:"capture_id"`
	RequestIDs []string                `json:"request_ids"`
	Endpoints  []model.HarvestEndpoint `json:"endpoints"`

	Schedule *model.HarvestSchedule `json:"schedule"`
	Throttle *model.HarvestThrottle `json:"throttle"`
	Sink     *model.HarvestSink     `json:"sink"`
	Session  *model.HarvestSession  `json:"session"`
	Status   string                 `json:"status"`
}

// createHarvestTask POST /api/collect/tasks
func (s *Server) createHarvestTask(w http.ResponseWriter, r *http.Request) {
	var req harvestTaskReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	task := model.HarvestTask{
		Name:        strings.TrimSpace(req.Name),
		Description: req.Description,
		StartURL:    req.StartURL,
		Schedule:    model.HarvestSchedule{Mode: "manual"},
		Throttle:    model.DefaultThrottle(),
		Sink:        model.DefaultSink(),
		Session:     model.HarvestSession{Mode: "none"},
		Status:      "active",
	}
	task.Endpoints = req.Endpoints
	if len(task.Endpoints) == 0 {
		eps, ua, err := s.endpointsFromCapture(req.CaptureID, req.RequestIDs)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		task.Endpoints = eps
		if task.Session.Mode == "none" && ua != "" {
			task.Session = model.HarvestSession{Mode: "browser", UserAgent: ua, Host: originOfFirst(eps)}
		}
	}
	if len(task.Endpoints) == 0 {
		writeErr(w, http.StatusBadRequest, "至少要选一个接口（endpoints 或 capture_id + request_ids）")
		return
	}
	if req.Schedule != nil {
		task.Schedule = *req.Schedule
	}
	if req.Throttle != nil {
		task.Throttle = *req.Throttle
	}
	if req.Sink != nil {
		task.Sink = *req.Sink
	}
	if req.Session != nil {
		task.Session = *req.Session
	}
	if req.Status != "" {
		task.Status = req.Status
	}

	out, err := s.DB.CreateHarvestTask(task)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	s.Collect.Scheduler.Recompute(out.ID)
	if t, err2 := s.DB.GetHarvestTask(out.ID); err2 == nil {
		out = t
	}
	writeJSON(w, http.StatusOK, harvestTaskView(out, s.Collect.Harvester.Running(out.ID), true))
}

// endpointsFromCapture 用录制结果生成接口定义。
func (s *Server) endpointsFromCapture(captureID string, requestIDs []string) ([]model.HarvestEndpoint, string, error) {
	if strings.TrimSpace(captureID) == "" {
		return nil, "", fmt.Errorf("没有提供接口定义，也没有提供录制会话（capture_id）")
	}
	entries, ok, err := s.Collect.Recorder.Entries(captureID)
	if err != nil || !ok {
		return nil, "", fmt.Errorf("录制会话不存在（服务重启后录制结果会清空，请重新录制）")
	}
	pick := map[string]bool{}
	for _, id := range requestIDs {
		pick[id] = true
	}
	var out []model.HarvestEndpoint
	for _, e := range entries {
		if len(pick) > 0 && !pick[e.ID] {
			continue
		}
		if len(pick) == 0 {
			continue
		}
		ep := collect.EndpointFromCaptured(e)
		ep.SampleBody = e.BodyPreview
		out = append(out, ep)
	}
	if len(out) == 0 {
		return nil, "", fmt.Errorf("勾选的请求 id 在录制结果里找不到")
	}
	ua := ""
	if sess, ok := s.Collect.Recorder.Get(captureID); ok {
		ua = sess.UserAgent
	}
	return out, ua, nil
}

// listHarvestTasks GET /api/collect/tasks?q=
func (s *Server) listHarvestTasks(w http.ResponseWriter, r *http.Request) {
	items := s.DB.ListHarvestTasks(r.URL.Query().Get("q"))
	out := make([]map[string]any, 0, len(items))
	for _, t := range items {
		out = append(out, harvestTaskView(t, s.Collect.Harvester.Running(t.ID), false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// harvestTaskView 任务列表/详情统一视图（补上人可读的计划描述与运行态）。
func harvestTaskView(t model.HarvestTask, running bool, withSamples bool) map[string]any {
	eps := t.Endpoints
	if !withSamples {
		eps = make([]model.HarvestEndpoint, len(t.Endpoints))
		for i, e := range t.Endpoints {
			e.SampleBody = ""
			eps[i] = e
		}
	}
	return map[string]any{
		"id":             t.ID,
		"name":           t.Name,
		"description":    t.Description,
		"start_url":      t.StartURL,
		"status":         t.Status,
		"endpoints":      eps,
		"endpoint_count": t.EndpointCount(),
		"schedule":       t.Schedule,
		"schedule_text":  collect.DescribeSchedule(t.Schedule),
		"throttle":       t.Throttle,
		"sink":           t.Sink,
		"sink_file":      sinkFileName(t.Sink),
		"session":        t.Session,
		"created_at":     t.CreatedAt,
		"updated_at":     t.UpdatedAt,
		"last_run_at":    t.LastRunAt,
		"next_run_at":    t.NextRunAt,
		"last_status":    t.LastStatus,
		"last_error":     t.LastError,
		"last_written":   t.LastWritten,
		"run_count":      t.RunCount,
		"fail_streak":    t.FailStreak,
		"running":        running,
	}
}

// getHarvestTask GET /api/collect/tasks/{id}
func (s *Server) getHarvestTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.DB.GetHarvestTask(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, harvestTaskView(t, s.Collect.Harvester.Running(t.ID), true))
}

type patchHarvestTaskReq struct {
	Name        *string                  `json:"name"`
	Description *string                  `json:"description"`
	StartURL    *string                  `json:"start_url"`
	Status      *string                  `json:"status"`
	Endpoints   *[]model.HarvestEndpoint `json:"endpoints"`
	Schedule    *model.HarvestSchedule   `json:"schedule"`
	Throttle    *model.HarvestThrottle   `json:"throttle"`
	Sink        *model.HarvestSink       `json:"sink"`
	Session     *model.HarvestSession    `json:"session"`
}

// patchHarvestTask PATCH /api/collect/tasks/{id}
func (s *Server) patchHarvestTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req patchHarvestTaskReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	t, err := s.DB.GetHarvestTask(id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		t.Name = strings.TrimSpace(*req.Name)
	}
	if req.Description != nil {
		t.Description = *req.Description
	}
	if req.StartURL != nil {
		t.StartURL = strings.TrimSpace(*req.StartURL)
	}
	if req.Status != nil {
		switch strings.TrimSpace(*req.Status) {
		case "active", "paused":
			t.Status = strings.TrimSpace(*req.Status)
			if t.Status == "active" {
				t.FailStreak = 0
			}
		}
	}
	if req.Endpoints != nil {
		eps := *req.Endpoints
		for i := range eps {
			if strings.TrimSpace(eps[i].ID) == "" {
				eps[i].ID = fmt.Sprintf("ep_%d_%d", time.Now().UnixNano()%1e6, i)
			}
		}
		if len(eps) == 0 {
			writeErr(w, http.StatusBadRequest, "endpoints 不能为空（至少保留一个接口）")
			return
		}
		t.Endpoints = eps
	}
	if req.Schedule != nil {
		t.Schedule = *req.Schedule
	}
	if req.Throttle != nil {
		t.Throttle = *req.Throttle
	}
	if req.Sink != nil {
		t.Sink = *req.Sink
	}
	if req.Session != nil {
		t.Session = *req.Session
	}
	saved, err := s.DB.SaveHarvestTask(t)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	s.Collect.Scheduler.Recompute(saved.ID)
	if t2, err2 := s.DB.GetHarvestTask(saved.ID); err2 == nil {
		saved = t2
	}
	writeJSON(w, http.StatusOK, harvestTaskView(saved, s.Collect.Harvester.Running(saved.ID), true))
}

// deleteHarvestTask DELETE /api/collect/tasks/{id}
func (s *Server) deleteHarvestTask(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.DeleteHarvestTask(r.PathValue("id")); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// runHarvestTask POST /api/collect/tasks/{id}/run —— 立刻跑一次
func (s *Server) runHarvestTask(w http.ResponseWriter, r *http.Request) {
	run, err := s.Collect.Harvester.Start(r.PathValue("id"), "manual")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

type previewReq struct {
	Limit int `json:"limit"`
}

// previewHarvestTask POST /api/collect/tasks/{id}/preview —— 试抓一次，不写库
func (s *Server) previewHarvestTask(w http.ResponseWriter, r *http.Request) {
	var req previewReq
	_ = readJSON(r, &req)
	t, err := s.DB.GetHarvestTask(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	ctx, cancel := timeoutCtx(r, 180*time.Second)
	defer cancel()
	results := s.Collect.Harvester.Preview(ctx, t, req.Limit)
	writeJSON(w, http.StatusOK, map[string]any{"items": results})
}

type analyzeEndpointReq struct {
	EndpointID  string `json:"endpoint_id"`
	RecordsPath string `json:"records_path"`
}

// analyzeHarvestEndpoint POST /api/collect/tasks/{id}/analyze
// 用任务里保存的样本重新识别字段映射与分页方式。
func (s *Server) analyzeHarvestEndpoint(w http.ResponseWriter, r *http.Request) {
	var req analyzeEndpointReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	t, err := s.DB.GetHarvestTask(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	for _, ep := range t.Endpoints {
		if ep.ID != req.EndpointID {
			continue
		}
		if strings.TrimSpace(ep.SampleBody) == "" {
			writeErr(w, http.StatusBadRequest, "这个接口没有样本响应体，请先点「试抓预览」拿一份数据")
			return
		}
		path := req.RecordsPath
		if path == "" {
			path = ep.RecordsPath
		}
		res := collect.AnalyzeBody(ep.SampleBody, path, 5)
		if root, perr := collect.ParseJSONBody(ep.SampleBody); perr == nil {
			records, _, _ := collect.RecordsFrom(root, path)
			pag := collect.DetectPagination(ep.URL, ep.Body, root, records)
			res.Pagination = pag
			res.PaginationText = pag.Describe()
			if collect.LooksPaginatedButUnknown(pag, root, records) {
				res.PaginationText = "响应看起来还有更多数据，但没识别出翻页参数，请手工填写分页参数名"
			}
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	writeErr(w, http.StatusNotFound, "接口不存在")
}

// detectPagination POST /api/collect/tasks/{id}/detect-pagination
// 只识别分页（不动字段映射），body {endpoint_id}。
func (s *Server) detectPagination(w http.ResponseWriter, r *http.Request) {
	var req analyzeEndpointReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	t, err := s.DB.GetHarvestTask(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	for _, ep := range t.Endpoints {
		if ep.ID != req.EndpointID {
			continue
		}
		var pag model.HarvestPagination
		hint := ""
		if strings.TrimSpace(ep.SampleBody) != "" {
			if root, perr := collect.ParseJSONBody(ep.SampleBody); perr == nil {
				records, _, _ := collect.RecordsFrom(root, ep.RecordsPath)
				pag = collect.DetectPagination(ep.URL, ep.Body, root, records)
				if collect.LooksPaginatedButUnknown(pag, root, records) {
					hint = "响应看起来还有更多数据，但没识别出翻页参数，请手工填写分页参数名"
				}
			}
		}
		if pag.Mode == "" {
			pag = collect.DetectPagination(ep.URL, ep.Body, nil, nil)
			if pag.Mode == "none" {
				hint = "这条 URL 里没看到常见的分页参数；如果接口确实分页，请手工填参数名（如 page / offset / cursor）"
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"pagination":      pag,
			"pagination_text": pag.Describe(),
			"hint":            hint,
		})
		return
	}
	writeErr(w, http.StatusNotFound, "接口不存在")
}

// testHarvestSink POST /api/collect/tasks/{id}/test-sink —— 试落库（只连一下，不写入数据）
func (s *Server) testHarvestSink(w http.ResponseWriter, r *http.Request) {
	t, err := s.DB.GetHarvestTask(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if t.Sink.Driver == "" {
		writeErr(w, http.StatusBadRequest, "还没配置落库目标")
		return
	}
	if err := s.Collect.ProbeSink(t.Sink); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "落库目标可用"})
}

/* ------------------------------------------------------------------ 运行 */

// listHarvestRuns GET /api/collect/runs?task_id=&limit=
func (s *Server) listHarvestRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	items := s.DB.ListHarvestRuns(q.Get("task_id"), limit)
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// getHarvestRun GET /api/collect/runs/{id}
func (s *Server) getHarvestRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.DB.GetHarvestRun(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// cancelHarvestRun POST /api/collect/runs/{id}/cancel
func (s *Server) cancelHarvestRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Collect.Harvester.Cancel(id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	run, _ := s.DB.GetHarvestRun(id)
	writeJSON(w, http.StatusOK, run)
}

// batchDeleteHarvestRuns POST /api/collect/runs/batch-delete
func (s *Server) batchDeleteHarvestRuns(w http.ResponseWriter, r *http.Request) {
	var req idsReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if len(req.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "没有指定要删除的运行记录")
		return
	}
	n := s.DB.DeleteHarvestRuns(req.IDs)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n, "message": fmt.Sprintf("已删除 %d 条采集记录", n)})
}

// clearHarvestRuns DELETE /api/collect/runs?all=1
func (s *Server) clearHarvestRuns(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") != "1" {
		writeErr(w, http.StatusBadRequest, "清空需要显式确认：DELETE /api/collect/runs?all=1")
		return
	}
	n := s.DB.DeleteAllHarvestRuns()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n, "message": fmt.Sprintf("已清空 %d 条采集记录", n)})
}

// harvestRunEvents GET /api/collect/runs/{id}/events —— SSE 实时日志
func (s *Server) harvestRunEvents(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if _, err := s.DB.GetHarvestRun(runID); err != nil {
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

/* -------------------------------------------------------------- 产出文件 */

// sinkFileName 落库到文件时对应的文件名（数据库类返回空）。
func sinkFileName(cfg model.HarvestSink) string {
	table := strings.TrimSpace(cfg.Table)
	if table == "" {
		table = "harvest_records"
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Driver)) {
	case "xlsx", "excel":
		return table + ".xlsx"
	case "json":
		return table + ".json"
	case "jsonl":
		return table + ".jsonl"
	default:
		return ""
	}
}

type harvestFileView struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	Kind     string    `json:"kind"`
	Download string    `json:"download"`
}

// listHarvestFiles GET /api/collect/files —— 列出 <数据目录>/harvest 下的产出文件（可下载）
func (s *Server) listHarvestFiles(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Join(s.DB.Dir(), "harvest")
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []harvestFileView{}, "dir": dir})
		return
	}
	items := make([]harvestFileView, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		kind := "other"
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".xlsx":
			kind = "xlsx"
		case ".json":
			kind = "json"
		case ".jsonl":
			kind = "jsonl"
		case ".db", ".sqlite", ".sqlite3":
			kind = "db"
		}
		items = append(items, harvestFileView{
			Name: e.Name(), Size: info.Size(), Modified: info.ModTime(), Kind: kind,
			Download: "/api/collect/files/" + url.PathEscape(e.Name()),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Modified.After(items[j].Modified) })
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "dir": dir})
}

// downloadHarvestFile GET /api/collect/files/{name} —— 下载产出文件
func (s *Server) downloadHarvestFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	// 只允许"纯文件名"，挡住 ../ 之类的路径穿越
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") || strings.HasPrefix(name, ".") {
		writeErr(w, http.StatusBadRequest, "文件名不合法")
		return
	}
	dir := filepath.Join(s.DB.Dir(), "harvest")
	path := filepath.Join(dir, name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		writeErr(w, http.StatusNotFound, "文件不存在："+name)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "打开文件失败："+err.Error())
		return
	}
	defer f.Close()

	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"; filename*=UTF-8''`+url.PathEscape(name))
	switch strings.ToLower(filepath.Ext(name)) {
	case ".xlsx":
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	case ".json", ".jsonl":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	case ".db", ".sqlite", ".sqlite3":
		w.Header().Set("Content-Type", "application/vnd.sqlite3")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}

/* --------------------------------------------------------------- 选项信息 */

// collectOptions GET /api/collect/options —— 控制台渲染下拉框需要的选项与默认值
func (s *Server) collectOptions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"drivers": []map[string]string{
			{"value": "sqlite", "label": "SQLite（本地数据库，零依赖）", "dsn_hint": "留空 = <数据目录>/harvest/harvest.db；也可填 data/harvest/x.db"},
			{"value": "xlsx", "label": "Excel 表格（.xlsx，可下载）", "dsn_hint": "无需 dsn，写到 <数据目录>/harvest/<表名>.xlsx，每次整份重写"},
			{"value": "json", "label": "JSON 文件（.json，可下载）", "dsn_hint": "无需 dsn，写到 <数据目录>/harvest/<表名>.json（一个 JSON 数组）"},
			{"value": "mysql", "label": "MySQL", "dsn_hint": "user:pass@tcp(127.0.0.1:3306)/dbname?charset=utf8mb4&parseTime=true"},
			{"value": "postgres", "label": "PostgreSQL", "dsn_hint": "postgres://user:pass@127.0.0.1:5432/dbname?sslmode=disable"},
			{"value": "jsonl", "label": "JSON Lines（.jsonl，追加写 / pandas 直读）", "dsn_hint": "无需 dsn，写到 <数据目录>/harvest/<表名>.jsonl"},
		},
		"session_modes": []map[string]string{
			{"value": "browser", "label": "复用内置浏览器登录态（推荐）"},
			{"value": "cookie", "label": "手工粘贴 Cookie"},
			{"value": "none", "label": "无会话（公开接口）"},
		},
		"resource_types": []string{"XHR", "Fetch", "Document"},
		"defaults": map[string]any{
			"throttle": model.DefaultThrottle(),
			"sink":     model.DefaultSink(),
			"schedule": model.HarvestSchedule{Mode: "interval", EverySeconds: 300, JitterPercent: 20},
		},
		"data_dir": s.DB.Dir(),
	})
}

/* ------------------------------------------------------------------ 小工具 */

func errMsg(err error, fallback string) string {
	if err != nil && err.Error() != "" {
		return err.Error()
	}
	return fallback
}

func originOfFirst(eps []model.HarvestEndpoint) string {
	for _, e := range eps {
		if strings.TrimSpace(e.URL) == "" {
			continue
		}
		u := e.URL
		if i := strings.Index(u, "://"); i > 0 {
			rest := u[i+3:]
			if j := strings.IndexAny(rest, "/?#"); j >= 0 {
				rest = rest[:j]
			}
			return u[:i+3] + rest
		}
	}
	return ""
}
