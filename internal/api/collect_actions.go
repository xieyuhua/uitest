package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"uitest-server/internal/collect"
	"uitest-server/internal/model"
	"uitest-server/internal/store"
)

// 本文件是「接口采集顺带录 UI 操作」的 HTTP 接口。
//
// 采集会话里除了接口请求，还会顺带记下用户在页面上的点击 / 输入 / 下拉 /
// 勾选 / 路由跳转。这些操作可以：
//   - 在结果页查看、单条删除、手动补充（POST /actions）；
//   - 一键生成一条 UI 回放流程（POST /recording），
//     再拿这条录制作定时采集任务的「前置登录」：先 UI 登录，再抓接口。

// getCaptureActions GET /api/collect/captures/{id}/actions
func (s *Server) getCaptureActions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	items, ok, err := s.Collect.Recorder.UIActions(id)
	if err != nil || !ok {
		writeErr(w, http.StatusNotFound, errMsg(err, "录制会话不存在"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"count": len(items),
	})
}

type addActionsReq struct {
	Items []model.UiAction `json:"items"`
}

// addCaptureActions POST /api/collect/captures/{id}/actions
// 插件上报 / 手动补充 UI 操作（同 id 幂等）。
func (s *Server) addCaptureActions(w http.ResponseWriter, r *http.Request) {
	var req addActionsReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if len(req.Items) == 0 {
		writeErr(w, http.StatusBadRequest, "items 为空")
		return
	}
	id := r.PathValue("id")
	added, err := s.Collect.Recorder.AddUIActions(id, req.Items)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"added": added,
		"count": s.Collect.Recorder.UIActionCount(id),
	})
}

// deleteCaptureActions DELETE /api/collect/captures/{id}/actions
// body: {"ids":["..."]}；也支持 ?ids=a,b,c 与 ?all=1（清空但保留会话）。
func (s *Server) deleteCaptureActions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		IDs []string `json:"ids"`
	}
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
	all := r.URL.Query().Get("all") == "1"
	n, err := s.Collect.Recorder.DeleteUIActions(id, ids, all)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": n,
		"count":   s.Collect.Recorder.UIActionCount(id),
		"message": fmt.Sprintf("已删除 %d 条 UI 操作", n),
	})
}

type captureRecordingReq struct {
	Name     string   `json:"name"`
	StartURL string   `json:"start_url"`
	AppendTo string   `json:"append_to"`
	IDs      []string `json:"ids"`
}

// createRecordingFromCapture POST /api/collect/captures/{id}/recording
// 把这次录制顺带录到的 UI 操作转成一条 UI 回放流程。
//
// 生成后可以直接：
//   - 去「UI 录制」页检查 / 调整步骤并回放；
//   - 在定时采集任务里把它选成「前置登录」，实现「先 UI 登录，再抓接口」。
func (s *Server) createRecordingFromCapture(w http.ResponseWriter, r *http.Request) {
	var req captureRecordingReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	id := r.PathValue("id")
	items, ok, err := s.Collect.Recorder.UIActions(id)
	if err != nil || !ok {
		writeErr(w, http.StatusNotFound, errMsg(err, "录制会话不存在"))
		return
	}
	if len(req.IDs) > 0 {
		want := make(map[string]bool, len(req.IDs))
		for _, x := range req.IDs {
			want[strings.TrimSpace(x)] = true
		}
		kept := make([]model.UiAction, 0, len(items))
		for _, it := range items {
			if want[it.ID] {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	steps := collect.ActionsToSteps(items)
	if len(steps) == 0 {
		writeErr(w, http.StatusBadRequest, "这次录制还没有录到 UI 操作（点击 / 输入 / 下拉 / 勾选 / 跳转），没有可生成的流程")
		return
	}

	startURL := strings.TrimSpace(req.StartURL)
	if startURL == "" {
		if sess, ok2 := s.Collect.Recorder.Get(id); ok2 {
			startURL = strings.TrimSpace(sess.StartURL)
		}
	}
	if startURL == "" {
		startURL = collect.StartURLFromActions(items)
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "接口录制 UI 流程 " + time.Now().Format("2006-01-02 15:04")
	}
	desc := fmt.Sprintf("由接口录制会话 %s 的 %d 条 UI 操作生成。密码框已替换成 {{password}}，在任务「前置登录 → 变量」里填真实密码即可复用。", id, len(steps))

	var rec model.Recording
	appendMode := strings.TrimSpace(req.AppendTo) != ""
	if appendMode {
		rec, err = s.DB.GetRecording(strings.TrimSpace(req.AppendTo))
		if err != nil {
			writeErr(w, http.StatusNotFound, "要追加的录制不存在："+err.Error())
			return
		}
	} else {
		rec, err = s.DB.CreateRecording(name, desc, startURL, []string{"接口录制", "UI 回放"})
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	saved, err := s.DB.SaveSteps(rec.ID, steps, appendMode)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存流程步骤失败："+err.Error())
		return
	}
	if ready := "ready"; saved.Status != ready {
		if _, e2 := s.DB.UpdateRecording(saved.ID, store.RecordingPatch{Status: &ready}); e2 == nil {
			saved.Status = ready
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"recording_id": saved.ID,
		"name":         saved.Name,
		"step_count":   len(saved.Steps),
		"start_url":    saved.StartURL,
		"url":          "#/recordings/" + saved.ID,
		"hint":         "流程已生成：可以先去「UI 录制」检查步骤；在定时采集任务里把它选成「前置登录」就能先登录再抓接口",
	})
}
