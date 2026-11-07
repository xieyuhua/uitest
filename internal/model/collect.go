package model

import (
	"strings"
	"time"
)

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

	// ---- 录制时的响应样本（只服务「从样本重新识别字段/分页」，不参与抓取）----
	SampleBody  string `json:"sample_body,omitempty"`
	CreatedFrom string `json:"created_from,omitempty"` // 来源 capture request id

	// ---- 录制快照（请求侧）：建任务时从录制会话拷一份，任务里随时能回看"当初录到了什么" ----
	// 这些都是**明文**（含 Cookie / Authorization），用于判断会话与参数是否真的生效。
	SampleHeaders map[string]string `json:"sample_headers,omitempty"`  // 录制时的原始请求头
	SampleReqBody string            `json:"sample_req_body,omitempty"` // 录制时的请求体（截断）
	SamplePageURL string            `json:"sample_page_url,omitempty"` // 触发这条请求的页面
	SampleAt      string            `json:"sample_at,omitempty"`       // 录制时间

	// Sink 可选：给这一个接口单独指定落库目标（不同接口写不同文件 / 不同表）。
	// 只填需要的字段，其余沿用任务的 sink；例如 {"table":"products"}。
	// 留空时：任务开了 split_by_endpoint 就按接口名自动分表/分文件，否则与任务共用。
	Sink *HarvestSink `json:"sink,omitempty"`
}

// EndpointSlug 接口名 → 可做表名 / 文件名的短标识（中文保留，其余非法字符转下划线）。
func EndpointSlug(ep HarvestEndpoint) string {
	if s := slugify(ep.Name); s != "" {
		return s
	}
	if s := slugify(ep.ID); s != "" {
		return s
	}
	if s := slugify(ep.Method + "_" + ep.URL); s != "" {
		if len(s) > 40 {
			s = s[:40]
		}
		return s
	}
	return "endpoint"
}

// slugify 只保留字母、数字、下划线与中文，其它字符统一替换成下划线。
func slugify(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		case r >= 0x4e00 && r <= 0x9fa5:
			b.WriteRune(r)
		case r == ' ' || r == '/' || r == '.' || r == ':':
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
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
	// Driver sqlite | mysql | postgres | jsonl / json / xlsx
	Driver string `json:"driver"`
	// DSN sqlite=文件路径（留空则用 data/harvest/harvest.db）；
	// mysql=user:pass@tcp(host:3306)/db?charset=utf8mb4；
	// postgres=postgres://user:pass@host:5432/db?sslmode=disable；文件类忽略
	DSN string `json:"dsn,omitempty"`
	// Table 表名（文件类即文件名，不含扩展名）
	Table string `json:"table"`
	// Mode append 追加 | upsert 按 KeyColumn 覆盖
	Mode string `json:"mode"`
	// KeyColumn upsert 时的唯一键列
	KeyColumn string `json:"key_column,omitempty"`
	// BatchSize 单次提交行数（默认 200）
	BatchSize int `json:"batch_size,omitempty"`
}

// Merge 用 other 覆盖自己已填的字段（空值不覆盖），返回新副本。
func (s HarvestSink) Merge(other HarvestSink) HarvestSink {
	out := s
	if other.Driver != "" {
		out.Driver = other.Driver
	}
	if other.DSN != "" {
		out.DSN = other.DSN
	}
	if other.Table != "" {
		out.Table = other.Table
	}
	if other.Mode != "" {
		out.Mode = other.Mode
	}
	if other.KeyColumn != "" {
		out.KeyColumn = other.KeyColumn
	}
	if other.BatchSize > 0 {
		out.BatchSize = other.BatchSize
	}
	return out
}

