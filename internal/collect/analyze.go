// Package collect 实现「接口采集」全流程：
//
//	录制（CDP 旁路记录真实请求）→ 筛选 → 建成采集任务 → 定时触发 → 提取字段 → 落库
//
// 设计原则（也是"不惊动对方服务器"的具体做法）：
//   - 单任务串行、相邻请求之间有最小间隔 + 随机抖动；
//   - 只复用录制时那个真实登录态（Cookie / UA / Referer），不伪造、不轮换；
//   - 遇到 401/403/429 立刻停下并说明原因，而不是换 IP 硬刷。
package collect

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"uitest-server/internal/model"
)

// ParseJSONBody 把响应体解析成通用结构（失败返回错误，调用方可降级处理）。
func ParseJSONBody(body string) (any, error) {
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return nil, err
	}
	return v, nil
}

/* ---------------------------------------------------------------- JSON 路径 */

type pathSeg struct {
	key   string
	index int // >=0 表示数组下标
}

func splitPath(path string) []pathSeg {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "$")
	path = strings.TrimPrefix(path, ".")
	var segs []pathSeg
	i := 0
	for i < len(path) {
		// 读标识符
		start := i
		for i < len(path) && path[i] != '.' && path[i] != '[' {
			i++
		}
		if i > start {
			segs = append(segs, pathSeg{key: path[start:i], index: -1})
		}
		// 读 [n]
		for i < len(path) && path[i] == '[' {
			end := strings.IndexByte(path[i:], ']')
			if end < 0 {
				return segs
			}
			numStr := strings.TrimSpace(path[i+1 : i+end])
			i += end + 1
			if n, err := strconv.Atoi(numStr); err == nil {
				segs = append(segs, pathSeg{index: n})
			}
		}
		for i < len(path) && path[i] == '.' {
			i++
		}
	}
	return segs
}

// DigPath 按 a.b[0].c 取值。
func DigPath(root any, path string) (any, bool) {
	cur := root
	for _, seg := range splitPath(path) {
		if seg.index >= 0 {
			arr, ok := cur.([]any)
			if !ok || seg.index < 0 || seg.index >= len(arr) {
				return nil, false
			}
			cur = arr[seg.index]
			continue
		}
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg.key]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			// 数组上直接用 key：对每个元素取 key（方便 listName → 收集）
			out := make([]any, 0, len(node))
			for _, el := range node {
				if m, ok := el.(map[string]any); ok {
					if v, ok := m[seg.key]; ok {
						out = append(out, v)
					}
				}
			}
			if len(out) == 0 {
				return nil, false
			}
			cur = out
		default:
			return nil, false
		}
	}
	return cur, true
}

// RecordsFrom 从响应里取出记录数组。
//
// recordsPath 为空时：响应本身是数组就用它；是对象就自动找"最像列表"的那个数组。
func RecordsFrom(root any, recordsPath string) ([]any, string, error) {
	if strings.TrimSpace(recordsPath) != "" {
		v, ok := DigPath(root, recordsPath)
		if !ok {
			return nil, recordsPath, fmt.Errorf("响应里找不到路径 %s", recordsPath)
		}
		arr, ok := v.([]any)
		if !ok {
			// 单对象也允许：当作一条记录
			if m, ok2 := v.(map[string]any); ok2 {
				return []any{m}, recordsPath, nil
			}
			return nil, recordsPath, fmt.Errorf("路径 %s 不是数组/对象", recordsPath)
		}
		return arr, recordsPath, nil
	}
	switch node := root.(type) {
	case []any:
		return node, "", nil
	case map[string]any:
		path, arr := detectRecordsArray(node, "")
		if arr == nil {
			return []any{node}, "", nil
		}
		return arr, path, nil
	default:
		return nil, "", fmt.Errorf("响应不是 JSON 对象/数组（可能是 HTML 或非 JSON 接口）")
	}
}

