// Package store 提供进程内数据存储：录制、运行、元素库、设置。
//
// 设计取舍：本平台的数据量很小（录制几十条、运行数百条），用单文件 JSON
// 持久化可以做到零外部依赖、零迁移成本、可读可备份；写入采用「脏标记 + 防抖
// 落盘 + 临时文件改原子替换」，崩溃时最多丢 500ms 内的日志行。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"uitest-server/internal/keyname"
	"uitest-server/internal/model"
)

// ErrNotFound 资源不存在。
var ErrNotFound = errors.New("资源不存在")

// Data 落盘结构。
type Data struct {
	Recordings map[string]*model.Recording `json:"recordings"`
	Runs       map[string]*model.Run       `json:"runs"`
	Elements   map[string]*model.Element   `json:"elements"`
	Settings   model.Settings              `json:"settings"`
}

// DB 存储实例，所有读写方法都是并发安全的。
type DB struct {
	mu       sync.RWMutex
	dir      string
	path     string
	keepRuns int
	data     Data
	dirty    bool
	stopCh   chan struct{}
	stopOnce sync.Once
}

// Open 打开（或初始化）数据目录。
func Open(dir string, keepRuns int) (*DB, error) {
	if keepRuns <= 0 {
		keepRuns = 200
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db := &DB{
		dir:      dir,
		path:     filepath.Join(dir, "uitest.json"),
		keepRuns: keepRuns,
		stopCh:   make(chan struct{}),
		data: Data{
			Recordings: map[string]*model.Recording{},
			Runs:       map[string]*model.Run{},
			Elements:   map[string]*model.Element{},
			Settings:   model.DefaultSettings(),
		},
	}
	// 启动时先给现有数据做一份滚动快照，避免误删/误改无法回退
	db.snapshotBackup()

	if raw, err := os.ReadFile(db.path); err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &db.data); err != nil {
			return nil, fmt.Errorf("数据文件 %s 解析失败: %w", db.path, err)
		}
	}
	// 兜底：老文件可能缺字段
	if db.data.Recordings == nil {
		db.data.Recordings = map[string]*model.Recording{}
	}
	if db.data.Runs == nil {
		db.data.Runs = map[string]*model.Run{}
	}
	if db.data.Elements == nil {
		db.data.Elements = map[string]*model.Element{}
	}
	if db.data.Settings.Browser.StepDelayMS == 0 {
		db.data.Settings.Browser.StepDelayMS = 260
	}
	if db.data.Settings.Browser.ActionTimeoutMS == 0 {
		db.data.Settings.Browser.ActionTimeoutMS = 15000
	}
	if db.data.Settings.AI.TimeoutSec == 0 {
		db.data.Settings.AI.TimeoutSec = 60
	}
	go db.flusher()
	return db, nil
}

// Dir 返回数据目录绝对路径。
func (db *DB) Dir() string { return db.dir }

// Close 停止后台落盘并同步写盘。
func (db *DB) Close() error {
	db.stopOnce.Do(func() { close(db.stopCh) })
	return db.saveNow()
}

func (db *DB) flusher() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-db.stopCh:
			return
		case <-t.C:
			db.mu.Lock()
			need := db.dirty
			db.mu.Unlock()
			if need {
				_ = db.saveNow()
			}
		}
	}
}

func (db *DB) markDirty() { db.dirty = true }

func (db *DB) saveNow() error {
	db.mu.Lock()
	raw, err := json.MarshalIndent(db.data, "", "  ")
	db.dirty = false
	db.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := db.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, db.path)
}

/* ------------------------------------------------------------------ 工具 */

const idAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

func randID(prefix string) string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return prefix + hex.EncodeToString([]byte(time.Now().Format("150405")))
	}
	out := make([]byte, 6)
	for i, b := range buf {
		out[i] = idAlphabet[int(b)%len(idAlphabet)]
	}
	return prefix + string(out)
}

/* ------------------------------------------------------------- 录制 CRUD */

