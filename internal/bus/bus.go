// Package bus 是一个极简的进程内发布订阅，用于把执行日志实时推给控制台（SSE）。
package bus

import (
	"sync"

	"uitest-server/internal/model"
)

// Event 推送给前端的事件。
type Event struct {
	Type    string            `json:"type"` // log | step | status | end
	Log     *model.LogEntry   `json:"log,omitempty"`
	Step    *model.StepResult `json:"step,omitempty"`
	Status  string            `json:"status,omitempty"`
	Message string            `json:"message,omitempty"`
}

// Bus 按 runID 分发事件。
type Bus struct {
	mu   sync.Mutex
	seq  int
	subs map[string]map[int]chan Event
}

// New 创建总线。
func New() *Bus {
	return &Bus{subs: map[string]map[int]chan Event{}}
}

// Subscribe 订阅指定运行的实时事件，返回取消函数。
func (b *Bus) Subscribe(runID string) (<-chan Event, func()) {
	ch := make(chan Event, 128)
	b.mu.Lock()
	b.seq++
	id := b.seq
	if b.subs[runID] == nil {
		b.subs[runID] = map[int]chan Event{}
	}
	b.subs[runID][id] = ch
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		if m, ok := b.subs[runID]; ok {
			if _, ok2 := m[id]; ok2 {
				delete(m, id)
				close(ch)
			}
			if len(m) == 0 {
				delete(b.subs, runID)
			}
		}
		b.mu.Unlock()
	}
	return ch, cancel
}

// Publish 广播事件（订阅者处理不过来就丢弃，前端另有轮询兜底）。
func (b *Bus) Publish(runID string, ev Event) {
	b.mu.Lock()
	list := make([]chan Event, 0, len(b.subs[runID]))
	for _, ch := range b.subs[runID] {
		list = append(list, ch)
	}
	b.mu.Unlock()
	for _, ch := range list {
		select {
		case ch <- ev:
		default:
		}
	}
}
