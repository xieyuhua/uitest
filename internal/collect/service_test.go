package collect

import (
	"testing"

	"uitest-server/internal/model"
)

// 起始页的 Document 请求不带 #fragment，要按会话起始地址补回 hash 路由。
func TestIngestBackfillsStartPageFragment(t *testing.T) {
	r := &Recorder{sessions: map[string]*CaptureSession{
		"cap_t": {
			ID:      "cap_t",
			Source:  "browser",
			PageURL: "https://merchant.example.com/#/vip_member",
			Deleted: map[string]bool{},
			Types:   []string{"Document"},
		},
	}}
	added, err := r.ingest("cap_t", []model.CapturedRequest{
		{ID: "a", Method: "GET", URL: "https://merchant.example.com/", Type: "Document", Kind: "page"},
	}, true)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if len(added) != 1 {
		t.Fatalf("added = %d, want 1", len(added))
	}
	if got, want := added[0].URL, "https://merchant.example.com/#/vip_member"; got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
	if got, want := added[0].Path, "/#/vip_member"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

// 已经有页面文档之后再来的 "/" 不应该硬塞 fragment（可能是用户主动回首页）。
func TestIngestKeepsLaterRootDocument(t *testing.T) {
	r := &Recorder{sessions: map[string]*CaptureSession{
		"cap_t": {
			ID:      "cap_t",
			Source:  "browser",
			PageURL: "https://merchant.example.com/#/vip_member",
			Deleted: map[string]bool{},
			Entries: []model.CapturedRequest{
				{ID: "a", URL: "https://merchant.example.com/#/vip_member", Type: "Document", Kind: "page"},
			},
		},
	}}
	added, err := r.ingest("cap_t", []model.CapturedRequest{
		{ID: "b", Method: "GET", URL: "https://merchant.example.com/", Type: "Document", Kind: "page"},
	}, true)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if added[0].URL != "https://merchant.example.com/" || added[0].Path != "/" {
		t.Fatalf("got url=%q path=%q, want https://merchant.example.com/ and /", added[0].URL, added[0].Path)
	}
}

// 其它 host 的 Document 不受影响。
func TestIngestIgnoresOtherHosts(t *testing.T) {
	r := &Recorder{sessions: map[string]*CaptureSession{
		"cap_t": {
			ID:      "cap_t",
			Source:  "browser",
			PageURL: "https://merchant.example.com/#/vip_member",
			Deleted: map[string]bool{},
		},
	}}
	added, _ := r.ingest("cap_t", []model.CapturedRequest{
		{ID: "a", Method: "GET", URL: "https://cdn.example.org/", Type: "Document", Kind: "page"},
	}, true)
	if added[0].Path != "/" {
		t.Fatalf("path = %q, want /", added[0].Path)
	}
}
// 起始页行展示时补回 hash 路由，并且只补第一张页面文档。
func TestBackfillStartFragment(t *testing.T) {
	s := &CaptureSession{Source: "browser", PageURL: "https://merchant.example.com/#/vip_member"}
	items := []model.CapturedRequest{
		{ID: "1", URL: "https://merchant.example.com/", Path: "/", Type: "Document", Kind: "page"},
		{ID: "2", URL: "https://merchant.example.com/", Path: "/", Type: "Document", Kind: "page"},
	}
	s.backfillStartFragment(items)
	if got, want := items[0].URL, "https://merchant.example.com/#/vip_member"; got != want {
		t.Fatalf("url[0] = %q, want %q", got, want)
	}
	if got, want := items[0].Path, "/#/vip_member"; got != want {
		t.Fatalf("path[0] = %q, want %q", got, want)
	}
	if items[1].Path != "/" {
		t.Fatalf("第二张页面文档不该被改写，path = %q", items[1].Path)
	}
}

// 插件会话 / 已经带 fragment 的条目都不动。
func TestBackfillStartFragmentSkips(t *testing.T) {
	ext := &CaptureSession{Source: "extension", PageURL: "https://merchant.example.com/#/vip_member"}
	items := []model.CapturedRequest{{ID: "1", URL: "https://merchant.example.com/", Path: "/", Type: "Document"}}
	ext.backfillStartFragment(items)
	if items[0].Path != "/" {
		t.Fatalf("插件会话不该改写，path = %q", items[0].Path)
	}

	s := &CaptureSession{Source: "browser", PageURL: "https://merchant.example.com/#/vip_member"}
	already := []model.CapturedRequest{{ID: "1", URL: "https://merchant.example.com/#/orders", Path: "/#/orders", Type: "Document"}}
	s.backfillStartFragment(already)
	if already[0].Path != "/#/orders" {
		t.Fatalf("已有 fragment 的条目不该改写，path = %q", already[0].Path)
	}
}
// 被重定向回 "#/"（还没登录）时，用用户填的起始地址补 fragment，别只显示 "/"。
func TestRootFragmentPrefersStartWhenRedirectedToRoot(t *testing.T) {
	s := &CaptureSession{
		Source:   "browser",
		StartURL: "https://merchant.example.com/#/vip_member",
		PageURL:  "https://merchant.example.com/#/",
	}
	if got := s.rootFragment("merchant.example.com"); got != "/vip_member" {
		t.Fatalf("fragment = %q, want /vip_member", got)
	}
}

// 浏览器已经跳到别的 hash 路由时，以浏览器的为准。
func TestRootFragmentPrefersLiveRoute(t *testing.T) {
	s := &CaptureSession{
		Source:   "browser",
		StartURL: "https://merchant.example.com/#/vip_member",
		PageURL:  "https://merchant.example.com/#/orders",
	}
	if got := s.rootFragment("merchant.example.com"); got != "/orders" {
		t.Fatalf("fragment = %q, want /orders", got)
	}
	if got := s.rootFragment("other.example.com"); got != "" {
		t.Fatalf("other host fragment = %q, want empty", got)
	}
}
// 浏览器发的 Document 请求不带 #fragment：hash 路由下要能复用这条，别补出重复页面。
func TestFindDocumentIgnoringFragment(t *testing.T) {
	r := &Recorder{sessions: map[string]*CaptureSession{
		"cap_t": {
			ID:      "cap_t",
			Source:  "browser",
			Deleted: map[string]bool{},
			Entries: []model.CapturedRequest{
				{ID: "a", URL: "https://merchant.example.com/", Path: "/", Type: "Document", Kind: "page"},
			},
		},
	}}
	it, ok := r.findDocumentIgnoringFragment("cap_t", "https://merchant.example.com/#/vip_member")
	if !ok {
		t.Fatal("should match fragment-less Document")
	}
	if it.URL != "https://merchant.example.com/#/vip_member" || it.Path != "/#/vip_member" {
		t.Fatalf("url=%q path=%q", it.URL, it.Path)
	}
	if _, ok := r.findDocumentIgnoringFragment("cap_t", "https://other.example.com/#/vip_member"); ok {
		t.Fatal("different host must not match")
	}
}