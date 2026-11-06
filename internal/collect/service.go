package collect

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"uitest-server/internal/browser"
	"uitest-server/internal/bus"
	"uitest-server/internal/model"
	"uitest-server/internal/store"
)

// 本文件把录制器、执行器、调度器装成一套 Service，并给 API 层一个稳定入口。

// maxCaptures 同时保留的录制会话数（每个会话占一个标签页，别开太多）。
const maxCaptures = 5

// Recorder 管理"接口录制"会话（一个会话 = 一个内置浏览器标签页）。
type Recorder struct {
	br *browser.Manager

	mu       sync.Mutex
	sessions map[string]*CaptureSession
	order    []string
}

// CaptureSession 一次录制会话。
//
// 两种来源：
//   - browser：服务端内置浏览器，用 CDP 旁路录制（Cap/Page 非空）；
//   - extension：浏览器插件在你自己的浏览器里录，然后把条目 POST 上来（Cap/Page 为空）。
type CaptureSession struct {
	ID        string
	Source    string // browser | extension
	Page      *browser.Page
	Cap       *browser.NetCapture
	PageURL   string
	StartedAt time.Time
	Stopped   bool
	Note      string
	UserAgent string
	// Entries 已录到的条目。browser 会话停止后由 Cap 快照填充；extension 会话就是全部数据。
	Entries []model.CapturedRequest
	// Deleted 被用户删掉的请求 id（内置浏览器会话没法从 CDP 里删，只能在这里屏蔽）
	Deleted map[string]bool
	// LastAt 最近一次收到数据的时间（插件会话长时间没动静就自动判为"已停止"，避免僵尸会话）
	LastAt time.Time
	// Types 这次录制要抓的资源类型（XHR / Fetch / Document …），列表和详情都会回显，便于确认设置生效
	Types []string
}

// CaptureInfo 会话摘要（列表接口返回）。
type CaptureInfo struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"` // browser | extension
	PageURL   string    `json:"page_url"`
	StartedAt time.Time `json:"started_at"`
	Count     int       `json:"count"`
	Stopped   bool      `json:"stopped"`
	Note      string    `json:"note,omitempty"`
	UserAgent string    `json:"user_agent,omitempty"`
	Types     []string  `json:"types,omitempty"`
}

