package collect

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/xuri/excelize/v2"
)

// 本文件是"文件类"落库目标：Excel 表格（.xlsx）与 JSON 文件（.json）。
//
// 共同点：
//   - 都在 <数据目录>/harvest/ 下，文件名 = <表名>.<ext>，控制台可以直接下载；
//   - 表头/字段集合自动增长（响应里多出字段就加一列）；
//   - append 追加、upsert 按主键覆盖；
//   - 每次写入整份重写：文件小、逻辑简单、用 Excel/WPS 打开永远是一份完整数据。

/* ------------------------------------------------------------------ Excel */

type excelSink struct {
	path  string
	sheet string
	mode  string
	key   string

	mu sync.Mutex
}

func newExcelSink(path, sheet, mode, key string) *excelSink {
	return &excelSink{path: path, sheet: sheetName(sheet), mode: mode, key: key}
}

func (s *excelSink) Describe() string { return "Excel 表格 " + s.path }

func (s *excelSink) Close() error { return nil }

// sheetName Excel 工作表名限制：<=31 字符，且不能含 []:*?/\
func sheetName(s string) string {
	if s == "" {
		s = "harvest"
	}
	r := strings.NewReplacer("[", "_", "]", "_", ":", "_", "*", "_", "?", "_", "/", "_", "\\", "_")
	s = r.Replace(s)
	if len([]rune(s)) > 31 {
		s = string([]rune(s)[:31])
	}
	return s
}

func (s *excelSink) Write(rows []map[string]any) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	existed := false
	if st, statErr := os.Stat(s.path); statErr == nil && !st.IsDir() {
		existed = true
	}
	var f *excelize.File
	var err error
	if existed {
		f, err = excelize.OpenFile(s.path)
		if err != nil {
			return 0, fmt.Errorf("打开 Excel 失败：%w", err)
		}
	} else {
		f = excelize.NewFile()
	}
	defer f.Close()

	sheet, header, data := s.readSheet(f)

	// 新列自动加在表头后面
	known := map[string]bool{}
	for _, c := range header {
		known[c] = true
	}
	for _, c := range unionColumns(rows) {
		if !known[c] {
			header = append(header, c)
			known[c] = true
		}
	}
	if len(header) == 0 {
		return 0, nil
	}

	colIdx := make(map[string]int, len(header))
	for i, c := range header {
		colIdx[c] = i
	}
	keyIdx := -1
	if s.mode == "upsert" && s.key != "" {
		if i, ok := colIdx[s.key]; ok {
			keyIdx = i
		}
	}

	written := 0
	if keyIdx >= 0 {
		rowAt := map[string]int{}
		for i, r := range data {
			if v := cellAt(r, keyIdx); v != "" {
				rowAt[v] = i
			}
		}
		for _, row := range rows {
			vals := rowToCells(row, header)
			if k := cellAt(vals, keyIdx); k != "" {
				if at, ok := rowAt[k]; ok {
					data[at] = vals
					written++
					continue
				}
				rowAt[k] = len(data)
			}
			data = append(data, vals)
			written++
		}
	} else {
		for _, row := range rows {
			data = append(data, rowToCells(row, header))
			written++
		}
	}

	if err := writeSheet(f, sheet, header, data); err != nil {
		return 0, err
	}
	if existed {
		err = f.Save()
	} else {
		err = f.SaveAs(s.path)
	}
	if err != nil {
		return 0, fmt.Errorf("保存 Excel 失败：%w", err)
	}
	return written, nil
}

// readSheet 读回已有内容（表头 + 数据行）；文件不存在时返回空表头。
func (s *excelSink) readSheet(f *excelize.File) (string, []string, [][]string) {
	sheet := s.sheet
	// 优先用同名 sheet；否则用第一个 sheet（兼容用户手工改过表名）
	found := false
	for _, name := range f.GetSheetList() {
		if name == sheet {
			found = true
			break
		}
	}
	if !found {
		for _, name := range f.GetSheetList() {
			if rows, err := f.GetRows(name); err == nil && len(rows) > 0 {
				sheet = name
				found = true
				break
			}
		}
	}
	if !found {
		return sheet, nil, nil
	}
	rows, err := f.GetRows(sheet)
	if err != nil || len(rows) == 0 {
		return sheet, nil, nil
	}
	header := make([]string, len(rows[0]))
	copy(header, rows[0])
	data := make([][]string, 0, len(rows)-1)
	data = append(data, rows[1:]...)
	return sheet, header, data
}