// SinkProfile 一套可复用的落库配置（数据库连接 + 表 / 文件）。
//
// 任务与接口都可以引用它；"数据写入"页负责它的新建 / 测试 / 保存 / 删除。
type SinkProfile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Sink 内嵌，JSON 里直接展开成 driver / dsn / table / mode / key_column / batch_size
	HarvestSink
	// Note 备注（给团队看的一句话说明）
	Note string `json:"note,omitempty"`
	// CreatedAt / UpdatedAt 维护时间
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HarvestPrelogin 采集前先跑一段 UI 自动化录制（通常是「登录」），把内置浏览器会话刷新好再抓数据。
//
//	token / Cookie 会随登录状态变化时这是最稳的办法：真的在浏览器里点一遍登录，
//	之后再导出 Cookie / token，抓取就能带上最新的登录态。
type HarvestPrelogin struct {
	Enabled bool `json:"enabled"`
	// RecordingID UI 自动化里录好的「登录」流程
	RecordingID string `json:"recording_id,omitempty"`
	// RecordingName 只用于展示
	RecordingName string `json:"recording_name,omitempty"`
	// Mode always 每次运行前都跑 | on-fail 只在会话失效（401/403）时补跑
	Mode string `json:"mode,omitempty"`
	// TimeoutSeconds 跑登录流程的超时（默认 180）
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// ContinueOnError 登录流程失败时是否继续抓（默认 false：本次运行直接失败）
	ContinueOnError bool `json:"continue_on_error,omitempty"`
	// Vars 登录流程要用的变量（key=value）。
	// 录制时密码框会被脱敏成 {{password}}，必须在这里填上真实值才能在回放时真正登录；
	// 值支持 {{env:XXX}}，可以把账号密码放环境变量里。
	Vars map[string]string `json:"vars,omitempty"`
}

// Effective 填好默认值。
func (p HarvestPrelogin) Effective() HarvestPrelogin {
	out := p
	if strings.TrimSpace(out.Mode) == "" {
		out.Mode = "always"
	}
	if out.TimeoutSeconds <= 0 {
		out.TimeoutSeconds = 180
	}
	if out.TimeoutSeconds > 3600 {
		out.TimeoutSeconds = 3600
	}
	return out
}

// Active 是否真的需要在运行前跑一遍登录流程。
func (p HarvestPrelogin) Active() bool {
	return p.Enabled && strings.TrimSpace(p.RecordingID) != ""
}

// ModeText 人可读的触发时机。
func (p HarvestPrelogin) ModeText() string {
	if p.Effective().Mode == "on-fail" {
		return "会话失效时才跑"
	}
	return "每次运行前都跑"
}

// HarvestSession 会话：统一用内置浏览器的登录态。
//
// 现在的模型只有一种来源——「前置登录（UI 自动化）」把浏览器登进去，
// 之后抓取用的 Cookie 和请求头全部从浏览器里取（只有 URL / 请求体按接口配置）。
// Mode / Cookie / UserAgent / Referer 字段仅为兼容老任务保留，运行时一律按浏览器会话处理。
type HarvestSession struct {
	Mode      string `json:"mode,omitempty"`
	Cookie    string `json:"cookie,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
	Referer   string `json:"referer,omitempty"`
	// Host 从这个站点导出 Cookie/请求头（如 https://example.com）；留空用起始页/第一个接口的 origin
	Host string `json:"host,omitempty"`
	// HeaderMode 浏览器实时请求头怎么覆盖接口里录制下来的头：
	//   all（默认）浏览器抓到什么就用什么（除内容相关的请求头外全部共享）
	//   auth       只覆盖鉴权类（Authorization / *token* / *auth* / x-*）
	//   off        不覆盖，只用接口配置里的值
	HeaderMode string `json:"header_mode,omitempty"`
	// Source 会话来源（所有接口共用同一份会话）：
	//   ui      UI 自动化登录（前置登录），Cookie / 请求头从内置浏览器导出（默认）
	//   http    调登录接口换 token（可顺带把 Set-Cookie 带进来），不需要浏览器
	//   import  只用「公共参数」里手工导入的 Cookie / 参数，完全不碰浏览器
	Source string `json:"source,omitempty"`
	// Token 登录接口 / 手填 token 的配置（source=http 或 manual 时生效）
	Token *HarvestToken `json:"token,omitempty"`
}

// 会话来源常量（给接口层与前端对齐用）。
const (
	SessionSourceUI     = "ui"
	SessionSourceHTTP   = "http"
	SessionSourceImport = "import"
)

// EffectiveSource 会话来源（默认 ui；老任务里配了登录接口的自动按 http 处理）。
func (s HarvestSession) EffectiveSource() string {
	switch strings.ToLower(strings.TrimSpace(s.Source)) {
	case "ui", "browser", "prelogin":
		return SessionSourceUI
	case "http", "login", "api":
		return SessionSourceHTTP
	case "import", "manual", "none", "cookie":
		return SessionSourceImport
	}
	if s.Token != nil && s.Token.Effective().Source == "login" {
		return SessionSourceHTTP
	}
	return SessionSourceUI
}

// SourceText 人可读的会话来源。
func (s HarvestSession) SourceText() string {
	switch s.EffectiveSource() {
	case SessionSourceHTTP:
		return "登录接口换 token"
	case SessionSourceImport:
		return "仅导入（不用浏览器）"
	default:
		return "UI 自动化登录（浏览器会话）"
	}
}

// EffectiveHeaderMode 浏览器请求头覆盖策略（默认 all：除请求参数外与浏览器共享）。
func (s HarvestSession) EffectiveHeaderMode() string {
	switch strings.ToLower(strings.TrimSpace(s.HeaderMode)) {
	case "off", "none":
		return "off"
	case "auth":
		return "auth"
	default:
		return "all"
	}
}

// HarvestToken token 型站点的凭证来源与注入方式（界面上已不再提供配置入口，
// 仅保留后端兼容：老任务里存过的 token 配置仍然生效）。
type HarvestToken struct {
	// Source browser 从内置浏览器存储读 | manual 手填 | login 调登录接口换
	Source string `json:"source"`
	// Storage browser 模式下从哪读：localStorage（默认）| sessionStorage
	Storage string `json:"storage,omitempty"`
	// Key browser 模式下的键名，如 token / access_token / Authorization
	Key string `json:"key,omitempty"`
	// Value manual 模式的 token；支持 {{env:NAME}}，便于把密钥放在环境变量里
	Value string `json:"value,omitempty"`
	// Header 注入到哪个请求头，默认 Authorization
	Header string `json:"header,omitempty"`
	// Prefix 前缀，默认 "Bearer "；填 "-" 表示不加前缀
	Prefix string `json:"prefix,omitempty"`
	// CacheSeconds token 缓存多久（秒）
	CacheSeconds int `json:"cache_seconds,omitempty"`
	// Param token 写进哪个公共参数名（默认 token）：写进去之后 URL / 请求体 / 请求头里
	// 都能用 {{token}}，参数绑定也会用它覆盖旧值
	Param string `json:"param,omitempty"`
	// Login source=login：直接调登录接口换 token（不用开浏览器）
	Login *HarvestLogin `json:"login,omitempty"`
}

// HarvestLogin 登录接口：POST 一份账号密码，从响应 JSON 里取 token。
type HarvestLogin struct {
	Method    string            `json:"method,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      string            `json:"body,omitempty"`
	TokenPath string            `json:"token_path,omitempty"`
	// WithCookies 登录返回的 Set-Cookie 也带进采集请求（默认 true）
	WithCookies *bool `json:"with_cookies,omitempty"`
	// Vars 登录接口要用的变量（如 username / password），在 URL / Headers / Body 里写 {{name}}；
	// 值支持 {{env:XXX}}，把账号密码放环境变量里更安全
	Vars map[string]string `json:"vars,omitempty"`
}

