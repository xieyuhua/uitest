package api

import (
	"context"
	"net/http"
	"time"
)

/* -------------------------------------------------- 一键刷新定位（重算 uid） */

// rebindRecording POST /api/recordings/{id}/rebind
//
// 打开录制起始页 → 按每步现有定位找到元素 → 把元素当前的 data-uitest-id 写回步骤。
// 老录制（旧算法 uid 或纯 css 定位）执行前跑一次，定位就从"结构路径"升级成"稳定 ID"。
func (s *Server) rebindRecording(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.DB.GetRecording(id); err != nil {
		writeErr(w, http.StatusNotFound, "录制不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	rep, err := s.Runner.Rebind(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

/* ----------------------------------------------------------- 数据快照恢复 */

// listBackups GET /api/backups
func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	items := s.DB.ListBackups()
	if items == nil {
		items = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// restoreBackup POST /api/backups/{name}/restore
//
// 恢复会用备份覆盖当前数据；覆盖前服务端会先把"当前状态"再存一份快照，
// 所以误恢复也能退回。
func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.DB.RestoreBackup(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	recs, runs := s.DB.Counts()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"restored":   name,
		"recordings": recs,
		"runs":       runs,
		"message":    "已恢复备份，当前数据已被覆盖（恢复前的状态也留了一份快照）",
	})
}
