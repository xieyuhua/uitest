package engine

import "testing"

func TestSplitLocatorSpec(t *testing.T) {
	cases := []struct {
		in string
		st string
		v  string
		ok bool
	}{
		{"css=#captcha-code", "css", "#captcha-code", true},
		{"css:#captcha-code", "css", "#captcha-code", true},
		{"uid=abc123", "uid", "abc123", true},
		{"xpath=//input[1]", "xpath", "//input[1]", true},
		{"#captcha-code", "", "", false},
		{"captcha-code", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		st, v, ok := splitLocatorSpec(c.in)
		if ok != c.ok || st != c.st || v != c.v {
			t.Errorf("splitLocatorSpec(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, st, v, ok, c.st, c.v, c.ok)
		}
	}
}
