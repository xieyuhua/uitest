// Package api 暴露控制台与浏览器插件使用的 HTTP 接口。
// 路由用标准库 net/http 的方法化模式（Go 1.22+），不引入第三方 Web 框架。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"uitest-server/internal/browser"
	"uitest-server/internal/bus"
	"uitest-server/internal/collect"
	"uitest-server/internal/engine"
	"uitest-server/internal/store"
)

// Server 聚合所有依赖。
type Server struct {
	DB        *store.DB
	BR        *browser.Manager
	Runner    *engine.Runner
	Bus       *bus.Bus
	Collect   *collect.Service
	Version   string
	Port      int
	StartedAt time.Time
}

// New 构造 API 服务。
func New(db *store.DB, br *browser.Manager, runner *engine.Runner, b *bus.Bus, cs *collect.Service, version string, port int) *Server {
	return &Server{DB: db, BR: br, Runner: runner, Bus: b, Collect: cs, Version: version, Port: port, StartedAt: time.Now()}
}

// Handler 返回挂好全部接口的处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.health)
	// 一键批量刷新定位（老录制升级成 uid 绑定）
	mux.HandleFunc("POST /api/recordings/{id}/rebind", s.rebindRecording)
	// 数据快照：列表 + 一键恢复
	mux.HandleFunc("GET /api/backups", s.listBackups)
	mux.HandleFunc("POST /api/backups/{name}/restore", s.restoreBackup)

	mux.HandleFunc("GET /api/recordings", s.listRecordings)
	mux.HandleFunc("POST /api/recordings", s.createRecording)
	mux.HandleFunc("POST /api/recordings/import", s.importRecording)
	mux.HandleFunc("GET /api/recordings/{id}", s.getRecording)
	mux.HandleFunc("PATCH /api/recordings/{id}", s.patchRecording)
	mux.HandleFunc("DELETE /api/recordings/{id}", s.deleteRecording)
	// 批量删除 / 清空（清空必须带 ?all=1 显式确认）
	mux.HandleFunc("POST /api/recordings/batch-delete", s.batchDeleteRecordings)
	mux.HandleFunc("DELETE /api/recordings", s.clearRecordings)
	mux.HandleFunc("POST /api/recordings/{id}/steps", s.addStep)
	mux.HandleFunc("PATCH /api/recordings/{id}/steps/{sid}", s.patchStep)
	mux.HandleFunc("DELETE /api/recordings/{id}/steps/{sid}", s.deleteStep)
	mux.HandleFunc("POST /api/recordings/{id}/reorder", s.reorderSteps)
	mux.HandleFunc("POST /api/recordings/{id}/duplicate", s.duplicateRecording)
	mux.HandleFunc("GET /api/recordings/{id}/export", s.exportRecording)

	mux.HandleFunc("POST /api/elements/bind", s.bindElement)
	mux.HandleFunc("GET /api/elements", s.listElements)
	mux.HandleFunc("DELETE /api/elements/{key}", s.deleteElement)
	mux.HandleFunc("POST /api/elements/batch-delete", s.batchDeleteElements)
	mux.HandleFunc("DELETE /api/elements", s.clearElements)

	mux.HandleFunc("POST /api/ai/dataset", s.aiDataset)

	mux.HandleFunc("POST /api/runs", s.createRun)
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.cancelRun)
	mux.HandleFunc("POST /api/runs/{id}/heal", s.healRun)
	mux.HandleFunc("POST /api/runs/batch-delete", s.batchDeleteRuns)
	mux.HandleFunc("DELETE /api/runs", s.clearRuns)
	mux.HandleFunc("GET /api/runs/{id}/events", s.runEvents)

	mux.HandleFunc("GET /api/browser/status", s.browserStatus)
	mux.HandleFunc("GET /api/browser/diagnose", s.browserDiagnose)
	mux.HandleFunc("POST /api/browser/launch", s.browserLaunch)
	mux.HandleFunc("POST /api/browser/close", s.browserClose)

	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)

	// ---- 接口采集：录制 → 筛选 → 建任务 → 定时 → 落库 ----
	mux.HandleFunc("POST /api/collect/captures", s.startCapture)
	mux.HandleFunc("GET /api/collect/captures", s.listCaptures)
	mux.HandleFunc("GET /api/collect/captures/{id}", s.getCapture)
	mux.HandleFunc("POST /api/collect/captures/{id}/stop", s.stopCapture)
	mux.HandleFunc("POST /api/collect/captures/{id}/requests", s.addCaptureRequests)
	mux.HandleFunc("DELETE /api/collect/captures/{id}/requests", s.deleteCaptureRequests)
	mux.HandleFunc("POST /api/collect/captures/{id}/analyze", s.analyzeCapture)
	mux.HandleFunc("DELETE /api/collect/captures/{id}", s.deleteCapture)
	mux.HandleFunc("POST /api/collect/analyze", s.analyzeBody)

	mux.HandleFunc("GET /api/collect/tasks", s.listHarvestTasks)
	mux.HandleFunc("POST /api/collect/tasks", s.createHarvestTask)
	mux.HandleFunc("GET /api/collect/tasks/{id}", s.getHarvestTask)
	mux.HandleFunc("PATCH /api/collect/tasks/{id}", s.patchHarvestTask)
	mux.HandleFunc("DELETE /api/collect/tasks/{id}", s.deleteHarvestTask)
	mux.HandleFunc("POST /api/collect/tasks/{id}/run", s.runHarvestTask)
	mux.HandleFunc("POST /api/collect/tasks/{id}/preview", s.previewHarvestTask)
	mux.HandleFunc("POST /api/collect/tasks/{id}/analyze", s.analyzeHarvestEndpoint)
	mux.HandleFunc("POST /api/collect/tasks/{id}/detect-pagination", s.detectPagination)
	mux.HandleFunc("POST /api/collect/tasks/{id}/test-sink", s.testHarvestSink)

	mux.HandleFunc("GET /api/collect/runs", s.listHarvestRuns)
	mux.HandleFunc("GET /api/collect/runs/{id}", s.getHarvestRun)
	mux.HandleFunc("POST /api/collect/runs/{id}/cancel", s.cancelHarvestRun)
	mux.HandleFunc("GET /api/collect/runs/{id}/events", s.harvestRunEvents)
	mux.HandleFunc("POST /api/collect/runs/batch-delete", s.batchDeleteHarvestRuns)
	mux.HandleFunc("DELETE /api/collect/runs", s.clearHarvestRuns)

	mux.HandleFunc("GET /api/collect/options", s.collectOptions)
	// 落库产出文件：列表 + 下载（Excel / JSON / JSONL / SQLite 都能下）
	mux.HandleFunc("GET /api/collect/files", s.listHarvestFiles)
	mux.HandleFunc("GET /api/collect/files/{name}", s.downloadHarvestFile)

	return cors(logRequests(mux))
}

