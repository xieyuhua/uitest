package collect

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"uitest-server/internal/browser"
	"uitest-server/internal/bus"
	"uitest-server/internal/model"
	"uitest-server/internal/store"
)

// 本文件是"怎么抓"：一次采集执行的完整流程。
//
// 礼貌采集（默认值都偏保守，可在任务里改，但有下限）：
//   - 相邻请求之间至少 MinIntervalMS，并叠加 JitterPercent 随机抖动
//   - 单任务同一时刻只有一个执行，绝不并发打对方
//   - 429 / 503 按 Retry-After 等待，重试次数用尽就停
//   - 401 / 403 视为会话失效或明确拒绝：立即停手并提示怎么处理
//   - 连续失败达到阈值 → 本次运行熔断，任务自动暂停

// PreloginRunner 跑一次 UI 自动化录制（登录流程）并等它跑完，由 engine.Runner 实现。
//
// dataset 是登录流程要用的变量（密码框录制时被脱敏成 {{password}}，必须靠它注入）。
type PreloginRunner interface {
	RunRecordingSync(ctx context.Context, recordingID, title string, dataset map[string]string) (runID string, err error)
}

// Harvester 采集执行器。
type Harvester struct {
	store   *store.DB
	br      *browser.Manager
	bus     *bus.Bus
	dataDir string
	// prelogin 前置登录用的 UI 自动化执行器（可为 nil：没装配就跳过）
	prelogin PreloginRunner

	mu       sync.Mutex
	running  map[string]string             // taskID → runID
	cancels  map[string]context.CancelFunc // runID → cancel
	cookieMu sync.Mutex
	cookies  map[string]cachedCookies // 来源站点 → 缓存的浏览器 Cookie
	tokens   map[string]cachedToken   // 站点+存储+键名 → 缓存的浏览器 Token（后端兼容）
	hdrs     map[string]cachedHeaders // 来源站点 → 浏览器里真实的登录态请求头

	// 礼貌采集的节流状态：每个任务记最近一次请求时间
	reqMu   sync.Mutex
	lastReq map[string]time.Time
}

type cachedCookies struct {
	list    []*http.Cookie
	fetched time.Time
}

// cachedToken 从内置浏览器存储里读到的 token（30 分钟内复用）。
type cachedToken struct {
	value   string
	fetched time.Time
}

// cachedHeaders 从浏览器实际请求里嗅到的登录态请求头（Authorization / X-Token 之类）。
type cachedHeaders struct {
	headers map[string]string
	fetched time.Time
}

// NewHarvester 创建执行器。
func NewHarvester(db *store.DB, br *browser.Manager, b *bus.Bus, dataDir string) *Harvester {
	return &Harvester{
		store:   db,
		br:      br,
		bus:     b,
		dataDir: dataDir,
		running: map[string]string{},
		cancels: map[string]context.CancelFunc{},
		cookies: map[string]cachedCookies{},
		tokens:  map[string]cachedToken{},
		hdrs:    map[string]cachedHeaders{},
		lastReq: map[string]time.Time{},
	}
}

// SetPreloginRunner 注入 UI 自动化执行器（「采集前先登录」用），启动时由 main 装配。
func (h *Harvester) SetPreloginRunner(r PreloginRunner) { h.prelogin = r }

// Running 任务是否正在执行。
func (h *Harvester) Running(taskID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.running[taskID]
	return ok
}

