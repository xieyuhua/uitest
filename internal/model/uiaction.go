package model

import "time"

// UiAction 录制过程中顺手抓到的一条 UI 操作（元素 + 动作）。
//
// 「接口采集」不再只是接口：同一次录制里，用户在页面上的点击 / 输入 / 下拉 /
// 勾选 / 跳转都会被记下来，可以直接生成一条 UI 操作回放流程（也可当作
// 定时采集任务的前置登录动作）。
type UiAction struct {
	ID       string `json:"id"`
	Index    int    `json:"index"`
	Action   string `json:"action"` // click | input | select | check | uncheck | key | navigate
	Selector string `json:"selector,omitempty"`
	// Fallback 备用定位，形如 text=登录 / xpath=//button
	Fallback string `json:"fallback,omitempty"`
	// StableID 元素上稳定的 id / data-testid（回放时优先用它）
	StableID string `json:"stable_id,omitempty"`
	Tag      string `json:"tag,omitempty"`
	Type     string `json:"type,omitempty"`
	Name     string `json:"name,omitempty"`
	Label    string `json:"label,omitempty"`
	Text     string `json:"text,omitempty"`
	// Placeholder 输入框提示语（没有 label 时靠它认元素）
	Placeholder string `json:"placeholder,omitempty"`
	Value       string `json:"value,omitempty"`
	// Masked 密码类输入：value 一律留空，只标记「这里输入过密码」
	Masked  bool      `json:"masked,omitempty"`
	PageURL string    `json:"page_url,omitempty"`
	At      time.Time `json:"at"`
	Source  string    `json:"source,omitempty"` // browser | extension | manual
}
