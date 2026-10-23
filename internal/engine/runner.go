// Package engine 是执行引擎：把一条录制「跑」成一次真实浏览器操作。
//
// 一次执行 = 打开内置浏览器标签页 → 注入共享运行时 → 逐步执行（变量替换 + 多级定位
// + 自愈）→ 逐步落库并通过事件总线实时推送日志 → 收尾（截图/结论）。
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	jsruntime "uitest-server/runtime"

	"uitest-server/internal/browser"
	"uitest-server/internal/bus"
	"uitest-server/internal/model"
	"uitest-server/internal/store"
)

// Runner 执行引擎。
type Runner struct {
	store *store.DB
	br    *browser.Manager
	bus   *bus.Bus

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// New 创建执行引擎。
func New(db *store.DB, br *browser.Manager, b *bus.Bus) *Runner {
	return &Runner{store: db, br: br, bus: b, cancels: map[string]context.CancelFunc{}}
}

/* ------------------------------------------------------------ 生命周期 */

// Start 异步执行一次运行（runID 必须已存在于存储）。
func (r *Runner) Start(runID string) {
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancels[runID] = cancel
	r.mu.Unlock()
	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.cancels, runID)
			r.mu.Unlock()
			cancel()
		}()
		r.execute(ctx, runID)
	}()
}

// Cancel 取消一次运行。
func (r *Runner) Cancel(runID string) error {
	r.mu.Lock()
	cancel, ok := r.cancels[runID]
	r.mu.Unlock()
	if !ok {
		return errors.New("该运行不在执行中")
	}
	cancel()
	return nil
}

/* ---------------------------------------------------------------- 执行 */

// actionResult 对应 runtime/uitest-keys.js 里 runAction 的返回结构。
type actionResult struct {
	OK       bool     `json:"ok"`
	Message  string   `json:"message"`
	Used     string   `json:"used"`
	Healed   bool     `json:"healed"`
	Tried    []string `json:"tried"`
	CSS      string   `json:"css"`
	Tag      string   `json:"tag"`
	Label    string   `json:"label"`
	URL      string   `json:"url"`
	Navigate string   `json:"navigate"`
	Snippet  string   `json:"snippet"`
	UID      string   `json:"uid"`       // 命中元素的稳定 uid（供自动回写）
	TestAttr string   `json:"test_attr"` // 元素上的 data-testid 一类锚点
}

func (r *Runner) execute(ctx context.Context, runID string) {
	run, err := r.store.GetRun(runID)
	if err != nil {
		return
	}
	rec, err := r.store.GetRecording(run.RecordingID)
	if err != nil {
		r.finish(runID, "failed", "录制不存在: "+err.Error())
		return
	}

	_, _ = r.store.MutateRun(runID, func(run *model.Run) { run.TotalSteps = len(rec.Steps) })
	r.setStatus(runID, "running")
	r.logf(runID, "info", 0, "开始执行「%s」，共 %d 步，数据来源：%s", rec.Name, len(rec.Steps), run.DatasetSource)
	if len(run.Dataset) > 0 {
		r.logf(runID, "info", 0, "本次数据准备：%s", datasetSummary(run.Dataset))
	}

	openCtx, cancelOpen := context.WithTimeout(ctx, 150*time.Second)
	page, err := r.br.OpenPage(openCtx, "about:blank")
	cancelOpen()
	if err != nil {
		r.finish(runID, "failed", "启动内置浏览器失败："+err.Error())
		return
	}
	// 运行结束后怎么处理浏览器由设置决定（auto / close_tab / close_browser / keep）
	defer r.settleBrowser(runID, page)
	r.logf(runID, "info", 0, "内置浏览器已就绪，标签页 targetId=%s", page.TargetID)

	startURL := strings.TrimSpace(rec.StartURL)
	if startURL == "" {
		for _, s := range rec.Steps {
			if s.Action == "navigate" && strings.TrimSpace(s.Value) != "" {
				startURL = s.Value
				break
			}
		}
	}
	if startURL != "" {
		navCtx, cancelNav := context.WithTimeout(ctx, 60*time.Second)
		navErr := page.Navigate(navCtx, startURL)
		cancelNav()
		if navErr != nil {
			r.finish(runID, "failed", "打开起始页失败："+navErr.Error())
			return
		}
		r.logf(runID, "info", 0, "已打开起始页 %s", startURL)
	}
	if err := r.ensureRuntime(ctx, page); err != nil {
		r.finish(runID, "failed", "注入页面运行时失败："+err.Error())
		return
	}

	failed := 0
	for _, step := range rec.Steps {
		if ctx.Err() != nil {
			r.logf(runID, "warn", step.Index, "已取消，停止后续步骤")
			r.finish(runID, "canceled", "")
			return
		}
		res := r.execStep(ctx, runID, page, step, run.Dataset)
		if res.Status == "failed" {
			failed++
			if step.Optional {
				r.logf(runID, "warn", step.Index, "该步骤标记为可跳过，继续执行")
				continue
			}
			r.finish(runID, "failed", fmt.Sprintf("第 %d 步失败：%s", step.Index, res.Message))
			return
		}
		// 步骤间隔，给前端渲染/请求留出时间
		delay := time.Duration(r.stepDelayMS()) * time.Millisecond
		wait := delay
		if step.WaitMS > 0 {
			wait += time.Duration(step.WaitMS) * time.Millisecond
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		}
	}

	if failed > 0 {
		r.finish(runID, "passed", fmt.Sprintf("全部步骤执行完毕（%d 个失败步骤被标记为可跳过）", failed))
		return
	}
	r.finish(runID, "passed", "")
}

