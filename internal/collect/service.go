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
	ID      string
	Source  string // browser | extension
	Page    *browser.Page
	Cap     *browser.NetCapture
	PageURL string
	// StartURL 用户填的起始地址（含原始 #fragment，不会被浏览器跳转覆盖）
	StartURL  string
	StartedAt time.Time
	Stopped   bool
	Note      string
	UserAgent string
	// Entries 已录到的条目。browser 会话停止后由 Cap 快照填充；extension 会话就是全部数据。
	Entries []model.CapturedRequest
	// Deleted 被用户删掉的请求 id（内置浏览器会话没法从 CDP 里删，只能在这里屏蔽）
	Deleted map[string]bool
	// Manual 手动补充 / 自动补录的条目：内置浏览器会话也可能没有新的 Document 请求
	// （SPA 前端路由、先录制后打开页面、页面走缓存），所以补录的条目单独存，
	// 读取时并到实时结果后面，停止录制也不会被 Cap 快照冲掉。
	Manual []model.CapturedRequest
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
		StartURL:  startURL,
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
			_ = page.Navigate(navCtx, startURL)
			cancel()
			// 选了「含页面文档」但 CDP 没抓到（走缓存 / 前端路由 / 重定向）时，
			// 用页面当前 DOM 兜底补一条，保证录制结果里一定有页面文档。
			if hasType(types, "Document") {
				docCtx, docCancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer docCancel()
				if _, derr := r.ensureDocument(docCtx, sess.ID, startURL, "初始页面文档（自动补录）"); derr != nil {
					log.Printf("[collect] 自动补录页面文档失败 %s：%v", sess.ID, derr)
				}
			}
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
//
// types 由插件按「录制范围」带上来：插件能拿到 XHR / Fetch，页面文档由 content script
// 读 document.documentElement.outerHTML 上报（type=Document）；其他资源类型插件拿不到。
func (r *Recorder) StartExternal(pageURL, note, userAgent string, types []string) (CaptureInfo, error) {
	if len(types) == 0 {
		types = []string{"XHR", "Fetch", "Document"}
	}
	keep := make([]string, 0, len(types))
	for _, t := range normalizeTypes(types) {
		switch t {
		case "XHR", "Fetch", "Document":
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		keep = []string{"XHR", "Fetch", "Document"}
	}
	sess := &CaptureSession{
		ID:        newID("cap_"),
		Source:    "extension",
		PageURL:   pageURL,
		StartedAt: time.Now(),
		LastAt:    time.Now(),
		Note:      note,
		StartURL:  pageURL,
		UserAgent: userAgent,
		Entries:   []model.CapturedRequest{},
		Deleted:   map[string]bool{},
		Types:     keep,
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
		StartedAt: sess.StartedAt, Note: sess.Note, UserAgent: sess.UserAgent, Types: sess.Types,
	}
	r.mu.Unlock()
	return info, nil
}

// AddRequests 由插件把录到的请求写入会话（批量，幂等：同 id 不会重复写入）。
//
// 内置浏览器会话也接收：手动「添加页面文档 / 打开页面补录」走的也是这条路。
func (r *Recorder) AddRequests(id string, items []model.CapturedRequest) (int, error) {
	added, err := r.ingest(id, items, false)
	return len(added), err
}

// ingest 把条目写进会话，返回真正写入的那些（同 id / 已删除的会跳过）。
//
// manual=true 表示不是页面钩子上报的，而是用户手动补录或录制器自动补录：
// 这类条目标 manual 标记，并且不会把「已停止」的会话重新变成录制中。
func (r *Recorder) ingest(id string, items []model.CapturedRequest, manual bool) ([]model.CapturedRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return nil, fmt.Errorf("录制会话不存在或已被清理（服务重启后会新建）")
	}
	live := s.Entries
	if s.Cap != nil {
		live = s.Cap.Snapshot()
	}
	seen := make(map[string]bool, len(live)+len(s.Manual))
	next := 1
	for _, it := range live {
		seen[it.ID] = true
		if it.Index >= next {
			next = it.Index + 1
		}
	}
	for _, it := range s.Manual {
		seen[it.ID] = true
		if it.Index >= next {
			next = it.Index + 1
		}
	}
	added := make([]model.CapturedRequest, 0, len(items))
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
		if len(live)+len(s.Manual)+len(added) >= maxCaptureEntries {
			break
		}
		it.Method = strings.ToUpper(strings.TrimSpace(it.Method))
		if it.Method == "" {
			it.Method = "GET"
		}
		if u, err := url.Parse(it.URL); err == nil {
			// 浏览器发出的 Document 请求本身不带 #fragment（HTTP 不会发送它），
			// 但 SPA 的 hash 路由（/#/vip_member）就是页面本身。会话里还没有任何
			// 页面文档时，这条就是起始页：用会话起始地址把 fragment 补回去，
			// 否则控制台里只能看到一个孤零零的 "/"。
			if u.Fragment == "" && (u.Path == "" || u.Path == "/") && s.Source == "browser" &&
				(strings.EqualFold(it.Type, "Document") || strings.EqualFold(it.Kind, "page")) &&
				noDocumentYet(live, added) {
				if frag := s.rootFragment(u.Host); frag != "" && frag != "/" {
					u.Fragment = frag
					it.URL = u.String()
				}
			}
			it.Host, it.Path, it.Query = u.Host, u.Path, u.RawQuery
			// hash 路由（/#/vip_member）的 fragment 也算路径的一部分，漏掉就全显示成 "/"
			if u.Fragment != "" {
				it.Path += "#" + u.Fragment
			}
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
		it.Index = next
		next++
		if manual || s.Cap != nil {
			it.Manual = true
		}
		seen[it.ID] = true
		if s.Cap != nil {
			s.Manual = append(s.Manual, it)
		} else {
			s.Entries = append(s.Entries, it)
		}
		added = append(added, it)
	}
	if len(added) > 0 {
		s.LastAt = time.Now()
		if s.Source == "extension" && !manual {
			s.Stopped = false
		}
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
	for _, it := range s.Manual {
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
	// 手动补录的条目直接摘掉；插件会话的数据就在 Entries 里，也直接摘掉；
	// 内置浏览器实时抓到的条目没法从 CDP 里删，只能靠 Deleted 过滤。
	if len(s.Manual) > 0 {
		out := s.Manual[:0]
		for _, it := range s.Manual {
			if !s.Deleted[it.ID] {
				out = append(out, it)
			}
		}
		s.Manual = out
	}
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
	// 顺手把内置浏览器里的真实地址同步回来：SPA 的 hash 路由（#/vip_member）
	// 只有浏览器知道，而 Document 请求本身不带 #fragment，不同步就只显示 "/"。
	if s.Page != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
		if raw, err := s.Page.Eval(ctx, "location.href"); err == nil {
			if cur := strings.TrimSpace(trimQuotes(string(raw))); strings.HasPrefix(strings.ToLower(cur), "http") {
				s.PageURL = cur
			}
		}
		cancel()
	}
	return s.visibleEntries(), true, nil
}

// visibleEntries 合并「实时抓到的」+「手动补充 / 自动补录的」条目，并过滤已删除的。
func (s *CaptureSession) visibleEntries() []model.CapturedRequest {
	var live []model.CapturedRequest
	if s.Cap != nil {
		live = s.Cap.Snapshot()
	}
	if len(live) == 0 {
		live = s.Entries
	}
	out := mergeCaptured(live, s.Manual, s.Deleted)
	s.backfillStartFragment(out)
	return out
}

// backfillStartFragment 起始页的 Document 请求不带 #fragment（HTTP 不会发送它），
// 但 SPA 的 hash 路由（/#/vip_member）就是页面本身。这里把会话起始地址的
// fragment 补到第一张根路径页面文档上，否则控制台里只能看到一个孤零零的 "/"。
func (s *CaptureSession) backfillStartFragment(items []model.CapturedRequest) {
	if s.Source != "browser" || len(items) == 0 {
		return
	}
	for i := range items {
		it := &items[i]
		if !strings.EqualFold(it.Type, "Document") && !strings.EqualFold(it.Kind, "page") {
			continue
		}
		if u, err := url.Parse(it.URL); err == nil && u.Fragment == "" && (u.Path == "" || u.Path == "/") {
			if frag := s.rootFragment(u.Host); frag != "" {
				u.Fragment = frag
				it.URL = u.String()
				it.Path = pagePath(u)
			}
		}
		return // 只补第一张页面文档
	}
}

// rootFragment 起始页该显示的 #fragment：
// 浏览器已经跳到别的 hash 路由（#/vip_member）就用浏览器的；
// 只是被重定向回 "#/"（还没登录）时，用用户填的起始地址，别只显示一个 "/"。
func (s *CaptureSession) rootFragment(host string) string {
	pick := func(raw string) (string, string) {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" || !strings.EqualFold(u.Host, host) {
			return "", ""
		}
		return u.Fragment, u.Host
	}
	if frag, _ := pick(s.PageURL); frag != "" && frag != "/" {
		return frag
	}
	if frag, _ := pick(s.StartURL); frag != "" {
		return frag
	}
	frag, _ := pick(s.PageURL)
	return frag
}