// Effective 登录接口的默认值（Method=POST，TokenPath=data）。
func (l HarvestLogin) Effective() HarvestLogin {
	out := l
	if strings.TrimSpace(out.Method) == "" {
		out.Method = "POST"
	}
	if strings.TrimSpace(out.TokenPath) == "" {
		out.TokenPath = "data"
	}
	return out
}

// UseCookies 登录响应的 Cookie 是否带进采集请求。
func (l HarvestLogin) UseCookies() bool {
	return l.WithCookies == nil || *l.WithCookies
}

// Effective 填好默认值（来源为空时按 Value/Key/Login 推断）。
func (t HarvestToken) Effective() HarvestToken {
	out := t
	if out.Source == "" {
		switch {
		case out.Login != nil && strings.TrimSpace(out.Login.URL) != "":
			out.Source = "login"
		case strings.TrimSpace(out.Value) != "":
			out.Source = "manual"
		case strings.TrimSpace(out.Key) != "":
			out.Source = "browser"
		default:
			out.Source = "none"
		}
	}
	if out.Storage == "" {
		out.Storage = "localStorage"
	}
	if out.Header == "" {
		out.Header = "Authorization"
	}
	if out.Prefix == "" {
		out.Prefix = "Bearer "
	}
	if out.Prefix == "-" {
		out.Prefix = ""
	}
	if strings.TrimSpace(out.Param) == "" {
		out.Param = "token"
	}
	if out.Login != nil {
		lg := out.Login.Effective()
		out.Login = &lg
	}
	return out
}

// Enabled 是否需要注入 token。
func (t HarvestToken) Enabled() bool {
	return t.Effective().Source != "none"
}

