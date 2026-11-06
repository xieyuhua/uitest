package collect

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"uitest-server/internal/model"
)

func TestParseFilterRejectsBadInput(t *testing.T) {
	q := url.Values{}
	q.Set("min_status", "abc")
	if _, err := ParseFilter(q); err == nil {
		t.Error("min_status 非数字应报错")
	}

	q = url.Values{}
	q.Set("include", "([a-z")
	if _, err := ParseFilter(q); err == nil {
		t.Error("非法 include 正则应报错")
	}

	q = url.Values{}
	q.Set("exclude", "([a-z")
	if _, err := ParseFilter(q); err == nil {
		t.Error("非法 exclude 正则应报错")
	}
}

func TestFilterApply(t *testing.T) {
	q := url.Values{}
	q.Set("method", "get, POST")
	q.Set("min_status", "200")
	q.Set("max_status", "299")
	q.Set("include", `/api/`)
	q.Set("exclude", `/internal/`)
	q.Set("with_body", "1")
	f, err := ParseFilter(q)
	if err != nil {
		t.Fatalf("ParseFilter: %v", err)
	}

	list := []model.CapturedRequest{
		{ID: "1", Method: "GET", URL: "https://a.com/api/users", Status: 200, HasBody: true},
		{ID: "2", Method: "POST", URL: "https://a.com/api/order", Status: 201, HasBody: true},
		{ID: "3", Method: "POST", URL: "https://a.com/internal/x", Status: 200, HasBody: true},
		{ID: "4", Method: "DELETE", URL: "https://a.com/api/x", Status: 200, HasBody: true},
		{ID: "5", Method: "GET", URL: "https://a.com/api/empty", Status: 200, HasBody: false},
		{ID: "6", Method: "GET", URL: "https://a.com/api/err", Status: 500, HasBody: true},
	}
	got := f.Apply(list)
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "1,2" {
		t.Errorf("筛选结果 = %v, want [1 2]", ids)
	}
}

func TestFilterQMatchesHostAndPath(t *testing.T) {
	f := Filter{Q: "user"}
	if !f.Match(model.CapturedRequest{Method: "GET", URL: "https://a.com/api/users", Path: "/api/users", Host: "a.com"}) {
		t.Error("Q 应能匹配 URL / Path")
	}
	f = Filter{Q: "a.com"}
	if !f.Match(model.CapturedRequest{Method: "GET", URL: "https://a.com/x", Host: "a.com"}) {
		t.Error("Q 应能匹配 Host")
	}
	if f.Match(model.CapturedRequest{Method: "GET", URL: "https://b.com/x", Host: "b.com"}) {
		t.Error("不匹配的记录应过滤掉")
	}
}

func TestFilterOnlyJSON(t *testing.T) {
	f := Filter{OnlyJSON: true}
	if !f.Match(model.CapturedRequest{URL: "https://a.com/x", MimeType: "application/json"}) {
		t.Error("mime 里含 json 应保留")
	}
	if !f.Match(model.CapturedRequest{URL: "https://a.com/x", BodyPreview: `{"a":1}`}) {
		t.Error("响应体以 { 开头应保留")
	}
	if f.Match(model.CapturedRequest{URL: "https://a.com/x", MimeType: "text/html", BodyPreview: "<html>"}) {
		t.Error("HTML 响应应被过滤")
	}
}

func TestBuildFacets(t *testing.T) {
	all := []model.CapturedRequest{
		{Method: "GET", Type: "XHR", Kind: "api", HasBody: true, Host: "a.com", MimeType: "application/json"},
		{Method: "GET", Type: "Document", Kind: "page", Host: "b.com"},
	}
	kept := all[:1]
	f := BuildFacets(all, kept)
	if f.Total != 2 || f.Kept != 1 {
		t.Errorf("Total/Kept = %d/%d", f.Total, f.Kept)
	}
	if f.ByMethod["GET"] != 2 || f.ByType["XHR"] != 1 || f.ByKind["page"] != 1 {
		t.Errorf("分项统计不对: %+v", f)
	}
	if f.WithBody != 1 || f.JSONCount != 1 {
		t.Errorf("WithBody/JSON = %d/%d", f.WithBody, f.JSONCount)
	}
	if len(f.Hosts) != 2 || f.Hosts[0] != "a.com" || f.Hosts[1] != "b.com" {
		t.Errorf("Hosts 应去重并排序: %v", f.Hosts)
	}
}

func BenchmarkFilterApply(b *testing.B) {
	f := Filter{Include: `/api/`, Exclude: `/internal/`}
	f.includeRe = nil
	list := make([]model.CapturedRequest, 0, 1000)
	for i := 0; i < 1000; i++ {
		url := "https://a.com/api/items/" + strconv.Itoa(i)
		if i%3 == 0 {
			url = "https://a.com/internal/" + strconv.Itoa(i)
		}
		list = append(list, model.CapturedRequest{ID: strconv.Itoa(i), Method: "GET", URL: url, Status: 200})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Apply(list)
	}
}
