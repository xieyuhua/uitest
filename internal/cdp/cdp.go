// Package cdp 是一个精简的 Chrome DevTools Protocol 客户端：
// 用 WebSocket 与浏览器通信，支持扁平化会话（Target.attachToTarget + flatten），
// 也就是一个浏览器连接里同时驱动多个标签页。
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ProtocolError 浏览器返回的协议级错误。
type ProtocolError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (e *ProtocolError) Error() string {
	if e.Data != "" {
		return "CDP " + e.Message + " (" + e.Data + ")"
	}
	return "CDP " + e.Message
}

// Event 浏览器侧推送的事件。
type Event struct {
	Method    string
	SessionID string
	Params    json.RawMessage
}

type rawMsg struct {
	ID        int             `json:"id"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Result    json.RawMessage `json:"result"`
	Error     *ProtocolError  `json:"error"`
	Params    json.RawMessage `json:"params"`
}

// Client 一条到浏览器（或某个 target 会话）的连接。
type Client struct {
	ws      *websocket.Conn
	writeMu sync.Mutex

	mu      sync.Mutex
	seq     int
	pending map[int]chan rawMsg

	subMu sync.Mutex
	subs  map[string][]chan Event

	closeOnce sync.Once
	closed    chan struct{}
}

// Dial 连接 CDP WebSocket 地址（形如 ws://127.0.0.1:9222/devtools/browser/<id>）。
func Dial(ctx context.Context, wsURL string) (*Client, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, ReadBufferSize: 1 << 20, WriteBufferSize: 1 << 20}
	// 不设置 Origin 头：本地 CDP 端点据此放行非浏览器客户端
	ws, _, err := dialer.DialContext(ctx, wsURL, http.Header{})
	if err != nil {
		return nil, err
	}
	c := &Client{
		ws:      ws,
		pending: map[int]chan rawMsg{},
		subs:    map[string][]chan Event{},
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *Client) readLoop() {
	defer c.shutdown(errors.New("CDP 连接已关闭"))
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var m rawMsg
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.ID != 0 {
			c.mu.Lock()
			ch, ok := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ok {
				ch <- m
			}
			continue
		}
		if m.Method == "" {
			continue
		}
		ev := Event{Method: m.Method, SessionID: m.SessionID, Params: m.Params}
		c.subMu.Lock()
		list := append([]chan Event(nil), c.subs[m.Method]...)
		if m.Method != "" {
			list = append(list, c.subs["*"]...)
		}
		c.subMu.Unlock()
		for _, ch := range list {
			select {
			case ch <- ev:
			default: // 订阅者太慢就丢事件，调用方另有轮询兜底
			}
		}
	}
}

func (c *Client) shutdown(err error) {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.ws.Close()
		c.mu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
	})
}

// Closed 返回连接关闭信号。
func (c *Client) Closed() <-chan struct{} { return c.closed }

// Close 关闭连接。
func (c *Client) Close() error {
	c.shutdown(errors.New("主动关闭"))
	return nil
}

// Subscribe 订阅某类事件（method 传 "*" 订阅全部）。
func (c *Client) Subscribe(method string) (<-chan Event, func()) {
	ch := make(chan Event, 256)
	c.subMu.Lock()
	c.subs[method] = append(c.subs[method], ch)
	c.subMu.Unlock()
	cancel := func() {
		c.subMu.Lock()
		list := c.subs[method]
		for i, x := range list {
			if x == ch {
				c.subs[method] = append(list[:i], list[i+1:]...)
				break
			}
		}
		c.subMu.Unlock()
	}
	return ch, cancel
}

// Call 调用一个 CDP 方法，result 可为 nil。
// 若调用方没给 deadline，这里补一个默认超时，避免页面崩溃/浏览器无响应时永久阻塞。
func (c *Client) Call(ctx context.Context, sessionID, method string, params any, result any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}
	if params == nil {
		params = struct{}{}
	}
	body := map[string]any{"method": method, "params": params}
	if sessionID != "" {
		body["sessionId"] = sessionID
	}

	c.mu.Lock()
	c.seq++
	id := c.seq
	ch := make(chan rawMsg, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	body["id"] = id

	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	err = c.ws.WriteMessage(websocket.TextMessage, raw)
	c.writeMu.Unlock()
	if err != nil {
		return err
	}

	select {
	case msg, ok := <-ch:
		if !ok {
			return errors.New("CDP 连接已关闭")
		}
		if msg.Error != nil {
			return msg.Error
		}
		if result != nil && len(msg.Result) > 0 {
			return json.Unmarshal(msg.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.closed:
		return errors.New("CDP 连接已关闭")
	}
}

// WithTimeout 便捷构造带超时的 context。
func WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = 15 * time.Second
	}
	return context.WithTimeout(parent, d)
}
