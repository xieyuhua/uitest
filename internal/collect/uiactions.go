package collect

import (
	"fmt"
	"log"
	"strings"
	"time"

	"uitest-server/internal/browser"
	"uitest-server/internal/model"
)

// 本文件实现「接口采集顺带录 UI 操作」。
//
// 内置浏览器里每次点击 / 输入 / 下拉 / 勾选 / 路由跳转都会被记下来，之后可以：
//  1) 一键生成一条 UI 操作回放流程（#/recordings/{id}）；
//  2) 直接当定时采集任务的「前置登录」动作：先 UI 自动化登录，再抓接口。
//
// 两种来源：browser（CDP 注入脚本回传）、extension（插件上报 / 手动补充）。

// maxUIActions 单个会话最多记多少条 UI 操作（防止长时间录制把内存吃满）。
const maxUIActions = 2000

// uiActionKinds 允许的动作（名称与 UI 回放引擎的 action 一致）。
var uiActionKinds = map[string]bool{
	"click": true, "input": true, "select": true,
	"check": true, "uncheck": true, "key": true, "navigate": true,
}

// normalizeUIAction 统一动作名（兼容 fill / type / goto 等别名），不认识返回空串。
func normalizeUIAction(v string) string {
	a := strings.ToLower(strings.TrimSpace(v))
	switch a {
	case "fill", "type", "setvalue", "set_value":
		a = "input"
	case "goto", "open", "nav":
		a = "navigate"
	}
	if !uiActionKinds[a] {
		return ""
	}
	return a
}

// startUICapture 在会话的内置浏览器标签页里启动 UI 操作录制。
// 失败只记日志：UI 录制挂掉不能影响接口录制这条主线。
func (r *Recorder) startUICapture(sess *CaptureSession) {
	if sess == nil || sess.Page == nil {
		return
	}
	ui, err := sess.Page.StartUiCapture(func(ev browser.UiActionEvent) {
		r.addUIAction(sess.ID, ev)
	})
	if err != nil {
		log.Printf("[collect] UI 操作录制启动失败（接口录制不受影响）%s：%v", sess.ID, err)
		return
	}
	r.mu.Lock()
	sess.UI = ui
	r.mu.Unlock()
	log.Printf("[collect] UI 操作录制已开启 %s", sess.ID)
}

// addUIAction 把页面回传的一条原始事件写进会话。
func (r *Recorder) addUIAction(id string, ev browser.UiActionEvent) {
	action := normalizeUIAction(ev.Action)
	if action == "" {
		return
	}
	at := time.Now()
	if ev.TS > 0 {
		at = time.UnixMilli(ev.TS)
	}
	item := model.UiAction{
		ID:          newID("ua_"),
		Action:      action,
		Selector:    strings.TrimSpace(ev.Selector),
		Fallback:    strings.TrimSpace(ev.Fallback),
		StableID:    strings.TrimSpace(ev.StableID),
		Tag:         strings.TrimSpace(ev.Tag),
		Type:        strings.TrimSpace(ev.Type),
		Name:        strings.TrimSpace(ev.Name),
		Label:       strings.TrimSpace(ev.Label),
		Text:        strings.TrimSpace(ev.Text),
		Placeholder: strings.TrimSpace(ev.Placeholder),
		Value:       ev.Value,
		Masked:      ev.Masked,
		PageURL:     strings.TrimSpace(ev.PageURL),
		At:          at,
		Source:      "browser",
	}
	if item.Masked {
		item.Value = "" // 密码永不落盘
	}
	if action == "navigate" {
		if item.PageURL == "" {
			item.PageURL = item.Value
		}
	} else if item.Selector == "" {
		return // 定位不到元素的操作回放不了，不如不记
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok || s.Stopped {
		return
	}
	if len(s.Actions) >= maxUIActions {
		return
	}
	// 起录时打开一次、补录时又打开一次：连续的同一个地址只留一条。
	if action == "navigate" {
		if n := len(s.Actions); n > 0 {
			if l := s.Actions[n-1]; l.Action == "navigate" && l.Value == item.Value {
				return
			}
		}
	}
	// 连点 / 输入防抖的重复上报：同动作同元素同值且间隔很短就跳过
	if n := len(s.Actions); n > 0 {
		last := s.Actions[n-1]
		if last.Action == item.Action && last.Selector == item.Selector && last.Value == item.Value &&
			item.At.Sub(last.At) < 300*time.Millisecond {
			return
		}
	}
	if u := strings.TrimSpace(item.PageURL); strings.HasPrefix(strings.ToLower(u), "http") {
		s.PageURL = u // 顺便把会话地址跟到最新路由
	}
	item.Index = len(s.Actions) + 1
	s.Actions = append(s.Actions, item)
}

// AddUIActions 插件 / 手动补充的 UI 操作。
//
// 按 id 幂等：同一个 id 再上报一次是「更新」（插件连续输入会合并到同一步，值会变），
// 新 id 才追加；用户删过的 id 不再复活。
func (r *Recorder) AddUIActions(id string, items []model.UiAction) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return 0, fmt.Errorf("录制会话不存在或已被清理（服务重启后会新建）")
	}
	if s.ActionDeleted == nil {
		s.ActionDeleted = map[string]bool{}
	}
	pos := make(map[string]int, len(s.Actions)+len(items))
	for i, a := range s.Actions {
		if a.ID != "" {
			pos[a.ID] = i
		}
	}
	added := 0
	for _, it := range items {
		action := normalizeUIAction(it.Action)
		if action == "" {
			continue
		}
		it.ID = strings.TrimSpace(it.ID)
		hadID := it.ID != ""
		if !hadID {
			it.ID = newID("ua_")
		}
		if s.ActionDeleted[it.ID] {
			continue
		}
		it.Action = action
		it.Selector = strings.TrimSpace(it.Selector)
		it.At = it.At.Truncate(time.Millisecond)
		if it.At.IsZero() {
			it.At = time.Now()
		}
		if it.Source == "" {
			it.Source = "extension"
		}
		if it.Masked {
			it.Value = "" // 密码永不落盘
		}
		if i, exists := pos[it.ID]; exists {
			// 同一步的更新：只覆盖内容，序号保持不变
			keep := s.Actions[i].Index
			it.Index = keep
			s.Actions[i] = it
			continue
		}
		if len(s.Actions) >= maxUIActions {
			break
		}
		// 没有 id 的重复上报（插件补发 / 用户手滑点两下）1.5 秒内只留一条
		if n := len(s.Actions); n > 0 && !hadID {
			last := s.Actions[n-1]
			if last.Action == it.Action && last.Selector == it.Selector && last.Value == it.Value {
				continue
			}
		}
		it.Index = len(s.Actions) + 1
		s.Actions = append(s.Actions, it)
		pos[it.ID] = len(s.Actions) - 1
		added++
	}
	reindexUIActions(s.Actions)
	return added, nil
}