// Start 异步执行一次采集。trigger 为 manual 或 schedule。
func (h *Harvester) Start(taskID, trigger string) (model.HarvestRun, error) {
	task, err := h.store.GetHarvestTask(taskID)
	if err != nil {
		return model.HarvestRun{}, err
	}
	if task.EndpointCount() == 0 {
		return model.HarvestRun{}, fmt.Errorf("这条任务里没有启用任何接口")
	}
	h.mu.Lock()
	if runID, busy := h.running[taskID]; busy {
		h.mu.Unlock()
		return model.HarvestRun{}, fmt.Errorf("任务正在执行中（%s），请等它跑完", runID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sinkText := describeSink(task.Sink)
	if task.SplitByEndpoint {
		sinkText = "按接口分别落库"
	}
	run, err := h.store.CreateHarvestRun(model.HarvestRun{
		TaskID:    task.ID,
		TaskName:  task.Name,
		Status:    "queued",
		Trigger:   trigger,
		StartedAt: time.Now(),
		Sink:      sinkText,
	})
	if err != nil {
		h.mu.Unlock()
		cancel()
		return model.HarvestRun{}, err
	}
	h.running[taskID] = run.ID
	h.cancels[run.ID] = cancel
	h.mu.Unlock()

	go h.execute(ctx, run.ID)
	return run, nil
}

// Cancel 取消一次执行。
func (h *Harvester) Cancel(runID string) error {
	h.mu.Lock()
	cancel, ok := h.cancels[runID]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("这次运行已经结束或不存在")
	}
	cancel()
	return nil
}

func (h *Harvester) release(taskID, runID string) {
	h.mu.Lock()
	if cur, ok := h.running[taskID]; ok && cur == runID {
		delete(h.running, taskID)
	}
	if cancel, ok := h.cancels[runID]; ok {
		cancel()
		delete(h.cancels, runID)
	}
	h.mu.Unlock()
}

/* ------------------------------------------------------------------ 执行 */

func (h *Harvester) execute(ctx context.Context, runID string) {
	run, err := h.store.GetHarvestRun(runID)
	if err != nil {
		return
	}
	defer h.release(run.TaskID, runID)

	task, err := h.store.GetHarvestTask(run.TaskID)
	if err != nil {
		h.finish(runID, run.TaskID, "failed", "任务不存在，可能已被删除")
		return
	}
	task.Normalize()
	h.setStatus(runID, "running")
	h.logf(runID, "info", "开始采集「%s」：%d 个接口，触发方式=%s", task.Name, task.EndpointCount(), triggerLabel(run.Trigger))

	sinks := newEndpointSinks(task, h.dataDir)
	// 把真正执行的 SQL 记进运行日志（建表 / 补列 / 插入模板 / 每批展开语句）
	sinks.logf = func(format string, args ...any) { h.logf(runID, "sql", format, args...) }
	defer sinks.Close()
	h.store.MutateHarvestRun(runID, func(r *model.HarvestRun) { r.Sink = sinks.DescribeAll() })
	h.logf(runID, "info", "写入目标：%s", sinks.DescribeAll())

	// 前置登录：先跑一遍 UI 自动化（登录），把内置浏览器会话刷新好再抓
	if task.Prelogin != nil && task.Prelogin.Active() {
		pl := task.Prelogin.Effective()
		if pl.Mode != "on-fail" {
			plv := h.preloginDataset(pl)
			switch {
			case len(plv) == 0:
				h.logf(runID, "warn",
					"前置登录没有配「登录用变量」：录制时密码框会被脱敏成 {{password}}，回放时就是字面量，登录多半会失败")
			case strings.TrimSpace(plv["password"]) == "":
				h.logf(runID, "warn",
					"前置登录变量里没有 password（或为空）：如果登录表单有密码框，回放会填成 {{password}}")
			default:
				h.logf(runID, "info", "前置登录变量：%s", varKeysText(plv))
			}
			if perr := h.runPrelogin(ctx, runID, &task); perr != nil {
				h.logf(runID, "error", "%v", perr)
				if !pl.ContinueOnError {
					h.finish(runID, task.ID, "failed", perr.Error())
					return
				}
				h.logf(runID, "warn", "按配置继续抓取（可能拿不到数据）")
			} else if strings.TrimSpace(task.Session.Host) == "" {
				// 登录已经把浏览器登好了：站点默认取起始页/第一个接口的 origin
				task.Session.Host = sessionTarget(&task)
			}
		} else {
			h.logf(runID, "info", "前置登录设为「%s」，本次先直接抓，401/403 时再补跑", pl.ModeText())
		}
	}

	sess, err := h.resolveSession(ctx, &task)
	if err != nil {
		h.logf(runID, "warn", "会话准备失败：%v", err)
		// 前置登录开了却没拿到会话 —— 继续抓只会全是 401，直接失败更清楚
		if task.Prelogin != nil && task.Prelogin.Active() && !task.Prelogin.Effective().ContinueOnError {
			h.finish(runID, task.ID, "failed", "前置登录跑完了但没拿到登录态："+err.Error())
			return
		}
		h.logf(runID, "warn", "本次将用无登录态请求（可能全是 401）")
	} else if sess.cookieCount > 0 || sess.token != "" {
		var bits []string
		if sess.cookieCount > 0 {
			bits = append(bits, fmt.Sprintf("%d 个 Cookie", sess.cookieCount))
		}
		if sess.token != "" {
			bits = append(bits, fmt.Sprintf("Token（%d 字符）", len(sess.token)))
		}
		h.logf(runID, "info", "会话已就绪：携带 %s（来源：%s）", strings.Join(bits, " + "), sessionLabel(task.Session))
		if names := cookieNameList(sess); len(names) > 0 {
			h.logf(runID, "info", "Cookie 名单：%s", strings.Join(names, "、"))
		}
		// 浏览器实时请求头：这是他最关心的"Authorization 到底换成新的没有"
		mode := orDefaultStr(sess.headerMode, "all")
		switch {
		case len(sess.headers) > 0:
			h.logf(runID, "info", "浏览器请求头（%s 模式）：%s —— 与接口里的同名头冲突时用浏览器实时值",
				mode, varKeysText(sess.headers))
		case mode == "off":
			h.logf(runID, "info", "浏览器请求头：按配置不共享（header_mode=off），接口里的录制头原样发送")
		default:
			h.logf(runID, "warn",
				"没从浏览器抓到接口请求头：Authorization 等可能还是录制时的旧值。"+
					"把「站点」改成登录后一打开就会请求接口的页面（列表页/首页），再跑一次")
		}
	}
	// 公共参数：先报一下，再跑「登录后脚本」把登录结果灌进来（脚本优先级最高）
	if len(sess.params) > 0 || len(sess.commonHeaders) > 0 {
		h.logf(runID, "info", "公共参数：query/body %d 项（%s）、公共请求头 %d 项（%s）",
			len(sess.params), varKeysText(sess.params),
			len(sess.commonHeaders), varKeysText(sess.commonHeaders))
	}
	if task.Common != nil && strings.TrimSpace(task.Common.Script) != "" {
		h.runCommonScript(ctx, runID, &task, &sess)
	}

	client := h.newClient(task.Throttle, sess)

	totalReq, totalFetched, totalWritten := 0, 0, 0
	failStreak := 0
	okCount, failCount := 0, 0
	stopAll := false
	stopReason := ""
	budget := newHTTPLogBudget()

	for _, ep := range task.Endpoints {
		if !ep.Enabled {
			continue
		}
		if stopAll || ctx.Err() != nil {
			break
		}
		h.logf(runID, "info", "▶ 采集接口 %s %s", strings.ToUpper(ep.Method), ep.URL)
		var res model.HarvestEndpointResult
		epSink, serr := sinks.For(ep)
		if serr != nil {
			res = model.HarvestEndpointResult{
				EndpointID: ep.ID, Name: ep.Name,
				Method: strings.ToUpper(ep.Method), URL: ep.URL,
				Error: "无法连接落库目标：" + serr.Error(),
				Hint:  "去「数据写入」检查这套配置（类型 / DSN / 表名 / 账号权限），或先点「测试连接」",
			}
			h.logf(runID, "error", "接口「%s」落库目标不可用：%s", ep.Name, serr.Error())
		} else {
			h.logf(runID, "info", "该接口写入目标：%s", epSink.Describe())
			res = h.collectEndpoint(ctx, runID, &task, ep, client, sess, epSink, budget)
		}
		totalReq += res.Pages
		totalFetched += res.Fetched
		totalWritten += res.Written
		_, _ = h.store.MutateHarvestRun(runID, func(r *model.HarvestRun) {
			r.Endpoints = append(r.Endpoints, res)
			r.Requests = totalReq
			r.Fetched = totalFetched
			r.Written = totalWritten
		})

		if res.Error != "" {
			failStreak++
			failCount++
			h.logf(runID, "error", "接口失败：%s（%s）", res.Error, res.Hint)
			if strings.Contains(res.Hint, "停止") {
				stopAll = true
				stopReason = res.Error
			}
			if failStreak >= task.Throttle.MaxFailStreak {
				stopAll = true
				stopReason = fmt.Sprintf("连续 %d 个接口失败，本次运行熔断（既保护对方服务器，也避免自己变成异常流量）", failStreak)
				h.logf(runID, "warn", "%s", stopReason)
			}
		} else {
			failStreak = 0
			okCount++
			h.logf(runID, "info", "接口完成：抓取 %d 条，写入 %d 行（%d 次请求）", res.Fetched, res.Written, res.Pages)
		}
	}

	if ctx.Err() != nil {
		h.finish(runID, task.ID, "canceled", "已手动取消")
		return
	}
	// 一个接口都没成功 = 这次运行失败（哪怕中途没有触发熔断）
	if okCount == 0 && failCount > 0 {
		if stopReason == "" {
			stopReason = fmt.Sprintf("全部 %d 个接口都失败了，请点开接口结果看具体原因", failCount)
		}
		h.finish(runID, task.ID, "failed", stopReason)
		return
	}
	msg := fmt.Sprintf("完成：%d 次请求，解析 %d 条记录，写入 %d 行", totalReq, totalFetched, totalWritten)
	if failCount > 0 {
		msg += fmt.Sprintf("；其中 %d 个接口失败", failCount)
	}
	if stopReason != "" {
		msg += "；提前停止：" + stopReason
	}
	h.finish(runID, task.ID, "passed", msg)
}

// collectEndpoint 抓一个接口（自动翻页，直到满足停止条件）。
//
// 翻页策略交给 Paginator：
//   - 它把当前页参数写进 URL 或请求体（page / offset / cursor 三种）；
//   - 它根据响应（总数 / has_more / 游标 / 短页）判断是否还有下一页；
//   - 它对"连续两页完全相同"做指纹检测，防止接口忽略分页参数时白跑 50 页；
//   - 跨页重复的行会被跳过（Duplicates 计数报给你）。
func (h *Harvester) collectEndpoint(ctx context.Context, runID string, task *model.HarvestTask, ep model.HarvestEndpoint,
	client *http.Client, sess resolvedSession, sink Sink, budget *httpLogBudget) model.HarvestEndpointResult {

	started := time.Now()
	res := model.HarvestEndpointResult{
		EndpointID: ep.ID, Name: ep.Name, Method: strings.ToUpper(ep.Method), URL: ep.URL,
	}
	if sink != nil {
		res.Target = sink.Describe()
	}
	pg := NewPaginatorFor(ep.Pagination, ep.Method)
	h.logf(runID, "info", "分页策略：%s", pg.Describe())
	// 401/403 只自动补一次登录态，避免死循环
	authRetried := false

	maxPages := 1
	if pg.Enabled() {
		// 接口上填了最多页数就以它为准；没填才用任务的分页默认上限。
		// 两者都受 MaxPagesHardLimit 兜底，防止配置写错把对方打穿。
		maxPages = pg.pag.MaxPages
		if maxPages <= 0 {
			maxPages = task.Throttle.MaxPagesCap
		}
		if maxPages <= 0 || maxPages > model.MaxPagesHardLimit {
			maxPages = model.MaxPagesHardLimit
		}
	}

	var allRows []map[string]any
	seen := map[string]bool{}
	fields := ep.Fields
	autoFields := false
	duplicates := 0

	for i := 0; i < maxPages; i++ {
		if ctx.Err() != nil {
			return res
		}
		vars := sessionVars(sess) // 含公共参数 / 脚本产出的变量（URL 里可写 {{token}}）
		vars["page"] = pg.Current()
		vars["host"] = hostOf(ep.URL)
		vars["url"] = ep.URL
		vars["endpoint"] = ep.Name
		target, body := pg.Apply(expand(ep.URL, vars), expand(ep.Body, vars))

		h.throttleWait(ctx, task.ID, task.Throttle)
		status, raw, retries, err := h.fetch(ctx, client, ep, target, body, sess, task.Throttle, nil)
		// 401/403：登录态过期（Cookie 失效 / token 随登录状态变化），刷新会话后重试这一次
		logged := false
		authErr := status == http.StatusUnauthorized || status == http.StatusForbidden
		canRelogin := task.Prelogin != nil && task.Prelogin.Active()
		if authErr && !authRetried && (canRelogin || sess.token != "" || sess.cookieCount > 0) {
			authRetried = true
			h.logHTTP(runID, budget, ep, target, body, status, raw, sess, i+1)
			logged = true
			h.logf(runID, "warn", "接口返回 %d，刷新登录态后重试本次请求", status)
			if canRelogin && task.Prelogin.Effective().Mode == "on-fail" {
				if perr := h.runPrelogin(ctx, runID, task); perr != nil {
					h.logf(runID, "warn", "%v", perr)
				}
			}
			h.invalidateSession(task)
			if fresh, serr := h.resolveSession(ctx, task); serr == nil {
				sess = fresh
				setJarCookies(client.Jar, sess)
			} else {
				h.logf(runID, "warn", "重新导出会话失败：%v", serr)
			}
			s2, r2, retry2, e2 := h.fetch(ctx, client, ep, target, body, sess, task.Throttle, nil)
			status, raw, retries, err = s2, r2, retries+retry2, e2
			h.logHTTP(runID, budget, ep, target, body, status, raw, sess, i+1)
			if e2 == nil && status >= 200 && status < 300 {
				h.logf(runID, "info", "刷新登录态后重试成功（%d）", status)
			}
		}
		res.Pages++
		res.Retries += retries
		res.Bytes += len(raw)
		// 每一次请求都写进运行日志（分页抓 50 页就有 50 组请求/响应，明文不脱敏）
		// 注意：刷新登录态重试的那条已经在上面记过了，别重复
		if !logged {
			h.logHTTP(runID, budget, ep, target, body, status, raw, sess, i+1)
		}
		if err != nil {
			res.Error = err.Error()
			switch {
			case status == http.StatusUnauthorized || status == http.StatusForbidden:
				res.Hint = "会话失效或接口拒绝访问：在内置浏览器里重新登录一次，或在任务里配「前置登录（UI 自动化）」自动重登；已停止后续接口"
			case status == http.StatusTooManyRequests:
				res.Hint = "对方返回 429（限流）：已停止后续接口，建议把请求间隔调大或降低频率"
			case ctx.Err() != nil:
				res.Hint = "已取消"
			default:
				res.Hint = "检查 URL / 参数 / 会话，必要时先点「试抓预览」看原始响应"
			}
			return res
		}
		res.Status = status

		root, perr := ParseJSONBody(string(raw))
		if perr != nil {
			res.Error = "响应解析失败：" + perr.Error()
			res.Hint = "响应不是 JSON（可能是 HTML 登录页或被拦截），请检查 URL 与会话"
			return res
		}
		records, path, rerr := RecordsFrom(root, ep.RecordsPath)
		if rerr != nil {
			res.Error = rerr.Error()
			res.Hint = "记录路径不对：把「记录路径」改成响应里真正的数组位置（例如 data.list）"
			return res
		}
		if len(records) == 0 {
			res.StopReason = "第 " + pg.Current() + " 页没有数据，停止翻页"
			h.logf(runID, "info", "%s", res.StopReason)
			break
		}
		if pg.Enabled() && pg.LoopDetected(records) {
			res.StopReason = "连续两页数据完全相同，判定接口忽略了分页参数，已停止（请检查分页参数名/位置）"
			h.logf(runID, "warn", "%s", res.StopReason)
			break
		}
		if len(fields) == 0 {
			fields = SuggestFields(records, 5)
			autoFields = true
			h.logf(runID, "warn", "接口没有配置字段映射，已按响应结构自动识别 %d 个字段（建议保存到任务里）", len(fields))
		}
		if ep.RecordsPath == "" && path != "" && i == 0 {
			h.logf(runID, "info", "自动识别记录数组位于 %s", path)
		}
		for _, rec := range records {
			row := FlattenRow(rec, fields, vars)
			if len(row) == 0 {
				continue
			}
			fp := rowFingerprint(row)
			if seen[fp] {
				duplicates++
				continue
			}
			seen[fp] = true
			row["_fetched_at"] = time.Now().Format("2006-01-02 15:04:05")
			allRows = append(allRows, row)
		}
		res.Fetched += len(records)

		if pg.Enabled() {
			h.logf(runID, "info", "第 %s 页：%d 条（累计 %d 条）", pg.Current(), len(records), len(allRows))
			if stop, reason := pg.Advance(records, root); stop {
				res.StopReason = reason
				h.logf(runID, "info", "停止翻页：%s", reason)
				break
			}
		}
	}
	res.Duplicates = duplicates
	if duplicates > 0 {
		h.logf(runID, "warn", "跨页出现 %d 条重复记录，已自动跳过（接口分页可能重叠）", duplicates)
	}

	if len(allRows) > 0 {
		written, werr := sink.Write(allRows)
		res.Written = written
		if werr != nil {
			res.Error = "写入失败：" + werr.Error()
			res.Hint = "检查落库配置（表名 / 账号权限 / 字段类型）"
			return res
		}
	}
	if autoFields {
		res.Hint = "字段映射是自动识别的，建议在任务详情里保存字段映射"
	}
	res.DurationMS = time.Since(started).Milliseconds()
	return res
}

// fetchTrace 一次请求的实际报文快照（试抓详情用；不参与抓取逻辑，nil 表示不记录）。
type fetchTrace struct {
	Method          string
	URL             string
	Headers         http.Header
	Body            string
	Attempts        int
	Status          int
	ResponseHeaders http.Header
	ResponseSize    int
}

// fetch 发一次请求（含重试与礼貌退避）。返回状态码、响应体、重试次数、错误。
// tr 非 nil 时把最后一次尝试真正发出去的报文记录下来，供「试抓详情」展示。
func (h *Harvester) fetch(ctx context.Context, client *http.Client, ep model.HarvestEndpoint, target, body string,
	sess resolvedSession, th model.HarvestThrottle, tr *fetchTrace) (int, []byte, int, error) {

	var lastStatus int
	var lastErr error
	// 公共参数先并进来（接口自己 / 分页已经给了同名参数时听接口的）
	target, body = mergeCommonParams(ep, target, body, sess)
	for attempt := 0; attempt <= th.Retries; attempt++ {
		if ctx.Err() != nil {
			return lastStatus, nil, attempt, ctx.Err()
		}
		var reader io.Reader
		if strings.TrimSpace(body) != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, strings.ToUpper(ep.Method), target, reader)
		if err != nil {
			return 0, nil, attempt, fmt.Errorf("URL 不合法：%w", err)
		}
		// 接口配置头 + 浏览器实时登录态头 + token 覆盖（顺序即优先级）
		for k, vals := range effectiveHeaders(ep, sess) {
			req.Header[k] = vals
		}
		if req.Header.Get("Accept") == "" {
			req.Header.Set("Accept", "application/json, text/plain, */*")
		}
		// 公共参数可能给 POST 合成了一个 JSON 请求体：没声明 Content-Type 时补上，否则很多站点会 415/400
		if trimmed := strings.TrimSpace(body); trimmed != "" && req.Header.Get("Content-Type") == "" {
			if trimmed[0] == '{' || trimmed[0] == '[' {
				req.Header.Set("Content-Type", "application/json")
			}
		}
		if sess.userAgent != "" {
			req.Header.Set("User-Agent", sess.userAgent)
		}
		if sess.referer != "" && req.Header.Get("Referer") == "" {
			req.Header.Set("Referer", sess.referer)
		}
		// 会话 Cookie：cookie jar 会自己带它认识的；如果它一个都没带（域名/secure/path 对不上），
		// 就把导出时按站点过滤好的 Cookie 明确写上——登录态不能因为 jar 的匹配规则被悄悄丢掉。
		if req.Header.Get("Cookie") == "" {
			jarEmpty := true
			if client.Jar != nil {
				jarEmpty = len(client.Jar.Cookies(req.URL)) == 0
			}
			if jarEmpty {
				if ck := cookieHeaderFor(req.URL.String(), sess); ck != "" {
					req.Header.Set("Cookie", ck)
				}
			}
		}

		if tr != nil {
			tr.Method = strings.ToUpper(ep.Method)
			tr.URL = target
			tr.Headers = req.Header.Clone()
			tr.Body = body
			tr.Attempts = attempt + 1
		}

		resp, err := client.Do(req)
		if err == nil && tr != nil {
			tr.Status = resp.StatusCode
			tr.ResponseHeaders = resp.Header.Clone()
			// 用最终发出的请求回填：cookie jar 里的 Cookie 是在 client.Do 内部才加上的，
			// 而且跟随重定向后 URL 也可能变了，这里才是"真正发出去的那一份"。
			if resp.Request != nil {
				tr.URL = resp.Request.URL.String()
				tr.Headers = resp.Request.Header.Clone()
			}
		}
		if err != nil {
			lastErr = err
			if attempt < th.Retries {
				if !sleepCtx(ctx, backoff(th.BackoffMS, attempt)) {
					return lastStatus, nil, attempt, ctx.Err()
				}
				continue
			}
			return lastStatus, nil, attempt, friendlyNetErr(lastErr)
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if tr != nil {
			tr.ResponseSize = len(raw)
		}
		if readErr != nil {
			lastErr = readErr
			if attempt < th.Retries {
				if !sleepCtx(ctx, backoff(th.BackoffMS, attempt)) {
					return resp.StatusCode, nil, attempt, ctx.Err()
				}
				continue
			}
			return resp.StatusCode, nil, attempt, fmt.Errorf("读取响应失败：%w", readErr)
		}
		lastStatus = resp.StatusCode

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return resp.StatusCode, raw, attempt, nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
			wait := retryAfter(resp.Header.Get("Retry-After"))
			if wait <= 0 {
				wait = backoff(th.BackoffMS, attempt) * 3
			}
			if wait > 5*time.Minute {
				wait = 5 * time.Minute
			}
			lastErr = fmt.Errorf("对方返回 %d（限流/暂不可用）", resp.StatusCode)
			if attempt < th.Retries {
				h.logf("", "warn", "对方返回 %d，等待 %s 后重试（%d/%d）", resp.StatusCode, wait.Round(time.Second), attempt+1, th.Retries)
				if !sleepCtx(ctx, wait) {
					return resp.StatusCode, nil, attempt, ctx.Err()
				}
				continue
			}
			return resp.StatusCode, raw, attempt, lastErr
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return resp.StatusCode, raw, attempt, fmt.Errorf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
			if attempt < th.Retries {
				if !sleepCtx(ctx, backoff(th.BackoffMS, attempt)) {
					return resp.StatusCode, nil, attempt, ctx.Err()
				}
				continue
			}
			return resp.StatusCode, raw, attempt, lastErr
		default:
			return resp.StatusCode, raw, attempt, fmt.Errorf("HTTP %d %s（响应体前 200 字：%s）",
				resp.StatusCode, http.StatusText(resp.StatusCode), truncate(string(raw), 200))
		}
	}
	return lastStatus, nil, th.Retries, lastErr
}

/* ------------------------------------------------------------------ 会话 */