func (r *Runner) execStep(ctx context.Context, runID string, page *browser.Page, step model.Step, ds map[string]string) model.StepResult {
	started := time.Now()
	value := Substitute(step.Value, ds)
	annotation := step.Annotation
	if annotation == "" {
		annotation = step.Action
	}
	res := model.StepResult{
		Index:      step.Index,
		StepID:     step.ID,
		Action:     step.Action,
		Annotation: annotation,
		ValueUsed:  value,
		Status:     "passed",
	}
	r.logf(runID, "info", step.Index, "▶ %s", annotation)

	timeout := time.Duration(r.actionTimeoutMS()) * time.Millisecond
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var failure string
	switch step.Action {
	case "navigate":
		if strings.TrimSpace(value) == "" {
			failure = "导航步骤缺少 URL"
			break
		}
		if err := page.Navigate(stepCtx, value); err != nil {
			failure = "导航失败：" + err.Error()
			break
		}
		res.LocatorUsed = "url=" + value
		if err := r.ensureRuntime(stepCtx, page); err != nil {
			failure = "注入运行时失败：" + err.Error()
		}
	case "upload":
		failure = r.execUpload(stepCtx, page, step, value, &res)
	default:
		if err := r.ensureRuntime(stepCtx, page); err != nil {
			failure = "注入运行时失败：" + err.Error()
			break
		}
		req := map[string]any{
			"action":    step.Action,
			"locator":   step.Locator,
			"target":    step.Target,
			"value":     value,
			"timeoutMs": r.actionTimeoutMS(),
		}
		rawReq, _ := json.Marshal(req)
		var out actionResult
		expr := fmt.Sprintf(`(async () => {
  if (!window.__uitestKeys) return {ok:false, message:"页面运行时未注入（可能是 CSP 拦截或页面已跳走）"};
  return await window.__uitestKeys.runAction(%s);
})()`, rawReq)
		if err := page.EvalInto(stepCtx, expr, &out); err != nil {
			failure = "执行异常：" + err.Error()
			break
		}
		res.LocatorUsed = out.Used
		res.Healed = out.Healed
		if !out.OK {
			failure = out.Message
			if len(out.Tried) > 0 {
				res.HealFrom = strings.Join(out.Tried, " → ")
			}
			break
		}
		if out.Message != "" {
			res.Message = out.Message
		}
		if out.Healed {
			res.HealFrom = strings.Join(out.Tried, " → ")
			r.logf(runID, "warn", step.Index, "定位失效，已自愈：%s（原定位 %s）", out.Used, res.HealFrom)
		} else if out.UID != "" && step.Locator.Strategy == "uid" && out.UID != step.Locator.Value {
			// uid 漂移但兜底命中了：把新 uid 写回录制（设置里可关）
			if r.store.Settings().Assist.AutoRebindUID {
				if err := r.rebindStep(step, out.UID); err == nil {
					r.logf(runID, "info", step.Index, "已自动刷新定位：uid %s → %s（靠 %s 命中的）", step.Locator.Value, out.UID, out.Used)
				}
			} else {
				r.logf(runID, "warn", step.Index, "uid 已过期（%s），本次靠 %s 命中；可在录制详情点「刷新定位」更新", step.Locator.Value, out.Used)
			}
		}
	}

	// 断言（步骤自带 expect，或独立的 assert 动作）
	if failure == "" && step.Action != "assert" && strings.TrimSpace(step.Expect) != "" {
		expect := Substitute(step.Expect, ds)
		var exp actionResult
		rawExp, _ := json.Marshal(expect)
		expr := fmt.Sprintf(`(async () => { const r = window.__uitestKeys.checkExpect(%s); return {ok:r.ok, message:r.message||"", snippet:r.snippet||""}; })()`, rawExp)
		if err := page.EvalInto(stepCtx, expr, &exp); err != nil {
			failure = "断言执行异常：" + err.Error()
		} else if !exp.OK {
			failure = "断言未通过：" + exp.Message
			res.Expected = expect
			res.Actual = exp.Snippet
		} else {
			res.Message = strings.TrimSpace(res.Message + " · 断言通过")
		}
	}

	res.DurationMS = time.Since(started).Milliseconds()
	if failure != "" {
		res.Status = "failed"
		res.Message = failure
		if shot := r.screenshot(stepCtx, runID, page, step.Index); shot != "" {
			res.Screenshot = shot
		}
		r.logf(runID, "error", step.Index, "✗ %s", failure)
	} else {
		r.logf(runID, "info", step.Index, "✓ %s（%dms）", firstNonEmpty(res.Message, annotation), res.DurationMS)
	}
	r.appendStepResult(runID, res)
	return res
}