// writeSheet 整份重写（表头加粗 + 冻结首行 + 合理列宽）。
func writeSheet(f *excelize.File, sheet string, header []string, data [][]string) error {
	// 没有这个 sheet 就建一个，并删掉默认的 Sheet1
	exists := false
	for _, name := range f.GetSheetList() {
		if name == sheet {
			exists = true
			break
		}
	}
	if !exists {
		if _, err := f.NewSheet(sheet); err != nil {
			return err
		}
	}
	if err := f.SetSheetRow(sheet, "A1", &header); err != nil {
		return fmt.Errorf("写表头失败：%w", err)
	}
	for i, row := range data {
		axis, err := excelize.CoordinatesToCellName(1, i+2)
		if err != nil {
			continue
		}
		vals := make([]any, len(row))
		for j, v := range row {
			vals[j] = excelCell(v)
		}
		if err := f.SetSheetRow(sheet, axis, &vals); err != nil {
			return fmt.Errorf("写第 %d 行失败：%w", i+2, err)
		}
	}

	// 样式：表头加粗 + 冻结首行 + 列宽
	lastCol, _ := excelize.ColumnNumberToName(max(len(header), 1))
	if style, serr := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"EEF2FF"}},
	}); serr == nil {
		_ = f.SetCellStyle(sheet, "A1", lastCol+"1", style)
	}
	_ = f.SetPanes(sheet, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
	for i, h := range header {
		col, cerr := excelize.ColumnNumberToName(i + 1)
		if cerr != nil {
			continue
		}
		width := 12.0
		if n := float64(len([]rune(h))); n*1.6 > width {
			width = n * 1.6
		}
		if width > 32 {
			width = 32
		}
		_ = f.SetColWidth(sheet, col, col, width)
	}

	// 删掉默认空表（新建文件时自带 Sheet1）
	if sheet != "Sheet1" {
		if rows, err := f.GetRows("Sheet1"); err == nil && len(rows) == 0 {
			_ = f.DeleteSheet("Sheet1")
		}
	}
	return nil
}

// excelCell 数字按数字写（Excel 里能排序/求和），其余按文本；超长数字串保持文本，避免精度丢失。
func excelCell(v string) any {
	if v == "" {
		return ""
	}
	digits := strings.TrimLeft(v, "-")
	if len(digits) > 15 {
		return v
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		return n
	}
	return v
}

func rowToCells(row map[string]any, header []string) []string {
	out := make([]string, len(header))
	for i, c := range header {
		out[i] = cellString(row[c])
	}
	return out
}

func cellAt(row []string, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[idx])
}

// cellString 任意 JSON 值 → 单元格文本。
func cellString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == float64(int64(x)) && x < 1e15 && x > -1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	default:
		if raw, err := json.Marshal(x); err == nil {
			return string(raw)
		}
		return fmt.Sprint(x)
	}
}

/* ------------------------------------------------------------------- JSON */

type jsonFileSink struct {
	path string
	mode string
	key  string

	mu sync.Mutex
}

func (s *jsonFileSink) Describe() string { return "JSON 文件 " + s.path }

func (s *jsonFileSink) Close() error { return nil }

func (s *jsonFileSink) Write(rows []map[string]any) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var existing []map[string]any
	if raw, err := os.ReadFile(s.path); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		// 兼容两种历史形态：对象数组，或 {"data":[...]}
		if err := json.Unmarshal(raw, &existing); err != nil {
			var wrap struct {
				Data []map[string]any `json:"data"`
			}
			if err2 := json.Unmarshal(raw, &wrap); err2 == nil {
				existing = wrap.Data
			}
		}
	}

	written := 0
	if s.mode == "upsert" && s.key != "" {
		at := map[string]int{}
		for i, r := range existing {
			if k := strings.TrimSpace(cellString(r[s.key])); k != "" {
				at[k] = i
			}
		}
		for _, row := range rows {
			k := strings.TrimSpace(cellString(row[s.key]))
			if k != "" {
				if i, ok := at[k]; ok {
					existing[i] = row
					written++
					continue
				}
				at[k] = len(existing)
			}
			existing = append(existing, row)
			written++
		}
	} else {
		existing = append(existing, rows...)
		written = len(rows)
	}

	// 列顺序按字母排序，保持每次写出的文件稳定、diff 友好
	normalized := make([]map[string]any, len(existing))
	for i, r := range existing {
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		m := make(map[string]any, len(keys))
		for _, k := range keys {
			m[k] = r[k]
		}
		normalized[i] = m
	}
	raw, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("序列化 JSON 失败：%w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return 0, fmt.Errorf("写 JSON 文件失败：%w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return 0, fmt.Errorf("替换 JSON 文件失败：%w", err)
	}
	return written, nil
}
