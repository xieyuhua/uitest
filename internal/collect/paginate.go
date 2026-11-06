package collect

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"uitest-server/internal/model"
)

// 本文件是"分页"的全部逻辑，分两块：
//
//  1. DetectPagination：从录到的那一条请求 + 它的响应样本里，推断出
//     「用哪个参数翻页、放在 query 还是 body、每页多少条、总数/结束标志/游标在哪个字段」，
//     也就是"用户什么都不用填"；
//  2. Paginator：执行期驱动。负责把当前页参数写进 URL/请求体，并根据返回内容判断
//     "还有没有下一页"，同时用指纹防止在"接口忽略分页参数"时原地打转。
//
// 支持的翻页方式：
//   - page   页码：page=1,2,3…（最常见）
//   - offset 偏移：offset=0,20,40…（配 page_size 递增）
//   - cursor 游标：把上一页响应里的 next_cursor 带进下一次请求（深分页、时间线常用）

/* ------------------------------------------------------------ 参数名候选 */

var (
	pageParamCandidates = []string{
		"page", "pageno", "page_no", "pagenum", "page_num", "pageindex", "page_index",
		"pagenumber", "page_no_", "current", "curpage", "cur_page", "pn", "p", "pageid",
	}
	offsetParamCandidates = []string{
		"offset", "start", "from", "skip", "begin", "startindex", "start_index",
	}
	cursorParamCandidates = []string{
		"cursor", "next_cursor", "nextcursor", "pagecursor", "page_cursor", "lastid", "last_id",
		"since", "maxid", "max_id", "after", "before", "token", "nexttoken", "page_token",
	}
	sizeParamCandidates = []string{
		"size", "pagesize", "page_size", "limit", "per_page", "perpage", "rows", "num", "pagesize_",
	}
	totalKeyCandidates = []string{
		"total", "totalcount", "total_count", "totalnum", "total_num", "totalrecords",
		"total_records", "totalelements", "total_elements", "recordstotal", "totalitems",
		"total_items", "totalrow", "totalrows", "count",
	}
	hasMoreKeyCandidates = []string{
		"hasmore", "has_more", "hasnext", "has_next", "more", "ismore", "is_more",
		"hasnextpage", "has_next_page",
	}
	nextKeyCandidates = []string{
		"next", "next_cursor", "nextcursor", "nextpage", "next_page", "nextid", "next_id",
		"cursor", "nexttoken", "next_token", "nextstart", "next_start",
	}
)

func lowerSet(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, s := range list {
		m[strings.ToLower(s)] = true
	}
	return m
}

/* ---------------------------------------------------------------- 探测 */

