package engine

import "testing"

// 纯数字 = 固定等待（在 Go 里 sleep）；其它值是「等某个元素出现」，交给页面运行时。
func TestFixedWaitMS(t *testing.T) {
	cases := []struct {
		value string
		want  int
		ok    bool
	}{
		{"2500", 2500, true},
		{" 800 ", 800, true},
		{"#loading", 0, false},
		{".spinner", 0, false},
		{"", 0, false},
		{"0", 0, false},
		{"-1", 0, false},
		{"abc", 0, false},
		{"{{wait}}", 0, false},
	}
	for _, c := range cases {
		got, ok := fixedWaitMS(c.value)
		if ok != c.ok || got != c.want {
			t.Errorf("fixedWaitMS(%q) = (%d, %v), want (%d, %v)", c.value, got, ok, c.want, c.ok)
		}
	}
	if got, ok := fixedWaitMS("99999999"); !ok || got != maxFixedWaitMS {
		t.Errorf("超大等待应被截到上限, got (%d, %v)", got, ok)
	}
}