/* ----------------------------------------------------------------- 中间件 */

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)
		if r.URL.Path != "/api/health" {
			log.Printf("[api] %s %s → %d (%s)", r.Method, r.URL.Path, rec.code, time.Since(start).Round(time.Millisecond))
		}
	})
}

/* ------------------------------------------------------------------ 辅助 */

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeJSONBody 只写 JSON 正文（响应头由调用方设置，用于文件下载）。
func writeJSONBody(w http.ResponseWriter, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "记录不存在")
		return
	}
	writeErr(w, http.StatusBadRequest, err.Error())
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// timeoutCtx 给耗时接口（试抓预览等）加一个兜底超时。
func timeoutCtx(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = 60 * time.Second
	}
	return context.WithTimeout(r.Context(), d)
}

/* ------------------------------------------------------------------ 健康 */

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	recs, runs := s.DB.Counts()
	htasks, hruns := s.DB.HarvestCounts()
	st := s.BR.Status()
	assist := s.DB.Settings().Assist
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"version":         s.Version,
		"uptime_sec":      int(time.Since(s.StartedAt).Seconds()),
		"recordings":      recs,
		"runs":            runs,
		"harvest_tasks":   htasks,
		"harvest_runs":    hruns,
		"browser_running": st.Running,
		"port":            s.Port,
		// 辅助能力开关：控制台据此决定是否显示"缺 testid"提醒等
		"assist": assist,
	})
}