// UIActions 取会话里录到的 UI 操作（副本，按录制顺序）。
func (r *Recorder) UIActions(id string) ([]model.UiAction, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return nil, false, fmt.Errorf("录制会话不存在或已被清理（服务重启后会清空）")
	}
	out := make([]model.UiAction, len(s.Actions))
	copy(out, s.Actions)
	return out, true, nil
}

// UIActionCount 会话里 UI 操作的条数。
func (r *Recorder) UIActionCount(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[id]; ok {
		return len(s.Actions)
	}
	return 0
}

// DeleteUIActions 删除 UI 操作。all=true 清空全部（保留会话）。
func (r *Recorder) DeleteUIActions(id string, ids []string, all bool) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return 0, fmt.Errorf("录制会话不存在或已被清理（服务重启后会清空）")
	}
	if s.ActionDeleted == nil {
		s.ActionDeleted = map[string]bool{}
	}
	if all {
		n := len(s.Actions)
		for _, a := range s.Actions {
			if a.ID != "" {
				s.ActionDeleted[a.ID] = true
			}
		}
		s.Actions = nil
		return n, nil
	}
	drop := make(map[string]bool, len(ids))
	for _, x := range ids {
		if v := strings.TrimSpace(x); v != "" {
			drop[v] = true
		}
	}
	if len(drop) == 0 {
		return 0, nil
	}
	kept := make([]model.UiAction, 0, len(s.Actions))
	deleted := 0
	for _, a := range s.Actions {
		if a.ID != "" && drop[a.ID] {
			s.ActionDeleted[a.ID] = true
			deleted++
			continue
		}
		kept = append(kept, a)
	}
	s.Actions = kept
	reindexUIActions(s.Actions)
	return deleted, nil
}

// reindexUIActions 重排序号（1 起）。
func reindexUIActions(list []model.UiAction) {
	for i := range list {
		list[i].Index = i + 1
	}
}

// sensitiveValue 密码类输入的占位值：回放时由任务变量 password 提供。
const sensitiveValue = "{{password}}"

