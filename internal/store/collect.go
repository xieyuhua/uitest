package store

import (
	"errors"
	"sort"
	"strings"
	"time"

	"uitest-server/internal/model"
)

// 本文件是「接口采集」相关的持久化：任务（HarvestTask）与运行记录（HarvestRun）。
// 与录制/运行共用同一个 JSON 文件，保持"零外部依赖、可读可备份"的取舍。

// keepHarvestRuns 采集运行记录保留条数。
const keepHarvestRuns = 300

/* -------------------------------------------------------------- 任务 CRUD */

// ListHarvestTasks 返回任务列表（不含样本响应体）。q 对名称/说明/接口 URL 做子串匹配。
func (db *DB) ListHarvestTasks(q string) []model.HarvestTask {
	db.mu.RLock()
	defer db.mu.RUnlock()
	q = strings.ToLower(strings.TrimSpace(q))
	out := make([]model.HarvestTask, 0, len(db.data.HarvestTasks))
	for _, t := range db.data.HarvestTasks {
		if q != "" {
			hay := strings.ToLower(t.Name + " " + t.Description + " " + t.Sink.Table)
			for _, e := range t.Endpoints {
				hay += " " + strings.ToLower(e.URL)
			}
			if !strings.Contains(hay, q) {
				continue
			}
		}
		out = append(out, t.Summary())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// GetHarvestTask 取任务全量深拷贝。
func (db *DB) GetHarvestTask(id string) (model.HarvestTask, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t, ok := db.data.HarvestTasks[id]
	if !ok {
		return model.HarvestTask{}, ErrNotFound
	}
	return t.Clone(), nil
}

// CreateHarvestTask 新建任务。
func (db *DB) CreateHarvestTask(t model.HarvestTask) (model.HarvestTask, error) {
	if strings.TrimSpace(t.Name) == "" {
		return model.HarvestTask{}, errors.New("任务名称不能为空")
	}
	now := time.Now()
	if t.ID == "" {
		t.ID = randID("hvt_")
	}
	t.CreatedAt = now
	t.UpdatedAt = now
	t.Normalize()
	db.mu.Lock()
	db.data.HarvestTasks[t.ID] = &t
	db.markDirty()
	out := t.Clone()
	db.mu.Unlock()
	return out, nil
}

// SaveHarvestTask 整体覆盖任务（保留创建时间与运行状态）。
func (db *DB) SaveHarvestTask(t model.HarvestTask) (model.HarvestTask, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	old, ok := db.data.HarvestTasks[t.ID]
	if !ok {
		return model.HarvestTask{}, ErrNotFound
	}
	t.CreatedAt = old.CreatedAt
	t.UpdatedAt = time.Now()
	// 运行状态由服务端维护，不允许前端覆盖
	t.LastRunAt = old.LastRunAt
	t.LastStatus = old.LastStatus
	t.LastError = old.LastError
	t.LastWritten = old.LastWritten
	t.RunCount = old.RunCount
	t.FailStreak = old.FailStreak
	t.Normalize()
	cp := t.Clone()
	db.data.HarvestTasks[t.ID] = &cp
	db.markDirty()
	return cp, nil
}

// MutateHarvestTask 在锁内修改任务（服务端状态更新用）。
func (db *DB) MutateHarvestTask(id string, fn func(*model.HarvestTask)) (model.HarvestTask, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	t, ok := db.data.HarvestTasks[id]
	if !ok {
		return model.HarvestTask{}, ErrNotFound
	}
	fn(t)
	t.UpdatedAt = time.Now()
	db.markDirty()
	return t.Clone(), nil
}

// DeleteHarvestTask 删除任务，连带删除它的运行记录。
func (db *DB) DeleteHarvestTask(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.data.HarvestTasks[id]; !ok {
		return ErrNotFound
	}
	delete(db.data.HarvestTasks, id)
	for rid, run := range db.data.HarvestRuns {
		if run.TaskID == id {
			delete(db.data.HarvestRuns, rid)
		}
	}
	db.markDirty()
	return nil
}

// DeleteAllHarvestTasks 清空全部采集任务，连带删除它们的运行记录（返回两个计数）。
func (db *DB) DeleteAllHarvestTasks() (int, int) {
	db.mu.Lock()
	defer db.mu.Unlock()
	tasks := len(db.data.HarvestTasks)
	runs := 0
	for rid, run := range db.data.HarvestRuns {
		if _, ok := db.data.HarvestTasks[run.TaskID]; ok {
			delete(db.data.HarvestRuns, rid)
			runs++
		}
	}
	db.data.HarvestTasks = map[string]*model.HarvestTask{}
	if tasks > 0 || runs > 0 {
		db.markDirty()
	}
	return tasks, runs
}

/* -------------------------------------------------------------- 运行 CRUD */ // CreateHarvestRun 写入一次采集运行（状态 queued）。
func (db *DB) CreateHarvestRun(r model.HarvestRun) (model.HarvestRun, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if r.ID == "" {
		r.ID = randID("hrv_")
	}
	r.Endpoints = []model.HarvestEndpointResult{}
	r.Logs = []model.LogEntry{}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}
	db.data.HarvestRuns[r.ID] = &r
	db.trimHarvestRunsLocked()
	db.markDirty()
	return r.Clone(), nil
}

// MutateHarvestRun 在锁内修改运行记录。
func (db *DB) MutateHarvestRun(id string, fn func(*model.HarvestRun)) (model.HarvestRun, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	r, ok := db.data.HarvestRuns[id]
	if !ok {
		return model.HarvestRun{}, ErrNotFound
	}
	fn(r)
	db.markDirty()
	return r.Clone(), nil
}

// GetHarvestRun 取运行全量深拷贝。
func (db *DB) GetHarvestRun(id string) (model.HarvestRun, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	r, ok := db.data.HarvestRuns[id]
	if !ok {
		return model.HarvestRun{}, ErrNotFound
	}
	return r.Clone(), nil
}

// ListHarvestRuns 按开始时间倒序列出运行摘要。
func (db *DB) ListHarvestRuns(taskID string, limit int) []model.HarvestRun {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]model.HarvestRun, 0, len(db.data.HarvestRuns))
	for _, r := range db.data.HarvestRuns {
		if taskID != "" && r.TaskID != taskID {
			continue
		}
		out = append(out, r.Summary())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// trimHarvestRunsLocked 只保留最近 keepHarvestRuns 条。
func (db *DB) trimHarvestRunsLocked() {
	if len(db.data.HarvestRuns) <= keepHarvestRuns {
		return
	}
	type pair struct {
		id string
		t  time.Time
	}
	list := make([]pair, 0, len(db.data.HarvestRuns))
	for id, r := range db.data.HarvestRuns {
		list = append(list, pair{id, r.StartedAt})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].t.Before(list[j].t) })
	for i := 0; i < len(list)-keepHarvestRuns; i++ {
		delete(db.data.HarvestRuns, list[i].id)
	}
}