// ListRecordings 返回录制摘要列表，q 对名称/说明/标签做子串匹配。
func (db *DB) ListRecordings(q, tag string) []model.Recording {
	db.mu.RLock()
	defer db.mu.RUnlock()
	q = strings.ToLower(strings.TrimSpace(q))
	out := make([]model.Recording, 0, len(db.data.Recordings))
	for _, r := range db.data.Recordings {
		if q != "" {
			hay := strings.ToLower(r.Name + " " + r.Description + " " + strings.Join(r.Tags, " "))
			if !strings.Contains(hay, q) {
				continue
			}
		}
		if tag != "" && !contains(r.Tags, tag) {
			continue
		}
		out = append(out, r.Summary())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// GetRecording 取录制（含步骤）的深拷贝。
func (db *DB) GetRecording(id string) (model.Recording, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	r, ok := db.data.Recordings[id]
	if !ok {
		return model.Recording{}, ErrNotFound
	}
	c := r.Clone()
	if c.StepCount == 0 {
		c.StepCount = len(c.Steps)
	}
	return c, nil
}

// CreateRecording 新建录制。
func (db *DB) CreateRecording(name, desc, startURL string, tags []string) (model.Recording, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return model.Recording{}, errors.New("录制名称不能为空")
	}
	now := time.Now()
	r := &model.Recording{
		ID:          randID("rec_"),
		Name:        name,
		Description: desc,
		StartURL:    startURL,
		Tags:        tags,
		Status:      "draft",
		CreatedAt:   now,
		UpdatedAt:   now,
		Steps:       []model.Step{},
	}
	if r.Tags == nil {
		r.Tags = []string{}
	}
	db.mu.Lock()
	db.data.Recordings[r.ID] = r
	db.markDirty()
	out := r.Clone()
	db.mu.Unlock()
	return out, nil
}

// RecordingPatch 只更新非 nil 字段。
type RecordingPatch struct {
	Name        *string   `json:"name"`
	Description *string   `json:"description"`
	StartURL    *string   `json:"start_url"`
	Status      *string   `json:"status"`
	Tags        *[]string `json:"tags"`
}

// UpdateRecording 局部更新录制元信息。
func (db *DB) UpdateRecording(id string, p RecordingPatch) (model.Recording, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	r, ok := db.data.Recordings[id]
	if !ok {
		return model.Recording{}, ErrNotFound
	}
	if p.Name != nil && strings.TrimSpace(*p.Name) != "" {
		r.Name = strings.TrimSpace(*p.Name)
	}
	if p.Description != nil {
		r.Description = *p.Description
	}
	if p.StartURL != nil {
		r.StartURL = *p.StartURL
	}
	if p.Status != nil && (*p.Status == "draft" || *p.Status == "ready") {
		r.Status = *p.Status
	}
	if p.Tags != nil {
		r.Tags = append([]string{}, (*p.Tags)...)
	}
	r.UpdatedAt = time.Now()
	db.markDirty()
	return r.Clone(), nil
}

// DeleteRecording 删除录制。
func (db *DB) DeleteRecording(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.data.Recordings[id]; !ok {
		return ErrNotFound
	}
	delete(db.data.Recordings, id)
	// 录制没了，它的运行记录也留着没意义
	victims := db.dropRunsLocked(func(run *model.Run) bool { return run.RecordingID == id })
	db.markDirty()
	db.removeRunFiles(victims)
	return nil
}

// AddStep 追加步骤（index 自动排到末尾）。
func (db *DB) AddStep(recordingID string, s model.Step) (model.Step, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	r, ok := db.data.Recordings[recordingID]
	if !ok {
		return model.Step{}, ErrNotFound
	}
	if s.ID == "" {
		s.ID = randID("stp_")
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	if s.Annotation == "" {
		s.Annotation = defaultAnnotation(s)
	}
	// 中文标签自动转英文字段名（设置里可关）：否则 data_key 会退化成一堆 field_25
	if db.data.Settings.Assist.AutoDataKey {
		s.DataKey = improveDataKey(s)
	}
	s.RecordingID = recordingID
	s.Index = len(r.Steps) + 1
	r.Steps = append(r.Steps, s)
	r.StepCount = len(r.Steps)
	r.UpdatedAt = time.Now()
	db.markDirty()
	return s, nil
}

func defaultAnnotation(s model.Step) string {
	name := s.Target.Describe()
	switch s.Action {
	case "click":
		return "点击【" + name + "】"
	case "input":
		return "在【" + name + "】输入内容"
	case "select":
		return "在【" + name + "】选择「" + s.Value + "」"
	case "check":
		return "勾选【" + name + "】"
	case "uncheck":
		return "取消勾选【" + name + "】"
	case "upload":
		return "上传文件到【" + name + "】"
	case "navigate":
		return "打开页面 " + s.Value
	case "key":
		return "按键 " + s.Value
	case "hover":
		return "悬停【" + name + "】"
	case "wait":
		return "等待 " + s.Value
	case "assert":
		return "断言 " + s.Value
	default:
		return s.Action + " " + name
	}
}

// StepPatch 步骤局部更新。
type StepPatch struct {
	Action     *string        `json:"action"`
	Annotation *string        `json:"annotation"`
	Value      *string        `json:"value"`
	DataKey    *string        `json:"data_key"`
	Expect     *string        `json:"expect"`
	Note       *string        `json:"note"`
	WaitMS     *int           `json:"wait_ms"`
	Optional   *bool          `json:"optional"`
	Locator    *model.Locator `json:"locator"`
	Target     *model.Target  `json:"target"`
}

// UpdateStep 局部更新某一步骤。
func (db *DB) UpdateStep(recordingID, stepID string, p StepPatch) (model.Step, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	r, ok := db.data.Recordings[recordingID]
	if !ok {
		return model.Step{}, ErrNotFound
	}
	for i := range r.Steps {
		s := &r.Steps[i]
		if s.ID != stepID {
			continue
		}
		if p.Action != nil && *p.Action != "" {
			s.Action = *p.Action
		}
		if p.Annotation != nil {
			s.Annotation = *p.Annotation
		}
		if p.Value != nil {
			s.Value = *p.Value
		}
		if p.DataKey != nil {
			s.DataKey = *p.DataKey
		}
		if p.Expect != nil {
			s.Expect = *p.Expect
		}
		if p.Note != nil {
			s.Note = *p.Note
		}
		if p.WaitMS != nil {
			s.WaitMS = *p.WaitMS
		}
		if p.Optional != nil {
			s.Optional = *p.Optional
		}
		if p.Locator != nil {
			s.Locator = *p.Locator
		}
		if p.Target != nil {
			s.Target = *p.Target
		}
		r.UpdatedAt = time.Now()
		db.markDirty()
		return *s, nil
	}
	return model.Step{}, ErrNotFound
}

// DeleteStep 删除步骤并重排序号。
func (db *DB) DeleteStep(recordingID, stepID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	r, ok := db.data.Recordings[recordingID]
	if !ok {
		return ErrNotFound
	}
	out := r.Steps[:0]
	found := false
	for _, s := range r.Steps {
		if s.ID == stepID {
			found = true
			continue
		}
		out = append(out, s)
	}
	if !found {
		return ErrNotFound
	}
	r.Steps = out
	reindex(r)
	r.UpdatedAt = time.Now()
	db.markDirty()
	return nil
}

func reindex(r *model.Recording) {
	for i := range r.Steps {
		r.Steps[i].Index = i + 1
		r.Steps[i].RecordingID = r.ID
	}
	r.StepCount = len(r.Steps)
}

// Reorder 按给定 step_ids 顺序重排。
func (db *DB) Reorder(recordingID string, stepIDs []string) (model.Recording, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	r, ok := db.data.Recordings[recordingID]
	if !ok {
		return model.Recording{}, ErrNotFound
	}
	idx := map[string]model.Step{}
	for _, s := range r.Steps {
		idx[s.ID] = s
	}
	if len(stepIDs) != len(r.Steps) {
		return model.Recording{}, errors.New("step_ids 数量与现有步骤不一致")
	}
	next := make([]model.Step, 0, len(stepIDs))
	for _, id := range stepIDs {
		s, ok := idx[id]
		if !ok {
			return model.Recording{}, errors.New("step_ids 含有未知步骤: " + id)
		}
		next = append(next, s)
	}
	r.Steps = next
	reindex(r)
	r.UpdatedAt = time.Now()
	db.markDirty()
	return r.Clone(), nil
}

// DuplicateRecording 复制录制（步骤 ID 重新生成）。
func (db *DB) DuplicateRecording(id, newName string) (model.Recording, error) {
	src, err := db.GetRecording(id)
	if err != nil {
		return model.Recording{}, err
	}
	if strings.TrimSpace(newName) == "" {
		newName = src.Name + " 副本"
	}
	dup, err := db.CreateRecording(newName, src.Description, src.StartURL, src.Tags)
	if err != nil {
		return model.Recording{}, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	target := db.data.Recordings[dup.ID]
	target.Status = src.Status
	for _, s := range src.Steps {
		s.ID = randID("stp_")
		s.RecordingID = dup.ID
		s.CreatedAt = time.Now()
		target.Steps = append(target.Steps, s)
	}
	reindex(target)
	target.UpdatedAt = time.Now()
	db.markDirty()
	return target.Clone(), nil
}

// ReplaceRecording 用导入的录制整体覆盖/新建。
func (db *DB) ImportRecording(in model.Recording) (model.Recording, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return model.Recording{}, errors.New("导入数据缺少 name")
	}
	now := time.Now()
	r := &model.Recording{
		ID:          randID("rec_"),
		Name:        name,
		Description: in.Description,
		StartURL:    in.StartURL,
		Tags:        in.Tags,
		Status:      in.Status,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if r.Status == "" {
		r.Status = "draft"
	}
	if r.Tags == nil {
		r.Tags = []string{}
	}
	for _, s := range in.Steps {
		s.ID = randID("stp_")
		s.RecordingID = r.ID
		s.CreatedAt = now
		if s.Annotation == "" {
			s.Annotation = defaultAnnotation(s)
		}
		r.Steps = append(r.Steps, s)
	}
	reindex(r)
	db.mu.Lock()
	db.data.Recordings[r.ID] = r
	db.markDirty()
	out := r.Clone()
	db.mu.Unlock()
	return out, nil
}

/* ------------------------------------------------------------- 运行 CRUD */

// CreateRun 写入一次运行（状态 queued）。
func (db *DB) CreateRun(r model.Run) (model.Run, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if r.ID == "" {
		r.ID = randID("run_")
	}
	if r.Dataset == nil {
		r.Dataset = map[string]string{}
	}
	r.StepResults = []model.StepResult{}
	r.Logs = []model.LogEntry{}
	db.data.Runs[r.ID] = &r
	db.trimRunsLocked()
	db.markDirty()
	return r.Clone(), nil
}

// MutateRun 在锁内修改运行记录并落盘。
func (db *DB) MutateRun(id string, fn func(*model.Run)) (model.Run, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	r, ok := db.data.Runs[id]
	if !ok {
		return model.Run{}, ErrNotFound
	}
	fn(r)
	db.markDirty()
	return r.Clone(), nil
}

// GetRun 取运行全量深拷贝。
func (db *DB) GetRun(id string) (model.Run, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	r, ok := db.data.Runs[id]
	if !ok {
		return model.Run{}, ErrNotFound
	}
	return r.Clone(), nil
}

// ListRuns 按开始时间倒序列出运行摘要。
func (db *DB) ListRuns(recordingID string, limit int) []model.Run {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]model.Run, 0, len(db.data.Runs))
	for _, r := range db.data.Runs {
		if recordingID != "" && r.RecordingID != recordingID {
			continue
		}
		out = append(out, r.Summary())
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].StartedAt, out[j].StartedAt
		if ti.IsZero() {
			ti = out[i].FinishedAt
		}
		if tj.IsZero() {
			tj = out[j].FinishedAt
		}
		return ti.After(tj)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// trimRunsLocked 只保留最近 keepRuns 条运行。
func (db *DB) trimRunsLocked() {
	if len(db.data.Runs) <= db.keepRuns {
		return
	}
	type pair struct {
		id string
		t  time.Time
	}
	list := make([]pair, 0, len(db.data.Runs))
	for id, r := range db.data.Runs {
		t := r.StartedAt
		if t.IsZero() {
			t = r.FinishedAt
		}
		list = append(list, pair{id, t})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].t.Before(list[j].t) })
	for i := 0; i < len(list)-db.keepRuns; i++ {
		delete(db.data.Runs, list[i].id)
	}
}