// mergeCaptured 按 id 去重合并两组条目；deleted 里的直接丢掉。
func mergeCaptured(live, manual []model.CapturedRequest, deleted map[string]bool) []model.CapturedRequest {
	if len(manual) == 0 && len(deleted) == 0 {
		return live
	}
	out := make([]model.CapturedRequest, 0, len(live)+len(manual))
	seen := make(map[string]bool, len(live)+len(manual))
	add := func(it model.CapturedRequest) {
		if it.ID != "" {
			if seen[it.ID] || deleted[it.ID] {
				return
			}
			seen[it.ID] = true
		}
		out = append(out, it)
	}
	for _, it := range live {
		add(it)
	}
	for _, it := range manual {
		add(it)
	}
	return out
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
	r.mu.Unlock()

	if list, _, err := r.Entries(id); err == nil {
		return list, nil
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
		// 条数按"合并 + 过滤已删除"后的结果算：手动补录的页面文档也算一条
		count := len(s.visibleEntries())
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

// noDocumentYet 会话里是否还没有任何页面文档（用来判断这条是不是起始页）。
func noDocumentYet(live, added []model.CapturedRequest) bool {
	for _, x := range live {
		if strings.EqualFold(x.Type, "Document") || strings.EqualFold(x.Kind, "page") {
			return false
		}
	}
	for _, x := range added {
		if strings.EqualFold(x.Type, "Document") || strings.EqualFold(x.Kind, "page") {
			return false
		}
	}
	return true
}