// ActionsToSteps 把录到的 UI 操作转成 UI 回放步骤（model.Step）。
func ActionsToSteps(actions []model.UiAction) []model.Step {
	steps := make([]model.Step, 0, len(actions))
	prev := ""
	for _, a := range actions {
		action := normalizeUIAction(a.Action)
		if action == "" {
			continue
		}
		// 点提交按钮后浏览器还会补一条 form 级 Enter，两条一起回放会重复提交，跳过。
		if action == "key" && strings.EqualFold(strings.TrimSpace(a.Tag), "form") && prev == "click" {
			continue
		}
		prev = action
		st := model.Step{
			Action: action,
			Value:  a.Value,
			Target: model.Target{
				Tag:         a.Tag,
				Type:        a.Type,
				Name:        a.Name,
				Label:       a.Label,
				Text:        a.Text,
				Placeholder: a.Placeholder,
				StableID:    a.StableID,
				Page:        a.PageURL,
			},
			Note: "来自接口录制的 UI 操作",
		}
		if action == "navigate" {
			url := strings.TrimSpace(a.Value)
			if url == "" {
				url = strings.TrimSpace(a.PageURL)
			}
			if url == "" {
				continue
			}
			// 连续两次打开同一地址（起录一次、补录一次）只留一条。
			if n := len(steps); n > 0 {
				if l := steps[n-1]; l.Action == "navigate" && strings.TrimSpace(l.Value) == url {
					continue
				}
			}
			st.Value = url
			st.Annotation = "打开页面 " + url
			steps = append(steps, st)
			continue
		}
		if a.Selector == "" {
			continue
		}
		st.Locator = model.Locator{Strategy: "css", Value: a.Selector}
		if fb := fallbackLocator(a); fb != nil {
			st.Locator.Fallbacks = append(st.Locator.Fallbacks, *fb)
		}
		st.Annotation = annotationForUIAction(a, action)
		if a.Masked {
			st.Value = sensitiveValue
			st.Note = "密码框：录制时已脱敏，值用 {{password}}，在任务「前置登录 → 变量」里填真实密码"
		}
		steps = append(steps, st)
	}
	return steps
}

// StartURLFromActions 推断流程起始地址：优先第一条 navigate，其次是第一个操作所在页。
func StartURLFromActions(actions []model.UiAction) string {
	ok := func(u string) bool {
		u = strings.TrimSpace(u)
		return strings.HasPrefix(strings.ToLower(u), "http")
	}
	for _, a := range actions {
		if normalizeUIAction(a.Action) != "navigate" {
			continue
		}
		if ok(a.Value) {
			return strings.TrimSpace(a.Value)
		}
		if ok(a.PageURL) {
			return strings.TrimSpace(a.PageURL)
		}
	}
	for _, a := range actions {
		if ok(a.PageURL) {
			return strings.TrimSpace(a.PageURL)
		}
	}
	return ""
}

// fallbackLocator 生成一个兜底定位：优先用录制时的 text=/label=/placeholder=，没有就按元素指纹补。
func fallbackLocator(a model.UiAction) *model.Locator {
	if raw := strings.TrimSpace(a.Fallback); raw != "" {
		if i := strings.Index(raw, "="); i > 0 {
			strategy := strings.ToLower(strings.TrimSpace(raw[:i]))
			value := strings.TrimSpace(raw[i+1:])
			if value != "" {
				switch strategy {
				case "text", "label", "placeholder":
					return &model.Locator{Strategy: strategy, Value: value}
				}
			}
		}
	}
	for _, pair := range [][2]string{{"label", a.Label}, {"placeholder", a.Placeholder}, {"text", a.Text}} {
		if v := strings.TrimSpace(pair[1]); v != "" {
			return &model.Locator{Strategy: pair[0], Value: v}
		}
	}
	return nil
}

// annotationForUIAction 给人看的一步说明（结果页 / 回放日志都用它）。
func annotationForUIAction(a model.UiAction, action string) string {
	what := strings.TrimSpace(a.Label)
	if what == "" {
		what = strings.TrimSpace(a.Text)
	}
	if what == "" {
		what = strings.TrimSpace(a.Placeholder)
	}
	if what == "" {
		what = strings.TrimSpace(a.Name)
	}
	if what == "" {
		what = strings.TrimSpace(a.Tag)
	}
	if r := []rune(what); len(r) > 40 {
		what = string(r[:40]) + "…"
	}
	if what == "" {
		what = "元素"
	}
	val := strings.TrimSpace(a.Value)
	switch action {
	case "click":
		return "点击 " + what
	case "input":
		if a.Masked {
			return "输入密码到 " + what + "（已脱敏）"
		}
		if r := []rune(val); len(r) > 20 {
			val = string(r[:20]) + "…"
		}
		return "输入「" + val + "」到 " + what
	case "select":
		return "选择「" + val + "」到 " + what
	case "check":
		return "勾选 " + what
	case "uncheck":
		return "取消勾选 " + what
	case "key":
		return "在 " + what + " 按 " + defaultStr(val, "Enter")
	}
	return action + " " + what
}