/* --------------------------------------------------------- 元素库 / 设置 */

// BindElement 登记元素绑定，返回最终 key。
func (db *DB) BindElement(e model.Element) model.Element {
	db.mu.Lock()
	defer db.mu.Unlock()
	if e.Key == "" {
		e.Key = randID("ut_")
	}
	if old, ok := db.data.Elements[e.Key]; ok {
		e.Hits = old.Hits + 1
		e.Broken = false
		if e.Label == "" {
			e.Label = old.Label
		}
		if e.Descriptor == "" {
			e.Descriptor = old.Descriptor
		}
	} else {
		e.Hits = 1
	}
	e.LastSeen = time.Now()
	cp := e
	db.data.Elements[e.Key] = &cp
	db.markDirty()
	return e
}

// ListElements 列出元素库。
func (db *DB) ListElements() []model.Element {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]model.Element, 0, len(db.data.Elements))
	for _, e := range db.data.Elements {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// MarkElementBroken 标记/恢复元素失效状态。
func (db *DB) MarkElementBroken(key string, broken bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if e, ok := db.data.Elements[key]; ok {
		e.Broken = broken
		db.markDirty()
	}
}

// DeleteElement 删除元素库条目。
func (db *DB) DeleteElement(key string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.data.Elements[key]; !ok {
		return ErrNotFound
	}
	delete(db.data.Elements, key)
	db.markDirty()
	return nil
}

// Settings 取设置副本。
func (db *DB) Settings() model.Settings {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.data.Settings
}

// SaveSettings 覆盖设置。
func (db *DB) SaveSettings(s model.Settings) model.Settings {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.data.Settings = s
	db.markDirty()
	return s
}

// Counts 返回统计（健康检查用）。
func (db *DB) Counts() (int, int) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return len(db.data.Recordings), len(db.data.Runs)
}

