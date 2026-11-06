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
	"sync"
	"time"
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

/* --------------------------------------------------- 「需要登录」的自测接口 */

// demoToken 自测用的一次性 token 前缀（每次登录都换一个，方便验证"抓取用的是最新值"）。
const demoTokenPrefix = "demo-token-"

// demoCookie 自测用的会话 Cookie 名。
const demoCookie = "demo_sid"

var (
	demoTokenMu   sync.Mutex
	demoLastToken string
)

// LoginHandler 「需要登录」示例：校验账号密码 → 种 Cookie + 返回 token（形如 {"code":0,"data":"<token>"}）。
//
// 用来演练：录制 → 建任务 → 前置登录/浏览器会话 → 抓取里 Authorization 是否被换成最新值。
func LoginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": 405, "message": "method not allowed"})
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		user := strings.TrimSpace(body.Username)
		pass := strings.TrimSpace(body.Password)
		if user == "" || pass == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": 1001, "message": "账号或密码不能为空"})
			return
		}
		if pass == "wrong" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"code": 1002, "message": "账号或密码错误（密码填 wrong 就是这个提示）"})
			return
		}
		token := demoTokenPrefix + strconv.FormatInt(time.Now().UnixNano(), 36)
		demoTokenMu.Lock()
		demoLastToken = token
		demoTokenMu.Unlock()
		http.SetCookie(w, &http.Cookie{
			Name: demoCookie, Value: token, Path: "/", MaxAge: 3600, SameSite: http.SameSiteLaxMode,
		})
		writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": "ok", "data": token})
	})
}

// demoAuthed 这个请求是否带着有效登录态（Cookie 或 Authorization: Bearer demo-token-…）。
func demoAuthed(r *http.Request) (string, bool) {
	if c, err := r.Cookie(demoCookie); err == nil && strings.HasPrefix(c.Value, demoTokenPrefix) {
		return c.Value, true
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		tok := strings.TrimSpace(auth[len("bearer "):])
		if strings.HasPrefix(tok, demoTokenPrefix) {
			return tok, true
		}
	}
	return "", false
}

// demoOrder 自测用的订单数据。
type demoOrder struct {
	OrderID  string  `json:"order_id"`
	Customer string  `json:"customer"`
	Amount   float64 `json:"amount"`
	Status   string  `json:"status"`
	Created  string  `json:"created_at"`
}

// DemoOrderTotal 自测订单总条数。
const DemoOrderTotal = 42

func buildDemoOrders() []demoOrder {
	names := []string{"张三", "李四", "王五", "赵六", "钱七", "孙八", "周九"}
	statuses := []string{"paid", "pending", "refunded", "shipped"}
	out := make([]demoOrder, 0, DemoOrderTotal)
	for i := 0; i < DemoOrderTotal; i++ {
		out = append(out, demoOrder{
			OrderID:  fmt.Sprintf("SO2024%04d", 1001+i),
			Customer: names[i%len(names)],
			Amount:   float64(99+i*37) + 0.5*float64(i%2),
			Status:   statuses[i%len(statuses)],
			Created:  fmt.Sprintf("2024-05-%02d 10:%02d:00", 1+i%28, i%60),
		})
	}
	return out
}

var demoOrders = buildDemoOrders()

// OrdersHandler 「需要登录」的分页接口：没登录直接 401（用来验证会话到底带没带上）。
func OrdersHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": 405, "message": "method not allowed"})
			return
		}
		if _, ok := demoAuthed(r); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"code": 401, "message": "未登录或登录已过期：先打开 /demo/collect-login.html 登录一次",
			})
			return
		}
		page := clampInt(queryInt(r, "page", 1), 1, 1, 10000)
		size := clampInt(queryInt(r, "size", 5), 5, 1, 50)
		offset := (page - 1) * size
		list := []demoOrder{}
		if offset < len(demoOrders) {
			end := offset + size
			if end > len(demoOrders) {
				end = len(demoOrders)
			}
			list = demoOrders[offset:end]
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"code":    0,
			"message": "ok",
			"data": map[string]any{
				"total": DemoOrderTotal,
				"page":  page,
				"size":  size,
				"pages": (DemoOrderTotal + size - 1) / size,
				"list":  list,
			},
		})
	})
}
