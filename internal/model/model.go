// Package model 定义平台的全部数据结构。
// 字段名与 docs/API.md 的 JSON 契约严格一致（snake_case），前端与插件都按此解析。
package model

import "time"

// Locator 元素定位描述。strategy 取值：
// uid(data-uitest-id，首选) | id | name | css | xpath | text | label | placeholder
type Locator struct {
	Strategy  string    `json:"strategy"`
	Value     string    `json:"value"`
	Fallbacks []Locator `json:"fallbacks,omitempty"`
	FrameURL  string    `json:"frame_url,omitempty"`
}

// Target 元素指纹：录制时保存，执行时用于自愈定位与 AI 理解字段语义。
type Target struct {
	Tag         string `json:"tag"`
	Type        string `json:"type"`
	Label       string `json:"label"`
	Text        string `json:"text"`
	Placeholder string `json:"placeholder"`
	Aria        string `json:"aria"`
	Role        string `json:"role"`
	Href        string `json:"href"`
	Page        string `json:"page"`
	FieldKind   string `json:"field_kind"`
	// Options 下拉框候选文本（录制时采集，供 AI 生成"一定能选中的值"）
	Options []string `json:"options,omitempty"`

	// ---- 2.0 重建安全定位用的语义（Vue/React 重新打包后依然有效）----
	// Name 元素 name 属性（仅在"看起来稳定"时才有值，hash/自动名会被插件过滤掉）
	Name string `json:"name,omitempty"`
	// StableID 稳定的 id（hash 样式、组件库自动 id 不会放进这里）
	StableID string `json:"stable_id,omitempty"`
	// TestAttr 团队自定义稳定属性，形如 data-testid=submit-btn
	TestAttr string `json:"test_attr,omitempty"`
	// FormItem 最近表单项的标题文字（表格行内元素则为行首单元格文字）
	FormItem string `json:"form_item,omitempty"`
	// Column 表格列名
	Column string `json:"column,omitempty"`
	// RowText 行首单元格文字
	RowText string `json:"row_text,omitempty"`
}

// Describe 返回人可读的目标描述（用于日志与默认备注）。
func (t Target) Describe() string {
	for _, s := range []string{t.Label, t.Text, t.Placeholder, t.Aria, t.Tag} {
		if s != "" {
			return s
		}
	}
	return "元素"
}

// Step 一个操作步骤。
type Step struct {
	ID          string    `json:"id"`
	RecordingID string    `json:"recording_id,omitempty"`
	Index       int       `json:"index"`
	Action      string    `json:"action"`
	Annotation  string    `json:"annotation"`
	Locator     Locator   `json:"locator"`
	Target      Target    `json:"target"`
	DataKey     string    `json:"data_key,omitempty"`
	Value       string    `json:"value,omitempty"`
	Expect      string    `json:"expect,omitempty"`
	WaitMS      int       `json:"wait_ms,omitempty"`
	Optional    bool      `json:"optional,omitempty"`
	Note        string    `json:"note,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Recording 一条录制 = 一个业务场景（新建服务商品 / 上传商品 / 设置活动 …）。
type Recording struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	StartURL    string    `json:"start_url"`
	Tags        []string  `json:"tags"`
	Status      string    `json:"status"` // draft | ready
	StepCount   int       `json:"step_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Steps       []Step    `json:"steps,omitempty"`
}

// Summary 列表用副本（不含 steps，保留 step_count）。
func (r Recording) Summary() Recording {
	count := len(r.Steps)
	if r.StepCount > 0 {
		count = r.StepCount
	}
	r.Steps = nil
	r.StepCount = count
	return r
}

// Clone 深拷贝，避免调用方与存储共享底层切片。
func (r Recording) Clone() Recording {
	c := r
	c.Tags = append([]string(nil), r.Tags...)
	if r.Steps != nil {
		c.Steps = make([]Step, len(r.Steps))
		copy(c.Steps, r.Steps)
		for i := range c.Steps {
			c.Steps[i].Locator.Fallbacks = append([]Locator(nil), r.Steps[i].Locator.Fallbacks...)
		}
	}
	return c
}