// execUpload 处理文件上传：先定位元素拿唯一选择器，再用 CDP 注入本地文件。
func (r *Runner) execUpload(ctx context.Context, page *browser.Page, step model.Step, value string, res *model.StepResult) string {
	req := map[string]any{"action": "upload", "locator": step.Locator, "target": step.Target, "value": value}
	rawReq, _ := json.Marshal(req)
	expr := fmt.Sprintf(`(async () => {
  if (!window.__uitestKeys) return {ok:false, message:"页面运行时未注入"};
  return await window.__uitestKeys.runAction(%s);
})()`, rawReq)
	var out actionResult
	if err := page.EvalInto(ctx, expr, &out); err != nil {
		return "执行异常：" + err.Error()
	}
	res.LocatorUsed = out.Used
	res.Healed = out.Healed
	if !out.OK {
		return out.Message
	}
	css := out.CSS
	if out.Tag != "" && out.Tag != "input" {
		css = css + " input[type=file]"
	}
	file := strings.TrimSpace(value)
	if file == "" || !fileExists(file) {
		gen, err := r.makeSampleImage()
		if err != nil {
			return "无法准备上传文件：" + err.Error()
		}
		file = gen
		res.Message = "已自动生成示例图片用于上传"
	}
	if err := page.SetFileInput(ctx, css, []string{file}); err != nil {
		return "设置上传文件失败：" + err.Error()
	}
	res.ValueUsed = file
	return ""
}