// DetectPagination 从录制样本推断分页方式。
//
// rawURL：录制时那条请求的完整 URL；
// reqBody：请求体（POST 分页参数可能在这里）；
// root：响应 JSON 解析结果（可为 nil）；records：已解析出的记录数组（可为 nil）。
func DetectPagination(rawURL, reqBody string, root any, records []any) model.HarvestPagination {
	pag := model.HarvestPagination{Mode: "none", In: "query"}

	// ---- 1. 请求侧：找翻页参数（先 query，再 body）----
	qName, qVal := findParamInQuery(rawURL, pageParamCandidates)
	oName, oVal := findParamInQuery(rawURL, offsetParamCandidates)
	cName, cVal := findParamInQuery(rawURL, cursorParamCandidates)
	sName, sVal := findParamInQuery(rawURL, sizeParamCandidates)

	in := "query"
	if qName == "" && oName == "" && cName == "" {
		// query 里没有，看看请求体
		bq, bqVal := findParamInBody(reqBody, pageParamCandidates)
		bo, boVal := findParamInBody(reqBody, offsetParamCandidates)
		bc, bcVal := findParamInBody(reqBody, cursorParamCandidates)
		bs, bsVal := findParamInBody(reqBody, sizeParamCandidates)
		if bq != "" || bo != "" || bc != "" {
			in = "body"
			qName, qVal, oName, oVal, cName, cVal = bq, bqVal, bo, boVal, bc, bcVal
			if sName == "" {
				sName, sVal = bs, bsVal
			}
		}
	}

	// ---- 2. 响应侧：总数 / 是否还有下一页 / 下一页游标 ----
	var totalPath string
	var totalVal int
	if root != nil {
		if f, ok := findKeyDeep(root, lowerSet(totalKeyCandidates), 6); ok {
			if n, ok2 := toInt(f.Value); ok2 {
				totalPath, totalVal = f.Path, n
			}
		}
		if f, ok := findKeyDeep(root, lowerSet(hasMoreKeyCandidates), 6); ok {
			if _, ok2 := toBool(f.Value); ok2 {
				pag.HasMorePath = f.Path
			}
		}
		if f, ok := findKeyDeep(root, lowerSet(nextKeyCandidates), 6); ok {
			if s := toScalarString(f.Value); s != "" {
				pag.NextPath = f.Path
			}
		}
	}

	pageSize := 0
	if sVal != "" {
		if n, ok := toInt(sVal); ok && n > 0 && n <= 100000 {
			pageSize = n
		}
	}
	if pageSize == 0 && len(records) > 0 {
		pageSize = len(records)
	}

	// ---- 3. 组装 ----
	switch {
	case cName != "":
		// 游标优先：URL 里就带 cursor 的接口，通常不能靠页码翻
		pag.Mode = "cursor"
		pag.Param = cName
		pag.In = in
		if pag.NextPath == "" {
			pag.NextPath = cName
		}
		pag.PageSize = pageSize
		if cVal != "" && len(records) == 0 {
			// 录到的就是"翻到一半"的页面：从这一页继续
			pag.Start = 0
		}

	case oName != "":
		pag.Mode = "offset"
		pag.Param = oName
		pag.In = in
		if n, ok := toInt(oVal); ok && n >= 0 {
			pag.Start = n
		}
		pag.PageSize = pageSize

	case qName != "":
		pag.Mode = "page"
		pag.Param = qName
		pag.In = in
		if n, ok := toInt(qVal); ok && n >= 1 {
			pag.Start = n
		} else {
			pag.Start = 1
		}
		pag.PageSize = pageSize

	default:
		// 没找到任何翻页参数：如果响应明显还有数据（total > 本页条数），
		// 保持 none 但把"疑似分页"的线索留给调用方提示用户手工填。
		pag.Mode = "none"
		pag.PageSize = pageSize
	}

	if sName != "" {
		pag.SizeParam = sName
	}
	if totalPath != "" {
		pag.TotalPath = totalPath
		if pageSize > 0 && totalVal > 0 {
			pages := (totalVal + pageSize - 1) / pageSize
			if pages > 0 && pages+1 > pag.MaxPages {
				pag.MaxPages = pages + 1 // +1 让"空页判定"也能兜住
			}
		}
	}
	if pag.MaxPages > 500 {
		pag.MaxPages = 500
	}
	pag.StopOnShort = pageSize > 0
	return pag.Effective()
}

// DetectForCaptured 用一条录制结果推断分页（录制结果里既有请求 URL/请求体，也有响应样本）。
func DetectForCaptured(r model.CapturedRequest) model.HarvestPagination {
	var root any
	var records []any
	if strings.TrimSpace(r.BodyPreview) != "" {
		if parsed, err := ParseJSONBody(r.BodyPreview); err == nil {
			root = parsed
			if recs, _, rerr := RecordsFrom(parsed, ""); rerr == nil {
				records = recs
			}
		}
	}
	return DetectPagination(r.URL, r.PostData, root, records)
}

// 疑似"其实还有更多、但没识别出翻页参数"——用于给用户提示。
func LooksPaginatedButUnknown(pag model.HarvestPagination, root any, records []any) bool {
	if pag.Mode != "none" || root == nil {
		return false
	}
	f, ok := findKeyDeep(root, lowerSet(totalKeyCandidates), 6)
	if !ok {
		return false
	}
	total, ok := toInt(f.Value)
	return ok && total > len(records)
}

func findParamInQuery(rawURL string, names []string) (string, string) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", ""
	}
	q := u.Query()
	set := lowerSet(names)
	// query 是无序的，为保证可复现：按候选表的顺序找
	for _, want := range names {
		want = strings.ToLower(want)
		for k, vals := range q {
			if strings.ToLower(k) != want || !set[want] {
				continue
			}
			if len(vals) > 0 {
				return k, vals[0]
			}
			return k, ""
		}
	}
	return "", ""
}

func findParamInBody(body string, names []string) (string, string) {
	body = strings.TrimSpace(body)
	if body == "" || !strings.HasPrefix(body, "{") {
		return "", ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return "", ""
	}
	for _, want := range names {
		want = strings.ToLower(want)
		for k, v := range m {
			if strings.ToLower(k) != want {
				continue
			}
			return k, toScalarString(v)
		}
	}
	return "", ""
}

type foundValue struct {
	Path  string
	Value any
}