// normalizeTypes 规范化资源类型（XHR / Fetch / Document …）：去空、去重、大小写统一。
func normalizeTypes(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, t := range in {
		v := strings.TrimSpace(t)
		if v == "" {
			continue
		}
		switch strings.ToLower(v) {
		case "xhr":
			v = "XHR"
		case "fetch":
			v = "Fetch"
		case "document", "doc", "documentbody":
			v = "Document"
		default:
			if len(v) > 1 {
				v = strings.ToUpper(v[:1]) + strings.ToLower(v[1:])
			}
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		out = []string{"XHR", "Fetch"}
	}
	return out
}

// maxCaptureEntries 单个会话最多保留多少条（防止长时间录制把内存吃满）。
const maxCaptureEntries = 5000

// NewRecorder 创建录制管理器。
func NewRecorder(br *browser.Manager) *Recorder {
	return &Recorder{br: br, sessions: map[string]*CaptureSession{}}
}

// Start 打开一个标签页并开始录制它的网络请求。
func (r *Recorder) Start(ctx context.Context, startURL string, types []string, captureBodies bool, note string) (CaptureInfo, error) {
	types = normalizeTypes(types)
	page, err := r.br.OpenPage(ctx, "about:blank")
	if err != nil {
		return CaptureInfo{}, fmt.Errorf("启动内置浏览器失败：%w", err)
	}
	cap, err := page.StartNetCapture(browser.NetCaptureOptions{
		Types:         types,
		CaptureBodies: captureBodies,
		PageURL:       startURL,
	})
	if err != nil {
		page.Close(context.Background())
		return CaptureInfo{}, fmt.Errorf("开启网络录制失败：%w", err)
	}
	sess := &CaptureSession{
		ID:        newID("cap_"),
		Source:    "browser",
		Page:      page,
		Cap:       cap,
		PageURL:   startURL,
		StartedAt: time.Now(),
		Note:      note,
		Deleted:   map[string]bool{},
		Types:     types,
	}
	log.Printf("[collect] 开始录制 %s：types=%v url=%s capture_bodies=%v", sess.ID, types, startURL, captureBodies)
	// 记下这台浏览器的 UA：定时采集默认复用它（不伪造、不轮换）
	uaCtx, uaCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if raw, e := page.Eval(uaCtx, "navigator.userAgent"); e == nil {
		sess.UserAgent = trimQuotes(string(raw))
	}
	uaCancel()
	if startURL != "" {
		go func() {
			navCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			_ = page.Navigate(navCtx, startURL)
		}()
	}

	r.mu.Lock()
	r.sessions[sess.ID] = sess
	r.order = append(r.order, sess.ID)
	// 超出上限就把最老的（已停止的）会话关掉
	for len(r.order) > maxCaptures {
		oldest := r.order[0]
		r.order = r.order[1:]
		if old, ok := r.sessions[oldest]; ok {
			if old.Cap != nil {
				old.Entries = old.Cap.Stop()
			}
			if old.Page != nil {
				old.Page.Close(context.Background())
			}
			delete(r.sessions, oldest)
		}
	}
	info := CaptureInfo{
		ID: sess.ID, Source: sess.Source, PageURL: sess.PageURL, StartedAt: sess.StartedAt,
		Note: sess.Note, UserAgent: sess.UserAgent, Types: sess.Types,
	}
	r.mu.Unlock()
	return info, nil
}

// StartExternal 登记一个"插件录制"会话。不打开浏览器、不占标签页，
// 由浏览器插件把录到的请求 POST 上来（POST /api/collect/captures/{id}/requests）。
func (r *Recorder) StartExternal(pageURL, note, userAgent string) (CaptureInfo, error) {
	sess := &CaptureSession{
		ID:        newID("cap_"),
		Source:    "extension",
		PageURL:   pageURL,
		StartedAt: time.Now(),
		LastAt:    time.Now(),
		Note:      note,
		UserAgent: userAgent,
		Entries:   []model.CapturedRequest{},
		Deleted:   map[string]bool{},
		// 插件只会上报接口请求（XHR/Fetch），不会有页面文档
		Types: []string{"XHR", "Fetch"},
	}

	r.mu.Lock()
	r.sessions[sess.ID] = sess
	r.order = append(r.order, sess.ID)
	// 超出上限：淘汰最老的一个（插件会话直接丢弃；内置浏览器会话顺手关掉标签页）
	for len(r.order) > maxCaptures {
		oldest := r.order[0]
		r.order = r.order[1:]
		if old, ok := r.sessions[oldest]; ok {
			delete(r.sessions, oldest)
			if old.Cap != nil {
				old.Cap.Stop()
			}
			if old.Page != nil {
				go old.Page.Close(context.Background())
			}
		}
	}
	info := CaptureInfo{
		ID: sess.ID, Source: sess.Source, PageURL: sess.PageURL,
		StartedAt: sess.StartedAt, Note: sess.Note, UserAgent: sess.UserAgent,
	}
	r.mu.Unlock()
	return info, nil
}

// AddRequests 由插件把录到的请求写入会话（批量，幂等：同 id 不会重复写入）。
func (r *Recorder) AddRequests(id string, items []model.CapturedRequest) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return 0, fmt.Errorf("录制会话不存在或已被清理（服务重启后会新建）")
	}
	if s.Cap != nil {
		return 0, fmt.Errorf("这是内置浏览器录制的会话，不接收外部上报")
	}
	seen := make(map[string]bool, len(s.Entries))
	for _, it := range s.Entries {
		seen[it.ID] = true
	}
	added := 0
	for _, it := range items {
		it.URL = strings.TrimSpace(it.URL)
		if it.URL == "" || !strings.HasPrefix(strings.ToLower(it.URL), "http") {
			continue
		}
		if it.ID == "" {
			it.ID = newID("req_")
		}
		if seen[it.ID] || s.Deleted[it.ID] {
			continue // 幂等 + 尊重用户已删除的条目
		}
		if len(s.Entries) >= maxCaptureEntries {
			break
		}
		it.Method = strings.ToUpper(strings.TrimSpace(it.Method))
		if it.Method == "" {
			it.Method = "GET"
		}
		if u, err := url.Parse(it.URL); err == nil {
			it.Host, it.Path, it.Query = u.Host, u.Path, u.RawQuery
		}
		if it.Type == "" {
			it.Type = "XHR"
		}
		if it.Kind == "" {
			it.Kind = kindOfType(it.Type)
		}
		if it.At.IsZero() {
			it.At = time.Now()
		}
		if len(it.PostData) > 64<<10 {
			it.PostData = it.PostData[:64<<10]
		}
		if len(it.BodyPreview) > 256<<10 {
			it.BodyPreview = it.BodyPreview[:256<<10]
			it.BodyTruncated = true
		}
		if it.BodyPreview != "" {
			it.HasBody = true
			if it.BodySize == 0 {
				it.BodySize = len(it.BodyPreview)
			}
		}
		it.Index = len(s.Entries) + 1
		seen[it.ID] = true
		s.Entries = append(s.Entries, it)
		added++
	}
	if added > 0 {
		s.Stopped = false
		s.LastAt = time.Now()
	}
	return added, nil
}