// StepResult 单个步骤的执行结果。
type StepResult struct {
	Index       int    `json:"index"`
	StepID      string `json:"step_id"`
	Action      string `json:"action"`
	Annotation  string `json:"annotation"`
	Status      string `json:"status"` // passed | failed | skipped
	ValueUsed   string `json:"value_used,omitempty"`
	LocatorUsed string `json:"locator_used,omitempty"`
	Healed      bool   `json:"healed"`
	HealFrom    string `json:"heal_from,omitempty"` // 自愈前失效的定位
	Message     string `json:"message,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
	Screenshot  string `json:"screenshot,omitempty"`
	Expected    string `json:"expected,omitempty"`
	Actual      string `json:"actual,omitempty"`
}

// LogEntry 执行日志一行。
type LogEntry struct {
	TS      time.Time `json:"ts"`
	Level   string    `json:"level"` // info | warn | error
	Index   int       `json:"index"` // 关联步骤序号，0 表示整体
	Message string    `json:"message"`
}

// Run 一次任务执行。
type Run struct {
	ID            string            `json:"id"`
	RecordingID   string            `json:"recording_id"`
	RecordingName string            `json:"recording_name"`
	Status        string            `json:"status"` // queued | running | passed | failed | canceled
	Mode          string            `json:"mode"`   // server
	Dataset       map[string]string `json:"dataset"`
	DatasetSource string            `json:"dataset_source"` // ai | rules | manual
	DatasetFields []AIField         `json:"dataset_fields,omitempty"`
	DatasetNotes  map[string]string `json:"dataset_notes,omitempty"`
	StepResults   []StepResult      `json:"step_results"`
	Logs          []LogEntry        `json:"logs"`
	StartedAt     time.Time         `json:"started_at,omitempty"`
	FinishedAt    time.Time         `json:"finished_at,omitempty"`
	Error         string            `json:"error,omitempty"`
	Healables     int               `json:"healables"`
	// TotalSteps / PassedSteps 给列表页用的摘要统计
	TotalSteps  int `json:"total_steps"`
	PassedSteps int `json:"passed_steps"`
}

// Summary 列表用副本（不含 logs / step_results）。
func (r Run) Summary() Run {
	r.Logs = nil
	r.StepResults = nil
	r.Dataset = nil
	r.DatasetFields = nil
	return r
}

// Clone 深拷贝。
func (r Run) Clone() Run {
	c := r
	if r.Dataset != nil {
		c.Dataset = make(map[string]string, len(r.Dataset))
		for k, v := range r.Dataset {
			c.Dataset[k] = v
		}
	}
	if r.DatasetNotes != nil {
		c.DatasetNotes = make(map[string]string, len(r.DatasetNotes))
		for k, v := range r.DatasetNotes {
			c.DatasetNotes[k] = v
		}
	}
	c.DatasetFields = append([]AIField(nil), r.DatasetFields...)
	c.StepResults = append([]StepResult(nil), r.StepResults...)
	c.Logs = append([]LogEntry(nil), r.Logs...)
	return c
}

// AIField 一条测试数据字段定义（AI 或规则生成）。
type AIField struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Kind  string   `json:"kind"`
	Value string   `json:"value"`
	Note  string   `json:"note,omitempty"`
	Enum  []string `json:"enum,omitempty"`
}

// Element 元素库条目：data-uitest-id 与页面语义的绑定索引。
type Element struct {
	Key        string    `json:"key"`
	Page       string    `json:"page"`
	Selector   string    `json:"selector"`
	Descriptor string    `json:"descriptor"`
	Tag        string    `json:"tag"`
	Type       string    `json:"type"`
	Label      string    `json:"label"`
	Hits       int       `json:"hits"`
	LastSeen   time.Time `json:"last_seen"`
	Broken     bool      `json:"broken"`
}

// Settings 运行时设置（控制台可改，落盘持久化）。
type Settings struct {
	AI      AISettings      `json:"ai"`
	Browser BrowserSettings `json:"browser"`
	Assist  AssistSettings  `json:"assist"`
}

// AssistSettings 三项"省事"辅助能力的开关（控制台设置页可逐项开关）。
type AssistSettings struct {
	// AutoDataKey 中文标签自动转英文字段名：记住密码 → remember_password，
	// 否则纯中文标签会退化成一堆 field_25，AI 生成数据和日志都看不懂。
	AutoDataKey bool `json:"auto_data_key"`
	// StableIDHint 控制台把"没有 data-testid 一类稳定属性"的元素标红，
	// 提醒前端补齐锚点（补齐后定位最稳）。
	StableIDHint bool `json:"stable_id_hint"`
	// AutoRebindUID 执行时如果 uid 没命中、但靠兜底策略命中了元素，
	// 把命中元素的新 uid 自动写回录制（不用人工点"刷新定位"）。
	AutoRebindUID bool `json:"auto_rebind_uid"`
}

// AISettings 大模型接入配置（OpenAI 兼容）。
type AISettings struct {
	BaseURL    string `json:"base_url"`
	APIKey     string `json:"api_key,omitempty"`
	Model      string `json:"model"`
	TimeoutSec int    `json:"timeout_sec"`
}

// BrowserSettings 内置浏览器配置。
type BrowserSettings struct {
	Executable      string `json:"executable"`
	Headless        bool   `json:"headless"`
	StepDelayMS     int    `json:"step_delay_ms"`
	ActionTimeoutMS int    `json:"action_timeout_ms"`
	// NoSandbox 某些受限/虚拟化环境里 Chromium 渲染进程会因沙箱注册失败而崩溃
	// （CDP 报 Target crashed），此时打开它即追加 --no-sandbox。
	NoSandbox bool `json:"no_sandbox"`
	// DebugURL 非空时不自己拉起浏览器，而是连接这个已存在的 CDP 端点
	// （例如先手工执行 msedge --remote-debugging-port=9400，再填 http://127.0.0.1:9400）。
	// 适用于 CI / 容器 / 受限环境，或调试时复用你已登录的浏览器。
	DebugURL string `json:"debug_url"`
	// KernelDir 自带内核目录：填了就先在这里找 chrome.exe/msedge.exe，找不到再回退到系统安装。
	KernelDir string `json:"kernel_dir"`
	// ExtraArgs 额外启动参数（高级用）。
	ExtraArgs []string `json:"extra_args,omitempty"`
	// ResolvedFlags 自动探测成功过的启动参数组合（由服务端写回，用户不用管）。
	ResolvedFlags []string `json:"resolved_flags,omitempty"`
	// ProfileDir 探测成功后固化下来的 user-data-dir（记住登录态、下次直接接管实例）。
	ProfileDir string `json:"profile_dir,omitempty"`
	// AfterRun 一次运行结束后如何处理内置浏览器：
	//   auto          通过就关掉本次标签页，失败则保留现场（默认，推荐）
	//   close_tab     每次都关掉本次标签页（浏览器进程继续运行）
	//   close_browser 每次都把整个内置浏览器关掉
	//   keep          都不关，保留现场随时查看
	AfterRun string `json:"after_run"`
}

// DefaultSettings 出厂默认值。
func DefaultSettings() Settings {
	return Settings{
		AI: AISettings{TimeoutSec: 60},
		Browser: BrowserSettings{
			StepDelayMS:     260,
			ActionTimeoutMS: 15000,
			AfterRun:        "auto",
		},
		// 三项辅助默认全开：都是"让定位/数据更清楚"的增益能力，关掉也不会报错
		Assist: AssistSettings{AutoDataKey: true, StableIDHint: true, AutoRebindUID: true},
	}
}