// detectRecordsArray 在 JSON 树里找"最像记录列表"的数组：
// 元素是对象的数组优先，元素多的优先，层级浅的优先。
func detectRecordsArray(root map[string]any, prefix string) (string, []any) {
	bestPath := ""
	var best []any
	var walk func(node any, path string, depth int)
	walk = func(node any, path string, depth int) {
		if depth > 6 {
			return
		}
		switch v := node.(type) {
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				child := v[k]
				childPath := k
				if path != "" {
					childPath = path + "." + k
				}
				switch cv := child.(type) {
				case []any:
					objects := 0
					for _, el := range cv {
						if _, ok := el.(map[string]any); ok {
							objects++
						}
					}
					if objects > 0 && (best == nil || objects > lenObjects(best)) {
						best, bestPath = cv, childPath
					}
				case map[string]any:
					walk(cv, childPath, depth+1)
				}
			}
		}
	}
	walk(root, prefix, 0)
	return bestPath, best
}

func lenObjects(arr []any) int {
	n := 0
	for _, el := range arr {
		if _, ok := el.(map[string]any); ok {
			n++
		}
	}
	return n
}

/* -------------------------------------------------------------- 字段识别 */

// SuggestFields 从样本记录里推断字段映射（列名取 JSON key，路径取嵌套路径）。
func SuggestFields(records []any, limit int) []model.HarvestField {
	if limit <= 0 {
		limit = 10
	}
	if len(records) < limit {
		limit = len(records)
	}
	seen := map[string]model.HarvestField{}
	var order []string
	add := func(column, path string, v any) {
		if _, ok := seen[column]; ok {
			return
		}
		seen[column] = model.HarvestField{Column: column, Path: path, Kind: kindOfValue(v)}
		order = append(order, column)
	}
	for i := 0; i < limit; i++ {
		m, ok := records[i].(map[string]any)
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
			switch tv := v.(type) {
			case map[string]any:
				// 嵌套对象摊平一层：user{id,name} → user_id / user_name
				sub := make([]string, 0, len(tv))
				for sk := range tv {
					sub = append(sub, sk)
				}
				sort.Strings(sub)
				for _, sk := range sub {
					add(sanitizeColumn(k+"_"+sk), k+"."+sk, tv[sk])
				}
			case []any:
				// 数组存成 JSON 文本列，不继续下钻（避免列爆炸）
				add(sanitizeColumn(k), k, tv)
			default:
				add(sanitizeColumn(k), k, v)
			}
		}
	}
	out := make([]model.HarvestField, 0, len(order))
	for _, c := range order {
		out = append(out, seen[c])
	}
	return out
}

func kindOfValue(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64, float32, int, int64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "string"
	}
}

// sanitizeColumn 列名清洗：只留字母数字下划线中文，其余转下划线。
func sanitizeColumn(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r >= 0x4e00 && r <= 0x9fa5:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	if out == "" {
		out = "col"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "c_" + out
	}
	return out
}

/* -------------------------------------------------------------- 行提取 */

// FlattenRow 按字段映射把一条记录变成一行（列名 → 值）。
// vars 支持内置列：@now/@date/@timestamp/@url/@endpoint/@page。
func FlattenRow(rec any, fields []model.HarvestField, vars map[string]string) map[string]any {
	row := make(map[string]any, len(fields))
	for _, f := range fields {
		col := sanitizeColumn(f.Column)
		if col == "" || col == "col" && strings.TrimSpace(f.Column) == "" {
			continue
		}
		path := strings.TrimSpace(f.Path)
		if strings.HasPrefix(path, "@") {
			row[col] = builtinValue(path, vars)
			continue
		}
		if path == "" {
			continue
		}
		if v, ok := DigPath(rec, path); ok {
			row[col] = v
		} else {
			row[col] = nil
		}
	}
	return row
}

func builtinValue(name string, vars map[string]string) any {
	switch name {
	case "@now", "@datetime":
		if v, ok := vars["now"]; ok {
			return v
		}
	case "@date":
		if v, ok := vars["date"]; ok {
			return v
		}
	case "@timestamp":
		if v, ok := vars["timestamp"]; ok {
			return v
		}
	case "@url":
		return vars["url"]
	case "@endpoint":
		return vars["endpoint"]
	case "@page":
		if v, ok := vars["page"]; ok {
			return v
		}
	}
	return nil
}

// Columns 返回字段映射的列名（去重、清洗后）。
func Columns(fields []model.HarvestField) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range fields {
		c := sanitizeColumn(f.Column)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}
