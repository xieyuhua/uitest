package model

import "time"

// ============================================================================
// 接口采集（录制 → 筛选 → 定时抓取 → 落库）
//
// 数据流：
//   内置浏览器里的真实请求 → CapturedRequest（录制结果）
//   → 勾选若干条 → HarvestEndpoint（采集任务里的接口）
//   → HarvestSchedule 定时触发 → HarvestRun（一次执行）
//   → HarvestSink 把提取出的行写进 SQLite / MySQL / PostgreSQL / JSONL
// ============================================================================

// CapturedRequest 录制阶段抓到的一条接口请求。
//
// 这是"原始素材"：只在内存里存在（可落一份快照方便排查），
// 用户在内置浏览器里正常操作页面，服务端通过 CDP 的 Network 域旁路记录。
type CapturedRequest struct {
	ID     string `json:"id"`
	Index  int    `json:"index"`
	Method string `json:"method"`
	URL    string `json:"url"`
	// Host / Path / Query 从 URL 拆出来，方便筛选与做接口模板
	Host  string `json:"host"`
	Path  string `json:"path"`
	Query string `json:"query,omitempty"`

	Status   int    `json:"status"`
	MimeType string `json:"mime_type,omitempty"`
	// Type CDP 资源类型：XHR / Fetch / Document / Script / Other
	Type string `json:"type"`
	// Kind 归一化后的类别，给筛选用：api / page / static / other
	Kind string `json:"kind"`

	Bytes      int   `json:"bytes"`
	DurationMS int64 `json:"duration_ms"`

	ReqHeaders map[string]string `json:"req_headers,omitempty"`
	// PostData 请求体（表单 / JSON），录制时截断保存
	PostData string `json:"post_data,omitempty"`

	// HasBody 是否有响应体；BodyPreview 为截断后的响应体（筛选/预览用）
	HasBody       bool   `json:"has_body"`
	BodyPreview   string `json:"body_preview,omitempty"`
	BodyTruncated bool   `json:"body_truncated,omitempty"`
	// BodySize 响应体原始字节数
	BodySize int `json:"body_size,omitempty"`

	// PageURL 触发这条请求的页面地址（同时作为默认 Referer）
	PageURL string `json:"page_url,omitempty"`
	// Error 请求失败原因（CDP loadingFailed）
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at"`
}

// HarvestField 一条字段映射：响应 JSON 里的某个路径 → 数据库里的某一列。
type HarvestField struct {
	Column string `json:"column"`
	// Path JSON 路径，支持 data.list[0].name 这种写法；
	// 也支持内置值：@now / @date / @timestamp / @url / @endpoint / @page
	Path string `json:"path"`
	// Kind 仅用于展示：string / number / bool / object
	Kind string `json:"kind,omitempty"`
}

// HarvestEndpoint 采集任务里的一个接口。
type HarvestEndpoint struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Method  string `json:"method"`
	Enabled bool   `json:"enabled"`
	// URL 支持变量：{{now}} {{date}} {{datetime}} {{timestamp}} {{uuid}}
	// {{page}} {{host}}，以及 {{env:名字}} 读环境变量（避免把密钥写进任务文件）
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	// Body 请求体模板（POST/PUT 用）
	Body string `json:"body,omitempty"`

	// ---- 分页（列表接口逐页抓）----
	// Pagination 分页策略（推荐用这个；mode=auto 时运行期自动判断怎么翻页、什么时候停）
	Pagination HarvestPagination `json:"pagination,omitempty"`

	// 以下是 1.0 的扁平写法，仅为兼容老任务保留；Normalize 会自动迁移到 Pagination
	Pages     int    `json:"pages,omitempty"`
	PageParam string `json:"page_param,omitempty"`
	PageStart int    `json:"page_start,omitempty"`
	PageStep  int    `json:"page_step,omitempty"`

	// ---- 提取 ----
	// RecordsPath 记录数组所在路径（如 data.list）；为空表示响应本身是数组/单条记录
	RecordsPath string         `json:"records_path,omitempty"`
	Fields      []HarvestField `json:"fields,omitempty"`

	// ---- 录制时的样本（只做预览/字段识别，不参与抓取）----
	SampleStatus int    `json:"sample_status,omitempty"`
	SampleBody   string `json:"sample_body,omitempty"`
	CreatedFrom  string `json:"created_from,omitempty"` // 来源 capture id
}

