// Package ai 负责「先让 AI 生成数据准备，再拿数据去录入」这一步。
//
// 两种模式：
//   - llm：配置了 OpenAI 兼容接口（base_url + api_key + model）时调用大模型，
//     按录制里的字段语义（label / field_kind / 备注 / 用户指令）生成一组合法数据；
//   - rules：未配置或调用失败时的内置规则生成，保证离线也能跑通，
//     并且会尽力理解「价格 99-299」「库存 50」这类中文指令。
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"uitest-server/internal/model"
)

// Result 一次数据生成的结果。
type Result struct {
	Fields  []model.AIField
	Values  map[string]string
	Notes   map[string]string
	Source  string // ai | rules
	Warning string
}

// Dataset 把字段值压成 map（执行时按 key 替换 {{key}}）。
func (r Result) Dataset() map[string]string {
	out := make(map[string]string, len(r.Values))
	for k, v := range r.Values {
		out[k] = v
	}
	return out
}

/* --------------------------------------------------------- 字段推导 */

var nonKey = regexp.MustCompile(`[^a-z0-9]+`)

// SlugKey 把中文/混合标签转成 snake_case 字段名；全中文时返回空串（由调用方兜底）。
func SlugKey(s string) string {
	v := strings.ToLower(strings.TrimSpace(s))
	v = nonKey.ReplaceAllString(v, "_")
	v = strings.Trim(v, "_")
	if len(v) > 40 {
		v = v[:40]
	}
	return v
}