type resolvedSession struct {
	cookies     map[string][]*http.Cookie // 来源站点 → cookie
	cookieCount int
	userAgent   string
	referer     string
	// headers 浏览器里真实的登录态请求头（Authorization / Blade-Auth / X-Token …），
	// 会覆盖接口配置里录制下来的旧值 —— 否则抓取还在用录制那一刻的 token。
	headers    map[string]string
	headerMode string // auth | all | off（来自 session.header_mode）
	// 任务级公共参数：GET/DELETE 进 query，POST/PUT/PATCH 进 JSON 请求体
	params map[string]string
	// 任务级公共请求头（浏览器共享的同名头会盖掉它）
	commonHeaders map[string]string
	// token 型站点（后端兼容保留，界面已不再提供入口）
	token       string
	tokenHeader string
	tokenPrefix string
}

func (h *Harvester) newClient(th model.HarvestThrottle, sess resolvedSession) *http.Client {
	jar, _ := cookiejar.New(nil)
	setJarCookies(jar, sess)
	timeout := time.Duration(th.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 3 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Jar: jar, Timeout: timeout, Transport: transport}
}

// setJarCookies 把会话里的 Cookie 灌进 jar（新建客户端、刷新会话都用它）。
func setJarCookies(jar http.CookieJar, sess resolvedSession) {
	if jar == nil {
		return
	}
	for origin, list := range sess.cookies {
		u, err := url.Parse(origin)
		if err != nil || u.Scheme == "" {
			continue
		}
		jar.SetCookies(u, list)
	}
}

// sessionScaffold 空会话骨架：带上 UA / Referer / 公共参数（其余字段靠浏览器导出填）。
func sessionScaffold(task *model.HarvestTask) resolvedSession {
	sess := resolvedSession{
		cookies:       map[string][]*http.Cookie{},
		params:        map[string]string{},
		commonHeaders: map[string]string{},
	}
	sess.userAgent = strings.TrimSpace(task.Session.UserAgent)
	sess.referer = strings.TrimSpace(task.Session.Referer)
	sess.headerMode = task.Session.EffectiveHeaderMode()
	if sess.referer == "" {
		sess.referer = strings.TrimSpace(task.StartURL)
	}
	if task.Common != nil {
		for k, v := range task.Common.Params {
			sess.params[k] = v
		}
		for k, v := range task.Common.Headers {
			sess.commonHeaders[k] = v
		}
	}
	return sess
}

// resolveSession 准备登录态：统一从内置浏览器取（前置登录之后浏览器里就是最新登录态）。
//
// 抓取用的 Cookie + 请求头全部来自浏览器；接口里只保留 URL / 请求体这些"请求参数"。
func (h *Harvester) resolveSession(ctx context.Context, task *model.HarvestTask) (resolvedSession, error) {
	sess := sessionScaffold(task)

	// token 配置属于老任务兼容（界面已不提供入口）
	if err := h.resolveToken(ctx, task, &sess); err != nil {
		return sess, err
	}

	target := sessionTarget(task)
	if target == "" {
		return sess, fmt.Errorf("任务里没有可用接口，判断不了站点")
	}
	urls := []string{target}
	for _, ep := range task.Endpoints {
		if o := originOf(ep.URL); o != "" && o != target {
			urls = append(urls, o)
		}
	}

	list, err := h.browserCookies(ctx, target, urls)
	if err != nil {
		return sess, err
	}
	if len(list) == 0 {
		return sess, fmt.Errorf("内置浏览器里没有 %s 的登录 Cookie（按 %s 取）：配一个「前置登录（UI 自动化）」把浏览器登进去，"+
			"或先在内置浏览器里手动登录一次", target, strings.Join(urls, "、"))
	}
	for _, ep := range task.Endpoints {
		if o := originOf(ep.URL); o != "" {
			addCookies(sess.cookies, o, list)
		}
	}
	sess.cookieCount = len(list)
	// 再把浏览器里"真实在用的"请求头抄一份：录制时存下的 Authorization / X-Token 已经过期了
	if hd := h.sniffSessionHeaders(ctx, task, target, urls); len(hd) > 0 {
		sess.headers = hd
	}
	return sess, nil
}

// runPrelogin 跑一次前置登录（UI 自动化录制），跑完把这个站点的会话缓存作废。
func (h *Harvester) runPrelogin(ctx context.Context, runID string, task *model.HarvestTask) error {
	if task.Prelogin == nil || !task.Prelogin.Active() {
		return nil
	}
	pl := task.Prelogin.Effective()
	label := strings.TrimSpace(pl.RecordingName)
	if label == "" {
		label = pl.RecordingID
	}
	h.logf(runID, "info", "▶ 前置登录：执行「%s」（超时 %ds）", label, pl.TimeoutSeconds)
	start := time.Now()
	if err := h.runPreloginQuiet(ctx, task); err != nil {
		return err
	}
	h.logf(runID, "info", "前置登录完成（%s），会话已刷新", time.Since(start).Round(time.Millisecond))
	return nil
}

// runPreloginQuiet 实际执行前置登录（不算日志，跑完作废会话缓存），运行与自检共用。
func (h *Harvester) runPreloginQuiet(ctx context.Context, task *model.HarvestTask) error {
	pl := task.Prelogin.Effective()
	if h.prelogin == nil {
		return fmt.Errorf("服务没有装配 UI 自动化执行器，无法执行前置登录")
	}
	// 登录流程的变量：录制时密码框被脱敏成 {{password}}，必须注入真实值才能真的登进去
	dataset := h.preloginDataset(pl)
	pctx, cancel := context.WithTimeout(ctx, time.Duration(pl.TimeoutSeconds)*time.Second)
	defer cancel()
	start := time.Now()
	loginRunID, err := h.prelogin.RunRecordingSync(pctx, pl.RecordingID, "采集前置登录", dataset)
	// 登录之后 Cookie / token 都变了，旧缓存必须作废，后面 resolveSession 会重新导出
	h.invalidateSession(task)
	if err != nil {
		if loginRunID != "" {
			return fmt.Errorf("前置登录失败（%s）：%v；去「UI 自动化 → 运行记录」打开 %s 看截图和逐步结果",
				time.Since(start).Round(time.Millisecond), err, loginRunID)
		}
		return fmt.Errorf("前置登录失败：%w", err)
	}
	return nil
}

// PreloginVars 展开「登录用变量」（API 手动触发前置登录时复用同一套逻辑）。
func (h *Harvester) PreloginVars(pl model.HarvestPrelogin) map[string]string {
	return h.preloginDataset(pl)
}

// preloginDataset 把任务里配的「登录用变量」展开成 UI 自动化数据（支持 {{env:XXX}}）。
func (h *Harvester) preloginDataset(pl model.HarvestPrelogin) map[string]string {
	ds := make(map[string]string, len(pl.Vars))
	vars := sessionVars(resolvedSession{})
	for k, v := range pl.Vars {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		ds[key] = strings.TrimSpace(expand(v, vars))
	}
	return ds
}

// invalidateSession 登录之后 Cookie / 请求头都可能变了，把该站点的缓存清掉，下次重新导出。
func (h *Harvester) invalidateSession(task *model.HarvestTask) {
	host := sessionTarget(task)
	h.cookieMu.Lock()
	defer h.cookieMu.Unlock()
	if host == "" {
		h.cookies = map[string]cachedCookies{}
		h.tokens = map[string]cachedToken{}
		h.hdrs = map[string]cachedHeaders{}
		return
	}
	delete(h.cookies, host)
	delete(h.hdrs, host)
	for k := range h.tokens {
		if strings.HasPrefix(k, host+"|") {
			delete(h.tokens, k)
		}
	}
}

// mergeCommonParams 把任务级公共参数并进这次请求：
// GET/DELETE/HEAD 进 URL 查询串；POST/PUT/PATCH 并进 JSON 请求体。
// 接口自己（或分页）已经给了同名参数时以接口为准。
func mergeCommonParams(ep model.HarvestEndpoint, target, body string, sess resolvedSession) (string, string) {
	if len(sess.params) == 0 {
		return target, body
	}
	vars := sessionVars(sess)
	merged := make(map[string]string, len(sess.params))
	for k, v := range sess.params {
		merged[k] = expand(v, vars)
	}
	method := strings.ToUpper(strings.TrimSpace(ep.Method))
	if method == "" {
		method = "GET"
	}
	switch method {
	case "GET", "DELETE", "HEAD":
		u, err := url.Parse(target)
		if err != nil {
			return target, body
		}
		q := u.Query()
		for k, v := range merged {
			if _, ok := q[k]; ok {
				continue
			}
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
		return u.String(), body
	}
	// 带请求体的方法：并进 JSON 请求体
	if strings.TrimSpace(body) == "" {
		b, err := json.Marshal(merged)
		if err != nil {
			return target, body
		}
		return target, string(b)
	}
	obj := map[string]any{}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		return target, body // 不是 JSON（表单等）：不动它
	}
	for k, v := range merged {
		if _, ok := obj[k]; ok {
			continue
		}
		obj[k] = v
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return target, body
	}
	return target, string(b)
}