// HarvestPagination 分页策略。
//
// 核心是"怎么翻"和"什么时候停"两件事：
//   - 怎么翻：page（页码）/ offset（偏移量）/ cursor（游标）
//   - 什么时候停：没有数据 / 短页（条数 < page_size）/ 总数已抓完 / has_more=false
//     / 游标为空或不再变化 / 连续两页内容完全相同（防止接口忽略分页参数打转）/ 达到 max_pages
//
// mode=auto 时：先按录制样本探测（参数名、位置、总数/结束标志/游标路径），
// 运行时再按响应里的实际特征决定是否继续，用户什么都不用填。
type HarvestPagination struct {
	// Mode auto | page | offset | cursor | none
	Mode string `json:"mode"`
	// Param 翻页参数名：page / pageNo / current / offset / cursor …
	Param string `json:"param,omitempty"`
	// In 参数放在哪：query（默认）| body（POST JSON 请求体）
	In string `json:"in,omitempty"`
	// Start 起始值：page 模式默认 1，offset 模式默认 0
	Start int `json:"start,omitempty"`
	// Step 每次递增：page 模式默认 1，offset 模式默认 = page_size
	Step int `json:"step,omitempty"`
	// PageSize 每页条数（判断"短页即最后一页"、计算 offset 步长、算总页数）
	PageSize int `json:"page_size,omitempty"`
	// SizeParam 每页条数参数名（探测时发现就记住，例如 size / limit / pageSize）
	SizeParam string `json:"size_param,omitempty"`
	// MaxPages 本次最多翻几页。填了就以填的为准（硬顶 MaxPagesHardLimit）；
	// 留空（0）= 用任务的分页默认上限 max_pages_cap；只抓一次请把 mode 设为 none。
	MaxPages int `json:"max_pages,omitempty"`
	// TotalPath 总数所在路径（如 data.total），有它就能精确算总页数
	TotalPath string `json:"total_path,omitempty"`
	// HasMorePath 是否还有下一页（如 data.has_more）
	HasMorePath string `json:"has_more_path,omitempty"`
	// NextPath 游标模式：下一页游标所在路径（如 data.next_cursor）
	NextPath string `json:"next_path,omitempty"`
	// StopOnShort 返回条数 < PageSize 视为最后一页（默认开）
	StopOnShort bool `json:"stop_on_short"`
}

// Effective 返回填好默认值的副本（执行与展示都用它，避免各处重复兜底）。
func (p HarvestPagination) Effective() HarvestPagination {
	out := p
	mode := out.Mode
	if mode == "" {
		mode = "none"
	}
	out.Mode = mode
	if out.In == "" {
		out.In = "query"
	}
	if out.MaxPages <= 0 {
		out.MaxPages = 50
	}
	switch mode {
	case "page", "auto":
		if out.Start == 0 {
			out.Start = 1
		}
		if out.Step == 0 {
			out.Step = 1
		}
		if out.Param == "" {
			out.Param = "page"
		}
	case "offset":
		if out.Start == 0 {
			out.Start = 0
		}
		if out.Step == 0 {
			if out.PageSize > 0 {
				out.Step = out.PageSize
			} else {
				out.Step = 1
			}
		}
		if out.Param == "" {
			out.Param = "offset"
		}
	case "cursor":
		if out.Param == "" {
			out.Param = "cursor"
		}
		if out.NextPath == "" {
			out.NextPath = out.Param
		}
	}
	return out
}

// Enabled 是否真的需要翻页（none / 只有一页时返回 false）。
func (p HarvestPagination) Enabled() bool {
	e := p.Effective()
	return e.Mode != "none" && e.MaxPages > 1
}

