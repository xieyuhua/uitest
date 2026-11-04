package collect

import (
	"log"
	"sync"
	"time"

	"uitest-server/internal/model"
	"uitest-server/internal/store"
)

// 本文件是"定时器"：每 10 秒扫一遍任务，到点就触发一次采集。
//
// 设计要点：
//   - 只有一个调度协程，任务串行触发，不会因为"服务器刚重启"一次性打爆一批任务；
//   - 任务正在跑就跳过这一轮（下一次触发时间会顺延），绝不叠加；
//   - next_run_at 由服务端维护，前端不用算 cron，也不会因为改时区而错乱。

const schedulerTick = 10 * time.Second

// Scheduler 定时调度器。
type Scheduler struct {
	store *store.DB
	h     *Harvester

	mu      sync.Mutex
	stopCh  chan struct{}
	running bool
}

// NewScheduler 创建调度器（不会自动启动）。
func NewScheduler(db *store.DB, h *Harvester) *Scheduler {
	return &Scheduler{store: db, h: h}
}

// Start 启动调度循环（幂等）。
func (s *Scheduler) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.stopCh = make(chan struct{})
	stop := s.stopCh
	s.mu.Unlock()

	s.bootstrap()
	go s.loop(stop)
	log.Printf("[collect] 定时调度已启动（每 %s 检查一次）", schedulerTick)
}

// Stop 停止调度循环。
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return
	}
	s.running = false
	close(s.stopCh)
}

// bootstrap 启动时校正 next_run_at：服务停机期间错过的任务不在启动瞬间补跑，
// 而是重新按计划排到下一个时间点（避免一开机就并发打一堆请求）。
func (s *Scheduler) bootstrap() {
	now := time.Now()
	for _, t := range s.store.HarvestTasksForSchedule() {
		if t.Status != "active" {
			continue
		}
		if t.Schedule.Mode == "manual" || t.Schedule.Mode == "" {
			continue
		}
		next, ok := NextRunAt(t.Schedule, now)
		if !ok {
			continue
		}
		if t.NextRunAt.IsZero() || t.NextRunAt.Before(now) {
			_, _ = s.store.MutateHarvestTask(t.ID, func(task *model.HarvestTask) {
				task.NextRunAt = next
			})
		}
	}
}

func (s *Scheduler) loop(stop chan struct{}) {
	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

// tick 一轮检查。
func (s *Scheduler) tick() {
	now := time.Now()
	for _, t := range s.store.HarvestTasksForSchedule() {
		if t.Status != "active" {
			continue
		}
		if t.Schedule.Mode == "manual" || t.Schedule.Mode == "" {
			continue
		}
		if t.NextRunAt.IsZero() {
			if next, ok := NextRunAt(t.Schedule, now); ok {
				_, _ = s.store.MutateHarvestTask(t.ID, func(task *model.HarvestTask) { task.NextRunAt = next })
			}
			continue
		}
		if t.NextRunAt.After(now) {
			continue
		}
		// 到点了：先把 next_run_at 推后，再触发（即使触发失败也不会死循环）
		next, ok := NextRunAt(t.Schedule, now)
		_, _ = s.store.MutateHarvestTask(t.ID, func(task *model.HarvestTask) {
			if ok {
				task.NextRunAt = next
			} else {
				task.NextRunAt = time.Time{}
			}
		})
		if s.h.Running(t.ID) {
			log.Printf("[collect] 任务 %s 上次还没跑完，跳过本轮", t.Name)
			continue
		}
		if _, err := s.h.Start(t.ID, "schedule"); err != nil {
			log.Printf("[collect] 定时触发「%s」失败：%v", t.Name, err)
		} else {
			log.Printf("[collect] 定时触发「%s」", t.Name)
		}
	}
}

// Recompute 重算某个任务的下一次触发时间（任务被新建/修改后调用）。
func (s *Scheduler) Recompute(taskID string) {
	t, err := s.store.GetHarvestTask(taskID)
	if err != nil {
		return
	}
	var next time.Time
	if t.Status == "active" {
		if n, ok := NextRunAt(t.Schedule, time.Now()); ok {
			next = n
		}
	}
	_, _ = s.store.MutateHarvestTask(taskID, func(task *model.HarvestTask) { task.NextRunAt = next })
}