// snapshotBackup 把当前数据文件复制到 <dir>/backups/uitest-<时间>.json，最多保留 5 份。
// 目的：录制与运行记录是用户手录出来的，误删代价很高，留几份快照能救命。
func (db *DB) snapshotBackup() {
	raw, err := os.ReadFile(db.path)
	if err != nil || len(raw) == 0 {
		return
	}
	dir := filepath.Join(db.dir, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	name := filepath.Join(dir, "uitest-"+time.Now().Format("20060102-150405")+".json")
	if err := os.WriteFile(name, raw, 0o644); err != nil {
		return
	}
	// 只保留最近 5 份
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "uitest-") && strings.HasSuffix(e.Name(), ".json") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	for len(files) > 5 {
		_ = os.Remove(files[0])
		files = files[1:]
	}
}

// placeholderKeyRe 匹配插件在"纯中文标签推导不出来"时给出的占位键：field_25
var placeholderKeyRe = regexp.MustCompile(`^field_\d+$`)

// improveDataKey 用业务词典把中文标签转成英文字段名。
// 只处理空值和 field_N 这类占位键，不覆盖用户/插件已经给出的可读键。
func improveDataKey(s model.Step) string {
	cur := strings.TrimSpace(s.DataKey)
	if cur != "" && !placeholderKeyRe.MatchString(cur) {
		return cur
	}
	// 只有"填数据"的动作才需要字段名；按钮/断言不该有 data_key（老数据里遗留的也清掉）
	switch s.Action {
	case "input", "select", "upload", "check", "uncheck":
	default:
		return ""
	}
	derived := keyname.DeriveKey(s.Target.Label, s.Target.Placeholder, s.Target.Name)
	if derived == "" {
		return cur
	}
	return dedupeTokens(derived)
}

