package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEtagMatches(t *testing.T) {
	tag := `"abc123"`
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"*", true},
		{tag, true},
		{"W/" + tag, true},
		{`"other"`, false},
		{`"other", "abc123"`, true},
		{`"other", W/"abc123"`, true},
	}
	for _, c := range cases {
		if got := etagMatches(c.header, tag); got != c.want {
			t.Errorf("etagMatches(%q)=%v, want %v", c.header, got, c.want)
		}
	}
}

// /data/runs/ 只提供具体文件，目录访问必须 404，不能列出跑过哪些用例。
func TestNoDirFSBlocksDirectoryListing(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run_1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "step_01.png"), []byte("PNG"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := http.FileServer(noDirFS{root: http.Dir(dir)})
	for _, p := range []string{"/", "/run_1/", "/run_1"} {
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d，期望 404", p, rr.Code)
		}
		if strings.Contains(rr.Body.String(), "step_01.png") {
			t.Errorf("GET %s 仍然列出了目录内容", p)
		}
	}

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/run_1/step_01.png", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "PNG" {
		t.Errorf("具体文件应可下载，得到 %d %q", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/run_1/missing.png", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("不存在的文件状态码 = %d，期望 404", rr.Code)
	}
}