// findKeyDeep 在 JSON 树里找某个键（BFS，天然偏向浅层；不进入数组，
// 避免命中"每条记录里的 count 字段"这种干扰）。
func findKeyDeep(root any, names map[string]bool, maxDepth int) (foundValue, bool) {
	type item struct {
		node  any
		path  string
		depth int
	}
	queue := []item{{root, "", 0}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		m, ok := cur.node.(map[string]any)
		if !ok {
			continue
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := m[k]
			path := k
			if cur.path != "" {
				path = cur.path + "." + k
			}
			if names[strings.ToLower(k)] {
				switch v.(type) {
				case map[string]any, []any:
					// 结构化的同名键不算（例如 data.total 才是数）
				default:
					return foundValue{Path: path, Value: v}, true
				}
			}
			if cur.depth+1 <= maxDepth {
				if child, ok := v.(map[string]any); ok {
					queue = append(queue, item{child, path, cur.depth + 1})
				}
			}
		}
	}
	return foundValue{}, false
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		i, err := strconv.Atoi(s)
		return i, err == nil
	default:
		return 0, false
	}
}

func toBool(v any) (bool, bool) {
	switch b := v.(type) {
	case bool:
		return b, true
	case float64:
		return b != 0, true
	case string:
		s := strings.ToLower(strings.TrimSpace(b))
		switch s {
		case "true", "1", "yes", "y":
			return true, true
		case "false", "0", "no", "n", "":
			return false, true
		}
	}
	return false, false
}

func toScalarString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		if b, err := json.Marshal(x); err == nil && len(b) < 512 {
			return string(b)
		}
		return ""
	}
}

/* -------------------------------------------------------------- 取值辅助 */

// ExtractString 按路径取标量字符串（数字也转成字符串，用于游标）。
func ExtractString(root any, path string) string {
	if strings.TrimSpace(path) == "" || root == nil {
		return ""
	}
	v, ok := DigPath(root, path)
	if !ok {
		return ""
	}
	return toScalarString(v)
}

// ExtractInt 按路径取整数。
func ExtractInt(root any, path string) (int, bool) {
	if strings.TrimSpace(path) == "" || root == nil {
		return 0, false
	}
	v, ok := DigPath(root, path)
	if !ok {
		return 0, false
	}
	return toInt(v)
}

// ExtractBool 按路径取布尔。
func ExtractBool(root any, path string) (bool, bool) {
	if strings.TrimSpace(path) == "" || root == nil {
		return false, false
	}
	v, ok := DigPath(root, path)
	if !ok {
		return false, false
	}
	return toBool(v)
}

/* ---------------------------------------------------------------- 驱动 */

// Paginator 一次采集里被一个接口独占的分页驱动器。
type Paginator struct {
	pag       model.HarvestPagination
	value     int    // 页码 / 偏移
	cursor    string // 游标
	pages     int
	prevPrint string // 上一页指纹（防原地打转）
}

// NewPaginator 构造分页驱动器（mode=auto 在运行期按 page 处理）。
func NewPaginator(pag model.HarvestPagination) *Paginator {
	e := pag.Effective()
	if e.Mode == "auto" {
		e.Mode = "page"
	}
	return &Paginator{pag: e, value: e.Start}
}

// NewPaginatorFor 同 NewPaginator，但会把 in=auto 按 HTTP 方法定下来：
// POST / PUT / PATCH 的翻页参数进 JSON 请求体，GET / DELETE 进 URL 查询串。
func NewPaginatorFor(pag model.HarvestPagination, method string) *Paginator {
	p := NewPaginator(pag)
	if strings.EqualFold(p.pag.In, "auto") {
		switch strings.ToUpper(strings.TrimSpace(method)) {
		case "POST", "PUT", "PATCH":
			p.pag.In = "body"
		default:
			p.pag.In = "query"
		}
	}
	return p
}

// Describe 当前分页策略描述。
func (p *Paginator) Describe() string { return p.pag.Describe() }

// Enabled 是否需要翻页。
func (p *Paginator) Enabled() bool { return p.pag.Enabled() }

// Current 当前页标识（日志用）。
func (p *Paginator) Current() string {
	if p.pag.Mode == "cursor" {
		if p.cursor == "" {
			return "(首页)"
		}
		return p.cursor
	}
	return strconv.Itoa(p.value)
}

