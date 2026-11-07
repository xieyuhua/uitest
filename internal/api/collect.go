package api

import (
	"context"
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
			"types":      sess.Types,
			"skipped":    s.Collect.Recorder.SkippedTypes(id),
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
	// Prelogin 运行前先跑一段 UI 自动化（登录）
	Prelogin *model.HarvestPrelogin `json:"prelogin"`
	// Notify 跑完把成功/失败推到企业微信群机器人
	Notify *model.HarvestNotify `json:"notify"`
	// Common 公共参数（所有接口共用；登录后的浏览器头与脚本会覆盖同名项）
	Common *model.HarvestCommon `json:"common"`
	Status string               `json:"status"`
	// SplitByEndpoint 每个接口单独分表 / 分文件
	SplitByEndpoint bool `json:"split_by_endpoint"`
	// SinkProfileID 来源的落库配置 id（仅做溯源）
	SinkProfileID string `json:"sink_profile_id"`
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
		Session:     model.HarvestSession{Mode: "browser"},
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
		// 会话统一走浏览器：顺手把录制时的 UA 与站点带上（留空时运行期再自动推断）
		task.Session.UserAgent = ua
		if strings.TrimSpace(task.Session.Host) == "" {
			task.Session.Host = originOfFirst(eps)
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
		if msg := sessionConfigErr(task.Session); msg != "" {
			writeErr(w, http.StatusBadRequest, msg)
			return
		}
	}
	task.Prelogin = req.Prelogin
	task.Notify = req.Notify
	task.Common = req.Common
	if req.Status != "" {
		task.Status = req.Status
	}
	task.SplitByEndpoint = req.SplitByEndpoint
	task.SinkProfileID = strings.TrimSpace(req.SinkProfileID)

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

// sessionConfigErr 会话来源的保存前校验（运行期还会再校验一次，这里先拦住明显配错的）。
func sessionConfigErr(s model.HarvestSession) string {
	if s.EffectiveSource() != model.SessionSourceHTTP {
		return ""
	}
	if s.Token == nil || s.Token.Login == nil || strings.TrimSpace(s.Token.Login.URL) == "" {
		return "会话来源选了「登录接口」，但没填登录接口地址"
	}
	return ""
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
		// 响应样本留一份（任务详情里能回看"当初这条接口返回的什么结构"）
		ep.SampleBody = collect.ClipText(e.BodyPreview, 48<<10)
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
		// 列表页不带录制快照的大字段（响应体 / 请求体 / 请求头）
		eps = make([]model.HarvestEndpoint, len(t.Endpoints))
		for i, e := range t.Endpoints {
			e.SampleBody = ""
			e.SampleReqBody = ""
			e.SampleHeaders = nil
			eps[i] = e
		}
	}
	// 每个接口实际会写到哪儿（接口级 sink 覆盖 / 按接口分表）
	endpointSinks := make(map[string]any, len(t.Endpoints))
	for _, e := range t.Endpoints {
		cfg := t.EndpointSink(e)
		endpointSinks[e.ID] = map[string]any{
			"driver": cfg.Driver,
			"table":  cfg.Table,
			"target": sinkTargetText(cfg),
		}
	}
	return map[string]any{
		"id":                t.ID,
		"name":              t.Name,
		"description":       t.Description,
		"start_url":         t.StartURL,
		"status":            t.Status,
		"endpoints":         eps,
		"endpoint_count":    t.EndpointCount(),
		"schedule":          t.Schedule,
		"schedule_text":     collect.DescribeSchedule(t.Schedule),
		"throttle":          t.Throttle,
		"sink":              t.Sink,
		"sink_file":         sinkFileName(t.Sink),
		"sink_target":       sinkTargetText(t.Sink),
		"split_by_endpoint": t.SplitByEndpoint,
		"sink_profile_id":   t.SinkProfileID,
		"endpoint_sinks":    endpointSinks,
		"session":           t.Session,
		"prelogin":          t.Prelogin,
		"prelogin_text":     preloginText(t.Prelogin),
		"notify":            t.Notify,
		"notify_text":       notifyText(t.Notify),
		"common":            t.Common,
		"created_at":        t.CreatedAt,
		"updated_at":        t.UpdatedAt,
		"last_run_at":       t.LastRunAt,
		"next_run_at":       t.NextRunAt,
		"last_status":       t.LastStatus,
		"last_error":        t.LastError,
		"last_written":      t.LastWritten,
		"run_count":         t.RunCount,
		"fail_streak":       t.FailStreak,
		"running":           running,
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
	// Prelogin 运行前先跑一段 UI 自动化（登录）
	Prelogin *model.HarvestPrelogin `json:"prelogin"`
	// Notify 跑完把成功/失败推到企业微信群机器人（传 null 表示关掉）
	Notify *model.HarvestNotify `json:"notify"`
	// Common 公共参数（传 null / 省略表示清空）
	Common *model.HarvestCommon `json:"common"`
	// SplitByEndpoint 每个接口单独分表 / 分文件
	SplitByEndpoint *bool `json:"split_by_endpoint"`
	// SinkProfileID 来源的落库配置 id（仅做溯源）
	SinkProfileID *string `json:"sink_profile_id"`
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
		if msg := sessionConfigErr(t.Session); msg != "" {
			writeErr(w, http.StatusBadRequest, msg)
			return
		}
	}
	if req.Prelogin != nil {
		pl := *req.Prelogin
		if pl.Enabled && strings.TrimSpace(pl.RecordingID) == "" {
			writeErr(w, http.StatusBadRequest, "前置登录开了但没有选「登录流程录制」")
			return
		}
		t.Prelogin = &pl
	}
	// 通知：传对象就按传的存，传 null / 省略视为关掉（控制台每次保存都会带上这个字段）
	t.Notify = req.Notify
	if t.Notify != nil && t.Notify.Enabled && strings.TrimSpace(t.Notify.Webhook) == "" {
		writeErr(w, http.StatusBadRequest, "通知开了但没填 Webhook 地址")
		return
	}
	// 公共参数：传对象就按传的存，传 null / 省略视为清空
	t.Common = req.Common
	if req.SplitByEndpoint != nil {
		t.SplitByEndpoint = *req.SplitByEndpoint
	}
	if req.SinkProfileID != nil {
		t.SinkProfileID = strings.TrimSpace(*req.SinkProfileID)
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
	// EndpointID 非空时只试抓这一个接口
	EndpointID string `json:"endpoint_id"`
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
	results := s.Collect.Harvester.Preview(ctx, t, req.Limit, strings.TrimSpace(req.EndpointID))
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

type selftestReq struct {
	// RunPrelogin 顺便跑一遍前置登录（UI 自动化），把整条链路都测了
	RunPrelogin bool `json:"run_prelogin"`
}

// selftestTask POST /api/collect/tasks/{id}/selftest —— 走一遍「登录 → 导出会话 → 真发一次请求」并逐步报告
func (s *Server) selftestTask(w http.ResponseWriter, r *http.Request) {
	var req selftestReq
	_ = readJSON(r, &req)
	t, err := s.DB.GetHarvestTask(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	ctx, cancel := timeoutCtx(r, 300*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.Collect.Harvester.SelfTest(ctx, t, req.RunPrelogin))
}

// runPrelogin POST /api/collect/tasks/{id}/prelogin —— 现在就跑一遍前置登录（顺便导出会话看结果）
func (s *Server) runPrelogin(w http.ResponseWriter, r *http.Request) {
	t, err := s.DB.GetHarvestTask(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if t.Prelogin == nil || !t.Prelogin.Active() {
		writeErr(w, http.StatusBadRequest, "这个任务没有配置前置登录（先选一个「登录流程录制」并打开开关）")
		return
	}
	if s.Runner == nil {
		writeErr(w, http.StatusBadRequest, "服务没有装配 UI 自动化执行器")
		return
	}
	cfg := t.Prelogin.Effective()
	ctx, cancel := timeoutCtx(r, time.Duration(cfg.TimeoutSeconds+30)*time.Second)
	defer cancel()

	start := time.Now()
	vars := s.Collect.Harvester.PreloginVars(cfg)
	runID, rerr := s.Runner.RunRecordingSync(ctx, cfg.RecordingID, "采集前置登录（手动）", vars)
	out := map[string]any{
		"ok":         rerr == nil,
		"run_id":     runID,
		"elapsed_ms": time.Since(start).Milliseconds(),
	}
	if rerr != nil {
		out["error"] = rerr.Error()
	}
	if len(vars) == 0 {
		out["warning"] = "没配「登录用变量」：录制时密码框会被脱敏成 {{password}}，回放填的是字面量，登录多半失败"
	} else if strings.TrimSpace(vars["password"]) == "" {
		out["warning"] = "变量里没有 password：如果登录表单有密码框，回放会填成 {{password}}"
	}
	// 顺便按当前登录状态导一次会话，让他确认"刷新好了没"
	cookies, token, serr := s.Collect.Harvester.ProbeSession(ctx, t)
	if serr != nil {
		out["session_error"] = serr.Error()
	} else {
		out["cookie_count"] = cookies
		if token != "" {
			out["token_preview"] = tokenPreview(token)
		}
	}
	// 顺便把「登录后刷新公共参数」算一遍：直接看到最新 token / Cookie 到底是多少
	if prev, perr := s.Collect.Harvester.RefreshSessionPreview(ctx, &t); perr == nil {
		out["common_refresh"] = prev
	} else {
		out["common_refresh_error"] = perr.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// testLoginReq 「测试登录」请求：带 login 就用传来的（未保存的）配置，否则用任务里存的。
type testLoginReq struct {
	Login  *model.HarvestLogin `json:"login"`
	Param  string              `json:"param"`
	Header string              `json:"header"`
	Prefix string              `json:"prefix"`
}

// testLogin POST /api/collect/tasks/{id}/login/test —— 只跑一次登录接口，看 token / Cookie 拿没拿到
func (s *Server) testLogin(w http.ResponseWriter, r *http.Request) {
	t, err := s.DB.GetHarvestTask(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var req testLoginReq
	_ = readJSON(r, &req)
	lg := req.Login
	if lg == nil || strings.TrimSpace(lg.URL) == "" {
		if t.Session.Token == nil || t.Session.Token.Login == nil || strings.TrimSpace(t.Session.Token.Login.URL) == "" {
			writeErr(w, http.StatusBadRequest, "没有登录接口配置：先在「会话来源 → 登录接口」里填地址 / 请求体 / token 取值路径")
			return
		}
		cp := *t.Session.Token.Login
		lg = &cp
	}
	tk := model.HarvestToken{Source: "login", Login: lg, Param: req.Param, Header: req.Header, Prefix: req.Prefix}
	if t.Session.Token != nil {
		base := t.Session.Token.Effective()
		if strings.TrimSpace(tk.Param) == "" {
			tk.Param = base.Param
		}
		if strings.TrimSpace(tk.Header) == "" {
			tk.Header = base.Header
		}
		if strings.TrimSpace(tk.Prefix) == "" {
			tk.Prefix = base.Prefix
		}
	}
	ctx, cancel := timeoutCtx(r, 90*time.Second)
	defer cancel()
	start := time.Now()
	trace, applied, lerr := s.Collect.Harvester.TestLogin(ctx, &t, tk)
	out := map[string]any{
		"ok":         lerr == nil,
		"elapsed_ms": time.Since(start).Milliseconds(),
		"trace":      trace,
		"applied":    applied,
	}
	if lerr != nil {
		out["error"] = lerr.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// preloginText 前置登录的人可读描述。
func preloginText(p *model.HarvestPrelogin) string {
	if p == nil || !p.Active() {
		return ""
	}
	cfg := p.Effective()
	name := strings.TrimSpace(cfg.RecordingName)
	if name == "" {
		name = cfg.RecordingID
	}
	return "运行前执行「" + name + "」（" + cfg.ModeText() + "）"
}

// notifyText 运行通知的人可读描述。
func notifyText(n *model.HarvestNotify) string {
	if n == nil || !n.Active() {
		return ""
	}
	var when []string
	if n.WantSuccess() {
		when = append(when, "成功")
	}
	if n.WantFailure() {
		when = append(when, "失败")
	}
	if len(when) == 0 {
		return "通知已配置（成功/失败都不推，等于没开）"
	}
	text := "企业微信群:" + strings.Join(when, "、") + "都推"
	if len(n.Mentioned) > 0 {
		text += "（@ " + strings.Join(n.Mentioned, "、") + "）"
	}
	return text
}

// taskToken POST /api/collect/tasks/{id}/token —— 现在读一次 Token（控制台「读取 Token」按钮用）
//
// 浏览器模式会真的打开内置浏览器按当前登录状态重读（不吃缓存），用来确认"token 现在是不是最新的"。
func (s *Server) taskToken(w http.ResponseWriter, r *http.Request) {
	t, err := s.DB.GetHarvestTask(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	ctx, cancel := timeoutCtx(r, 90*time.Second)
	defer cancel()
	header, prefix, token, err := s.Collect.Harvester.ProbeToken(ctx, t)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"header":  header,
		"prefix":  prefix,
		"token":   token,
		"length":  len(token),
		"preview": tokenPreview(token),
	})
}

// tokenPreview 只露头尾（列表/提示里用），完整值单独放在 token 字段里。
func tokenPreview(tok string) string {
	if len(tok) <= 24 {
		return tok
	}
	return tok[:12] + "…" + tok[len(tok)-8:]
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

// testHarvestNotify POST /api/collect/tasks/{id}/test-notify —— 用系统设置里的企业微信机器人发一条测试消息
func (s *Server) testHarvestNotify(w http.ResponseWriter, r *http.Request) {
	cfg := s.DB.Settings().Notify
	if !cfg.Active() {
		writeErr(w, http.StatusBadRequest, "还没启用通知，或 Webhook 地址为空（去「设置 → 运行通知」配置）")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	content := collect.WeComTestMessage(cfg)
	if err := collect.SendWeCom(ctx, cfg.Webhook, content, cfg.Mentioned); err != nil {
		writeErr(w, http.StatusBadRequest, "发送失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "测试消息已发送，去企业微信群里看一眼", "content": content,
	})
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

/* -------------------------------------------------------- 数据写入（落库配置） */

// sinkProfileView 落库配置的前端视图（补上人可读的写入目标）。
func sinkProfileView(p model.SinkProfile) map[string]any {
	return map[string]any{
		"id":         p.ID,
		"name":       p.Name,
		"driver":     p.Driver,
		"dsn":        p.DSN,
		"table":      p.Table,
		"mode":       p.Mode,
		"key_column": p.KeyColumn,
		"batch_size": p.BatchSize,
		"note":       p.Note,
		"target":     sinkTargetText(p.HarvestSink),
		"file":       sinkFileName(p.HarvestSink),
		"created_at": p.CreatedAt,
		"updated_at": p.UpdatedAt,
	}
}

// sinkTargetText 人可读的写入目标描述。
func sinkTargetText(cfg model.HarvestSink) string {
	if f := sinkFileName(cfg); f != "" {
		return "文件 harvest/" + f
	}
	table := strings.TrimSpace(cfg.Table)
	if table == "" {
		table = "harvest_records"
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Driver)) {
	case "mysql":
		return "MySQL 表 " + table
	case "postgres", "postgresql", "pgsql":
		return "PostgreSQL 表 " + table
	default:
		return "SQLite 表 " + table
	}
}

// listSinkProfiles GET /api/collect/sinks
func (s *Server) listSinkProfiles(w http.ResponseWriter, r *http.Request) {
	items := s.DB.ListSinkProfiles()
	out := make([]map[string]any, 0, len(items))
	for _, p := range items {
		out = append(out, sinkProfileView(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// getSinkProfile GET /api/collect/sinks/{id}
func (s *Server) getSinkProfile(w http.ResponseWriter, r *http.Request) {
	p, err := s.DB.GetSinkProfile(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sinkProfileView(p))
}

// sinkProfileReq 新建 / 保存落库配置的请求体（sink 字段直接展开在同一层）。
type sinkProfileReq struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Note string `json:"note"`
	model.HarvestSink
}

// saveSinkProfile POST /api/collect/sinks | PUT /api/collect/sinks/{id}
func (s *Server) saveSinkProfile(w http.ResponseWriter, r *http.Request) {
	var req sinkProfileReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	p := model.SinkProfile{
		ID:          strings.TrimSpace(r.PathValue("id")),
		Name:        strings.TrimSpace(req.Name),
		Note:        req.Note,
		HarvestSink: req.HarvestSink,
	}
	out, err := s.DB.SaveSinkProfile(p)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sinkProfileView(out))
}

// deleteSinkProfile DELETE /api/collect/sinks/{id}
func (s *Server) deleteSinkProfile(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.DeleteSinkProfile(r.PathValue("id")); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// testSinkProfile POST /api/collect/sinks/test —— 试连一次（id 或直接给 sink 字段都行）
func (s *Server) testSinkProfile(w http.ResponseWriter, r *http.Request) {
	var req sinkProfileReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	cfg := req.HarvestSink
	if cfg.Driver == "" {
		id := strings.TrimSpace(r.PathValue("id"))
		if id == "" {
			id = strings.TrimSpace(req.ID)
		}
		if id != "" {
			if p, err := s.DB.GetSinkProfile(id); err == nil {
				cfg = p.HarvestSink
			}
		}
	}
	if strings.TrimSpace(cfg.Driver) == "" {
		writeErr(w, http.StatusBadRequest, "还没选择落库类型")
		return
	}
	if err := s.Collect.ProbeSink(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "连接成功：" + sinkTargetText(cfg),
		"target":  sinkTargetText(cfg),
	})
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

/* ---------------------------------------------------------------- 清空数据 */

// clearHarvestTasks DELETE /api/collect/tasks?all=1 —— 清空全部采集任务（连带运行记录）
func (s *Server) clearHarvestTasks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") != "1" {
		writeErr(w, http.StatusBadRequest, "清空需要显式确认：DELETE /api/collect/tasks?all=1")
		return
	}
	tasks, runs := s.DB.DeleteAllHarvestTasks()
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted_tasks": tasks, "deleted_runs": runs,
		"message": fmt.Sprintf("已清空 %d 个采集任务（连带 %d 条运行记录）", tasks, runs),
	})
}

// clearHarvestFiles DELETE /api/collect/files?all=1 —— 删除 data/harvest 下的产出文件
//
// 就是"把采集到的数据删掉"：Excel / JSON / JSONL / SQLite 表格文件全删。
// 正在被占用的文件（例如别人还用着 harvest.db）会跳过，并在 failed 里列出来。
func (s *Server) clearHarvestFiles(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") != "1" {
		writeErr(w, http.StatusBadRequest, "清空需要显式确认：DELETE /api/collect/files?all=1")
		return
	}
	dir := filepath.Join(s.DB.Dir(), "harvest")
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"deleted": 0, "freed_bytes": 0, "failed": []string{},
			"message": "没有产出文件可删（目录不存在）",
		})
		return
	}
	deleted, freed := 0, int64(0)
	failed := make([]string, 0, 4)
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		size := int64(0)
		if info, ierr := e.Info(); ierr == nil {
			size = info.Size()
		}
		if rerr := os.Remove(path); rerr != nil {
			failed = append(failed, e.Name())
			continue
		}
		deleted++
		freed += size
	}
	msg := fmt.Sprintf("已删除 %d 个产出文件，释放 %s", deleted, humanBytes(freed))
	if len(failed) > 0 {
		msg += fmt.Sprintf("；%d 个文件被占用未删（%s）", len(failed), strings.Join(failed, "、"))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": deleted, "freed_bytes": freed, "failed": failed, "message": msg,
	})
}

// clearCaptures DELETE /api/collect/captures?all=1 —— 丢弃全部录制结果（内存里的会话）
func (s *Server) clearCaptures(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") != "1" {
		writeErr(w, http.StatusBadRequest, "清空需要显式确认：DELETE /api/collect/captures?all=1")
		return
	}
	n := s.Collect.Recorder.DiscardAll()
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": n,
		"message": fmt.Sprintf("已丢弃 %d 个录制结果（已建任务与已入库数据不受影响）", n),
	})
}