// HarvestCommon 任务级「公共参数」：所有接口共用一份。
//
// 优先级（后者覆盖前者）：
//
//	公共参数 → 接口自己的 URL / 请求头 / 请求体 → 公共 Cookie（导入 / 录制汇总）
//	→ 前置登录后的浏览器 Cookie / 请求头 → 写了 {{变量}} 的公共请求头 → 参数绑定 → 登录后脚本
//
// 两个「显式」开关用来解决"登录后拿到的新 token 一定要生效"：
//   - 公共请求头里写了 {{token}} 视为显式，浏览器里的旧头盖不掉它；
//   - Bindings（参数绑定）优先级最高，可直接把参数写进 header / cookie / query / body。
type HarvestCommon struct {
	// Params 公共参数：GET/DELETE 进 URL query，POST/PUT/PATCH 进 JSON 请求体；
	// 也可以被 URL / 请求头 / 请求体里的 {{变量名}} 引用。
	Params map[string]string `json:"params,omitempty"`
	// Headers 公共请求头（浏览器共享的同名头会覆盖它；需要"一定生效"请在值里写 {{变量}} 或用 Bindings）
	Headers map[string]string `json:"headers,omitempty"`
	// Cookies 公共 Cookie：导入或从录制汇总而来。运行时先种进 Cookie jar，
	// 前置登录后浏览器里的同名 Cookie 会覆盖它（浏览器永远是最新登录态）。
	Cookies map[string]string `json:"cookies,omitempty"`
	// Bindings 参数绑定：把公共参数的值写进请求头 / Cookie / query / 请求体，优先级最高。
	Bindings []HarvestParamBinding `json:"bindings,omitempty"`
	// Refresh 登录后自动刷新公共参数（从浏览器请求头 / Cookie / localStorage 取值）
	Refresh *HarvestSessionRefresh `json:"refresh,omitempty"`
	// Script 登录成功后在内置浏览器里执行的脚本，返回值是 JSON：
	//   {"params":{"token":"…"},"headers":{"X-Token":"…"}}
	//   {"token":"…"}  → 自动写成 Authorization: Bearer <token>
	// 可以读 localStorage / cookie / 页面变量，用来把登录结果灌进公共参数。
	Script string `json:"script,omitempty"`
}

// HarvestParamBinding 公共参数 → 请求位置的绑定。
//
// 典型场景：登录拿到 token 后，用它替换请求头里的 Authorization：
//
//	{Param: "token", Target: "header", Name: "Authorization", Template: "Bearer {{token}}"}
//
// Template 不填时默认 {{param}}，支持 {{参数名}} / {{env:XXX}} 等变量。
type HarvestParamBinding struct {
	// Param 公共参数名（参数的值来自登录后刷新 / 脚本 / 手动填写）
	Param string `json:"param"`
	// Target 写到哪里：header | cookie | query | body
	Target string `json:"target,omitempty"`
	// Name 目标名称：请求头名 / Cookie 名 / query 参数名 / 请求体字段名
	Name string `json:"name,omitempty"`
	// Template 值模板，默认 "{{参数名}}"；如 "Bearer {{token}}"
	Template string `json:"template,omitempty"`
}

// Effective 归一化绑定：target 小写、默认 Name / Template。
func (b HarvestParamBinding) Effective() HarvestParamBinding {
	out := b
	out.Param = strings.TrimSpace(out.Param)
	out.Name = strings.TrimSpace(out.Name)
	out.Target = strings.ToLower(strings.TrimSpace(out.Target))
	switch out.Target {
	case "", "header", "headers":
		out.Target = "header"
	case "cookie", "cookies":
		out.Target = "cookie"
	case "query", "url", "param", "params":
		out.Target = "query"
	case "body", "json", "form":
		out.Target = "body"
	}
	if out.Name == "" {
		out.Name = out.Param
	}
	if strings.TrimSpace(out.Template) == "" && out.Param != "" {
		out.Template = "{{" + out.Param + "}}"
	}
	return out
}

// Valid 绑定是否可用（参数名与目标名都不能为空）。
func (b HarvestParamBinding) Valid() bool {
	e := b.Effective()
	return e.Param != "" && e.Name != ""
}