// DeriveFields 从录制的输入类步骤推导出数据字段清单。
func DeriveFields(rec model.Recording) []model.AIField {
	seen := map[string]bool{}
	out := make([]model.AIField, 0, 8)
	for _, s := range rec.Steps {
		switch s.Action {
		case "input", "select", "upload", "check":
		default:
			continue
		}
		key := strings.TrimSpace(s.DataKey)
		if key == "" {
			key = SlugKey(s.Target.Label)
		}
		if key == "" {
			key = SlugKey(s.Target.Placeholder)
		}
		if key == "" {
			key = SlugKey(s.Target.Describe())
		}
		if key == "" {
			key = fmt.Sprintf("field_%d", s.Index)
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		label := s.Target.Label
		if label == "" {
			label = s.Target.Placeholder
		}
		if label == "" {
			label = s.Target.Describe()
		}
		kind := s.Target.FieldKind
		if kind == "" {
			kind = kindFromAction(s)
		}
		out = append(out, model.AIField{
			Key:   key,
			Label: label,
			Kind:  kind,
			Note:  s.Note,
			Enum:  append([]string(nil), s.Target.Options...),
		})
	}
	return out
}

func kindFromAction(s model.Step) string {
	switch s.Action {
	case "select":
		return "select"
	case "upload":
		return "file"
	case "check":
		return "checkbox"
	default:
		return "text"
	}
}

/* --------------------------------------------------------- 对外入口 */

// Generate 生成测试数据。instruction 是用户在控制台写的中文指令（可为空）。
func Generate(ctx context.Context, cfg model.AISettings, fields []model.AIField, instruction string) Result {
	if len(fields) == 0 {
		return Result{Fields: nil, Values: map[string]string{}, Notes: map[string]string{}, Source: "rules",
			Warning: "这条录制里没有可识别的输入字段（没有 input/select 步骤），无需生成数据"}
	}
	if strings.TrimSpace(cfg.BaseURL) != "" && strings.TrimSpace(cfg.APIKey) != "" && strings.TrimSpace(cfg.Model) != "" {
		res, err := generateByLLM(ctx, cfg, fields, instruction)
		if err == nil && len(res.Values) > 0 {
			res.Fields = fields
			res.Source = "ai"
			return res
		}
		fallback := generateByRules(fields, instruction)
		fallback.Source = "rules"
		wrapped := ""
		if err != nil {
			wrapped = fmt.Sprintf("大模型调用失败（%v），已回退到内置规则生成", err)
		} else {
			wrapped = "大模型未返回有效数据，已回退到内置规则生成"
		}
		fallback.Warning = wrapped
		return fallback
	}
	res := generateByRules(fields, instruction)
	res.Source = "rules"
	res.Warning = "未配置大模型接口（设置页填写 base_url / api_key / model 即可启用 AI），当前使用内置规则生成"
	return res
}

/* ----------------------------------------------------------- LLM 通道 */

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func generateByLLM(ctx context.Context, cfg model.AISettings, fields []model.AIField, instruction string) (Result, error) {
	var sb strings.Builder
	sb.WriteString("请为浏览器自动化测试生成一组真实、合法的中文业务数据。字段如下：\n")
	for _, f := range fields {
		sb.WriteString(fmt.Sprintf("- key=%s | 中文含义=%s | 类型=%s", f.Key, f.Label, f.Kind))
		if len(f.Enum) > 0 {
			sb.WriteString(" | 可选值(必须从中选一个)=" + strings.Join(f.Enum, "/"))
		}
		sb.WriteString("\n")
	}
	if extra := strings.TrimSpace(instruction); extra != "" {
		sb.WriteString("\n用户额外要求（必须遵守）：" + extra + "\n")
	}
	sb.WriteString(`
要求：
1) 严格返回 JSON 对象，形如 {"values":{"字段key":"值"},"notes":{"字段key":"生成理由"}}；
2) values 的 key 必须与上面列出的 key 完全一致，不能多也不能少；
3) 值必须是字符串；日期用 yyyy-MM-dd 或 yyyy-MM-dd HH:mm:ss；价格用数字字符串；
4) 保证数据的唯一性（例如名称带时间戳或随机后缀），避免与历史数据冲突；
5) 只输出 JSON，不要任何解释文字。`)

	body := map[string]any{
		"model":       cfg.Model,
		"temperature": 0.7,
		"messages": []chatMessage{
			{Role: "system", Content: "你是资深测试数据构造专家，只输出 JSON。"},
			{Role: "user", Content: sb.String()},
		},
		"response_format": map[string]string{"type": "json_object"},
	}
	raw, _ := json.Marshal(body)

	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	endpoint := strings.TrimRight(cfg.BaseURL, "/")
	if !strings.Contains(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return Result{}, err
	}
	if resp.StatusCode/100 != 2 {
		return Result{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(buf.String(), 300))
	}

	var payload struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		return Result{}, err
	}
	if len(payload.Choices) == 0 {
		return Result{}, errors.New("大模型响应没有 choices")
	}
	content := stripFence(payload.Choices[0].Message.Content)

	var parsed struct {
		Values map[string]any `json:"values"`
		Notes  map[string]any `json:"notes"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return Result{}, fmt.Errorf("大模型返回的不是合法 JSON: %w", err)
	}
	values := map[string]string{}
	notes := map[string]string{}
	for _, f := range fields {
		if v, ok := parsed.Values[f.Key]; ok {
			values[f.Key] = toString(v)
		}
		if n, ok := parsed.Notes[f.Key]; ok {
			notes[f.Key] = toString(n)
		}
	}
	if len(values) == 0 {
		return Result{}, errors.New("大模型未返回任何匹配字段")
	}
	return Result{Values: values, Notes: notes}, nil
}

func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i > 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		s = s[start : end+1]
	}
	return strings.TrimSpace(s)
}

func toString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "是"
		}
		return "否"
	default:
		raw, _ := json.Marshal(t)
		return string(raw)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

/* --------------------------------------------------------- 规则通道 */

var (
	priceRangeRe = regexp.MustCompile(`价格\s*(\d+(?:\.\d+)?)\s*(?:-|~|到|至)\s*(\d+(?:\.\d+)?)`)
	numRangeRe   = regexp.MustCompile(`(?:库存|数量|件数|次数)\s*(\d+)\s*(?:-|~|到|至)\s*(\d+)`)
	fixedNumRe   = regexp.MustCompile(`(?:库存|数量|件数|次数)\s*(?:为|=|是)?\s*(\d+)`)
)

func generateByRules(fields []model.AIField, instruction string) Result {
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	now := time.Now()
	stamp := now.Format("0102150405")
	values := map[string]string{}
	notes := map[string]string{}

	priceMin, priceMax := 99.0, 299.0
	if m := priceRangeRe.FindStringSubmatch(instruction); m != nil {
		priceMin, _ = strconv.ParseFloat(m[1], 64)
		priceMax, _ = strconv.ParseFloat(m[2], 64)
		if priceMax < priceMin {
			priceMin, priceMax = priceMax, priceMin
		}
	}
	numMin, numMax := 10, 500
	if m := numRangeRe.FindStringSubmatch(instruction); m != nil {
		numMin, _ = strconv.Atoi(m[1])
		numMax, _ = strconv.Atoi(m[2])
		if numMax < numMin {
			numMin, numMax = numMax, numMin
		}
	} else if m := fixedNumRe.FindStringSubmatch(instruction); m != nil {
		n, _ := strconv.Atoi(m[1])
		numMin, numMax = n, n
	}

	for i, f := range fields {
		label := f.Label
		hay := label + " " + f.Key + " " + f.Note
		var val, why string
		switch f.Kind {
		case "price":
			v := priceMin + rnd.Float64()*(priceMax-priceMin)
			val = fmt.Sprintf("%.2f", float64(int(v*100))/100)
			why = fmt.Sprintf("价格字段，按区间 %.0f-%.0f 随机，保留两位小数", priceMin, priceMax)
		case "number":
			n := numMin
			if numMax > numMin {
				n = numMin + rnd.Intn(numMax-numMin+1)
			}
			val = strconv.Itoa(n)
			why = "数量类字段，按整数区间生成"
		case "phone":
			val = "138" + fmt.Sprintf("%08d", rnd.Intn(100000000))
			why = "手机号，11 位合法测试号段"
		case "email":
			val = fmt.Sprintf("auto_test_%s@example.com", stamp)
			why = "邮箱，带时间戳保证唯一"
		case "url":
			val = "https://example.com/auto-test/" + stamp
			why = "URL 字段"
		case "date", "datetime":
			start := now.AddDate(0, 0, 1+rnd.Intn(5))
			if strings.Contains(hay, "结束") || strings.Contains(hay, "end") {
				start = now.AddDate(0, 0, 10+rnd.Intn(10))
			}
			if f.Kind == "datetime" || strings.Contains(hay, "时间") || strings.Contains(hay, "time") {
				val = start.Format("2006-01-02 15:04:05")
			} else {
				val = start.Format("2006-01-02")
			}
			why = "时间字段，取未来日期避免与历史数据冲突"
		case "file":
			val = "auto-test.png"
			why = "文件上传字段：执行时由服务端生成本地图片并注入"
		case "checkbox":
			val = "是"
			why = "勾选项"
		case "select":
			if len(f.Enum) > 0 {
				val = f.Enum[rnd.Intn(len(f.Enum))]
				why = "下拉字段：从录制时采集到的候选项中随机取值"
			} else {
				val = ""
				why = "下拉字段：未采集到候选项，执行时按选项文本模糊匹配"
			}
		case "textarea":
			val = fmt.Sprintf("自动化测试数据（%s）：本记录由 UI 自动化测试平台生成，用于验证表单交互与保存流程。", stamp)
			why = "多行文本，含唯一时间戳便于检索"
		case "password":
			val = "Auto@Test" + stamp
			why = "密码，满足常见复杂度要求"
		default:
			val = defaultText(hay, stamp, i, rnd)
			why = "文本字段，按中文语义取名并带唯一后缀"
		}
		if val == "" {
			val = fmt.Sprintf("auto_%s_%d", stamp, i+1)
			why = "无法判断语义，使用通用唯一值"
		}
		values[f.Key] = val
		notes[f.Key] = why
	}
	return Result{Fields: fields, Values: values, Notes: notes}
}

func defaultText(hay, stamp string, idx int, rnd *rand.Rand) string {
	switch {
	case regexp.MustCompile(`名称|标题|名字|name|title`).MatchString(hay):
		return pickKeyWord(hay, []string{"商品", "服务", "套餐"}) + "-自动化测-" + stamp
	case regexp.MustCompile(`编码|编号|sku|code|序号`).MatchString(hay):
		return "SKU" + stamp + strconv.Itoa(100+rnd.Intn(900))
	case regexp.MustCompile(`分类|类别|类型|category`).MatchString(hay):
		return "服务类"
	case regexp.MustCompile(`备注|说明|remark`).MatchString(hay):
		return "自动化测试备注-" + stamp
	default:
		return "自动化测试" + strconv.Itoa(idx+1) + "-" + stamp
	}
}

func pickKeyWord(hay string, words []string) string {
	for _, w := range words {
		if strings.Contains(hay, w) {
			return w
		}
	}
	return "测试数据"
}