// DeleteRequests 删除会话里的若干条录制结果（用户在控制台"去掉不想要的接口"）。
func (r *Recorder) DeleteRequests(id string, ids []string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return 0, fmt.Errorf("录制会话不存在")
	}
	want := make(map[string]bool, len(ids))
	for _, x := range ids {
		if x = strings.TrimSpace(x); x != "" {
			want[x] = true
		}
	}
	if len(want) == 0 {
		return 0, fmt.Errorf("没有指定要删除的请求 id")
	}
	if s.Deleted == nil {
		s.Deleted = map[string]bool{}
	}

	// 先确认这些 id 真的存在于当前结果里（已删除的不重复计数）
	exist := map[string]bool{}
	if s.Cap != nil {
		for _, it := range s.Cap.Snapshot() {
			exist[it.ID] = true
		}
	}
	for _, it := range s.Entries {
		exist[it.ID] = true
	}

	deleted := 0
	for rid := range want {
		if !exist[rid] || s.Deleted[rid] {
			continue
		}
		s.Deleted[rid] = true
		deleted++
	}
	// 插件会话的数据就在 Entries 里，直接摘掉；内置浏览器会话靠 Deleted 过滤
	if s.Cap == nil {
		out := s.Entries[:0]
		for _, it := range s.Entries {
			if !s.Deleted[it.ID] {
				out = append(out, it)
			}
		}
		s.Entries = out
		for i := range s.Entries {
			s.Entries[i].Index = i + 1
		}
	}
	return deleted, nil
}

// kindOfType 把资源类型归一化成 api / page / static / other（与内置浏览器录制口径一致）。
func kindOfType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "xhr", "fetch":
		return "api"
	case "document":
		return "page"
	case "":
		return "api"
	default:
		return "static"
	}
}

// Get 取会话。
func (r *Recorder) Get(id string) (*CaptureSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	return s, ok
}

// SkippedTypes 这次录制"因为不在录制范围里"被跳过的请求，按类型统计（诊断"文档没录到"用）。
func (r *Recorder) SkippedTypes(id string) map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok || s.Cap == nil {
		return nil
	}
	return s.Cap.SkippedTypes()
}

// Entries 取当前录到的请求（运行中实时取，停止后取快照；已删除的条目不再返回）。
func (r *Recorder) Entries(id string) ([]model.CapturedRequest, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return nil, false, fmt.Errorf("录制会话不存在或已被清理（服务重启后会清空）")
	}
	var list []model.CapturedRequest
	if s.Cap != nil {
		list = s.Cap.Snapshot()
	}
	if len(list) == 0 {
		list = append([]model.CapturedRequest(nil), s.Entries...)
	}
	if len(s.Deleted) > 0 {
		out := make([]model.CapturedRequest, 0, len(list))
		for _, it := range list {
			if !s.Deleted[it.ID] {
				out = append(out, it)
			}
		}
		list = out
	}
	return list, true, nil
}

// Stop 停止录制并返回结果（幂等）。
func (r *Recorder) Stop(ctx context.Context, id string) ([]model.CapturedRequest, error) {
	r.mu.Lock()
	s, ok := r.sessions[id]
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("录制会话不存在")
	}
	if s.Cap != nil {
		s.Entries = s.Cap.Stop()
	}
	s.Stopped = true
	entries := append([]model.CapturedRequest(nil), s.Entries...)
	deleted := len(s.Deleted)
	r.mu.Unlock()

	if len(entries) == 0 || deleted > 0 {
		if list, _, err := r.Entries(id); err == nil {
			return list, nil
		}
	}
	return entries, nil
}