// HarvestSessionRefresh 登录后自动刷新公共参数。
//
// 典型用法（token 站点）：Enabled=true、TokenParam="token"，
// 再配一条 token → header:Authorization 的绑定（或直接打开自动绑定）。
// 运行顺序：前置登录 → 导出浏览器 Cookie / 请求头 → 本刷新写入公共参数 → 参数绑定生效。
type HarvestSessionRefresh struct {
	Enabled bool `json:"enabled"`
	// TokenParam 把浏览器实际使用的登录 token 写进这个公共参数（默认 token）
	TokenParam string `json:"token_param,omitempty"`
	// TokenHeader 从哪个请求头取 token；留空自动按 Authorization / *token* / *auth* 找
	TokenHeader string `json:"token_header,omitempty"`
	// Cookies Cookie 名 → 公共参数名（如 SESSION=sid）
	Cookies map[string]string `json:"cookies,omitempty"`
	// Storage 浏览器存储 → 公共参数名，键名格式 localStorage:key / sessionStorage:key
	Storage map[string]string `json:"storage,omitempty"`
}

// Effective 填默认值。
func (r *HarvestSessionRefresh) Effective() HarvestSessionRefresh {
	if r == nil {
		return HarvestSessionRefresh{}
	}
	out := *r
	out.TokenParam = strings.TrimSpace(out.TokenParam)
	if out.TokenParam == "" {
		out.TokenParam = "token"
	}
	out.TokenHeader = strings.TrimSpace(out.TokenHeader)
	return out
}

// Active 是否启用登录后刷新。
func (r *HarvestSessionRefresh) Active() bool {
	return r != nil && r.Enabled
}

// Normalize 清理公共参数：去空键、去无效/重复绑定、补默认值。
func (c *HarvestCommon) Normalize() {
	if c == nil {
		return
	}
	c.Params = cleanKV(c.Params)
	c.Headers = cleanKV(c.Headers)
	c.Cookies = cleanKV(c.Cookies)
	if len(c.Bindings) > 0 {
		out := make([]HarvestParamBinding, 0, len(c.Bindings))
		seen := map[string]bool{}
		for _, b := range c.Bindings {
			e := b.Effective()
			if !e.Valid() {
				continue
			}
			key := e.Target + "|" + strings.ToLower(e.Name) + "|" + e.Param
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, e)
		}
		c.Bindings = out
	}
	if c.Refresh != nil {
		rf := c.Refresh.Effective()
		c.Refresh = &rf
	}
}

// cleanKV 去掉 key / value 两端的空白，丢掉空 key。
func cleanKV(in map[string]string) map[string]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out[k] = v
	}
	return out
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
	// SplitByEndpoint true = 每个接口单独落库：表名/文件名 = sink.table + "_" + 接口名
	SplitByEndpoint bool `json:"split_by_endpoint,omitempty"`
	// SinkProfileID 从哪套「数据写入」配置复制来的（仅做溯源，值已展开进 Sink）
	SinkProfileID string         `json:"sink_profile_id,omitempty"`
	Session       HarvestSession `json:"session"`
	// Prelogin 可选：运行前先跑一段 UI 自动化（登录），刷新内置浏览器会话后再抓
	Prelogin *HarvestPrelogin `json:"prelogin,omitempty"`
	// Notify 可选：跑完把成功 / 失败结果推到企业微信群机器人
	Notify *HarvestNotify `json:"notify,omitempty"`
	// Common 公共参数：所有接口共用（前置登录的浏览器请求头、登录后脚本会覆盖同名项）
	Common *HarvestCommon `json:"common,omitempty"`

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
		if e.SampleHeaders != nil {
			ne.SampleHeaders = make(map[string]string, len(e.SampleHeaders))
			for k, v := range e.SampleHeaders {
				ne.SampleHeaders[k] = v
			}
		}
		if e.Sink != nil {
			s := *e.Sink
			ne.Sink = &s
		}
		c.Endpoints[i] = ne
	}
	c.Schedule.Times = append([]string(nil), t.Schedule.Times...)
	c.Schedule.Weekdays = append([]int(nil), t.Schedule.Weekdays...)
	if t.Session.Token != nil {
		tk := *t.Session.Token
		if tk.Login != nil {
			lg := *tk.Login
			if lg.Headers != nil {
				lg.Headers = make(map[string]string, len(tk.Login.Headers))
				for k, v := range tk.Login.Headers {
					lg.Headers[k] = v
				}
			}
			if lg.Vars != nil {
				lg.Vars = make(map[string]string, len(tk.Login.Vars))
				for k, v := range tk.Login.Vars {
					lg.Vars[k] = v
				}
			}
			tk.Login = &lg
		}
		c.Session.Token = &tk
	}
	if t.Prelogin != nil {
		pl := *t.Prelogin
		if t.Prelogin.Vars != nil {
			pl.Vars = make(map[string]string, len(t.Prelogin.Vars))
			for k, v := range t.Prelogin.Vars {
				pl.Vars[k] = v
			}
		}
		c.Prelogin = &pl
	}
	if t.Notify != nil {
		nt := *t.Notify
		if t.Notify.Mentioned != nil {
			nt.Mentioned = append([]string(nil), t.Notify.Mentioned...)
		}
		c.Notify = &nt
	}
	if t.Common != nil {
		cm := *t.Common
		cm.Params = cloneMap(t.Common.Params)
		cm.Headers = cloneMap(t.Common.Headers)
		cm.Cookies = cloneMap(t.Common.Cookies)
		cm.Bindings = append([]HarvestParamBinding(nil), t.Common.Bindings...)
		if t.Common.Refresh != nil {
			rf := *t.Common.Refresh
			rf.Cookies = cloneMap(t.Common.Refresh.Cookies)
			rf.Storage = cloneMap(t.Common.Refresh.Storage)
			cm.Refresh = &rf
		}
		c.Common = &cm
	}
	return c
}