// runCommonScript 登录成功后在内置浏览器里跑「公共参数脚本」，把返回值并进公共参数 / 请求头。
//
// 返回真正生效的项（"参数:token" / "头:Authorization" 这种），方便自检展示。
func (h *Harvester) runCommonScript(ctx context.Context, runID string, task *model.HarvestTask, sess *resolvedSession) []string {
	if task.Common == nil || strings.TrimSpace(task.Common.Script) == "" || h.br == nil {
		return nil
	}
	target := sessionTarget(task)
	if target == "" {
		h.logf(runID, "warn", "公共参数脚本跳过：判断不了站点（填「站点」或至少一个接口）")
		return nil
	}
	openCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	page, err := h.br.OpenPage(openCtx, target)
	if err != nil {
		h.logf(runID, "warn", "公共参数脚本跳过：打不开 %s（%v）", target, err)
		return nil
	}
	defer page.Close(context.Background())

	raw, err := page.Eval(openCtx, task.Common.Script)
	if err != nil {
		h.logf(runID, "warn", "公共参数脚本执行失败：%v", err)
		return nil
	}
	text := strings.TrimSpace(trimQuotes(string(raw)))
	if text == "" || text == "undefined" || text == "null" {
		h.logf(runID, "warn", "公共参数脚本没返回内容：脚本里要 return 一个 JSON 字符串，例如 "+
			`return JSON.stringify({params:{token:localStorage.getItem('token')}})`)
		return nil
	}
	var out struct {
		Params      map[string]string `json:"params"`
		Headers     map[string]string `json:"headers"`
		Token       string            `json:"token"`
		AccessToken string            `json:"access_token"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		h.logf(runID, "warn", "公共参数脚本返回值不是 JSON：%s", previewText(text, 200))
		return nil
	}
	if sess.params == nil {
		sess.params = map[string]string{}
	}
	if sess.headers == nil {
		sess.headers = map[string]string{}
	}
	var names []string
	for k, v := range out.Params {
		sess.params[k] = v
		names = append(names, "参数:"+k)
	}
	for k, v := range out.Headers {
		sess.headers[k] = v
		names = append(names, "头:"+k)
	}
	if tk := orDefaultStr(strings.TrimSpace(out.Token), strings.TrimSpace(out.AccessToken)); tk != "" {
		sess.headers["Authorization"] = "Bearer " + tk
		names = append(names, "头:Authorization")
	}
	sort.Strings(names)
	if len(names) == 0 {
		h.logf(runID, "info", "公共参数脚本执行了，但没有返回可用的 params / headers / token")
		return nil
	}
	h.logf(runID, "info", "公共参数脚本已生效：%s", strings.Join(names, "、"))
	return names
}

// sessionTarget 会话要用的站点地址：优先 session.host，否则第一个启用接口的 origin。
func sessionTarget(task *model.HarvestTask) string {
	if v := strings.TrimSpace(task.Session.Host); v != "" {
		return v
	}
	for _, ep := range task.Endpoints {
		if ep.Enabled {
			if o := originOf(ep.URL); o != "" {
				return o
			}
		}
	}
	return ""
}

// refreshSessionToken 重新取一次 token（用于 401/403 后重试，或控制台「读取 Token」）。
//
// 浏览器模式强制重读、不吃缓存——token 跟着登录状态变的时候就是靠这个跟上；
// 手填模式重新展开一次 {{env:XXX}}（进程内改了环境变量也能生效）。
func (h *Harvester) refreshSessionToken(ctx context.Context, task *model.HarvestTask, sess *resolvedSession) error {
	if task.Session.Token == nil {
		return fmt.Errorf("任务没有配置 Token")
	}
	tk := task.Session.Token.Effective()
	switch tk.Source {
	case "browser":
		target := sessionTarget(task)
		if target == "" {
			return fmt.Errorf("无法确定站点地址：填「站点」或至少一个接口")
		}
		v, err := h.browserToken(ctx, target, tk.Storage, tk.Key, 0)
		if err != nil {
			return err
		}
		sess.token = v
		return nil
	case "manual":
		v := strings.TrimSpace(expand(tk.Value, sessionVars(*sess)))
		if v == "" {
			return fmt.Errorf("Token 手填值为空")
		}
		sess.token = v
		return nil
	case "login":
		// 强制重登（不吃缓存）：token 过期时最直接的修法
		fresh := tk
		fresh.CacheSeconds = 0
		return h.loginToken(ctx, task, fresh, sess)
	default:
		return fmt.Errorf("Token 来源是 %q，没有可刷新的值", tk.Source)
	}
}

// ProbeSession 立即按当前浏览器登录状态导一次会话（控制台确认「刷新好了没」用）。
func (h *Harvester) ProbeSession(ctx context.Context, task model.HarvestTask) (int, string, error) {
	task.Normalize()
	if strings.TrimSpace(task.Session.Host) == "" {
		task.Session.Host = sessionTarget(&task)
	}
	h.invalidateSession(&task)
	sess, err := h.resolveSession(ctx, &task)
	if err != nil {
		return 0, "", err
	}
	return sess.cookieCount, sess.token, nil
}

// ProbeToken 现在读一次 token（控制台「读取 Token」按钮用），返回注入头、前缀与实际值。
func (h *Harvester) ProbeToken(ctx context.Context, task model.HarvestTask) (header, prefix, token string, err error) {
	task.Normalize()
	sess := sessionScaffold(&task)
	if task.Session.Token == nil || !task.Session.Token.Enabled() {
		return "", "", "", fmt.Errorf("任务没有配置 Token（会话区块的「Token 来源」还是空的）")
	}
	tk := task.Session.Token.Effective()
	sess.tokenHeader = tk.Header
	sess.tokenPrefix = tk.Prefix
	if err := h.refreshSessionToken(ctx, &task, &sess); err != nil {
		return "", "", "", err
	}
	return tk.Header, tk.Prefix, sess.token, nil
}

// resolveToken 取 token 并写进 sess：manual 手填（支持 {{env:XXX}}）/ browser 从内置浏览器存储读 /
// login 调登录接口换（还会把登录返回的 Cookie 一起带上）。
//
// token 型站点（SPA + localStorage，请求带 Authorization: Bearer xxx）靠它，和 Cookie 模式互不影响。
func (h *Harvester) resolveToken(ctx context.Context, task *model.HarvestTask, sess *resolvedSession) error {
	if task.Session.Token == nil {
		return nil
	}
	tk := task.Session.Token.Effective()
	if !tk.Enabled() {
		return nil
	}
	sess.tokenHeader = tk.Header
	sess.tokenPrefix = tk.Prefix

	switch tk.Source {
	case "manual":
		v := strings.TrimSpace(expand(tk.Value, sessionVars(*sess)))
		if v == "" {
			return fmt.Errorf("Token 手填值为空（可写 {{env:XXX}} 从环境变量读）")
		}
		sess.token = v
		return nil
	case "browser":
		key := strings.TrimSpace(tk.Key)
		if key == "" {
			return fmt.Errorf("从浏览器读 Token 需要填存储键名（如 token / access_token）")
		}
		target := sessionTarget(task)
		if target == "" {
			return fmt.Errorf("读取 Token 需要能确定站点地址：填「站点」或至少一个接口")
		}
		v, err := h.browserToken(ctx, target, tk.Storage, key, time.Duration(tk.CacheSeconds)*time.Second)
		if err != nil {
			return err
		}
		sess.token = v
		return nil
	case "login":
		return h.loginToken(ctx, task, tk, sess)
	default:
		return fmt.Errorf("未知的 Token 来源 %q", tk.Source)
	}
}

// browserToken 从内置浏览器存储（localStorage / sessionStorage）里读 token。
//
// ttl > 0 才走跨运行缓存；ttl == 0 表示每次重新读——token 会随登录状态变化时用这个。
func (h *Harvester) browserToken(ctx context.Context, origin, storage, key string, ttl time.Duration) (string, error) {
	store := "localStorage"
	if strings.EqualFold(strings.TrimSpace(storage), "sessionStorage") {
		store = "sessionStorage"
	}
	cacheKey := origin + "|" + store + "|" + key
	if ttl > 0 {
		h.cookieMu.Lock()
		if c, ok := h.tokens[cacheKey]; ok && time.Since(c.fetched) < ttl {
			v := c.value
			h.cookieMu.Unlock()
			return v, nil
		}
		h.cookieMu.Unlock()
	}
	openCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	pageURL := strings.TrimRight(origin, "/") + "/"
	page, err := h.br.OpenPage(openCtx, pageURL)
	if err != nil {
		return "", fmt.Errorf("启动内置浏览器失败（%s）：%w；可改用 Token 手填（配合 {{env:XXX}}）", origin, err)
	}
	defer page.Close(context.Background())
	// localStorage 必须同源才能读：OpenPage 已导航，这里再等一次 load（SPA 常在首屏之后才写入 token）
	if err := page.Navigate(openCtx, pageURL); err != nil {
		return "", fmt.Errorf("打开 %s 失败：%w", origin, err)
	}
	expr := fmt.Sprintf("(function(){try{return %s.getItem(%q)||''}catch(e){return ''}})()", store, key)
	// SPA 常常是登录接口回来之后才写 localStorage，load 事件之后再多试几次
	var v string
	for i := 0; i < 4; i++ {
		var raw string
		if err := page.EvalInto(openCtx, expr, &raw); err != nil {
			return "", fmt.Errorf("读取 %s.getItem(%q) 失败：%w", store, key, err)
		}
		v = tokenFromValue(raw)
		if v != "" {
			break
		}
		if i < 3 {
			select {
			case <-openCtx.Done():
				return "", openCtx.Err()
			case <-time.After(700 * time.Millisecond):
			}
		}
	}
	if v == "" {
		return "", fmt.Errorf("内置浏览器里 %s 的 %s.getItem(%q) 是空的：先在内置浏览器登录一次，或改「Token 键名」", origin, store, key)
	}
	if ttl > 0 {
		h.cookieMu.Lock()
		h.tokens[cacheKey] = cachedToken{value: v, fetched: time.Now()}
		h.cookieMu.Unlock()
	}
	return v, nil
}

// loginToken 调登录接口换 token（不用开浏览器）：
// 发一次登录请求 → 从响应 JSON 的 TokenPath 取 token → 顺便把 Set-Cookie 带进采集会话。
func (h *Harvester) loginToken(ctx context.Context, task *model.HarvestTask, tk model.HarvestToken, sess *resolvedSession) error {
	if tk.Login == nil || strings.TrimSpace(tk.Login.URL) == "" {
		return fmt.Errorf("Token 来源是「登录接口」，但没填登录接口地址")
	}
	lg := tk.Login.Effective()
	loginURL := strings.TrimSpace(expand(lg.URL, sessionVars(*sess)))
	bodyText := expand(lg.Body, sessionVars(*sess))

	// 同一套登录配置 + 同样的请求体，在缓存有效期内复用
	cacheKey := "login|" + loginURL + "|" + bodyText
	ttl := time.Duration(tk.CacheSeconds) * time.Second
	if ttl > 0 {
		h.cookieMu.Lock()
		if c, ok := h.tokens[cacheKey]; ok && time.Since(c.fetched) < ttl {
			v := c.value
			h.cookieMu.Unlock()
			sess.token = v
			return nil
		}
		h.cookieMu.Unlock()
	}

	method := strings.ToUpper(strings.TrimSpace(lg.Method))
	if method == "" {
		method = http.MethodPost
	}
	var reader io.Reader
	if bodyText != "" {
		reader = strings.NewReader(bodyText)
	}
	req, err := http.NewRequestWithContext(ctx, method, loginURL, reader)
	if err != nil {
		return fmt.Errorf("登录请求构造失败：%w", err)
	}
	sessVars := sessionVars(*sess)
	for k, v := range lg.Headers {
		req.Header.Set(k, expand(v, sessVars))
	}
	if req.Header.Get("Content-Type") == "" && bodyText != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json, text/plain, */*")
	}
	if sess.userAgent != "" {
		req.Header.Set("User-Agent", sess.userAgent)
	}

	// 单独一个带 jar 的客户端：登录返回的 Set-Cookie 要留着给采集请求用
	jar, _ := cookiejar.New(nil)
	client := h.newClient(task.Throttle, resolvedSession{cookies: map[string][]*http.Cookie{}})
	client.Jar = jar

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("登录接口请求失败（%s）：%w", loginURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("登录接口返回 %d：%s", resp.StatusCode, previewText(string(raw), 300))
	}
	root, perr := ParseJSONBody(string(raw))
	if perr != nil {
		return fmt.Errorf("登录响应不是 JSON（%v）：%s", perr, previewText(string(raw), 300))
	}
	token := strings.TrimSpace(ExtractString(root, lg.TokenPath))
	if token == "" {
		return fmt.Errorf("登录响应里 %q 取不到 token（实际返回：%s）", lg.TokenPath, previewText(string(raw), 300))
	}
	sess.token = token

	// 登录顺手种下的 Cookie 也带给采集请求（同域才会带）
	if lg.UseCookies() {
		for _, ep := range task.Endpoints {
			origin := originOf(ep.URL)
			if origin == "" {
				continue
			}
			u, uerr := url.Parse(origin)
			if uerr != nil {
				continue
			}
			if cks := jar.Cookies(u); len(cks) > 0 {
				addCookies(sess.cookies, origin, cks)
				sess.cookieCount += len(cks)
			}
		}
	}

	if ttl > 0 {
		h.cookieMu.Lock()
		h.tokens[cacheKey] = cachedToken{value: token, fetched: time.Now()}
		h.cookieMu.Unlock()
	}
	return nil
}

// addCookies 往会话里追加 Cookie（同一站点多渠道来的就合并，不互相覆盖）。
func addCookies(dst map[string][]*http.Cookie, origin string, list []*http.Cookie) {
	if dst == nil || origin == "" || len(list) == 0 {
		return
	}
	dst[origin] = append(dst[origin], list...)
}

// tokenFromValue 有些站点把 token 存成 JSON 字符串（{"token":"…","expires":…}），这里顺手取出来。
func tokenFromValue(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "{") {
		if root, err := ParseJSONBody(v); err == nil {
			for _, k := range []string{"token", "access_token", "accessToken", "value", "jwt", "id_token"} {
				if s := strings.TrimSpace(ExtractString(root, k)); s != "" {
					return s
				}
			}
		}
	}
	return v
}

// cookieNameList 会话里带上的 Cookie 名（去重排序，只给运行日志看）。
func cookieNameList(sess resolvedSession) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range sess.cookies {
		for _, c := range list {
			if c == nil || seen[c.Name] {
				continue
			}
			seen[c.Name] = true
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

// sniffSessionHeaders 在浏览器里打开站点，抓一个真实接口请求，把它的请求头抄下来。
//
// 为什么需要：接口配置里存的是**录制那一刻**的 Authorization / X-Token，登录之后浏览器里的值已经变了；
// 不刷新就会一直用旧 token（表现为"看起来带了头，实际全是 401"）。
func (h *Harvester) sniffSessionHeaders(ctx context.Context, task *model.HarvestTask, host string, urls []string) map[string]string {
	if h.br == nil || !strings.HasPrefix(host, "http") || len(urls) == 0 {
		return nil
	}
	h.cookieMu.Lock()
	if c, ok := h.hdrs[host]; ok && time.Since(c.fetched) < 30*time.Minute {
		out := c.headers
		h.cookieMu.Unlock()
		return out
	}
	h.cookieMu.Unlock()

	openCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	page, err := h.br.OpenPage(openCtx, host)
	if err != nil {
		return nil
	}
	defer page.Close(context.Background())

	nc, err := page.StartNetCapture(browser.NetCaptureOptions{MaxEntries: 60, PageURL: host, AllHeaders: true})
	if err != nil {
		return nil
	}
	// 再导航一次：让页面按"刚打开"的状态起一遍首屏请求
	_ = page.Navigate(openCtx, host)

	var picked, fallback map[string]string
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		for _, it := range nc.Snapshot() {
			if hostMatchesAny(it.URL, urls) {
				if hd := pickSessionHeaders(it.ReqHeaders); len(hd) > 0 {
					picked = hd
					break
				}
				continue
			}
			// 兜底：同一站点（同父域）的接口请求也认，接口域和页面域不同时常用
			if fallback == nil && sameSiteHost(it.URL, urls) {
				if hd := pickSessionHeaders(it.ReqHeaders); len(hd) > 0 {
					fallback = hd
				}
			}
		}
		if len(picked) > 0 {
			break
		}
		select {
		case <-openCtx.Done():
			nc.Stop()
			return nil
		case <-time.After(400 * time.Millisecond):
		}
	}
	nc.Stop()
	if len(picked) == 0 {
		picked = fallback
	}
	if len(picked) == 0 {
		// 没抓到接口请求也别白开一次页面：至少把浏览器的 User-Agent 抄下来
		if ua := pageUA(ctx, page); ua != "" {
			picked = map[string]string{"user-agent": ua}
		}
	}
	if len(picked) == 0 {
		return nil
	}
	h.cookieMu.Lock()
	h.hdrs[host] = cachedHeaders{headers: picked, fetched: time.Now()}
	h.cookieMu.Unlock()
	return picked
}

// pageUA 取这个标签页的 User-Agent（没抓到请求头时也保证 UA 是浏览器的）。
func pageUA(ctx context.Context, page *browser.Page) string {
	uaCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := page.Eval(uaCtx, "navigator.userAgent")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(trimQuotes(string(raw)))
}

// pickSessionHeaders 从一条真实请求里挑出可以抄的请求头（Cookie 单独走 jar，不在这里）。
//
// 不只抄鉴权头：header_mode=all 时整份都要用，过滤留到 effectiveHeaders 里按模式做。
func pickSessionHeaders(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if strings.TrimSpace(v) == "" {
			continue
		}
		lk := strings.ToLower(strings.TrimSpace(k))
		switch lk {
		case "cookie", "content-length", "host", "connection", "accept-encoding",
			// Content-Type 属于"请求参数"（跟请求体走），接口自己配的更准，不能用别的请求的值覆盖
			"content-type",
			// 每次请求都要重算的头，抄下来反而会出错
			"x-timestamp", "x-nonce", "x-request-id", "x-trace-id":
			continue
		}
		if strings.HasPrefix(lk, "x-b3-") {
			continue
		}
		out[k] = v
	}
	return out
}

// hostMatchesAny 这条请求的 host 是不是我们关心的站点之一。
func hostMatchesAny(rawURL string, urls []string) bool {
	h := strings.ToLower(hostOf(rawURL))
	if h == "" {
		return false
	}
	for _, u := range urls {
		if strings.ToLower(hostOf(u)) == h {
			return true
		}
	}
	return false
}

// sameSiteHost 同父域（example.com 下面任意子域），用于接口域与页面域不同的兜底匹配。
func sameSiteHost(rawURL string, urls []string) bool {
	base := lastTwoLabels(strings.ToLower(hostOf(rawURL)))
	if base == "" {
		return false
	}
	for _, u := range urls {
		if lastTwoLabels(strings.ToLower(hostOf(u))) == base {
			return true
		}
	}
	return false
}

// lastTwoLabels 取 host 的最后两段（a.b.example.com → example.com）。
func lastTwoLabels(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return host
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// browserCookies 从内置浏览器导出 Cookie（30 分钟内复用，避免每次跑都拉一次浏览器）。
//
// 先按 URL 精确取（Network.getCookies），拿不到就退回全量取再按域名过滤（Storage.getCookies）——
// 前置登录刚跑完、登录页在别的子域、或页面还没落地时，前者可能取不到。
func (h *Harvester) browserCookies(ctx context.Context, host string, urls []string) ([]*http.Cookie, error) {
	h.cookieMu.Lock()
	if c, ok := h.cookies[host]; ok && time.Since(c.fetched) < 30*time.Minute {
		h.cookieMu.Unlock()
		return c.list, nil
	}
	h.cookieMu.Unlock()

	openCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	// 直接开站点首页而不是 about:blank：页面落地后再取，登录态更稳
	pageURL := host
	if !strings.HasPrefix(pageURL, "http") {
		pageURL = "about:blank"
	}
	page, err := h.br.OpenPage(openCtx, pageURL)
	if err != nil {
		return nil, fmt.Errorf("启动内置浏览器失败（%s）：%w；检查 browser 配置（executable / headless / no_sandbox / debug_url）", host, err)
	}
	defer page.Close(context.Background())

	raw, err := page.Cookies(openCtx, urls)
	if err != nil {
		return nil, fmt.Errorf("读取浏览器 Cookie 失败：%w", err)
	}
	if len(raw) == 0 {
		// 兜底：整个 profile 全量取，再按站点域名过滤
		if all, aerr := page.AllCookies(openCtx); aerr == nil {
			raw = filterCookiesByHosts(all, urls)
		}
	}

	out := make([]*http.Cookie, 0, len(raw))
	for _, c := range raw {
		if c.Value == "" || strings.TrimSpace(c.Name) == "" {
			continue
		}
		hc := &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Secure: c.Secure, HttpOnly: c.HTTPOnly}
		if c.Expires > 0 {
			hc.Expires = time.Unix(int64(c.Expires), 0)
		}
		out = append(out, hc)
	}
	h.cookieMu.Lock()
	h.cookies[host] = cachedCookies{list: out, fetched: time.Now()}
	h.cookieMu.Unlock()
	return out, nil
}

// filterCookiesByHosts 只留域名能匹配到这些 URL 的 Cookie（全量取之后用）。
func filterCookiesByHosts(list []browser.Cookie, urls []string) []browser.Cookie {
	hosts := make([]string, 0, len(urls))
	for _, raw := range urls {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			hosts = append(hosts, strings.ToLower(u.Hostname()))
		}
	}
	if len(hosts) == 0 {
		return list
	}
	var out []browser.Cookie
	for _, c := range list {
		dom := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(c.Domain), "."))
		if dom == "" {
			continue
		}
		for _, h := range hosts {
			if dom == h || strings.HasSuffix(h, "."+dom) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

/* -------------------------------------------------------------- 限速/退避 */

// throttleWait 保证同一任务的相邻请求之间至少间隔 MinIntervalMS，并叠加随机抖动。
func (h *Harvester) throttleWait(ctx context.Context, taskID string, th model.HarvestThrottle) {
	min := th.MinIntervalMS
	if min < 200 {
		min = 200
	}
	base := jitterDuration(time.Duration(min)*time.Millisecond, th.JitterPercent)

	h.reqMu.Lock()
	last := h.lastReq[taskID]
	h.reqMu.Unlock()
	if !last.IsZero() {
		if wait := base - time.Since(last); wait > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		}
	}
	h.reqMu.Lock()
	h.lastReq[taskID] = time.Now()
	h.reqMu.Unlock()
}

func backoff(baseMS, attempt int) time.Duration {
	if baseMS <= 0 {
		baseMS = 1500
	}
	d := time.Duration(baseMS) * time.Millisecond
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	if d > 2*time.Minute {
		d = 2 * time.Minute
	}
	return jitterDuration(d, 20)
}

func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func friendlyNetErr(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "context deadline exceeded"):
		return fmt.Errorf("请求超时（可在任务里调大 timeout_ms）")
	case strings.Contains(msg, "no such host"):
		return fmt.Errorf("域名解析失败：%v", err)
	case strings.Contains(msg, "connection refused"):
		return fmt.Errorf("连接被拒绝：%v", err)
	case strings.Contains(msg, "certificate"):
		return fmt.Errorf("TLS 证书校验失败：%v", err)
	default:
		return err
	}
}

/* ---------------------------------------------------------------- 状态维护 */

func (h *Harvester) setStatus(runID, status string) {
	_, _ = h.store.MutateHarvestRun(runID, func(r *model.HarvestRun) { r.Status = status })
	h.bus.Publish(runID, bus.Event{Type: "status", Status: status})
}

func (h *Harvester) finish(runID, taskID, status, message string) {
	now := time.Now()
	_, _ = h.store.MutateHarvestRun(runID, func(r *model.HarvestRun) {
		r.Status = status
		r.FinishedAt = now
		if status == "failed" || status == "canceled" {
			r.Error = message
		}
	})
	level := "info"
	switch status {
	case "failed":
		level = "error"
	case "canceled":
		level = "warn"
	}
	h.logf(runID, level, "%s", message)

	// 回写任务状态与下一次触发时间（先读运行结果，再进任务写锁，避免锁嵌套）
	written := 0
	if r, err := h.store.GetHarvestRun(runID); err == nil {
		written = r.Written
	}
	_, _ = h.store.MutateHarvestTask(taskID, func(t *model.HarvestTask) {
		t.LastRunAt = now
		t.LastStatus = status
		t.RunCount++
		t.LastWritten = written
		if status == "failed" {
			t.FailStreak++
			t.LastError = message
			if t.FailStreak >= 3 {
				t.Status = "paused"
				t.LastError = message + "（已连续失败 3 次，任务自动暂停）"
			}
		} else {
			t.FailStreak = 0
			t.LastError = ""
		}
		if next, ok := NextRunAt(t.Schedule, now); ok {
			t.NextRunAt = next
		} else {
			t.NextRunAt = time.Time{}
		}
	})

	// 结果推送（企业微信群机器人）：异步，配了才发，失败只记日志
	if t, err := h.store.GetHarvestTask(taskID); err == nil {
		h.notifyRunAsync(t, runID, status, message)
	}

	h.bus.Publish(runID, bus.Event{Type: "status", Status: status})
	h.bus.Publish(runID, bus.Event{Type: "end", Status: status})
}

func (h *Harvester) logf(runID, level, format string, args ...any) {
	entry := model.LogEntry{TS: time.Now(), Level: level, Message: fmt.Sprintf(format, args...)}
	if runID == "" {
		log.Printf("[collect] %s: %s", entry.Level, entry.Message)
		return
	}
	_, _ = h.store.MutateHarvestRun(runID, func(r *model.HarvestRun) {
		r.Logs = append(r.Logs, entry)
		if len(r.Logs) > 3000 {
			r.Logs = r.Logs[len(r.Logs)-3000:]
		}
		// 字节兜底：报文不逐条截断，靠这里保证单次运行的日志不会把 data/uitest.json 撑爆
		total := 0
		for _, e := range r.Logs {
			total += len(e.Message) + 48
		}
		for total > maxRunLogBytes && len(r.Logs) > 50 {
			total -= len(r.Logs[0].Message) + 48
			r.Logs = r.Logs[1:]
		}
	})
	h.bus.Publish(runID, bus.Event{Type: "log", Log: &entry})
}

func triggerLabel(t string) string {
	if t == "schedule" {
		return "定时触发"
	}
	return "手动触发"
}

// varKeysText 变量名字列表（不打印值，账号密码不外泄到日志里）。
func varKeysText(ds map[string]string) string {
	keys := make([]string, 0, len(ds))
	for k := range ds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "、")
}

// cookieHeaderFor 按站点拼出 Cookie 请求头：先精确匹配 origin，再按域名后缀匹配
// （登录在 admin.x.com、接口在 openapi.x.com 时也能带上）。
func cookieHeaderFor(rawURL string, sess resolvedSession) string {
	origin := originOf(rawURL)
	host := strings.ToLower(hostOf(rawURL))
	var picked []*http.Cookie
	if list := sess.cookies[origin]; len(list) > 0 {
		picked = list
	} else {
		for o, list := range sess.cookies {
			oh := strings.ToLower(hostOf(o))
			if oh == "" || host == "" {
				continue
			}
			if host == oh || strings.HasSuffix(host, "."+oh) || strings.HasSuffix(oh, "."+host) {
				picked = append(picked, list...)
			}
		}
	}
	if len(picked) == 0 {
		return ""
	}
	parts := make([]string, 0, len(picked))
	for _, c := range picked {
		if c == nil || strings.TrimSpace(c.Name) == "" || c.Value == "" {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

/* ------------------------------------------------------------------ 自检 */

// tokenDigest 令牌只露头几个字符 + 长度，用于在自检里对比"换没换"。
func tokenDigest(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "（空）"
	}
	head := s
	if len(head) > 12 {
		head = head[:12]
	}
	return "「" + head + "…」（" + strconv.Itoa(len(s)) + " 字符）"
}

// SelfTestStep 自检的一步结果。
type SelfTestStep struct {
	Title  string `json:"title"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// SelfTestReport 采集任务自检报告。
type SelfTestReport struct {
	OK    bool           `json:"ok"`
	Steps []SelfTestStep `json:"steps"`
}

// SelfTest 把「跑前置登录 → 导出会话 → 真发一次请求」整条链路走一遍，逐步报告断在哪。
func (h *Harvester) SelfTest(ctx context.Context, task model.HarvestTask, withPrelogin bool) SelfTestReport {
	task.Normalize()
	rep := SelfTestReport{OK: true}
	add := func(title string, ok bool, format string, args ...any) {
		if !ok {
			rep.OK = false
		}
		rep.Steps = append(rep.Steps, SelfTestStep{Title: title, OK: ok, Detail: fmt.Sprintf(format, args...)})
	}

	// 1) 会话配置：只有一种——前置登录把浏览器登好，Cookie/请求头全部从浏览器取
	plText := "未配置（直接用浏览器当前登录态）"
	if task.Prelogin != nil && task.Prelogin.Active() {
		plText = "启用（" + task.Prelogin.ModeText() + "）"
	}
	add("会话配置", true, "浏览器会话 · 前置登录=%s · 站点=%q · 请求头=%s · 启用接口 %d 个",
		plText, orDefaultStr(task.Session.Host, "（自动取起始页/第一个接口）"),
		task.Session.EffectiveHeaderMode(), task.EndpointCount())

	// 1.5) 登录变量：录制时密码框被脱敏成 {{password}}，必须靠它注入
	if task.Prelogin != nil && task.Prelogin.Active() {
		pl := task.Prelogin.Effective()
		ds := h.preloginDataset(pl)
		switch {
		case len(ds) == 0:
			add("登录变量", false, "没配：录制时密码框是 {{password}}，回放会填字面量，登录必然失败")
		case strings.TrimSpace(ds["password"]) == "":
			add("登录变量", false, "有 %s，但缺 password", varKeysText(ds))
		default:
			add("登录变量", true, "已配 %s（值不显示）", varKeysText(ds))
		}
	}

	// 2) 前置登录
	if task.Prelogin != nil && task.Prelogin.Active() {
		if withPrelogin {
			start := time.Now()
			if err := h.runPreloginQuiet(ctx, &task); err != nil {
				add("前置登录", false, "%v", err)
			} else {
				add("前置登录", true, "执行完成（%s），旧会话缓存已作废", time.Since(start).Round(time.Millisecond))
				if strings.TrimSpace(task.Session.Host) == "" {
					task.Session.Host = sessionTarget(&task)
				}
			}
		} else {
			add("前置登录", true, "已配置，本次未执行（想一起测就勾「先跑前置登录」）")
		}
	} else {
		add("前置登录", true, "未配置，直接用当前浏览器登录态")
	}

	// 3) 浏览器里到底有哪些 Cookie
	target := sessionTarget(&task)
	var urls []string
	if target != "" {
		urls = append(urls, target)
	}
	for _, ep := range task.Endpoints {
		if o := originOf(ep.URL); o != "" && o != target {
			urls = append(urls, o)
		}
	}
	if len(urls) == 0 {
		add("浏览器 Cookie", false, "任务里没有可用的接口 URL，判断不了站点")
	} else if total, names, err := h.profileCookies(ctx, urls); err != nil {
		add("浏览器 Cookie", false, "读取失败：%v", err)
	} else if len(names) == 0 {
		add("浏览器 Cookie", false,
			"profile 里共 %d 个 Cookie，但没有一个匹配 %s —— 登录没成功，或接口域名和登录域名不同",
			total, strings.Join(urls, "、"))
	} else {
		add("浏览器 Cookie", true, "profile 共 %d 个，匹配 %d 个：%s", total, len(names), strings.Join(names, "、"))
	}

	// 4) 导出会话
	h.invalidateSession(&task)
	sess, sessErr := h.resolveSession(ctx, &task)
	if sessErr != nil {
		add("导出会话", false, "%v", sessErr)
		add("试发请求", false, "会话没准备好，跳过")
		return rep
	}
	add("导出会话", true, "Cookie %d 个（%s）",
		sess.cookieCount, orDefaultStr(strings.Join(cookieNameList(sess), "、"), "无"))

	// 3.5) 请求头接力：接口里录制下来的头 → 这次真正会发出去的值（浏览器共享了哪些）
	if ep0, ok := firstEnabledEndpoint(task); ok {
		mode := orDefaultStr(sess.headerMode, "all")
		eff := effectiveHeaders(ep0, sess)
		var changed []string
		for k, v := range ep0.Headers {
			if nv := eff.Get(k); nv != "" && nv != v {
				changed = append(changed, k+"："+tokenDigest(v)+" → "+tokenDigest(nv))
			}
		}
		sort.Strings(changed)
		switch {
		case len(changed) > 0:
			add("请求头共享", true, "模式=%s；%s", mode, strings.Join(changed, "；"))
		case len(sess.headers) > 0:
			add("请求头共享", true, "模式=%s；接口里的头与浏览器一致，另共享了 %s",
				mode, varKeysText(sess.headers))
		default:
			add("请求头共享", false,
				"模式=%s；没从浏览器抓到请求头，接口里的录制值原样发送", mode)
		}
	}

	// 4.5) 公共参数 + 登录后脚本（脚本优先级最高，会影响后面这次试发）
	if task.Common != nil {
		np, nh := len(task.Common.Params), len(task.Common.Headers)
		if np > 0 || nh > 0 {
			add("公共参数", true, "query/body %d 项（%s）、公共请求头 %d 项（%s）",
				np, orDefaultStr(varKeysText(task.Common.Params), "无"),
				nh, orDefaultStr(varKeysText(task.Common.Headers), "无"))
		} else {
			add("公共参数", true, "没配（只按接口自己的参数抓）")
		}
		if strings.TrimSpace(task.Common.Script) != "" {
			names := h.runCommonScript(ctx, "", &task, &sess)
			if len(names) > 0 {
				add("参数脚本", true, "登录后脚本生效：%s", strings.Join(names, "、"))
			} else {
				add("参数脚本", false, "登录后脚本没拿到可用结果（看服务端日志里的 warn 原因）")
			}
		}
	}

	// 5) 真发一次第一个启用的接口
	ep, ok := firstEnabledEndpoint(task)
	if !ok {
		add("试发请求", false, "没有启用的接口")
		return rep
	}
	pg := NewPaginator(ep.Pagination)
	now := time.Now()
	vars := map[string]string{
		"page": pg.Current(), "host": hostOf(ep.URL), "url": ep.URL, "endpoint": ep.Name,
		"now": now.Format("2006-01-02 15:04:05"), "date": now.Format("2006-01-02"),
		"datetime": now.Format("2006-01-02 15:04:05"), "timestamp": strconv.FormatInt(now.Unix(), 10),
		"uuid": randomUUID(),
	}
	targetURL, bodyText := pg.Apply(expand(ep.URL, vars), expand(ep.Body, vars))
	client := h.newClient(task.Throttle, sess)
	status, raw, _, ferr := h.fetch(ctx, client, ep, targetURL, bodyText, sess, model.HarvestThrottle{TimeoutMS: 20000}, nil)
	ckLen := len(cookieHeaderFor(targetURL, sess))
	method := strings.ToUpper(ep.Method)
	switch {
	case ferr != nil:
		add("试发请求", false, "%s %s → 失败：%v", method, targetURL, ferr)
	case status >= 200 && status < 300:
		add("试发请求", true, "%s %s → %d，本次携带 Cookie %d 字符，响应 %s：%s",
			method, targetURL, status, ckLen, sizeText(len(raw)), previewText(string(raw), 200))
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		add("试发请求", false, "%s %s → %d（登录态没生效）：%s",
			method, targetURL, status, previewText(string(raw), 200))
	default:
		add("试发请求", true, "%s %s → %d：%s", method, targetURL, status, previewText(string(raw), 200))
	}
	return rep
}

// profileCookies 浏览器 profile 里与该站点相关的 Cookie（名字@域名，用于自检）。
func (h *Harvester) profileCookies(ctx context.Context, urls []string) (int, []string, error) {
	openCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	page, err := h.br.OpenPage(openCtx, "about:blank")
	if err != nil {
		return 0, nil, fmt.Errorf("启动内置浏览器失败：%w", err)
	}
	defer page.Close(context.Background())
	all, err := page.AllCookies(openCtx)
	if err != nil {
		return 0, nil, err
	}
	matched := filterCookiesByHosts(all, urls)
	names := make([]string, 0, len(matched))
	for _, c := range matched {
		names = append(names, c.Name+"@"+strings.TrimPrefix(c.Domain, "."))
	}
	sort.Strings(names)
	return len(all), names, nil
}

// firstEnabledEndpoint 第一个启用的接口。
func firstEnabledEndpoint(task model.HarvestTask) (model.HarvestEndpoint, bool) {
	for _, ep := range task.Endpoints {
		if ep.Enabled {
			return ep, true
		}
	}
	return model.HarvestEndpoint{}, false
}

// orDefaultStr 空值兜底。
func orDefaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// sessionLabel 会话来源的中文名（现在只有一种：内置浏览器登录态）。
func sessionLabel(s model.HarvestSession) string {
	return "内置浏览器（Cookie + 请求头）"
}

func describeSink(cfg model.HarvestSink) string {
	switch strings.ToLower(cfg.Driver) {
	case "jsonl":
		return "JSONL 文件 " + cfg.Table + ".jsonl"
	case "json":
		return "JSON 文件 " + cfg.Table + ".json"
	case "xlsx", "excel":
		return "Excel 表格 " + cfg.Table + ".xlsx"
	case "mysql":
		return "MySQL 表 " + cfg.Table
	case "postgres", "postgresql", "pgsql":
		return "PostgreSQL 表 " + cfg.Table
	default:
		return "SQLite 表 " + cfg.Table
	}
}

/* ------------------------------------------------------------ 模板与 URL */

var tmplVarRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_@:\-]+)\s*\}\}`)

// expand 展开 {{变量}}：内置时间变量、{{page}}、{{env:NAME}} 等。
func expand(tpl string, vars map[string]string) string {
	if !strings.Contains(tpl, "{{") {
		return tpl
	}
	return tmplVarRe.ReplaceAllStringFunc(tpl, func(all string) string {
		m := tmplVarRe.FindStringSubmatch(all)
		if len(m) < 2 {
			return all
		}
		name := strings.TrimSpace(m[1])
		if v, ok := vars[name]; ok {
			return v
		}
		if strings.HasPrefix(name, "env:") {
			return os.Getenv(strings.TrimSpace(strings.TrimPrefix(name, "env:")))
		}
		return all
	})
}

func sessionVars(sess resolvedSession) map[string]string {
	now := time.Now()
	vars := map[string]string{
		"now":       now.Format("2006-01-02 15:04:05"),
		"date":      now.Format("2006-01-02"),
		"datetime":  now.Format("2006-01-02 15:04:05"),
		"timestamp": strconv.FormatInt(now.Unix(), 10),
		"uuid":      randomUUID(),
	}
	// 公共参数 / 登录后脚本产出的值也能当变量用：URL 里可以写 ?token={{token}}
	for k, v := range sess.params {
		if _, exists := vars[k]; !exists {
			vars[k] = v
		}
	}
	return vars
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}

func originOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func setQueryParam(raw, name, value string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set(name, value)
	u.RawQuery = q.Encode()
	return u.String()
}

func randomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// truncate 截断到 max 字节：按 UTF-8 边界回退，避免把中文/emoji 切成半个字符
// （日志是 JSON 序列化的，半个字符会变成 U+FFFD，看起来像乱码）。
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

/* ------------------------------------------------------------ 试抓与预览 */

// PreviewResult 试抓（不写库）结果，给"先看数据长什么样再决定落库"用。
type PreviewResult struct {
	EndpointID  string               `json:"endpoint_id"`
	Name        string               `json:"name"`
	URL         string               `json:"url"`
	Status      int                  `json:"status"`
	Records     int                  `json:"records"`
	RecordsPath string               `json:"records_path"`
	Columns     []string             `json:"columns"`
	Rows        []map[string]any     `json:"rows"`
	Fields      []model.HarvestField `json:"fields"`
	// Pagination 分页策略描述（试抓只取第一页，不会真翻页）
	Pagination string `json:"pagination,omitempty"`
	// Total / EstimatedPages 响应里能读到总数时给出"预计抓多少条 / 多少页"
	Total          int    `json:"total,omitempty"`
	EstimatedPages int    `json:"estimated_pages,omitempty"`
	Error          string `json:"error,omitempty"`
	Hint           string `json:"hint,omitempty"`
	// Warning 非致命提示（例如没拿到登录态、按无登录态试抓），不影响结果本身
	Warning string `json:"warning,omitempty"`
	// Request / Response 这次试抓真正发出去、收回来的报文（Cookie / Authorization 等已脱敏）
	Request  *PreviewRequestInfo  `json:"request,omitempty"`
	Response *PreviewResponseInfo `json:"response,omitempty"`
	// Process 这次试抓干了什么：会话 → 公共参数 → 变量 → 分页 → 请求 → 响应
	Process []string `json:"process,omitempty"`
	// Session 这次试抓用的登录态摘要（Cookie 个数、浏览器头个数）
	Session string `json:"session,omitempty"`
}

// PreviewRequestInfo 试抓实际发出去的请求。请求头 / query / 变量里的敏感值都已打码，
// 请求体超过 1MB 会截断，方便直接贴给别人排查而不会泄漏登录态。
type PreviewRequestInfo struct {
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        string            `json:"body,omitempty"`
	QueryParams map[string]string `json:"query_params,omitempty"`
	Vars        map[string]string `json:"vars,omitempty"`
	Retries     int               `json:"retries"`
	DurationMS  int64             `json:"duration_ms"`
}

// PreviewResponseInfo 试抓实际收到的响应（响应头脱敏，响应体只保留前 2000 字节）。
type PreviewResponseInfo struct {
	Status      int               `json:"status"`
	StatusText  string            `json:"status_text,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Size        int               `json:"size"`
	BodyPreview string            `json:"body_preview,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
}

// Preview 试抓一次并返回解析出的前 limit 行（不写库）。
// limit<=0 取 5 行；endpointID 非空时只抓这一个接口。
//
// 除了数据本身，还会带上这一次请求的完整过程：请求方法 / URL / 请求头 / 请求体 /
// query 参数 / 展开后的变量 / 重试次数 / 耗时，以及响应的状态、响应头、大小和响应体预览。
// Cookie、Authorization、password 这类敏感值一律打码，可以直接截图或复制给同事排查。
func (h *Harvester) Preview(ctx context.Context, task model.HarvestTask, limit int, endpointID string) []PreviewResult {
	task.Normalize()
	if limit <= 0 {
		limit = 5
	}
	sess, sessErr := h.resolveSession(ctx, &task)
	warning := ""
	if sessErr != nil {
		// 与正式运行（execute）保持一致：拿不到登录态也照样试抓。
		// 公开接口本来就不需要登录，硬拦下来只会让人以为「试抓坏了」；
		// 真需要登录的接口，试抓会直接显示 401/403，反而更直观。
		sess = sessionScaffold(&task)
		warning = "没拿到登录态（" + sessErr.Error() + "）；下面按无登录态试抓，返回 401/403 说明这个接口需要登录"
	}
	// 与正式运行保持一致：先把「登录后脚本」跑一遍，否则试抓看不到脚本注入的参数
	scriptRan := false
	if task.Common != nil && strings.TrimSpace(task.Common.Script) != "" {
		h.runCommonScript(ctx, "", &task, &sess)
		scriptRan = h.br != nil
	}
	client := h.newClient(task.Throttle, sess)

	// 各接口共用的准备步骤：复制明细时每个接口都能独立看懂
	prep := make([]string, 0, 4)
	if sessErr != nil {
		prep = append(prep, "会话：无登录态（"+sessErr.Error()+"）")
	} else {
		prep = append(prep, "会话："+sessionLabel(task.Session)+"（Cookie "+strconv.Itoa(sess.cookieCount)+" 个）")
	}
	if len(sess.headers) > 0 {
		prep = append(prep, fmt.Sprintf("登录态请求头：从浏览器取到 %d 个（Authorization / Cookie 等敏感值已脱敏）", len(sess.headers)))
	}
	if len(sess.params) > 0 {
		prep = append(prep, fmt.Sprintf("公共参数：%d 个（%s）；GET/DELETE 进 query，POST/PUT/PATCH 进 JSON 请求体",
			len(sess.params), strings.Join(sortedStringKeys(sess.params), "、")))
	}
	if task.Common != nil && strings.TrimSpace(task.Common.Script) != "" {
		if scriptRan {
			prep = append(prep, "公共脚本：已在内置浏览器里执行，产出的参数 / 请求头已并入")
		} else {
			prep = append(prep, "公共脚本：跳过（内置浏览器不可用）")
		}
	}

	var out []PreviewResult
	matched := false
	first := true
	for _, ep := range task.Endpoints {
		if !ep.Enabled {
			continue
		}
		if endpointID != "" && ep.ID != endpointID {
			continue
		}
		matched = true
		sel := ep
		pg := NewPaginatorFor(ep.Pagination, ep.Method)
		vars := sessionVars(sess)
		vars["page"] = pg.Current()
		vars["host"] = hostOf(ep.URL)
		vars["url"] = ep.URL
		vars["endpoint"] = ep.Name
		if !first {
			h.throttleWait(ctx, task.ID, task.Throttle)
		}
		first = false
		target, reqBody := pg.Apply(expand(ep.URL, vars), expand(ep.Body, vars))
		var tr fetchTrace
		started := time.Now()
		status, raw, retries, ferr := h.fetch(ctx, client, sel, target, reqBody, sess, task.Throttle, &tr)
		elapsed := time.Since(started).Milliseconds()

		pr := PreviewResult{EndpointID: ep.ID, Name: ep.Name, URL: target, Status: status, Pagination: pg.Describe()}
		pr.Request = buildPreviewRequest(sel, target, reqBody, sess, pg, &tr, retries, elapsed)
		pr.Response = buildPreviewResponse(status, &tr, raw)
		pr.Session = sessionSummary(task.Session, sess)
		pr.Process = append(append([]string{}, prep...), endpointProcess(sel, pg, vars, &tr, retries, elapsed, reqBody)...)

		if ferr != nil {
			pr.Error = ferr.Error()
			pr.Hint = "检查 URL / 会话 / 限流状态"
			out = append(out, pr)
			continue
		}
		root, perr := ParseJSONBody(string(raw))
		if perr != nil {
			pr.Error = "响应不是 JSON：" + perr.Error()
			pr.Rows = []map[string]any{{"raw": truncate(string(raw), 500)}}
			out = append(out, pr)
			continue
		}
		records, path, rerr := RecordsFrom(root, ep.RecordsPath)
		if rerr != nil {
			pr.Error = rerr.Error()
			out = append(out, pr)
			continue
		}
		pr.RecordsPath = path
		pr.Records = len(records)
		// 用第一页的响应反推"预计还有多少页"，让你试抓时就知道会抓多少
		if total, ok := ExtractInt(root, pg.pag.TotalPath); ok && pg.pag.PageSize > 0 && total > 0 {
			pr.EstimatedPages = (total + pg.pag.PageSize - 1) / pg.pag.PageSize
			pr.Total = total
		}
		if LooksPaginatedButUnknown(pg.pag, root, records) {
			pr.Hint = "响应里 total 大于本页条数，说明还有更多数据，但没识别出翻页参数——请在任务里手工指定分页参数名"
		}
		fields := ep.Fields
		if len(fields) == 0 {
			fields = SuggestFields(records, 5)
		}
		pr.Fields = fields
		pr.Columns = Columns(fields)
		rows := make([]map[string]any, 0, limit)
		for i, rec := range records {
			if i >= limit {
				break
			}
			rows = append(rows, FlattenRow(rec, fields, vars))
		}
		pr.Rows = rows
		out = append(out, pr)
	}
	if endpointID != "" && !matched {
		// 指定接口不存在时接口层返回空数组，前端只会显示「没有结果」，很容易误判成"抓不到数据"
		out = append(out, PreviewResult{
			EndpointID: endpointID,
			Error:      "任务里没有这个接口",
			Hint:       "可能已经被删除；返回任务列表刷新后再试",
		})
	}
	if warning != "" {
		for i := range out {
			out[i].Warning = warning
		}
	}
	return out
}

// buildPreviewRequest 组装「实际发出去的请求」。以 fetch 记录下来的最终报文为准，
// 拿不到快照（理论上不会）时退回按配置推算的理想报文。
func buildPreviewRequest(ep model.HarvestEndpoint, target, body string, sess resolvedSession,
	pg *Paginator, tr *fetchTrace, retries int, durationMS int64) *PreviewRequestInfo {

	info := &PreviewRequestInfo{
		Method:     strings.ToUpper(strings.TrimSpace(orDefaultStr(ep.Method, "GET"))),
		URL:        target,
		Body:       maskRequestBody(body),
		Retries:    retries,
		DurationMS: durationMS,
	}
	if tr != nil {
		if tr.Method != "" {
			info.Method = tr.Method
		}
		if tr.URL != "" {
			info.URL = tr.URL
		}
		if len(tr.Headers) > 0 {
			info.Headers = maskHeaders(tr.Headers)
		}
	}
	if info.Headers == nil {
		info.Headers = maskHeaders(effectiveHeaders(ep, sess))
	}
	if u, err := url.Parse(info.URL); err == nil && u.RawQuery != "" {
		q := make(map[string]string, len(u.Query()))
		for k, vs := range u.Query() {
			v := ""
			if len(vs) > 0 {
				v = vs[0]
			}
			q[k] = maskIfSensitive(k, v)
		}
		info.QueryParams = q
	}
	info.Vars = previewVars(ep, sess, pg)
	return info
}

// previewVars 只展示这次请求真正会用到的变量（内置 now/uuid 这类每次都会变的略过）。
func previewVars(ep model.HarvestEndpoint, sess resolvedSession, pg *Paginator) map[string]string {
	out := make(map[string]string, len(sess.params)+4)
	for k, v := range sess.params {
		out[k] = maskIfSensitive(k, v)
	}
	out["page"] = pg.Current()
	out["host"] = hostOf(ep.URL)
	out["url"] = ep.URL
	out["endpoint"] = ep.Name
	return out
}

// buildPreviewResponse 组装「实际收到的响应」。
func buildPreviewResponse(status int, tr *fetchTrace, raw []byte) *PreviewResponseInfo {
	info := &PreviewResponseInfo{Status: status, Size: len(raw)}
	if tr != nil {
		if status == 0 && tr.Status != 0 {
			info.Status = tr.Status
		}
		if info.Size == 0 && tr.ResponseSize > 0 {
			info.Size = tr.ResponseSize
		}
		info.Headers = maskHeaders(tr.ResponseHeaders)
		if tr.ResponseHeaders != nil {
			info.ContentType = tr.ResponseHeaders.Get("Content-Type")
		}
	}
	if info.Status > 0 {
		info.StatusText = http.StatusText(info.Status)
	}
	if len(raw) > 0 {
		info.BodyPreview = maskBodyPreview(raw)
	}
	return info
}

// endpointProcess 单个接口的执行步骤（会话 / 公共参数由调用方拼在前面）。
func endpointProcess(ep model.HarvestEndpoint, pg *Paginator, vars map[string]string,
	tr *fetchTrace, retries int, elapsedMS int64, reqBody string) []string {

	out := make([]string, 0, 4)
	if len(vars) > 0 {
		out = append(out, fmt.Sprintf("变量：展开 %d 个（%s）", len(vars), strings.Join(sortedStringKeys(vars), "、")))
	}
	if pg.Enabled() {
		loc := "URL 查询参数"
		if pg.pag.In == "body" {
			loc = "JSON 请求体"
		}
		step := "分页：" + pg.Describe() + "；本页把 " + pg.pag.Param + "=" + pg.Current() + " 写进" + loc
		if pg.pag.PageSize > 0 && strings.TrimSpace(pg.pag.SizeParam) != "" {
			step += "（顺带 " + pg.pag.SizeParam + "=" + strconv.Itoa(pg.pag.PageSize) + "）"
		}
		out = append(out, step)
	} else {
		out = append(out, "分页：不分页（试抓只取第一页）")
	}
	method := strings.ToUpper(strings.TrimSpace(orDefaultStr(ep.Method, "GET")))
	target := ""
	ct := ""
	respSize := 0
	respStatus := 0
	attempts := 1
	if tr != nil {
		if tr.Method != "" {
			method = tr.Method
		}
		target = tr.URL
		respSize = tr.ResponseSize
		respStatus = tr.Status
		if tr.Headers != nil {
			ct = tr.Headers.Get("Content-Type")
		}
		if tr.Attempts > 0 {
			attempts = tr.Attempts
		}
	}
	reqLine := "请求：" + method + " " + target
	if ct != "" {
		reqLine += "（Content-Type: " + ct + "）"
	}
	if n := len(strings.TrimSpace(reqBody)); n > 0 {
		reqLine += " · 请求体 " + sizeText(n)
	}
	out = append(out, reqLine)
	statusText := http.StatusText(respStatus)
	if statusText != "" {
		statusText = " " + statusText
	}
	out = append(out, fmt.Sprintf("响应：%d%s · %s · %d ms", respStatus, statusText, sizeText(respSize), elapsedMS))
	if retries > 0 {
		out = append(out, fmt.Sprintf("重试：%d 次（共发出 %d 次请求）", retries, attempts))
	}
	return out
}

// sessionSummary 登录态一句话摘要。
func sessionSummary(s model.HarvestSession, sess resolvedSession) string {
	parts := []string{sessionLabel(s)}
	if sess.cookieCount > 0 {
		parts = append(parts, "Cookie "+strconv.Itoa(sess.cookieCount)+" 个")
	}
	if len(sess.headers) > 0 {
		parts = append(parts, "浏览器头 "+strconv.Itoa(len(sess.headers))+" 个")
	}
	if sess.referer != "" {
		parts = append(parts, "Referer "+sess.referer)
	}
	return strings.Join(parts, " · ")
}

/* ------------------------------------------------------ 试抓明细的脱敏 */

// maskSecret 保留首尾少量字符，中间打码：既够确认"是不是同一个值"，又不泄漏明文。
func maskSecret(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	r := []rune(v)
	switch {
	case len(r) <= 6:
		return "***"
	case len(r) <= 16:
		return string(r[:2]) + "…" + string(r[len(r)-2:])
	default:
		return string(r[:4]) + "…" + string(r[len(r)-4:])
	}
}

// maskAuthValue 保留认证方案前缀（Bearer / Basic …），只对凭据本体打码。
func maskAuthValue(v string) string {
	for _, p := range []string{"Bearer ", "bearer ", "Basic ", "basic ", "Token ", "token ", "JWT "} {
		if strings.HasPrefix(v, p) {
			return p + maskSecret(strings.TrimPrefix(v, p))
		}
	}
	return maskSecret(v)
}

// sensitiveHeader 展示时要打码的请求/响应头。
func sensitiveHeader(name string) bool {
	lk := strings.ToLower(strings.TrimSpace(name))
	switch lk {
	case "cookie", "set-cookie", "authorization", "proxy-authorization", "token",
		"x-token", "x-auth-token", "x-access-token", "x-csrf-token", "x-xsrf-token",
		"blade-auth", "authenticity-token", "x-api-key", "api-key":
		return true
	}
	return strings.Contains(lk, "auth") || strings.Contains(lk, "token") ||
		strings.Contains(lk, "secret") || strings.Contains(lk, "session") ||
		strings.Contains(lk, "api-key") || strings.Contains(lk, "apikey")
}

// maskHeaders 把头表转成"可直接展示"的字符串表（值已脱敏）。
func maskHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vs := range h {
		v := strings.Join(vs, ", ")
		out[k] = maskHeaderValue(k, v)
	}
	return out
}

func maskHeaderValue(name, v string) string {
	lk := strings.ToLower(strings.TrimSpace(name))
	switch lk {
	case "cookie", "set-cookie":
		return maskCookieHeader(v)
	}
	if sensitiveHeader(name) {
		return maskAuthValue(v)
	}
	return v
}

// maskCookieHeader 只给 Cookie 的值打码，保留 Cookie 名（和 Set-Cookie 的属性）。
func maskCookieHeader(v string) string {
	parts := strings.Split(v, ";")
	for i, p := range parts {
		item := strings.TrimSpace(p)
		if item == "" {
			continue
		}
		idx := strings.Index(item, "=")
		if idx <= 0 {
			parts[i] = item
			continue
		}
		name, val := item[:idx], item[idx+1:]
		if i > 0 && isCookieAttr(name) {
			parts[i] = item
			continue
		}
		parts[i] = name + "=" + maskSecret(val)
	}
	return strings.Join(parts, "; ")
}

func isCookieAttr(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "path", "domain", "expires", "max-age", "samesite", "secure", "httponly", "priority":
		return true
	}
	return false
}

// maskIfSensitive 按参数名判断要不要打码（query / JSON 字段 / 变量通用）。
func maskIfSensitive(name, value string) string {
	lk := strings.ToLower(strings.TrimSpace(name))
	for _, k := range []string{"token", "password", "passwd", "pwd", "secret", "sign",
		"apikey", "api_key", "api-key", "access_key", "accesskey", "auth", "session", "ticket", "cookie"} {
		if strings.Contains(lk, k) {
			return maskSecret(value)
		}
	}
	return value
}

// maskRequestBody 请求体展示：JSON 按字段名递归打码，表单按参数名打码，其它原样（只做长度保护）。
func maskRequestBody(body string) string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return ""
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		var v any
		if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
			if b, err := json.Marshal(maskJSONValue(v, 0)); err == nil {
				return clipRequest(string(b))
			}
		}
		return clipRequest(body)
	}
	if strings.Contains(trimmed, "=") && !strings.ContainsAny(trimmed, "\n\r{") {
		if vals, err := url.ParseQuery(trimmed); err == nil {
			for k := range vals {
				vals.Set(k, maskIfSensitive(k, vals.Get(k)))
			}
			return vals.Encode()
		}
	}
	return clipRequest(body)
}

// maskBodyPreview 响应体预览：先打码再截断，避免首屏就把 token 露出来。
// 超过 64KB 的响应体不做 JSON 解析（省 CPU），直接截断展示。
func maskBodyPreview(raw []byte) string {
	const parseLimit = 64 << 10
	text := string(raw)
	if len(text) > parseLimit {
		return previewText(text, 2000)
	}
	trimmed := strings.TrimSpace(text)
	if trimmed != "" && (trimmed[0] == '{' || trimmed[0] == '[') {
		var v any
		if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
			if b, err := json.Marshal(maskJSONValue(v, 0)); err == nil {
				return previewText(string(b), 2000)
			}
		}
	}
	return previewText(text, 2000)
}

func maskJSONValue(v any, depth int) any {
	if depth > 8 {
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if s, ok := val.(string); ok {
				out[k] = maskIfSensitive(k, s)
				continue
			}
			out[k] = maskJSONValue(val, depth+1)
		}
		return out
	case []any:
		for i := range x {
			x[i] = maskJSONValue(x[i], depth+1)
		}
		return x
	}
	return v
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// AnalyzeResult 响应结构分析结果。
type AnalyzeResult struct {
	RecordsPath string               `json:"records_path"`
	Records     int                  `json:"records"`
	Fields      []model.HarvestField `json:"fields"`
	Columns     []string             `json:"columns"`
	Rows        []map[string]any     `json:"rows"`
	// Pagination 从这条请求推断出的分页策略（没有分页时为 none）
	Pagination     model.HarvestPagination `json:"pagination,omitempty"`
	PaginationText string                  `json:"pagination_text,omitempty"`
	Error          string                  `json:"error,omitempty"`
}

// AnalyzeBody 分析一段响应体：给出记录路径、候选字段、前几行预览。
func AnalyzeBody(body string, recordsPath string, limit int) AnalyzeResult {
	if limit <= 0 {
		limit = 5
	}
	root, err := ParseJSONBody(body)
	if err != nil {
		return AnalyzeResult{Error: "响应不是合法 JSON：" + err.Error()}
	}
	records, path, rerr := RecordsFrom(root, recordsPath)
	if rerr != nil {
		return AnalyzeResult{Error: rerr.Error()}
	}
	fields := SuggestFields(records, 5)
	res := AnalyzeResult{RecordsPath: path, Records: len(records), Fields: fields, Columns: Columns(fields)}
	for i, rec := range records {
		if i >= limit {
			break
		}
		res.Rows = append(res.Rows, FlattenRow(rec, fields, map[string]string{}))
	}
	if res.Rows == nil {
		res.Rows = []map[string]any{}
	}
	return res
}

/* -------------------------------------------------- 按接口解析的落库目标（sink） */

// endpointSinks 按接口解析落库目标：不同接口可以写不同表 / 不同文件，
// 但同一个目标在一次运行里只开一次连接（连接池复用）。
type endpointSinks struct {
	task    model.HarvestTask
	dataDir string
	opened  map[string]Sink
	order   []string
	// logf 把 SQL 语句写进运行日志（nil = 不记）
	logf func(format string, args ...any)
	// sqlCount 已经打了多少条 SQL（防止把日志撑爆）
	sqlCount int
}

// maxSQLLogPerRun 一次运行最多打多少条 SQL（再多就只留摘要）。
const maxSQLLogPerRun = 60

// sqlLogger 给 sink 用的 SQL 钩子：把实际执行的语句写进运行日志。
func (p *endpointSinks) sqlLogger() func(string) {
	if p.logf == nil {
		return nil
	}
	return func(sql string) {
		if p.sqlCount >= maxSQLLogPerRun {
			return
		}
		p.sqlCount++
		if p.sqlCount == maxSQLLogPerRun {
			p.logf("SQL：已达本次运行打印上限（%d 条），后续语句不再逐条打印", maxSQLLogPerRun)
		}
		p.logf("SQL %s", sql)
	}
}

func newEndpointSinks(task model.HarvestTask, dataDir string) *endpointSinks {
	return &endpointSinks{task: task, dataDir: dataDir, opened: map[string]Sink{}}
}

func sinkKey(cfg model.HarvestSink) string {
	return strings.ToLower(cfg.Driver) + "|" + cfg.DSN + "|" + cfg.Table
}

// For 取这个接口要用的 sink（首次使用时才真正连接）。
func (p *endpointSinks) For(ep model.HarvestEndpoint) (Sink, error) {
	cfg := p.task.EndpointSink(ep)
	k := sinkKey(cfg)
	if s, ok := p.opened[k]; ok {
		return s, nil
	}
	s, err := OpenSink(cfg, p.dataDir)
	if err != nil {
		return nil, err
	}
	s = WithSQLLogger(s, p.sqlLogger())
	p.opened[k] = s
	p.order = append(p.order, k)
	return s, nil
}

// DescribeAll 人可读的写入目标（单目标给详细描述，多目标给汇总）。
func (p *endpointSinks) DescribeAll() string {
	if len(p.order) == 1 {
		return p.opened[p.order[0]].Describe()
	}
	if len(p.order) == 0 {
		// 还没打开任何目标：按配置推算一下（只用于日志展示）
		var descs []string
		seen := map[string]bool{}
		for _, ep := range p.task.Endpoints {
			if !ep.Enabled {
				continue
			}
			cfg := p.task.EndpointSink(ep)
			text := describeSink(cfg)
			if seen[text] {
				continue
			}
			seen[text] = true
			descs = append(descs, text)
		}
		if len(descs) == 1 {
			return descs[0]
		}
		return "按接口分别落库（" + fmt.Sprint(len(descs)) + " 个目标）"
	}
	return "按接口分别落库（" + fmt.Sprint(len(p.order)) + " 个目标）"
}

// Close 关闭所有已打开的 sink。
func (p *endpointSinks) Close() {
	for _, k := range p.order {
		if s, ok := p.opened[k]; ok {
			_ = s.Close()
		}
	}
}

/* ------------------------------------------------ 请求 / 响应写进运行日志 */

// 日志里的报文口径：
//   - 请求报文（请求行 + 请求头 + 请求体）**不省略也不截断**（只有超大保护：单条 > 1MB 才截断并标注）；
//   - 响应体按单条上限预览，整个运行还有一份总预算，超了只省略响应体（请求照旧完整）。
const (
	httpLogLimit    = 1024 << 10 // 1MB / 次运行：响应体总预算
	httpRespPreview = 32 << 10   // 单条响应体最多留 32KB
	httpReqMaxBytes = 1 << 20    // 单条请求体的大保护（正常请求体远小于这个值）
	// maxRunLogBytes 单次运行日志的字节兜底（超了从最老的日志开始丢，单条报文不截断）
	maxRunLogBytes = 2 << 20
)

// httpLogBudget 本次运行的响应体日志预算（单条执行协程内使用，不需要加锁）。
type httpLogBudget struct {
	left    int
	warned  bool
	skipped int
}

func newHTTPLogBudget() *httpLogBudget { return &httpLogBudget{left: httpLogLimit} }

func (b *httpLogBudget) take(n int) bool {
	if b == nil {
		return true
	}
	if b.left <= 0 || n > b.left {
		b.left = 0
		return false
	}
	b.left -= n
	return true
}

// logHTTP 把这一次请求与响应原样写进运行日志。
//
//   - 明文，不脱敏：Cookie / Authorization 都照原样打出来，方便确认登录态、参数是否真的生效；
//   - 每一次请求都记（分页抓 N 页就是 N 组），不是只留一个样本；
//   - **请求报文（请求行 / 请求头 / 请求体）永远完整**，不因预算省略；
//   - 响应体太大或本次运行响应体预算用完时，只省略响应体并标注原因。
func (h *Harvester) logHTTP(runID string, budget *httpLogBudget, ep model.HarvestEndpoint, target, body string,
	status int, raw []byte, sess resolvedSession, seq int) {

	method := strings.ToUpper(ep.Method)
	reqHead := requestHeadLine(ep, sess)
	reqBody := clipRequest(body)
	respBody := previewText(string(raw), httpRespPreview)

	// 1) 请求报文：完整打印（不被预算影响）
	msg := "→ " + method + " " + target + "　#" + strconv.Itoa(seq)
	if reqHead != "" {
		msg += "\n请求头  " + reqHead
	}
	if reqBody != "" {
		msg += "\n请求体  " + reqBody
	}
	h.logf(runID, "http", "%s", msg)

	// 2) 响应：体按单条上限 + 本次运行预算
	rmsg := "← " + strconv.Itoa(status) + "　" + sizeText(len(raw))
	switch {
	case respBody == "":
	case budget.take(len(respBody)):
		rmsg += "\n" + respBody
	default:
		budget.skipped++
		if !budget.warned {
			budget.warned = true
			h.logf(runID, "warn",
				"本次运行的响应体日志已达 %d KB 上限：后续只记响应摘要（请求报文仍然完整）",
				httpLogLimit>>10)
		}
		rmsg += "\n（响应体已省略，共 " + sizeText(len(raw)) + "；请求报文完整保留）"
	}
	h.logf(runID, "http", "%s", rmsg)
}

// clipRequest 请求体的大保护：正常请求体都远小于 1MB，只有极端情况才截断并标注。
func clipRequest(body string) string {
	if len(body) <= httpReqMaxBytes {
		return body
	}
	return body[:httpReqMaxBytes] +
		"\n…（请求体超过 " + strconv.Itoa(httpReqMaxBytes>>20) + "MB，已截断；可在「公共参数」或接口里精简）"
}

// effectiveHeaders 最终发出去的请求头，优先级：接口配置 → 浏览器实时头 → 新鲜 token。
//
// 关键点：录制时存下的 Authorization / blade-auth 都是**过期值**，必须被覆盖，
// 所以只要浏览器里嗅到了同名头就会被盖掉；配了 Token 来源时，鉴权类头一律换成新 token。
func effectiveHeaders(ep model.HarvestEndpoint, sess resolvedSession) http.Header {
	out := make(http.Header, len(ep.Headers)+len(sess.headers)+1)
	vars := sessionVars(sess)
	// 1) 任务级公共请求头（最低优先级）
	for k, v := range sess.commonHeaders {
		if strings.TrimSpace(v) == "" {
			continue
		}
		out.Set(k, expand(v, vars))
	}
	// 2) 接口自己的请求头（Cookie / Authorization / UA / Referer 这类登录态头一律不发，
	//    统一由前置登录后的浏览器会话提供，避免过期值与重复）
	for k, v := range ep.Headers {
		if isSessionHeader(k) {
			continue
		}
		out.Set(k, expand(v, vars))
	}

	mode := sess.headerMode
	if mode == "" {
		mode = "all"
	}
	// 3) 前置登录后浏览器里真实在用的请求头（覆盖同名项）
	if mode != "off" {
		for k, v := range sess.headers {
			if strings.TrimSpace(v) == "" {
				continue
			}
			if mode == "auth" && !isAuthHeader(k) {
				continue
			}
			out.Set(k, v)
		}
	}

	// 有新鲜 token：鉴权类头统一换成它（保留录制时的前缀风格，如 bearer / Bearer ）
	if sess.token != "" {
		hdr := sess.tokenHeader
		if hdr == "" {
			hdr = "Authorization"
		}
		want := sess.tokenPrefix + sess.token
		out.Set(hdr, want)
		// 其它鉴权类头（blade-auth / x-access-token…）也一起换，保留原前缀风格
		for k := range out {
			lk := strings.ToLower(k)
			if strings.EqualFold(k, hdr) || (!strings.Contains(lk, "auth") && !strings.Contains(lk, "token")) {
				continue
			}
			out.Set(k, keepScheme(out.Get(k), want))
		}
	}
	return out
}

// isSessionHeader 这些头由「前置登录 + 浏览器会话」统一提供：接口配置里存了也不发，
// 避免把录制那一刻的过期 Cookie / Authorization 一起带上去（也正是"Cookie 重复"的来源）。
func isSessionHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cookie", "authorization", "user-agent", "referer", "host",
		"content-length", "connection", "accept-encoding", "origin":
		return true
	}
	return false
}

// isAuthHeader 这个头是不是"登录态相关"（决定浏览器实时值要不要覆盖它）。
func isAuthHeader(name string) bool {
	lk := strings.ToLower(strings.TrimSpace(name))
	switch lk {
	case "authorization", "blade-auth", "token", "x-token", "x-auth-token", "x-access-token",
		"x-csrf-token", "x-xsrf-token", "origin", "referer", "user-agent":
		return true
	}
	return strings.Contains(lk, "token") || strings.Contains(lk, "auth") || strings.HasPrefix(lk, "x-")
}

// keepScheme 保留录制值的认证前缀风格（"Bearer xxx" / "bearer xxx" / 无前缀）。
//
// want 自己可能已经带了前缀（tokenPrefix，默认 "Bearer "），必须先摘掉再按
// 老值的风格重贴，否则会拼出 "bearer Bearer xxx" 这种废值。
func keepScheme(old, want string) string {
	schemes := []string{"bearer ", "bearer:", "basic "}
	body := strings.TrimSpace(want)
	wantLow := strings.ToLower(body)
	for _, p := range schemes {
		if strings.HasPrefix(wantLow, p) {
			body = strings.TrimSpace(body[len(p):])
			break
		}
	}
	low := strings.ToLower(strings.TrimSpace(old))
	for _, p := range schemes {
		if strings.HasPrefix(low, p) {
			return p + body
		}
	}
	return want
}

// hasHeader 判断已经拼好的头列表里有没有某一项（前缀匹配，如 "Cookie: "）。
func hasHeader(parts []string, prefix string) bool {
	for _, p := range parts {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// requestHeadLine 把这次请求真正会发出去的头拼成一行行文本（明文，日志用）。
func requestHeadLine(ep model.HarvestEndpoint, sess resolvedSession) string {
	var parts []string
	eff := effectiveHeaders(ep, sess)
	hasUA, hasRef := false, false
	for k, vs := range eff {
		v := ""
		if len(vs) > 0 {
			v = vs[0]
		}
		parts = append(parts, k+": "+v)
		switch strings.ToLower(k) {
		case "user-agent":
			hasUA = true
		case "referer":
			hasRef = true
		}
	}
	if sess.userAgent != "" && !hasUA {
		parts = append(parts, "User-Agent: "+sess.userAgent)
	}
	if sess.referer != "" && !hasRef {
		parts = append(parts, "Referer: "+sess.referer)
	}
	// Cookie：接口配置里已经带了就不再重复；否则按会话导出一次（同名只留一份，避免多个 origin 重复刷屏）
	if len(sess.cookies) > 0 && !hasHeader(parts, "Cookie: ") {
		seen := map[string]bool{}
		var cks []string
		for _, list := range sess.cookies {
			for _, c := range list {
				if c == nil || seen[c.Name] {
					continue
				}
				seen[c.Name] = true
				cks = append(cks, c.Name+"="+c.Value)
			}
		}
		sort.Strings(cks)
		if len(cks) > 0 {
			parts = append(parts, "Cookie: "+strings.Join(cks, "; "))
		}
	}
	// token 已经在 effectiveHeaders 里注入了，这里不再重复打印
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return strings.Join(parts, " | ")
}

// previewText 截断到 max 字节（按 UTF-8 边界切），并标注原始大小。
func previewText(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("…（共 %d 字节，已截断）", len(s))
}

// ClipText 截断到 max 字节（按 UTF-8 边界切）并标注原始大小，供 API / 控制台复用。
func ClipText(s string, max int) string { return previewText(s, max) }

// sizeText 人可读的字节数。
func sizeText(n int) string {
	switch {
	case n < 1024:
		return strconv.Itoa(n) + " B"
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
