// Package demoapi 提供"带分页的示例接口"，专门用来演练接口采集的分页抓取。
//
// 提供四种常见分页形态，一种一种试过去，就知道自己的系统属于哪类：
//
//	GET  /demo/api/goods?page=1&size=3        页码分页（最常见，返回 total/pages）
//	POST /demo/api/goods   {"page":1,"size":3} 分页参数在请求体里（前端表格组件常见）
//	GET  /demo/api/goods-offset?offset=0&limit=3  偏移分页（返回 total）
//	GET  /demo/api/goods-cursor?cursor=1003&limit=3  游标分页（返回 next_cursor / has_more）
//
// 数据是固定的 23 条，方便校验"到底有没有抓全"。
package demoapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type good struct {
	ID     int      `json:"id"`
	Name   string   `json:"name"`
	Price  float64  `json:"price"`
	Stock  int      `json:"stock"`
	Status string   `json:"status"`
	Tags   []string `json:"tags"`
	Owner  struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"owner"`
}

// Total 示例数据总条数（分页校验的基准值）。
const Total = 23

var owners = []struct {
	id   int
	name string
}{{7, "张三"}, {8, "李四"}, {9, "王五"}, {10, "赵六"}}

var goods = buildGoods()

func buildGoods() []good {
	names := []string{
		"云服务器 2 核 4G", "云服务器 4 核 8G", "云服务器 8 核 16G", "对象存储 100G",
		"对象存储 500G", "CDN 流量包 1T", "CDN 流量包 5T", "数据库 MySQL 2 核 4G",
		"数据库 MySQL 4 核 16G", "数据库 Redis 1G", "负载均衡 SLB", "弹性公网 IP",
		"容器镜像仓库", "日志服务 100G", "消息队列 Kafka", "函数计算 100 万次",
		"SSL 证书 单域名", "SSL 证书 通配符", "WAF 基础版", "WAF 企业版",
		"DDoS 高防 10G", "云监控 基础版", "备份服务 1T",
	}
	tagSets := [][]string{{"热销"}, {"新品"}, {"年付优惠"}, {"清仓"}, {}, {"新客"}}
	out := make([]good, 0, len(names))
	for i, name := range names {
		g := good{
			ID:     1001 + i,
			Name:   name,
			Price:  float64(12+i*13) + 0.5*float64(i%3),
			Stock:  10 + (i*17)%180,
			Status: "on",
			Tags:   tagSets[i%len(tagSets)],
		}
		if i%7 == 5 {
			g.Status = "off"
		}
		o := owners[i%len(owners)]
		g.Owner.ID, g.Owner.Name = o.id, o.name
		out = append(out, g)
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func clampInt(v, def, min, max int) int {
	if v <= 0 {
		v = def
	}
	if v < min {
		v = min
	}
	if v > max {
		v = max
	}
	return v
}

func queryInt(r *http.Request, name string, def int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// slice 按偏移切一段。
func slice(offset, size int) []good {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(goods) {
		return []good{}
	}
	end := offset + size
	if end > len(goods) {
		end = len(goods)
	}
	return goods[offset:end]
}

// GoodsHandler 页码分页（GET 走 query，POST 走 JSON 请求体）。
func GoodsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodPost:
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": 405, "message": "method not allowed"})
			return
		}
		page, size := 1, 3
		if r.Method == http.MethodGet {
			page = queryInt(r, "page", 1)
			size = queryInt(r, "size", 3)
		} else {
			var body struct {
				Page int `json:"page"`
				Size int `json:"size"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			page, size = body.Page, body.Size
		}
		page = clampInt(page, 1, 1, 10000)
		size = clampInt(size, 3, 1, 50)
		offset := (page - 1) * size
		pages := (len(goods) + size - 1) / size
		writeJSON(w, http.StatusOK, map[string]any{
			"code":    0,
			"message": "ok",
			"data": map[string]any{
				"total": len(goods),
				"page":  page,
				"size":  size,
				"pages": pages,
				"list":  slice(offset, size),
			},
		})
	})
}

// GoodsOffsetHandler 偏移分页：offset + limit，返回 total。
func GoodsOffsetHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := queryInt(r, "offset", 0)
		limit := clampInt(queryInt(r, "limit", 3), 3, 1, 50)
		if offset < 0 {
			offset = 0
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"total":  len(goods),
				"offset": offset,
				"limit":  limit,
				"list":   slice(offset, limit),
			},
		})
	})
}

// GoodsCursorHandler 游标分页：cursor 传"上一页最后一条的 id"，返回 next_cursor 与 has_more。
func GoodsCursorHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := queryInt(r, "cursor", 0)
		limit := clampInt(queryInt(r, "limit", 3), 3, 1, 50)
		start := 0
		if cursor > 0 {
			for i, g := range goods {
				if g.ID > cursor {
					start = i
					break
				}
				start = len(goods)
			}
		}
		page := slice(start, limit)
		next := ""
		hasMore := false
		if len(page) > 0 {
			last := page[len(page)-1].ID
			next = strconv.Itoa(last)
			if start+len(page) < len(goods) {
				hasMore = true
			} else {
				next = "" // 最后一页不再给游标
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"list":        page,
				"next_cursor": next,
				"has_more":    hasMore,
				"total":       len(goods),
			},
		})
	})
}

// InfoHandler 给示例页用的元信息（总条数、可用的分页接口）。
func InfoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"total": len(goods),
			"endpoints": map[string]string{
				"page":   "/demo/api/goods?page=1&size=3",
				"body":   "POST /demo/api/goods  {\"page\":1,\"size\":3}",
				"offset": "/demo/api/goods-offset?offset=0&limit=3",
				"cursor": "/demo/api/goods-cursor?cursor=0&limit=3",
			},
			"note": fmt.Sprintf("固定 %d 条示例数据，用于校验分页是否抓全", len(goods)),
		})
	})
}
