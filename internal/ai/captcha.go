package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"uitest-server/internal/model"
)

// CaptchaResult 一次图形验证码识别的结果。
// Model / DurationMS 会写进日志与试抓过程，便于排查「到底用的哪个视觉模型」。
type CaptchaResult struct {
	Text       string `json:"text"`
	Model      string `json:"model"`
	Hint       string `json:"hint,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Raw        string `json:"raw,omitempty"`
}

// CaptchaReady 判断当前 AI 设置是否够用来识别验证码。
func CaptchaReady(cfg model.AISettings) bool {
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.APIKey) == "" {
		return false
	}
	return CaptchaModel(cfg) != ""
}

// CaptchaModel 返回真正用于识别的视觉模型：优先 vision_model，留空回退 model。
func CaptchaModel(cfg model.AISettings) string {
	if v := strings.TrimSpace(cfg.VisionModel); v != "" {
		return v
	}
	return strings.TrimSpace(cfg.Model)
}

// RecognizeCaptcha 把验证码图片发给多模态模型，返回识别出的字符。
// hint 是可选的补充说明（例如「只取前四位数字」），会拼到提示词里。
func RecognizeCaptcha(ctx context.Context, cfg model.AISettings, imagePNG []byte, hint string) (CaptchaResult, error) {
	if !CaptchaReady(cfg) {
		return CaptchaResult{}, errors.New("未配置 AI 视觉模型：请在设置页填写 base_url / api_key / model（建议再单独填 vision_model，需支持图片输入）")
	}
	if len(imagePNG) == 0 {
		return CaptchaResult{}, errors.New("验证码截图为空，无法识别")
	}
	modelName := CaptchaModel(cfg)

	instruction := "请识别这张图片里的验证码，只输出验证码字符本身：不要解释、不要加标点、不要空格、不要换行。"
	if h := strings.TrimSpace(hint); h != "" {
		instruction += "补充要求：" + h
	}

	body := map[string]any{
		"model":       modelName,
		"temperature": 0,
		"max_tokens":  32,
		"messages": []map[string]any{
			{
				"role":    "system",
				"content": "你是图形验证码识别器，只输出验证码字符，不输出任何其它内容。",
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": instruction},
					{
						"type": "image_url",
						"image_url": map[string]string{
							"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(imagePNG),
						},
					},
				},
			},
		},
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
		return CaptchaResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	started := time.Now()
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return CaptchaResult{}, err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return CaptchaResult{}, err
	}
	if resp.StatusCode/100 != 2 {
		return CaptchaResult{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(buf.String(), 300))
	}

	var payload struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		return CaptchaResult{}, err
	}
	if len(payload.Choices) == 0 {
		return CaptchaResult{}, errors.New("视觉模型响应没有 choices")
	}
	content := messageContentText(payload.Choices[0].Message.Content)
	text := cleanCaptchaText(content)
	if text == "" {
		return CaptchaResult{}, fmt.Errorf("视觉模型没有返回可用字符（原始返回：%s）", truncate(content, 120))
	}
	return CaptchaResult{
		Text:       text,
		Model:      modelName,
		Hint:       strings.TrimSpace(hint),
		DurationMS: time.Since(started).Milliseconds(),
		Raw:        truncate(content, 120),
	}, nil
}

// messageContentText 兼容 content 为字符串或 [{type:"text",text:"..."}] 两种返回。
func messageContentText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					sb.WriteString(s)
				}
			}
		}
		return sb.String()
	default:
		return fmt.Sprint(t)
	}
}

// cleanCaptchaText 清理模型返回：去代码块、去引号、去空格换行、去「验证码：」前缀。
func cleanCaptchaText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	// 只取第一行，避免模型带解释
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	for _, p := range []string{"验证码是", "验证码为", "验证码：", "验证码:", "答案是", "结果：", "result:", "answer:"} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimSpace(s[len(p):])
		}
	}
	s = strings.Trim(s, "\"'` ")
	// 去掉所有空白字符
	s = strings.Join(strings.Fields(s), "")
	return s
}