// Apply 把当前页参数写进目标 URL（query）或请求体（body），返回改写后的两者。
//
// 配了「每页条数参数名」时，顺便把 page_size 也带上（size / limit / pageSize 这类站点很常见）。
func (p *Paginator) Apply(target, body string) (string, string) {
	if !p.pag.Enabled() {
		return target, body
	}
	var value string
	if p.pag.Mode == "cursor" {
		value = p.cursor
	} else {
		value = strconv.Itoa(p.value)
	}
	size := ""
	if p.pag.PageSize > 0 && strings.TrimSpace(p.pag.SizeParam) != "" {
		size = strconv.Itoa(p.pag.PageSize)
	}
	if p.pag.In == "body" {
		if size != "" {
			body = setJSONBodyParam(body, p.pag.SizeParam, size)
		}
		return target, setJSONBodyParam(body, p.pag.Param, value)
	}
	if size != "" {
		target = setQueryParam(target, p.pag.SizeParam, size)
	}
	if value == "" {
		return removeQueryParam(target, p.pag.Param), body
	}
	return setQueryParam(target, p.pag.Param, value), body
}

// Advance 记录本页结果并决定是否继续。返回 stop=true 时 reason 说明原因。
func (p *Paginator) Advance(records []any, root any) (bool, string) {
	p.pages++
	pages := p.pages
	if pages == 1 && p.pag.Mode == "cursor" && p.cursor == "" {
		// 第一页就是"从头开始"；把响应里的游标记下来供下一页用
		if next := ExtractString(root, p.pag.NextPath); next != "" {
			p.prevPrint = fingerprintRecords(records)
			p.cursor = next
			return false, ""
		}
	}

	if p.pag.MaxPages > 0 && pages >= p.pag.MaxPages {
		return true, fmt.Sprintf("已达最大页数 %d（可在任务里调大 max_pages）", p.pag.MaxPages)
	}

	switch p.pag.Mode {
	case "cursor":
		next := ExtractString(root, p.pag.NextPath)
		if next == "" {
			return true, "响应里没有下一页游标，判定抓完"
		}
		if next == p.cursor {
			return true, "游标没有变化，判定抓完（避免死循环）"
		}
		p.cursor = next
		return false, ""
	}

	if p.pag.HasMorePath != "" {
		if more, ok := ExtractBool(root, p.pag.HasMorePath); ok && !more {
			return true, "响应标记没有更多数据（" + p.pag.HasMorePath + "=false）"
		}
	}
	if p.pag.TotalPath != "" && p.pag.PageSize > 0 {
		if total, ok := ExtractInt(root, p.pag.TotalPath); ok && total >= 0 {
			totalPages := (total + p.pag.PageSize - 1) / p.pag.PageSize
			if totalPages > 0 && pages >= totalPages {
				return true, fmt.Sprintf("总数 %d 条已抓完（每页 %d 条）", total, p.pag.PageSize)
			}
		}
	}
	if p.pag.StopOnShort && p.pag.PageSize > 0 && len(records) < p.pag.PageSize {
		return true, fmt.Sprintf("本页 %d 条 < 每页 %d 条，判定最后一页", len(records), p.pag.PageSize)
	}
	if p.pag.Step > 0 {
		p.value += p.pag.Step
	}
	return false, ""
}

// LoopDetected 判断这一页是不是和上一页完全一样（接口忽略分页参数时会出现）。
func (p *Paginator) LoopDetected(records []any) bool {
	fp := fingerprintRecords(records)
	if fp == "" {
		return false
	}
	same := fp == p.prevPrint
	p.prevPrint = fp
	return same
}

func fingerprintRecords(records []any) string {
	if len(records) == 0 {
		return ""
	}
	limit := len(records)
	if limit > 200 {
		limit = 200
	}
	raw, err := json.Marshal(records[:limit])
	if err != nil {
		return ""
	}
	sum := sha1.Sum(raw)
	return hex.EncodeToString(sum[:])
}

// rowFingerprint 一行数据的指纹（用于跨页去重）。
func rowFingerprint(row map[string]any) string {
	raw, err := json.Marshal(row)
	if err != nil {
		return ""
	}
	sum := sha1.Sum(raw)
	return hex.EncodeToString(sum[:])
}

/* -------------------------------------------------------- URL/Body 改写 */

// removeQueryParam 删掉一个 query 参数（游标首页时用）。
func removeQueryParam(raw, name string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if !q.Has(name) {
		return raw
	}
	q.Del(name)
	u.RawQuery = q.Encode()
	return u.String()
}

// setJSONBodyParam 把参数写进 JSON 请求体（数字按数字写，其余按字符串）。
func setJSONBodyParam(body, name, value string) string {
	body = strings.TrimSpace(body)
	if body == "" || !strings.HasPrefix(body, "{") {
		return body
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return body
	}
	if n, err := strconv.Atoi(value); err == nil {
		m[name] = n
	} else {
		m[name] = value
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return string(raw)
}
