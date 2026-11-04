package collect

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"uitest-server/internal/model"
)

// 本文件负责"接口筛选"：把录制到的一堆请求按条件过滤，并转成可采集的接口定义。

// Filter 筛选条件（控制台筛选栏与 API 查询参数一一对应）。
type Filter struct {
	// Q 关键字：匹配 URL / 路径 / 主机
	Q string
	// Methods / Types / Kinds 多选过滤（空 = 不限）
	Methods []string
	Types   []string
	Kinds   []string
	// MinStatus / MaxStatus 状态码区间（0 = 不限）
	MinStatus int
	MaxStatus int
	// Include / Exclude 正则（URL 维度）
	Include string
	Exclude string
	// OnlyWithBody 只保留有响应体的（一般就是真正返回数据的接口）
	OnlyWithBody bool
	// OnlyJSON 只保留 JSON 响应
	OnlyJSON bool
	// Host 只保留某个站点
	Host string
}

// ParseFilter 从 query string 解析筛选条件。
func ParseFilter(q url.Values) (Filter, error) {
	f := Filter{
		Q:            strings.TrimSpace(q.Get("q")),
		Methods:      splitCSV(q.Get("method")),
		Types:        splitCSV(q.Get("type")),
		Kinds:        splitCSV(q.Get("kind")),
		Include:      strings.TrimSpace(q.Get("include")),
		Exclude:      strings.TrimSpace(q.Get("exclude")),
		Host:         strings.TrimSpace(q.Get("host")),
		OnlyWithBody: q.Get("with_body") == "1",
		OnlyJSON:     q.Get("json") == "1",
	}
	if v := q.Get("min_status"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return f, fmt.Errorf("min_status 不是数字: %s", v)
		}
		f.MinStatus = n
	}
	if v := q.Get("max_status"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return f, fmt.Errorf("max_status 不是数字: %s", v)
		}
		f.MaxStatus = n
	}
	if f.Include != "" {
		if _, err := regexp.Compile(f.Include); err != nil {
			return f, fmt.Errorf("include 正则不合法: %v", err)
		}
	}
	if f.Exclude != "" {
		if _, err := regexp.Compile(f.Exclude); err != nil {
			return f, fmt.Errorf("exclude 正则不合法: %v", err)
		}
	}
	return f, nil
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func inSet(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return true
		}
	}
	return false
}

// Match 判断一条录制结果是否通过筛选。
func (f Filter) Match(r model.CapturedRequest) bool {
	if len(f.Methods) > 0 && !inSet(f.Methods, r.Method) {
		return false
	}
	if len(f.Types) > 0 && !inSet(f.Types, r.Type) {
		return false
	}
	if len(f.Kinds) > 0 && !inSet(f.Kinds, r.Kind) {
		return false
	}
	if f.MinStatus > 0 && r.Status < f.MinStatus {
		return false
	}
	if f.MaxStatus > 0 && r.Status > f.MaxStatus {
		return false
	}
	if f.Host != "" && !strings.Contains(strings.ToLower(r.Host), strings.ToLower(f.Host)) {
		return false
	}
	if f.OnlyWithBody && !r.HasBody {
		return false
	}
	if f.OnlyJSON && !isJSONResponse(r) {
		return false
	}
	if f.Q != "" {
		hay := strings.ToLower(r.Method + " " + r.URL + " " + r.Path + " " + r.Host)
		if !strings.Contains(hay, strings.ToLower(f.Q)) {
			return false
		}
	}
	if f.Include != "" {
		re, err := regexp.Compile(f.Include)
		if err != nil || !re.MatchString(r.URL) {
			return false
		}
	}
	if f.Exclude != "" {
		re, err := regexp.Compile(f.Exclude)
		if err == nil && re.MatchString(r.URL) {
			return false
		}
	}
	return true
}

// Apply 过滤整个列表（保持录制顺序）。
func (f Filter) Apply(list []model.CapturedRequest) []model.CapturedRequest {
	out := make([]model.CapturedRequest, 0, len(list))
	for _, r := range list {
		if f.Match(r) {
			out = append(out, r)
		}
	}
	return out
}

func isJSONResponse(r model.CapturedRequest) bool {
	mt := strings.ToLower(r.MimeType)
	if strings.Contains(mt, "json") {
		return true
	}
	body := strings.TrimSpace(r.BodyPreview)
	return strings.HasPrefix(body, "{") || strings.HasPrefix(body, "[")
}