// dedupeTokens 去掉 snake_case 里连续重复的词：product_product → product
func dedupeTokens(s string) string {
	parts := strings.Split(s, "_")
	out := parts[:0]
	for _, p := range parts {
		if p == "" {
			continue
		}
		if len(out) > 0 && out[len(out)-1] == p {
			continue
		}
		out = append(out, p)
	}
	res := strings.Join(out, "_")
	if strings.Trim(res, "0123456789_") == "" {
		return ""
	}
	return res
}

// ImproveDataKeys 批量修正某条录制里所有步骤的 data_key（设置里可关）。
// 返回被改动的步骤数。
func (db *DB) ImproveDataKeys(recordingID string) int {
	db.mu.Lock()
	defer db.mu.Unlock()
	r, ok := db.data.Recordings[recordingID]
	if !ok {
		return 0
	}
	changed := 0
	for i := range r.Steps {
		old := r.Steps[i].DataKey
		next := improveDataKey(r.Steps[i])
		if next != old {
			r.Steps[i].DataKey = next
			changed++
		}
	}
	if changed > 0 {
		r.UpdatedAt = time.Now()
		db.markDirty()
	}
	return changed
}

/* --------------------------------------------------------------- 备份恢复 */

// BackupInfo 一份数据快照的摘要。
type BackupInfo struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	CreatedAt  time.Time `json:"created_at"`
	Recordings int       `json:"recordings"`
	Runs       int       `json:"runs"`
}