func (r *Runner) makeSampleImage() (string, error) {
	dir := filepath.Join(r.store.Dir(), "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "auto-test-upload.png")
	if fileExists(path) {
		return path, nil
	}
	img := image.NewRGBA(image.Rect(0, 0, 240, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 240; x++ {
			img.Set(x, y, color.RGBA{R: uint8(79), G: uint8(70), B: uint8(229), A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return "", err
	}
	return path, nil
}

func (r *Runner) screenshot(ctx context.Context, runID string, page *browser.Page, index int) string {
	if page == nil {
		return ""
	}
	shotCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	data, err := page.Screenshot(shotCtx)
	if err != nil || len(data) == 0 {
		return ""
	}
	rel := filepath.Join("data", "runs", runID)
	abs := filepath.Join(r.store.Dir(), "runs", runID)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return ""
	}
	name := fmt.Sprintf("step_%02d.png", index)
	if err := os.WriteFile(filepath.Join(abs, name), data, 0o644); err != nil {
		return ""
	}
	_ = ctx
	return filepath.ToSlash(filepath.Join(rel, name))
}

// ensureRuntime 保证页面里有共享运行时（幂等，版本不符则重装）。
func (r *Runner) ensureRuntime(ctx context.Context, page *browser.Page) error {
	var version string
	if err := page.EvalInto(ctx, `(window.__uitestKeys && window.__uitestKeys.version) || ""`, &version); err != nil {
		return err
	}
	if version == jsruntime.Version && version != "" {
		return nil
	}
	if _, err := page.Eval(ctx, jsruntime.KeysJS); err != nil {
		return err
	}
	var markers any
	_ = page.EvalInto(ctx, `window.__uitestKeys.markAll()`, &markers)
	return nil
}

/* --------------------------------------------------------- 存储/日志辅助 */

func (r *Runner) setStatus(runID, status string) {
	now := time.Now()
	_, _ = r.store.MutateRun(runID, func(run *model.Run) {
		run.Status = status
		if status == "running" && run.StartedAt.IsZero() {
			run.StartedAt = now
		}
		if status != "running" && status != "queued" {
			run.FinishedAt = now
		}
	})
	r.bus.Publish(runID, bus.Event{Type: "status", Status: status})
}

func (r *Runner) finish(runID, status, errMsg string) {
	_, _ = r.store.MutateRun(runID, func(run *model.Run) {
		run.Status = status
		run.FinishedAt = time.Now()
		if errMsg != "" {
			run.Error = errMsg
		}
		healables := 0
		passed := 0
		for _, sr := range run.StepResults {
			if sr.Healed {
				healables++
			}
			if sr.Status == "passed" {
				passed++
			}
		}
		run.Healables = healables
		run.PassedSteps = passed
		if run.TotalSteps == 0 {
			run.TotalSteps = len(run.StepResults)
		}
	})
	if errMsg != "" {
		r.logf(runID, "error", 0, "%s", errMsg)
	}
	level := "info"
	switch status {
	case "failed":
		level = "error"
	case "canceled":
		level = "warn"
	}
	r.logf(runID, level, 0, "执行结束：%s", statusLabel(status))
	r.bus.Publish(runID, bus.Event{Type: "status", Status: status})
	r.bus.Publish(runID, bus.Event{Type: "end", Status: status})
}

func (r *Runner) logf(runID, level string, index int, format string, args ...any) {
	entry := model.LogEntry{
		TS:      time.Now(),
		Level:   level,
		Index:   index,
		Message: fmt.Sprintf(format, args...),
	}
	_, _ = r.store.MutateRun(runID, func(run *model.Run) {
		run.Logs = append(run.Logs, entry)
		if len(run.Logs) > 4000 {
			run.Logs = run.Logs[len(run.Logs)-4000:]
		}
	})
	r.bus.Publish(runID, bus.Event{Type: "log", Log: &entry})
}

func (r *Runner) appendStepResult(runID string, sr model.StepResult) {
	_, _ = r.store.MutateRun(runID, func(run *model.Run) {
		run.StepResults = append(run.StepResults, sr)
	})
	r.bus.Publish(runID, bus.Event{Type: "step", Step: &sr})
}

func statusLabel(s string) string {
	switch s {
	case "passed":
		return "通过"
	case "failed":
		return "失败"
	case "canceled":
		return "已取消"
	case "running":
		return "执行中"
	default:
		return s
	}
}

func datasetSummary(ds map[string]string) string {
	keys := make([]string, 0, len(ds))
	for k := range ds {
		keys = append(keys, k)
	}
	// 稳定输出顺序，便于日志比对
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+ds[k])
	}
	return strings.Join(parts, ", ")
}

func (r *Runner) stepDelayMS() int {
	v := r.store.Settings().Browser.StepDelayMS
	if v <= 0 {
		v = 260
	}
	return v
}

func (r *Runner) actionTimeoutMS() int {
	v := r.store.Settings().Browser.ActionTimeoutMS
	if v <= 0 {
		v = 15000
	}
	return v
}

// semanticLocator 依据元素指纹生成"语义定位"（表单项标题 / 按钮文字 / 表格行），
// 用于刷新定位时避免踩到老 css 的歧义。取不到语义时返回零值。
func semanticLocator(t model.Target) model.Locator {
	tag := strings.ToLower(strings.TrimSpace(t.Tag))
	role := strings.TrimSpace(t.Role)
	isButton := tag == "button" || tag == "a" || role == "button" || role == "link" ||
		(tag == "input" && (t.Type == "submit" || t.Type == "button"))
	label := strings.TrimSpace(t.Label)
	if isButton {
		if label != "" {
			if t.Column != "" && t.RowText != "" {
				return model.Locator{Strategy: "cell", Value: t.RowText + ">>" + label}
			}
			if role == "" {
				role = "button"
			}
			return model.Locator{Strategy: "role", Value: role + "|" + label}
		}
		if strings.TrimSpace(t.Text) != "" {
			return model.Locator{Strategy: "text", Value: t.Text}
		}
		return model.Locator{}
	}
	if label != "" {
		tagSel := ""
		switch tag {
		case "textarea":
			tagSel = "textarea"
		case "select":
			tagSel = "select"
		case "input":
			tagSel = "input:not([type=hidden])"
		}
		return model.Locator{Strategy: "near", Value: label + ">>" + tagSel}
	}
	if t.TestAttr != "" {
		return model.Locator{Strategy: "attr", Value: t.TestAttr}
	}
	return model.Locator{}
}

// settleBrowser 运行结束后按设置收尾浏览器。
//
//	keep          不关，保留现场（失败时方便直接看页面）
//	close_tab     关掉本次标签页，浏览器进程继续
//	close_browser 关掉整个内置浏览器
//	auto（默认）  通过就关标签页，未通过保留现场
func (r *Runner) settleBrowser(runID string, page *browser.Page) {
	if page == nil {
		return
	}
	mode := strings.TrimSpace(r.store.Settings().Browser.AfterRun)
	if mode == "" {
		mode = "auto"
	}
	status := ""
	if run, err := r.store.GetRun(runID); err == nil {
		status = run.Status
	}
	closeTab := func() { page.Close(context.Background()) }
	switch mode {
	case "keep":
		r.logf(runID, "info", 0, "按设置保留浏览器现场：标签页留在当前页面，可直接查看")
	case "close_browser":
		closeTab()
		if err := r.br.Close(); err != nil {
			r.logf(runID, "warn", 0, "按设置关闭内置浏览器时出错：%s", err.Error())
		} else {
			r.logf(runID, "info", 0, "按设置已关闭内置浏览器")
		}
	case "close_tab":
		closeTab()
		r.logf(runID, "info", 0, "按设置已关闭本次标签页（内置浏览器继续运行）")
	default: // auto
		if status == "passed" {
			closeTab()
			r.logf(runID, "info", 0, "执行通过，已关闭本次标签页（内置浏览器继续运行）")
		} else {
			r.logf(runID, "warn", 0, "执行未通过，已保留现场：标签页停在失败页面，可直接查看")
		}
	}
}

// rebindStep 把某一步的定位换成新的 uid，旧定位降级为兜底（顺序保持，最多留 6 条）。
func (r *Runner) rebindStep(step model.Step, newUID string) error {
	if newUID == "" || newUID == step.Locator.Value {
		return nil
	}
	fb := []model.Locator{}
	if step.Locator.Strategy != "" && step.Locator.Value != "" && step.Locator.Value != newUID {
		fb = append(fb, model.Locator{Strategy: step.Locator.Strategy, Value: step.Locator.Value})
	}
	for _, f := range step.Locator.Fallbacks {
		if f.Value == newUID || f.Strategy == "" || f.Value == "" {
			continue
		}
		dup := false
		for _, x := range fb {
			if x.Strategy == f.Strategy && x.Value == f.Value {
				dup = true
				break
			}
		}
		if !dup {
			fb = append(fb, f)
		}
		if len(fb) >= 6 {
			break
		}
	}
	loc := model.Locator{Strategy: "uid", Value: newUID, Fallbacks: fb, FrameURL: step.Locator.FrameURL}
	_, err := r.store.UpdateStep(step.RecordingID, step.ID, store.StepPatch{Locator: &loc})
	return err
}

// RebindReport 批量刷新定位的结果。
type RebindReport struct {
	Total      int               `json:"total"`
	Updated    int               `json:"updated"`
	Failed     int               `json:"failed"`
	KeyFixed   int               `json:"key_fixed"`
	Message    string            `json:"message"`
	Steps      []RebindStepBrief `json:"steps,omitempty"`
	DurationMS int64             `json:"duration_ms"`
}

// RebindStepBrief 单步结果（只给控制台看摘要）。
type RebindStepBrief struct {
	Index  int    `json:"index"`
	Action string `json:"action"`
	From   string `json:"from"`
	To     string `json:"to"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Rebind 批量刷新一条录制里所有步骤的定位（重算 uid）。
//
// 用途：老录制是旧算法算的 uid（或干脆是 css 定位），页面改版后虽然能靠兜底跑通，
// 但不稳。这里打开起始页、按每步现有定位找到元素、把它当前的 uid 写回去，
// 顺便按业务词典修正 data_key（设置里可关）。
func (r *Runner) Rebind(ctx context.Context, recordingID string) (RebindReport, error) {
	started := time.Now()
	rep := RebindReport{}
	rec, err := r.store.GetRecording(recordingID)
	if err != nil {
		return rep, err
	}
	rep.Total = len(rec.Steps)
	if len(rec.Steps) == 0 {
		rep.Message = "这条录制还没有步骤"
		return rep, nil
	}

	startURL := strings.TrimSpace(rec.StartURL)
	if startURL == "" {
		for _, s := range rec.Steps {
			if s.Action == "navigate" && strings.TrimSpace(s.Value) != "" {
				startURL = s.Value
				break
			}
		}
	}
	if startURL == "" {
		return rep, fmt.Errorf("录制没有起始 URL，无法打开页面")
	}

	openCtx, cancelOpen := context.WithTimeout(ctx, 150*time.Second)
	page, err := r.br.OpenPage(openCtx, "about:blank")
	cancelOpen()
	if err != nil {
		return rep, fmt.Errorf("启动内置浏览器失败：%w", err)
	}
	// 运行结束后怎么处理浏览器由设置决定（auto / close_tab / close_browser / keep）
	defer page.Close(context.Background()) // 批量刷新定位是内部操作，用完即关

	navCtx, cancelNav := context.WithTimeout(ctx, 60*time.Second)
	navErr := page.Navigate(navCtx, startURL)
	cancelNav()
	if navErr != nil {
		return rep, fmt.Errorf("打开起始页失败：%w", navErr)
	}
	if err := r.ensureRuntime(ctx, page); err != nil {
		return rep, fmt.Errorf("注入页面运行时失败：%w", err)
	}
	// 等页面真正就绪：刚导航完时 DOM/文本可能还没稳定，
	// 语义定位（按表单项标题匹配）会因此失败，退化成有歧义的 css。
	if err := waitReady(ctx, page, 20*time.Second); err != nil {
		return rep, fmt.Errorf("等待页面就绪失败：%w", err)
	}

	for _, step := range rec.Steps {
		if ctx.Err() != nil {
			break
		}
		brief := RebindStepBrief{Index: step.Index, Action: step.Action, From: step.Locator.Strategy + "=" + step.Locator.Value}
		if step.Action == "navigate" || step.Action == "script" || step.Action == "wait" || step.Action == "assert" {
			brief.OK = true
			brief.Detail = "无需定位"
			rep.Steps = append(rep.Steps, brief)
			continue
		}
		// 语义优先：老录制的 css 定位常常是有歧义的（input.el-input__inner 会指向第一个输入框），
		// 直接按它重算会把错误固化下来。所以先用表单项标题/按钮文字重新定位，失败再退回原定位。
		resolveLoc := semanticLocator(step.Target)
		if resolveLoc.Strategy != "" {
			base := []model.Locator{}
			if step.Locator.Strategy != "" && step.Locator.Value != "" {
				base = append(base, model.Locator{Strategy: step.Locator.Strategy, Value: step.Locator.Value})
			}
			base = append(base, step.Locator.Fallbacks...)
			resolveLoc.Fallbacks = base
			resolveLoc.FrameURL = step.Locator.FrameURL
		} else {
			resolveLoc = step.Locator
		}
		req := map[string]any{"action": "resolve", "locator": resolveLoc, "target": step.Target}
		rawReq, _ := json.Marshal(req)
		expr := fmt.Sprintf(`(async () => {
  if (!window.__uitestKeys) return {ok:false, message:"页面运行时未注入"};
  return await window.__uitestKeys.runAction(%s);
})()`, rawReq)
		var out actionResult
		stepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		evalErr := page.EvalInto(stepCtx, expr, &out)
		cancel()
		if evalErr != nil || !out.OK || out.UID == "" {
			brief.OK = false
			if evalErr != nil {
				brief.Detail = "定位失败：" + evalErr.Error()
			} else if !out.OK {
				brief.Detail = "元素未找到"
			} else {
				brief.Detail = "拿不到 uid"
			}
			rep.Failed++
			rep.Steps = append(rep.Steps, brief)
			continue
		}
		brief.To = "uid=" + out.UID
		brief.OK = true
		if out.Used != "" && !strings.HasPrefix(out.Used, "uid=") {
			brief.Detail = "原定位已失效，靠 " + out.Used + " 命中"
		}
		if out.UID != step.Locator.Value || step.Locator.Strategy != "uid" {
			if err := r.rebindStep(step, out.UID); err == nil {
				rep.Updated++
			} else {
				brief.OK = false
				brief.Detail = "写回失败：" + err.Error()
				rep.Failed++
			}
		}
		rep.Steps = append(rep.Steps, brief)
	}

	if r.store.Settings().Assist.AutoDataKey {
		rep.KeyFixed = r.store.ImproveDataKeys(recordingID)
	}
	rep.DurationMS = time.Since(started).Milliseconds()
	rep.Message = fmt.Sprintf("共 %d 步：刷新 %d 个定位，%d 个失败，修正 %d 个字段名", rep.Total, rep.Updated, rep.Failed, rep.KeyFixed)
	return rep, nil
}

// waitReady 等 document.readyState 变成 complete，并且 body 有文本（最多等 timeout）。
func waitReady(ctx context.Context, page *browser.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var state struct {
			Ready string `json:"ready"`
			Text  int    `json:"text"`
		}
		if err := page.EvalInto(ctx, `(() => {
  const b = document.body;
  return { ready: document.readyState, text: b ? (b.textContent || '').trim().length : 0 };
})()`, &state); err == nil {
			if state.Ready == "complete" && state.Text > 0 {
				// 再给框架一点渲染时间（Vue/React 首屏渲染、异步接口回来后才会出现表单）
				time.Sleep(500 * time.Millisecond)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("页面在 %s 内没有就绪", timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func firstNonEmpty(list ...string) string {
	for _, s := range list {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

/* ------------------------------------------------------------ 变量替换 */

var varRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_\-]+)(?::(\d+))?\s*\}\}`)

// Substitute 把 {{key}} 替换成数据准备里的值，并支持内置变量：
// {{now}} {{today}} {{datetime}} {{timestamp}} {{uuid}} {{random:8}}
func Substitute(tpl string, ds map[string]string) string {
	if !strings.Contains(tpl, "{{") {
		return tpl
	}
	now := time.Now()
	return varRe.ReplaceAllStringFunc(tpl, func(m string) string {
		groups := varRe.FindStringSubmatch(m)
		key := groups[1]
		arg := groups[2]
		if v, ok := ds[key]; ok {
			return v
		}
		switch key {
		case "now":
			return now.Format("2006-01-02 15:04:05")
		case "today":
			return now.Format("2006-01-02")
		case "datetime":
			return now.Format("20060102150405")
		case "date":
			return now.Format("2006-01-02")
		case "timestamp":
			return fmt.Sprintf("%d", now.Unix())
		case "uuid":
			return randHex(8)
		case "random":
			n := 6
			if arg != "" {
				if v, err := parseInt(arg); err == nil && v > 0 && v <= 64 {
					n = v
				}
			}
			return randHex(n)
		default:
			return m // 未知变量保留原样，便于在运行日志里发现遗漏
		}
	})
}

func parseInt(s string) (int, error) {
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("非法数字")
		}
		v = v*10 + int(c-'0')
	}
	return v, nil
}

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())[:n]
	}
	s := hex.EncodeToString(buf)
	if len(s) > n {
		s = s[:n]
	}
	return s
}