// Facets 录制结果的统计维度（给筛选栏显示"每种方法/类型多少条"）。
type Facets struct {
	Total     int            `json:"total"`
	Kept      int            `json:"kept"`
	ByMethod  map[string]int `json:"by_method"`
	ByType    map[string]int `json:"by_type"`
	ByKind    map[string]int `json:"by_kind"`
	WithBody  int            `json:"with_body"`
	JSONCount int            `json:"json"`
	Hosts     []string       `json:"hosts"`
}

// BuildFacets 统计（all=全部录制结果，kept=筛选后的）。
func BuildFacets(all, kept []model.CapturedRequest) Facets {
	f := Facets{Total: len(all), Kept: len(kept), ByMethod: map[string]int{}, ByType: map[string]int{}, ByKind: map[string]int{}}
	hosts := map[string]bool{}
	for _, r := range all {
		f.ByMethod[r.Method]++
		f.ByType[r.Type]++
		f.ByKind[r.Kind]++
		if r.HasBody {
			f.WithBody++
		}
		if isJSONResponse(r) {
			f.JSONCount++
		}
		if r.Host != "" {
			hosts[r.Host] = true
		}
	}
	for h := range hosts {
		f.Hosts = append(f.Hosts, h)
	}
	sort.Strings(f.Hosts)
	return f
}

// 不进入采集定义的"浏览器/传输层"请求头：这些由会话配置统一处理，
// 原样带过去反而容易过期（Content-Length 之类甚至会直接让请求失败）。
var skipHeaders = map[string]bool{
	"cookie": true, "host": true, "content-length": true, "connection": true,
	"accept-encoding": true, "origin": true, "user-agent": true, "referer": true,
	"sec-fetch-site": true, "sec-fetch-mode": true, "sec-fetch-dest": true,
	"sec-fetch-user": true, "sec-ch-ua": true, "sec-ch-ua-mobile": true,
	"sec-ch-ua-platform": true, "priority": true, "upgrade-insecure-requests": true,
	"if-none-match": true, "if-modified-since": true, "cache-control": true,
	"pragma": true, "accept-language": true,
}

// EndpointFromCaptured 把一条录制结果转成采集接口定义（含样本、默认字段映射）。
//
// 需要保留的请求头：Content-Type / Accept / Authorization / X-* 等业务头；
// Cookie / UA / Referer 走 Session，避免把过期时间点钉死在接口配置里。
func EndpointFromCaptured(r model.CapturedRequest) model.HarvestEndpoint {
	ep := model.HarvestEndpoint{
		ID:           newID("ep_"),
		Name:         defaultEndpointName(r),
		Method:       strings.ToUpper(r.Method),
		Enabled:      true,
		URL:          r.URL,
		SampleStatus: r.Status,
		CreatedFrom:  r.ID,
	}
	headers := map[string]string{}
	for k, v := range r.ReqHeaders {
		lk := strings.ToLower(k)
		if skip, ok := skipHeaders[lk]; ok && skip {
			continue
		}
		if strings.HasPrefix(lk, "sec-") || strings.HasPrefix(lk, ":") {
			continue
		}
		// 伪头（:authority 之类）一律丢掉
		if strings.HasPrefix(k, ":") {
			continue
		}
		headers[k] = v
	}
	if len(headers) > 0 {
		ep.Headers = headers
	}
	if r.PostData != "" {
		ep.Body = r.PostData
	}
	// 分页：从"这条请求的 URL / 请求体 + 响应样本"自动推断（page / offset / cursor）
	var sampleRoot any
	var sampleRecords []any
	if r.HasBody && r.BodyPreview != "" {
		if root, err := ParseJSONBody(r.BodyPreview); err == nil {
			sampleRoot = root
			if records, path, rerr := RecordsFrom(root, ""); rerr == nil && len(records) > 0 {
				ep.RecordsPath = path
				ep.Fields = SuggestFields(records, 5)
				sampleRecords = records
			}
		}
	}
	ep.Pagination = DetectPagination(r.URL, r.PostData, sampleRoot, sampleRecords)
	return ep
}

// defaultEndpointName 生成易读的接口名：GET /api/items。
func defaultEndpointName(r model.CapturedRequest) string {
	p := r.Path
	if p == "" {
		if u, err := url.Parse(r.URL); err == nil {
			p = u.Path
		}
	}
	if p == "" {
		p = "/"
	}
	name := strings.ToUpper(r.Method) + " " + p
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}