// Summary 列表用副本（去掉录制快照里的响应体/请求体，列表不背大字段）。
func (t HarvestTask) Summary() HarvestTask {
	c := t.Clone()
	for i := range c.Endpoints {
		c.Endpoints[i].SampleBody = ""
		c.Endpoints[i].SampleReqBody = ""
		c.Endpoints[i].SampleHeaders = nil
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

// EndpointSink 计算某个接口实际使用的落库目标。
//
// 优先级：接口自己的 sink > 任务 sink；接口没写表名且任务开了 split_by_endpoint 时，
// 表名 = 任务表名 + "_" + 接口名（"不同接口写不同文件 / 不同表"就是这么来的）。
func (t HarvestTask) EndpointSink(ep HarvestEndpoint) HarvestSink {
	out := t.Sink
	explicitTable := ""
	if ep.Sink != nil {
		out = out.Merge(*ep.Sink)
		explicitTable = strings.TrimSpace(ep.Sink.Table)
	}
	if out.Table == "" {
		out.Table = "harvest_records"
	}
	if explicitTable == "" && t.SplitByEndpoint {
		out.Table = out.Table + "_" + EndpointSlug(ep)
	}
	if out.Driver == "" {
		out.Driver = "sqlite"
	}
	if out.BatchSize <= 0 {
		out.BatchSize = 200
	}
	if !strings.EqualFold(out.Mode, "upsert") {
		out.Mode = "append"
	}
	return out
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
	// Target 本次写入目标（表名 / 文件路径），便于确认"这个接口写到哪儿了"
	Target string `json:"target,omitempty"`
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

// HarvestNotify 任务跑完推送结果（企业微信群机器人）。
//
// Webhook 形如 https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=<群机器人 key>，
// 用的是"群机器人"接口（不需要 access_token），失败只会记进运行日志，不影响采集结果。
type HarvestNotify struct {
	Enabled bool `json:"enabled"`
	// Webhook 群机器人地址（含 key）
	Webhook string `json:"webhook,omitempty"`
	// OnSuccess 成功也推（不填默认推）
	OnSuccess *bool `json:"on_success,omitempty"`
	// OnFailure 失败也推（不填默认推）
	OnFailure *bool `json:"on_failure,omitempty"`
	// Mentioned text 消息里 @ 的人（userid / 手机号；填 all 表示 @所有人）
	Mentioned []string `json:"mentioned,omitempty"`
	// BaseURL 详情链接前缀，如 http://127.0.0.1:8490（留空则消息里不带链接）
	BaseURL string `json:"base_url,omitempty"`
}

// Active 是否真的会推送。
func (n HarvestNotify) Active() bool {
	return n.Enabled && strings.TrimSpace(n.Webhook) != ""
}

// WantSuccess 成功时是否推送。
func (n HarvestNotify) WantSuccess() bool { return n.OnSuccess == nil || *n.OnSuccess }

// WantFailure 失败时是否推送。
func (n HarvestNotify) WantFailure() bool { return n.OnFailure == nil || *n.OnFailure }

// HarvestRun 一次采集运行记录。
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

// Summary 列表用副本（去掉日志，列表不背大字段）。
func (r HarvestRun) Summary() HarvestRun {
	out := r
	out.Logs = nil
	return out
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
	// 会话统一走浏览器：老任务里的 none / cookie 一律当作 browser 处理
	t.Session.Mode = "browser"
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
	if t.Common != nil {
		t.Common.Normalize()
	}
}

// cloneMap 复制 string map（nil 保持 nil）。
func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