// DeleteHarvestRuns 批量删除运行记录。
func (db *DB) DeleteHarvestRuns(ids []string) int {
	db.mu.Lock()
	defer db.mu.Unlock()
	n := 0
	for _, id := range ids {
		if _, ok := db.data.HarvestRuns[id]; ok {
			delete(db.data.HarvestRuns, id)
			n++
		}
	}
	if n > 0 {
		db.markDirty()
	}
	return n
}

// DeleteAllHarvestRuns 清空运行记录（任务保留）。
func (db *DB) DeleteAllHarvestRuns() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	n := len(db.data.HarvestRuns)
	if n == 0 {
		return 0
	}
	db.data.HarvestRuns = map[string]*model.HarvestRun{}
	db.markDirty()
	return n
}

// HarvestCounts 任务数与运行记录数（健康检查用）。
func (db *DB) HarvestCounts() (int, int) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return len(db.data.HarvestTasks), len(db.data.HarvestRuns)
}

// HarvestTasksForSchedule 返回调度需要的任务快照（只读副本）。
func (db *DB) HarvestTasksForSchedule() []model.HarvestTask {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]model.HarvestTask, 0, len(db.data.HarvestTasks))
	for _, t := range db.data.HarvestTasks {
		out = append(out, t.Summary())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NextRunAt.Before(out[j].NextRunAt) })
	return out
}