// Delete 关闭会话并释放标签页。
func (r *Recorder) Delete(id string) error {
	r.mu.Lock()
	s, ok := r.sessions[id]
	if ok {
		delete(r.sessions, id)
		for i, x := range r.order {
			if x == id {
				r.order = append(r.order[:i], r.order[i+1:]...)
				break
			}
		}
	}
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("录制会话不存在")
	}
	if s.Cap != nil {
		s.Cap.Stop()
	}
	if s.Page != nil {
		s.Page.Close(context.Background())
	}
	return nil
}

// DiscardAll 丢弃全部录制会话（内存里的录制结果），返回丢弃的会话数。
// 只影响"还没建任务的录制结果"：已建好的采集任务与已入库数据不受影响。
func (r *Recorder) DiscardAll() int {
	r.mu.Lock()
	ids := append([]string(nil), r.order...)
	r.mu.Unlock()
	for _, id := range ids {
		_ = r.Delete(id) // 顺手关掉内置浏览器里占着的标签页
	}
	return len(ids)
}

// List 列出会话摘要（新的在前）。
func (r *Recorder) List() []CaptureInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]CaptureInfo, 0, len(r.sessions))
	for i := len(r.order) - 1; i >= 0; i-- {
		s, ok := r.sessions[r.order[i]]
		if !ok {
			continue
		}
		// 插件会话：30 分钟没有任何上报，就当它已经停了，别一直占着「录制中」
		if s.Source == "extension" && !s.Stopped && !s.LastAt.IsZero() &&
			time.Since(s.LastAt) > 30*time.Minute {
			s.Stopped = true
		}
		count := len(s.Entries)
		if s.Cap != nil {
			if live := s.Cap.Count(); live > 0 || !s.Stopped {
				count = live
			}
		}
		if len(s.Deleted) > 0 {
			// 列表里的条数也要反映"删掉之后"的数量
			visible := 0
			if s.Cap != nil {
				for _, it := range s.Cap.Snapshot() {
					if !s.Deleted[it.ID] {
						visible++
					}
				}
			} else {
				for _, it := range s.Entries {
					if !s.Deleted[it.ID] {
						visible++
					}
				}
			}
			count = visible
		}
		out = append(out, CaptureInfo{
			ID: s.ID, Source: s.Source, PageURL: s.PageURL, StartedAt: s.StartedAt,
			Count: count, Stopped: s.Stopped, Note: s.Note, UserAgent: s.UserAgent, Types: s.Types,
		})
	}
	return out
}

// Close 关闭全部会话（服务退出时调用）。
func (r *Recorder) Close() {
	r.mu.Lock()
	ids := append([]string(nil), r.order...)
	r.mu.Unlock()
	for _, id := range ids {
		_ = r.Delete(id)
	}
}

/* ------------------------------------------------------------------ Service */

// Service 接口采集的总入口。
type Service struct {
	Store     *store.DB
	Recorder  *Recorder
	Harvester *Harvester
	Scheduler *Scheduler

	dataDir string
}

// NewService 装配 Service。
func NewService(db *store.DB, br *browser.Manager, b *bus.Bus, dataDir string) *Service {
	h := NewHarvester(db, br, b, dataDir)
	return &Service{
		Store:     db,
		Recorder:  NewRecorder(br),
		Harvester: h,
		Scheduler: NewScheduler(db, h),
		dataDir:   dataDir,
	}
}

// ProbeSink 试连落库目标（只建立连接，不写入任何数据）。
func (s *Service) ProbeSink(cfg model.HarvestSink) error {
	sink, err := OpenSink(cfg, s.dataDir)
	if err != nil {
		return err
	}
	return sink.Close()
}

// Start 启动调度器（服务启动时调用一次）。
func (s *Service) Start() {
	s.Scheduler.Start()
}

// Close 停止调度器并关闭录制会话（服务退出时调用）。
func (s *Service) Close() {
	s.Scheduler.Stop()
	s.Recorder.Close()
}

func trimQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