// backupDir 备份目录。
func (db *DB) backupDir() string { return filepath.Join(db.dir, "backups") }

// ListBackups 列出可用快照（按时间倒序，最多 20 份）。
func (db *DB) ListBackups() []BackupInfo {
	entries, err := os.ReadDir(db.backupDir())
	if err != nil {
		return nil
	}
	out := make([]BackupInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "uitest-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		bi := BackupInfo{Name: e.Name(), Size: info.Size(), CreatedAt: info.ModTime()}
		// 轻量统计：只读一次计数，方便用户判断该恢复哪份
		if raw, rerr := os.ReadFile(filepath.Join(db.backupDir(), e.Name())); rerr == nil {
			var snapshot struct {
				Recordings map[string]json.RawMessage `json:"recordings"`
				Runs       map[string]json.RawMessage `json:"runs"`
			}
			if json.Unmarshal(raw, &snapshot) == nil {
				bi.Recordings = len(snapshot.Recordings)
				bi.Runs = len(snapshot.Runs)
			}
		}
		out = append(out, bi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// RestoreBackup 用备份覆盖当前数据。
//
// 约定：恢复前先把"当前状态"再存一份快照，避免恢复错了没法回头。
// name 只允许 data/backups 下的文件，防止路径穿越。
func (db *DB) RestoreBackup(name string) error {
	name = filepath.Base(strings.TrimSpace(name))
	if !strings.HasPrefix(name, "uitest-") || !strings.HasSuffix(name, ".json") {
		return errors.New("备份文件名不合法")
	}
	raw, err := os.ReadFile(filepath.Join(db.backupDir(), name))
	if err != nil {
		return fmt.Errorf("读取备份失败：%w", err)
	}
	var snapshot Data
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return fmt.Errorf("备份文件不是合法数据：%w", err)
	}
	if snapshot.Recordings == nil {
		snapshot.Recordings = map[string]*model.Recording{}
	}
	if snapshot.Runs == nil {
		snapshot.Runs = map[string]*model.Run{}
	}
	if snapshot.Elements == nil {
		snapshot.Elements = map[string]*model.Element{}
	}
	if snapshot.Settings.Browser.StepDelayMS == 0 {
		snapshot.Settings = model.DefaultSettings()
	}

	// 先把当前数据快照一份，再覆盖
	db.snapshotBackup()

	db.mu.Lock()
	db.data = snapshot
	db.dirty = true
	db.mu.Unlock()
	return db.saveNow()
}

/* ------------------------------------------------------- 批量删除 / 清空 */

// dropRunsLocked 删除满足条件的运行记录，返回被删掉的 runID（调用方负责删文件）。
// 注意：必须在已持有 db.mu 的情况下调用。
func (db *DB) dropRunsLocked(match func(*model.Run) bool) []string {
	var victims []string
	for id, run := range db.data.Runs {
		if match(run) {
			victims = append(victims, id)
			delete(db.data.Runs, id)
		}
	}
	return victims
}

// removeRunFiles 异步清理运行截图目录（失败不影响数据一致性）。
func (db *DB) removeRunFiles(runIDs []string) {
	for _, id := range runIDs {
		_ = os.RemoveAll(filepath.Join(db.dir, "runs", id))
	}
}

// DeleteRecordings 批量删除录制（连带它们的运行记录与截图）。返回实际删除条数。
func (db *DB) DeleteRecordings(ids []string) int {
	db.mu.Lock()
	defer db.mu.Unlock()
	deleted := 0
	gone := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := db.data.Recordings[id]; ok {
			delete(db.data.Recordings, id)
			gone[id] = true
			deleted++
		}
	}
	if deleted == 0 {
		return 0
	}
	victims := db.dropRunsLocked(func(run *model.Run) bool { return gone[run.RecordingID] })
	db.markDirty()
	db.removeRunFiles(victims)
	return deleted
}

// DeleteAllRecordings 清空所有录制（连带全部运行记录与截图）。返回删除的录制条数。
func (db *DB) DeleteAllRecordings() int {
	db.mu.Lock()
	n := len(db.data.Recordings)
	if n == 0 {
		db.mu.Unlock()
		return 0
	}
	db.data.Recordings = map[string]*model.Recording{}
	victims := db.dropRunsLocked(func(*model.Run) bool { return true })
	db.markDirty()
	db.mu.Unlock()
	db.removeRunFiles(victims)
	return n
}

// DeleteRuns 批量删除运行记录（不影响录制）。返回实际删除条数。
func (db *DB) DeleteRuns(ids []string) int {
	db.mu.Lock()
	defer db.mu.Unlock()
	deleted := 0
	var victims []string
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := db.data.Runs[id]; ok {
			delete(db.data.Runs, id)
			victims = append(victims, id)
			deleted++
		}
	}
	if deleted > 0 {
		db.markDirty()
	}
	db.removeRunFiles(victims)
	return deleted
}

// DeleteAllRuns 清空所有运行记录（保留录制）。
func (db *DB) DeleteAllRuns() int {
	db.mu.Lock()
	n := len(db.data.Runs)
	if n == 0 {
		db.mu.Unlock()
		return 0
	}
	victims := db.dropRunsLocked(func(*model.Run) bool { return true })
	db.markDirty()
	db.mu.Unlock()
	db.removeRunFiles(victims)
	return n
}

// DeleteElements 批量删除元素绑定。返回实际删除条数。
func (db *DB) DeleteElements(keys []string) int {
	db.mu.Lock()
	defer db.mu.Unlock()
	deleted := 0
	for _, k := range keys {
		if _, ok := db.data.Elements[k]; ok {
			delete(db.data.Elements, k)
			deleted++
		}
	}
	if deleted > 0 {
		db.markDirty()
	}
	return deleted
}

// DeleteAllElements 清空元素绑定。
func (db *DB) DeleteAllElements() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	n := len(db.data.Elements)
	if n == 0 {
		return 0
	}
	db.data.Elements = map[string]*model.Element{}
	db.markDirty()
	return n
}