// Describe 人可读的分页描述（控制台与日志用）。
func (p HarvestPagination) Describe() string {
	e := p.Effective()
	switch e.Mode {
	case "none":
		return "不分页（只抓一次）"
	case "cursor":
		s := "游标分页：" + e.Param + "（最多 " + itoa(e.MaxPages) + " 页）"
		if e.NextPath != "" {
			s += "，下一页取自 " + e.NextPath
		}
		return s
	case "offset":
		return "偏移分页：" + e.Param + "（从 " + itoa(e.Start) + " 起，每页 +" + itoa(e.Step) + "，最多 " + itoa(e.MaxPages) + " 页）"
	default:
		s := "页码分页：" + e.Param
		if e.In == "body" {
			s += "（请求体）"
		}
		s += "（从 " + itoa(e.Start) + " 起，最多 " + itoa(e.MaxPages) + " 页"
		if e.PageSize > 0 {
			s += "，每页约 " + itoa(e.PageSize) + " 条"
		}
		if e.TotalPath != "" {
			s += "，总数取自 " + e.TotalPath
		}
		return s + "）"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// HarvestSchedule 定时策略。
type HarvestSchedule struct {
	// Mode interval 固定间隔 | daily 每天定时 | manual 仅手动
	Mode string `json:"mode"`
	// EverySeconds interval 模式的间隔秒数（最小 60）
	EverySeconds int `json:"every_seconds,omitempty"`
	// Times daily 模式的时刻列表，如 ["09:00", "21:30"]
	Times []string `json:"times,omitempty"`
	// Weekdays 限定周几（1=周一 … 7=周日）；空 = 每天
	Weekdays []int `json:"weekdays,omitempty"`
	// JitterPercent 触发时刻随机抖动百分比（0-50），避免整点扎堆
	JitterPercent int `json:"jitter_percent,omitempty"`
}

// HarvestThrottle 限速与容错——"别给对方造成压力"的具体参数。
//
// 默认值刻意保守：相邻请求至少 1 秒、带随机抖动、单任务串行、
// 遇到 401/403/429 立刻收手，而不是硬扛着刷。
type HarvestThrottle struct {
	// MinIntervalMS 相邻请求最小间隔毫秒（默认 1000，低于 200 会拒绝）
	MinIntervalMS int `json:"min_interval_ms"`
	// JitterPercent 请求间隔随机抖动百分比（默认 30）
	JitterPercent int `json:"jitter_percent"`
	// TimeoutMS 单次请求超时（默认 20000）
	TimeoutMS int `json:"timeout_ms"`
	// Retries 失败重试次数（默认 2，指数退避）
	Retries int `json:"retries"`
	// BackoffMS 退避基数（默认 1500）
	BackoffMS int `json:"backoff_ms"`
	// MaxFailStreak 连续失败多少次后本次运行熔断（默认 3）
	MaxFailStreak int `json:"max_fail_streak"`
	// MaxPagesCap 分页默认上限：只在接口的 max_pages 留空（自动）时生效，默认 50；
	// 接口上显式填了 max_pages 就按填的走，只受 MaxPagesHardLimit 兜底。
	MaxPagesCap int `json:"max_pages_cap"`
}

// HarvestSink 数据落库目标。
type HarvestSink struct {
	// Driver sqlite | mysql | postgres | jsonl
	Driver string `json:"driver"`
	// DSN sqlite=文件路径（留空则用 data/harvest/harvest.db）；
	// mysql=user:pass@tcp(host:3306)/db?charset=utf8mb4；
	// postgres=postgres://user:pass@host:5432/db?sslmode=disable；jsonl 忽略
	DSN string `json:"dsn,omitempty"`
	// Table 表名（jsonl 模式即文件名）
	Table string `json:"table"`
	// Mode append 追加 | upsert 按 KeyColumn 覆盖
	Mode string `json:"mode"`
	// KeyColumn upsert 时的唯一键列
	KeyColumn string `json:"key_column,omitempty"`
	// BatchSize 单次提交行数（默认 200）
	BatchSize int `json:"batch_size,omitempty"`
}

// HarvestSession 会话复用：让定时抓取带着"录这条接口时那个登录态"。
type HarvestSession struct {
	// Mode none 无会话 | cookie 用下面粘贴的 Cookie | browser 运行时从内置浏览器导出
	Mode string `json:"mode"`
	// Cookie 形如 "a=1; b=2"
	Cookie string `json:"cookie,omitempty"`
	// UserAgent 留空则用录制时的 UA（不轮换、不伪造）
	UserAgent string `json:"user_agent,omitempty"`
	// Referer 留空则用录制时的页面地址
	Referer string `json:"referer,omitempty"`
	// Host 仅 browser 模式：从这个站点导出 Cookie（如 https://example.com）
	Host string `json:"host,omitempty"`
}

// HarvestTask 一个定时采集任务。
type HarvestTask struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// StartURL 录制时用的页面地址（browser 模式导 Cookie 也用它）
	StartURL string `json:"start_url,omitempty"`

	Endpoints []HarvestEndpoint `json:"endpoints"`
	Schedule  HarvestSchedule   `json:"schedule"`
	Throttle  HarvestThrottle   `json:"throttle"`
	Sink      HarvestSink       `json:"sink"`
	Session   HarvestSession    `json:"session"`

	// Status active | paused | error（error=连续失败被自动暂停）
	Status string `json:"status"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// ---- 运行状态（由服务端维护）----
	LastRunAt   time.Time `json:"last_run_at,omitempty"`
	NextRunAt   time.Time `json:"next_run_at,omitempty"`
	LastStatus  string    `json:"last_status,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LastWritten int       `json:"last_written"`
	RunCount    int       `json:"run_count"`
	FailStreak  int       `json:"fail_streak"`
}

// Clone 深拷贝。
func (t HarvestTask) Clone() HarvestTask {
	c := t
	c.Endpoints = make([]HarvestEndpoint, len(t.Endpoints))
	for i, e := range t.Endpoints {
		ne := e
		if e.Headers != nil {
			ne.Headers = make(map[string]string, len(e.Headers))
			for k, v := range e.Headers {
				ne.Headers[k] = v
			}
		}
		ne.Fields = append([]HarvestField(nil), e.Fields...)
		c.Endpoints[i] = ne
	}
	c.Schedule.Times = append([]string(nil), t.Schedule.Times...)
	c.Schedule.Weekdays = append([]int(nil), t.Schedule.Weekdays...)
	return c
}

// Summary 列表用副本（去掉样本响应体，列表不背大字段）。
func (t HarvestTask) Summary() HarvestTask {
	c := t.Clone()
	for i := range c.Endpoints {
		c.Endpoints[i].SampleBody = ""
	}
	return c
}

// EndpointCount 启用的接口数。
func (t HarvestTask) EndpointCount() int {
	n := 0
	for _, e := range t.Endpoints {
		if e.Enabled {
			n++
		}
	}
	return n
}

// HarvestEndpointResult 一次运行里单个接口的结果。
type HarvestEndpointResult struct {
	EndpointID string `json:"endpoint_id"`
	Name       string `json:"name"`
	Method     string `json:"method"`
	URL        string `json:"url"`
	Status     int    `json:"status"`
	Pages      int    `json:"pages"`
	Fetched    int    `json:"fetched"` // 解析出的记录数
	Written    int    `json:"written"` // 写库成功的行数
	// Duplicates 跨页去重时被跳过的重复记录数（接口分页不干净时会 > 0）
	Duplicates int    `json:"duplicates,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Retries    int    `json:"retries"`
	Bytes      int    `json:"bytes"`
	Error      string `json:"error,omitempty"`
	// Hint 给用户的处置建议（例如会话失效该怎么做）
	Hint string `json:"hint,omitempty"`
	// StopReason 为什么不再继续翻页（"短页即止" / "总数已抓完" / "连续两页相同" …）
	StopReason string `json:"stop_reason,omitempty"`
}

// HarvestRun 一次采集执行。
type HarvestRun struct {
	ID       string `json:"id"`
	TaskID   string `json:"task_id"`
	TaskName string `json:"task_name"`
	// Status queued | running | passed | failed | canceled
	Status string `json:"status"`
	// Trigger manual | schedule
	Trigger    string    `json:"trigger"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	Endpoints []HarvestEndpointResult `json:"endpoints"`
	Logs      []LogEntry              `json:"logs"`

	Requests int `json:"requests"`
	Fetched  int `json:"fetched"`
	Written  int `json:"written"`
	// Sink 本次写入位置描述（表名 / 文件路径）
	Sink  string `json:"sink"`
	Error string `json:"error,omitempty"`
}

// Summary 列表用副本。
func (r HarvestRun) Summary() HarvestRun {
	r.Logs = nil
	return r
}

// Clone 深拷贝。
func (r HarvestRun) Clone() HarvestRun {
	c := r
	c.Endpoints = append([]HarvestEndpointResult(nil), r.Endpoints...)
	c.Logs = append([]LogEntry(nil), r.Logs...)
	return c
}

// MaxPagesHardLimit 分页页数的硬顶：无论怎么填都不超过它。
// 作用是"配置写错也不会把对方打穿"，而不是限制正常需求（500 页这种是允许的）。
const MaxPagesHardLimit = 2000

// DefaultThrottle 出厂限速参数（保守）。
func DefaultThrottle() HarvestThrottle {
	return HarvestThrottle{
		MinIntervalMS: 1000,
		JitterPercent: 30,
		TimeoutMS:     20000,
		Retries:       2,
		BackoffMS:     1500,
		MaxFailStreak: 3,
		MaxPagesCap:   50,
	}
}

// DefaultSink 出厂落库配置。
func DefaultSink() HarvestSink {
	return HarvestSink{Driver: "sqlite", Table: "harvest_records", Mode: "append", BatchSize: 200}
}

// Normalize 把任务里明显不合理的值夹到安全范围（保存与执行前都调用一次）。
func (t *HarvestTask) Normalize() {
	if t.Schedule.Mode == "" {
		t.Schedule.Mode = "manual"
	}
	if t.Schedule.EverySeconds > 0 && t.Schedule.EverySeconds < 60 {
		t.Schedule.EverySeconds = 60
	}
	if t.Schedule.JitterPercent < 0 {
		t.Schedule.JitterPercent = 0
	}
	if t.Schedule.JitterPercent > 50 {
		t.Schedule.JitterPercent = 50
	}
	d := DefaultThrottle()
	if t.Throttle.MinIntervalMS < 200 {
		t.Throttle.MinIntervalMS = d.MinIntervalMS
	}
	if t.Throttle.JitterPercent < 0 {
		t.Throttle.JitterPercent = 0
	}
	if t.Throttle.JitterPercent > 90 {
		t.Throttle.JitterPercent = 90
	}
	if t.Throttle.TimeoutMS <= 0 {
		t.Throttle.TimeoutMS = d.TimeoutMS
	}
	if t.Throttle.Retries < 0 {
		t.Throttle.Retries = 0
	}
	if t.Throttle.Retries > 5 {
		t.Throttle.Retries = 5
	}
	if t.Throttle.BackoffMS <= 0 {
		t.Throttle.BackoffMS = d.BackoffMS
	}
	if t.Throttle.MaxFailStreak <= 0 {
		t.Throttle.MaxFailStreak = d.MaxFailStreak
	}
	if t.Throttle.MaxPagesCap <= 0 {
		t.Throttle.MaxPagesCap = d.MaxPagesCap
	}
	if t.Throttle.MaxPagesCap > MaxPagesHardLimit {
		t.Throttle.MaxPagesCap = MaxPagesHardLimit
	}
	if t.Sink.Driver == "" {
		t.Sink.Driver = "sqlite"
	}
	if t.Sink.Table == "" {
		t.Sink.Table = "harvest_records"
	}
	if t.Sink.Mode != "upsert" {
		t.Sink.Mode = "append"
	}
	if t.Sink.BatchSize <= 0 {
		t.Sink.BatchSize = 200
	}
	if t.Session.Mode == "" {
		t.Session.Mode = "none"
	}
	if t.Status == "" {
		t.Status = "active"
	}
	for i := range t.Endpoints {
		e := &t.Endpoints[i]
		if e.Method == "" {
			e.Method = "GET"
		}
		// 老任务的扁平分页字段 → 迁移到 Pagination（新字段优先）
		if e.Pagination.Mode == "" {
			if e.Pages > 1 {
				e.Pagination.Mode = "page"
				e.Pagination.Param = e.PageParam
				if e.Pagination.Param == "" {
					e.Pagination.Param = "page"
				}
				e.Pagination.Start = e.PageStart
				e.Pagination.Step = e.PageStep
				e.Pagination.MaxPages = e.Pages
			} else if e.PageParam != "" {
				// 只填了参数名：按"自动翻到没有为止"处理
				e.Pagination.Mode = "page"
				e.Pagination.Param = e.PageParam
				e.Pagination.MaxPages = 0
			}
		}
		// 最大页数：填了就以接口上填的为准（只受硬顶约束）；
		// 没填（0/自动）才用任务的分页默认上限，方便"自动识别"时有个安全阀。
		if e.Pagination.Mode != "" && e.Pagination.MaxPages <= 0 {
			e.Pagination.MaxPages = t.Throttle.MaxPagesCap
		}
		if e.Pagination.MaxPages > MaxPagesHardLimit {
			e.Pagination.MaxPages = MaxPagesHardLimit
		}
		e.Pagination = e.Pagination.Effective()
	}
}
