package keyname

import (
	"regexp"
	"testing"
)

var snake = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

func TestDeriveKeyKnownLabels(t *testing.T) {
	cases := map[string]string{
		"用户名":       "username",
		"手机号":       "mobile",
		"登录密码":      "login_password",
		"订单编号":      "order_no",
		"Email":     "email",
		"user_name": "username",
	}
	for in, want := range cases {
		if got := DeriveKey(in, "", ""); got != want {
			t.Errorf("DeriveKey(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestDeriveKeyFallbackOrder(t *testing.T) {
	if got := DeriveKey("!!!", "请输入手机号", ""); got != "mobile" {
		t.Errorf("label 不可用时应用 placeholder, got %q", got)
	}
	if got := DeriveKey("!!!", "###", "orderNo"); got != "orderno" {
		t.Errorf("应回退到 name, got %q", got)
	}
	if got := DeriveKey("", "", ""); got != "" {
		t.Errorf("全为空应返回空串, got %q", got)
	}
	if got := DeriveKey("123", "", ""); got != "" {
		t.Errorf("纯数字没有意义, got %q", got)
	}
}

func TestDeriveKeyShape(t *testing.T) {
	for _, in := range []string{"用户名", "手机号", "身份证号", "收货地址", "创建时间", "Order Id", "a-very-long-label-with-many-words-here"} {
		got := DeriveKey(in, "", "")
		if got == "" {
			continue
		}
		if !snake.MatchString(got) {
			t.Errorf("DeriveKey(%q)=%q 不是 snake_case 形式", in, got)
		}
		if len(got) > 40 {
			t.Errorf("DeriveKey(%q)=%q 超过 40 字节", in, got)
		}
	}
}
