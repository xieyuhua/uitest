package browser

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// 重试用的 profile 目录要能被识别出来清理掉，但不能误删正式目录、正在用的目录或刚创建的新目录。
func TestStaleRetryProfiles(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "browser-profile")
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().Add(-age)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mk("browser-profile", 48*time.Hour)
	old1 := mk("browser-profile-retry1-100", 48*time.Hour)
	old2 := mk("browser-profile-retry2-200", 48*time.Hour)
	mk("browser-profile-retry3-300", time.Minute) // 刚创建：可能正被别的实例用
	mk("other-dir", 48*time.Hour)

	got := staleRetryProfiles(base, "", 10*time.Minute, time.Now())
	sort.Strings(got)
	want := []string{old1, old2}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("staleRetryProfiles = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("staleRetryProfiles = %v，期望 %v", got, want)
		}
	}

	// 当前在用的目录（即使是旧的）不能删
	if got := staleRetryProfiles(base, old1, 10*time.Minute, time.Now()); len(got) != 1 || got[0] != old2 {
		t.Fatalf("指定 inUse 后 = %v，期望只剩 %v", got, old2)
	}

	if got := staleRetryProfiles("", "", 10*time.Minute, time.Now()); got != nil {
		t.Fatalf("base 为空时应返回 nil，得到 %v", got)
	}
}

// 当前 profile 目录名本身带重试后缀时（曾被固化过），也要能清掉更早的重试目录。
func TestStaleRetryProfilesWithRetrySuffixedProfile(t *testing.T) {
	if got := stripRetrySuffix("browser-profile-retry1-134100-retry2-955600"); got != "browser-profile" {
		t.Fatalf("stripRetrySuffix = %q", got)
	}
}