// humanBytes 字节数转人可读（用于"释放了多少空间"这类提示）。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	f := float64(n)
	i := -1
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
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
		// 会话来源只剩一种：浏览器登录态（Cookie + 请求头都从内置浏览器取）
		"session_modes": []map[string]string{
			{"value": "browser", "label": "浏览器登录态（前置登录后共享 Cookie 与请求头）"},
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

/* ------------------------------------- 公共参数：粘贴导入 / 录制汇总 / 登录后刷新 */

// commonSuggestReq 从录制结果汇总公共参数：给 capture_id + request_ids，
// 或只给任务 id（用接口的 created_from 反查录制）。
type commonSuggestReq struct {
	CaptureID  string   `json:"capture_id,omitempty"`
	RequestIDs []string `json:"request_ids,omitempty"`
}

// suggestCommon POST /api/collect/common/suggest —— 不绑定任务，建任务弹窗用。
func (s *Server) suggestCommon(w http.ResponseWriter, r *http.Request) {
	var req commonSuggestReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	entries, source, err := s.capturedEntries(nil, req.CaptureID, req.RequestIDs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sug := collect.SuggestCommonFromCaptured(entries)
	writeJSON(w, http.StatusOK, map[string]any{
		"common":  collect.MergeCommonIntoSuggest(nil, sug),
		"notes":   suggestNotes(sug),
		"matched": len(entries),
		"source":  source,
	})
}

// suggestTaskCommon POST /api/collect/tasks/{id}/common/suggest —— 合并进任务已有的公共参数。
func (s *Server) suggestTaskCommon(w http.ResponseWriter, r *http.Request) {
	t, err := s.DB.GetHarvestTask(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var req commonSuggestReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	entries, source, err := s.capturedEntries(&t, req.CaptureID, req.RequestIDs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sug := collect.SuggestCommonFromCaptured(entries)
	writeJSON(w, http.StatusOK, map[string]any{
		"common":  collect.MergeCommonIntoSuggest(t.Common, sug),
		"notes":   suggestNotes(sug),
		"matched": len(entries),
		"source":  source,
	})
}

// suggestNotes 汇总结果的说明；什么都没提取到时给一条能照做的提示。
func suggestNotes(sug collect.CommonSuggest) []string {
	if len(sug.Notes) > 0 {
		return sug.Notes
	}
	return []string{"这次录制里没有找到「所有接口都一样」的参数：分页参数（page / offset / limit）不会汇总；" +
		"可以手工填，或换个登录后的列表页重新录一次"}
}

// capturedEntries 按 capture_id + request_ids 取录制条目；
// captureID 为空时用任务的 created_from 反查（服务重启后录制结果会清空）。
func (s *Server) capturedEntries(task *model.HarvestTask, captureID string, requestIDs []string) ([]model.CapturedRequest, string, error) {
	pick := map[string]bool{}
	for _, id := range requestIDs {
		if v := strings.TrimSpace(id); v != "" {
			pick[v] = true
		}
	}
	if cid := strings.TrimSpace(captureID); cid != "" {
		list, ok, err := s.Collect.Recorder.Entries(cid)
		if err != nil || !ok {
			return nil, "", fmt.Errorf("录制会话不存在（服务重启后录制结果会清空，请重新录制一次）")
		}
		out := make([]model.CapturedRequest, 0, len(list))
		for _, e := range list {
			if len(pick) > 0 && !pick[e.ID] {
				continue
			}
			out = append(out, e)
		}
		if len(out) == 0 {
			return nil, "", fmt.Errorf("这个录制会话里没有勾选的请求，先回录制页重新勾选接口")
		}
		return out, fmt.Sprintf("录制会话 %s（%d 条请求）", cid, len(out)), nil
	}
	if task == nil {
		return nil, "", fmt.Errorf("没有提供录制会话（capture_id），也没有任务可以用来反查")
	}
	want := map[string]bool{}
	for _, ep := range task.Endpoints {
		if v := strings.TrimSpace(ep.CreatedFrom); v != "" {
			want[v] = true
		}
	}
	if len(want) == 0 {
		return nil, "", fmt.Errorf("这个任务的接口没有录制来源（手工填的），没法从录制汇总；可以在「公共参数 → 粘贴导入」里直接贴 Cookie / 请求头")
	}
	var out []model.CapturedRequest
	source := ""
	for _, info := range s.Collect.Recorder.List() {
		list, ok, err := s.Collect.Recorder.Entries(info.ID)
		if err != nil || !ok {
			continue
		}
		for _, e := range list {
			if !want[e.ID] || (len(pick) > 0 && !pick[e.ID]) {
				continue
			}
			if source == "" {
				source = info.ID
			}
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, "", fmt.Errorf("找不到这些接口的录制记录了（服务重启后录制结果会清空）；可以在「公共参数 → 粘贴导入」里直接贴 Cookie / 请求头")
	}
	return out, fmt.Sprintf("录制会话 %s（%d 条请求）", source, len(out)), nil
}

// importCommon POST /api/collect/common/import —— 只解析粘贴内容，不落库（建任务弹窗用）。
func (s *Server) importCommon(w http.ResponseWriter, r *http.Request) {
	var req collect.CommonImport
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	parsed, notes := collect.ParseCommonImport(req)
	writeJSON(w, http.StatusOK, map[string]any{
		"common": collect.MergeCommonIntoSuggest(nil, commonSuggestOf(parsed)),
		"notes":  notes,
	})
}

// importTaskCommon POST /api/collect/tasks/{id}/common/import —— 解析后合并进任务已有的公共参数。
func (s *Server) importTaskCommon(w http.ResponseWriter, r *http.Request) {
	t, err := s.DB.GetHarvestTask(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var req collect.CommonImport
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	parsed, notes := collect.ParseCommonImport(req)
	writeJSON(w, http.StatusOK, map[string]any{
		"common": collect.MergeCommonIntoSuggest(t.Common, commonSuggestOf(parsed)),
		"notes":  notes,
	})
}

// commonSuggestOf 把解析出的公共参数转成建议结构，复用「不覆盖已有值」的合并逻辑。
func commonSuggestOf(c model.HarvestCommon) collect.CommonSuggest {
	return collect.CommonSuggest{Params: c.Params, Headers: c.Headers, Cookies: c.Cookies, Bindings: c.Bindings}
}

type commonRefreshReq struct {
	// RunPrelogin 先跑一遍前置登录再刷新（登录态最新）
	RunPrelogin bool `json:"run_prelogin"`
}

// refreshTaskCommon POST /api/collect/tasks/{id}/common/refresh
//
// 按当前浏览器登录态刷新一次公共参数（只读，不保存任务配置）：
// 返回脱敏后的参数 / Cookie / 绑定，以及"这次实际改了什么"。
// 可选 run_prelogin=true，先跑一遍前置登录再取值。
func (s *Server) refreshTaskCommon(w http.ResponseWriter, r *http.Request) {
	t, err := s.DB.GetHarvestTask(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var req commonRefreshReq
	_ = readJSON(r, &req)
	ctx, cancel := timeoutCtx(r, 300*time.Second)
	defer cancel()

	out := map[string]any{"ok": true}
	if req.RunPrelogin {
		if perr := s.Collect.Harvester.RunPreloginNow(ctx, &t); perr != nil {
			out["prelogin_error"] = perr.Error()
		} else {
			out["prelogin_ok"] = true
		}
	}
	prev, rerr := s.Collect.Harvester.RefreshSessionPreview(ctx, &t)
	if rerr != nil {
		out["ok"] = false
		out["error"] = rerr.Error()
		out["hint"] = "先在「登录」页签跑一次前置登录，或在「公共参数 → 粘贴导入」里贴一份 Cookie"
		writeJSON(w, http.StatusOK, out)
		return
	}
	out["refresh"] = prev
	writeJSON(w, http.StatusOK, out)
}
