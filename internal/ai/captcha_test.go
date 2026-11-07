package ai

import (
	"testing"

	"uitest-server/internal/model"
)

func TestCleanCaptchaText(t *testing.T) {
	cases := map[string]string{
		"```\nAB12\n```":     "AB12",
		"验证码：ab12":           "ab12",
		"答案是 7k9m":           "7k9m",
		"  \"X9Y8\"  ":       "X9Y8",
		"result: 1234\n解释文字": "1234",
		"1234 5678":          "12345678",
		"":                   "",
	}
	for in, want := range cases {
		if got := cleanCaptchaText(in); got != want {
			t.Errorf("cleanCaptchaText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCaptchaModelFallback(t *testing.T) {
	cfg := model.AISettings{Model: "gpt-x"}
	if got := CaptchaModel(cfg); got != "gpt-x" {
		t.Fatalf("应该回退到 Model，实际 %q", got)
	}
	cfg.VisionModel = "vision-x"
	if got := CaptchaModel(cfg); got != "vision-x" {
		t.Fatalf("应该优先 VisionModel，实际 %q", got)
	}
	if CaptchaReady(model.AISettings{BaseURL: "http://x", APIKey: "k", Model: "m"}) != true {
		t.Fatal("配置齐全时应判定可用")
	}
	if CaptchaReady(model.AISettings{BaseURL: "", APIKey: "k", Model: "m"}) {
		t.Fatal("缺 base_url 应判定不可用")
	}
}
