package collect

import (
	"net/url"
	"testing"
)

// hash 路由站点：地址里的 #/vip_member 必须原样保留，不能退化成 "/"。
func TestNormalizePageURLKeepsFragment(t *testing.T) {
	cases := []string{
		"https://merchant.bgjtsvip.com/#/vip_member",
		"https://merchant.bgjtsvip.com/#/order/list?page=2",
		"https://example.com/a?x=1#/b",
	}
	for _, raw := range cases {
		got, err := normalizePageURL(raw)
		if err != nil {
			t.Fatalf("normalizePageURL(%q) 报错：%v", raw, err)
		}
		if got != raw {
			t.Errorf("normalizePageURL(%q) = %q，fragment 丢了", raw, got)
		}
	}
}

// 缺协议的地址补 https；本机地址补 http。
func TestNormalizePageURLAddsScheme(t *testing.T) {
	if got, err := normalizePageURL("merchant.bgjtsvip.com/#/vip_member"); err != nil ||
		got != "https://merchant.bgjtsvip.com/#/vip_member" {
		t.Fatalf("外网地址补协议不对：%q %v", got, err)
	}
	if got, err := normalizePageURL("localhost:8490/#/home"); err != nil ||
		got != "http://localhost:8490/#/home" {
		t.Fatalf("本机地址补协议不对：%q %v", got, err)
	}
}

func TestNormalizePageURLRejectsBadInput(t *testing.T) {
	for _, raw := range []string{"", "   ", "ftp://example.com/a", "javascript:alert(1)"} {
		if _, err := normalizePageURL(raw); err == nil {
			t.Errorf("normalizePageURL(%q) 应该报错", raw)
		}
	}
}

// 展示用的路径要带上 fragment，否则 hash 路由全显示成 "/"。
func TestPagePathKeepsFragment(t *testing.T) {
	u, _ := url.Parse("https://merchant.bgjtsvip.com/#/vip_member")
	if got := pagePath(u); got != "/#/vip_member" {
		t.Fatalf("pagePath = %q，想要 /#/vip_member", got)
	}
	u2, _ := url.Parse("https://example.com/a/b/")
	if got := pagePath(u2); got != "/a/b/" {
		t.Fatalf("pagePath = %q，想要 /a/b/", got)
	}
	u3, _ := url.Parse("https://example.com")
	if got := pagePath(u3); got != "/" {
		t.Fatalf("pagePath = %q，想要 /", got)
	}
}

// 不同 hash 是两个页面，不能当成同一页去重复合并。
func TestSamePageURLRespectsFragment(t *testing.T) {
	if samePageURL("https://x.com/#/a", "https://x.com/#/b") {
		t.Error("#/a 和 #/b 被当成同一页")
	}
	if !samePageURL("https://x.com/#/a", "https://X.com/#/a") {
		t.Error("只有 host 大小写不同，应该是同一页")
	}
	if !samePageURL("https://x.com/a/#/b", "https://x.com/a#/b") {
		t.Error("只有结尾斜杠不同，应该是同一页")
	}
}

// 补录条目：URL / Path / Query 都要对，hash 路由的 Path 带 fragment。
func TestBuildDocumentItemKeepsHashAndBody(t *testing.T) {
	item, err := buildDocumentItem(DocumentInput{
		URL:    "https://merchant.bgjtsvip.com/#/vip_member",
		Status: 200,
		Note:   "登录后的会员页",
		Body:   "<html><body>hello</body></html>",
	})
	if err != nil {
		t.Fatalf("buildDocumentItem 报错：%v", err)
	}
	if item.URL != "https://merchant.bgjtsvip.com/#/vip_member" {
		t.Errorf("URL = %q", item.URL)
	}
	if item.Path != "/#/vip_member" {
		t.Errorf("Path = %q，想要 /#/vip_member", item.Path)
	}
	if item.Host != "merchant.bgjtsvip.com" {
		t.Errorf("Host = %q", item.Host)
	}
	if item.Type != "Document" || item.Kind != "page" || !item.Manual {
		t.Errorf("Type/Kind/Manual = %q/%q/%v", item.Type, item.Kind, item.Manual)
	}
	if !item.HasBody || item.BodyPreview == "" {
		t.Error("有 HTML 时应该有 BodyPreview")
	}
}

// 查询串仍然拆到 Query 里；hash 里的问号不算查询串。
func TestBuildDocumentItemQuery(t *testing.T) {
	item, err := buildDocumentItem(DocumentInput{URL: "https://a.com/list?page=2&size=10"})
	if err != nil {
		t.Fatalf("buildDocumentItem 报错：%v", err)
	}
	if item.Query != "page=2&size=10" {
		t.Errorf("Query = %q", item.Query)
	}
	if item.Path != "/list" {
		t.Errorf("Path = %q", item.Path)
	}
}
