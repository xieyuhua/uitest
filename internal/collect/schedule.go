package collect

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"uitest-server/internal/model"
)

// 本文件负责"什么时候抓"：把 HarvestSchedule 翻译成下一次触发时间。

// NextRunAt 计算 from 之后的下一次触发时刻。
func NextRunAt(s model.HarvestSchedule, from time.Time) (time.Time, bool) {
	switch s.Mode {
	case "interval":
		base := s.EverySeconds
		if base < 60 {
			base = 60
		}
		d := time.Duration(base) * time.Second
		return from.Add(jitterDuration(d, s.JitterPercent)), true
	case "daily":
		return nextDaily(s, from)
	default:
		return time.Time{}, false
	}
}

// jitterDuration 给固定周期加随机抖动：±p%。
func jitterDuration(d time.Duration, p int) time.Duration {
	if p <= 0 || d <= 0 {
		return d
	}
	if p > 50 {
		p = 50
	}
	delta := float64(d) * float64(p) / 100.0
	// [-delta, +delta]
	off := (rand.Float64()*2 - 1) * delta
	return d + time.Duration(off)
}

// dailyJitterSeconds 每日定时任务的最大抖动（±5 分钟以内，避免整点同时打）。
func dailyJitterSeconds(p int) int {
	if p <= 0 {
		return 0
	}
	if p > 50 {
		p = 50
	}
	max := 300 * p / 50
	if max <= 0 {
		return 0
	}
	return rand.Intn(max + 1)
}

// nextDaily 找下一次"每天定时"的时刻（本地时区）。
func nextDaily(s model.HarvestSchedule, from time.Time) (time.Time, bool) {
	hours := parseTimes(s.Times)
	if len(hours) == 0 {
		return time.Time{}, false
	}
	loc := from.Location()
	// 最多往后看 8 天，覆盖"每周某几天"的场景
	for day := 0; day <= 8; day++ {
		base := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, day)
		if len(s.Weekdays) > 0 && !weekdayAllowed(base, s.Weekdays) {
			continue
		}
		for _, hm := range hours {
			cand := time.Date(base.Year(), base.Month(), base.Day(), hm.h, hm.m, 0, 0, loc)
			if day == 0 {
				cand = cand.Add(time.Duration(dailyJitterSeconds(s.JitterPercent)) * time.Second)
			}
			if cand.After(from) {
				return cand, true
			}
		}
	}
	return time.Time{}, false
}

type hourMin struct{ h, m int }

func parseTimes(list []string) []hourMin {
	var out []hourMin
	for _, raw := range list {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parts := strings.Split(raw, ":")
		if len(parts) < 2 {
			continue
		}
		h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			continue
		}
		out = append(out, hourMin{h, m})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].h != out[j].h {
			return out[i].h < out[j].h
		}
		return out[i].m < out[j].m
	})
	return out
}

// weekdayAllowed ISO 周几：1=周一 … 7=周日。
func weekdayAllowed(t time.Time, days []int) bool {
	iso := int(t.Weekday())
	if iso == 0 {
		iso = 7
	}
	for _, d := range days {
		if d == iso {
			return true
		}
	}
	return false
}

var weekdayNames = map[int]string{1: "一", 2: "二", 3: "三", 4: "四", 5: "五", 6: "六", 7: "日"}

// DescribeSchedule 生成人可读的计划描述（控制台列表直接用）。
func DescribeSchedule(s model.HarvestSchedule) string {
	switch s.Mode {
	case "interval":
		base := s.EverySeconds
		if base < 60 {
			base = 60
		}
		desc := "每 " + humanDuration(time.Duration(base)*time.Second)
		if s.JitterPercent > 0 {
			desc += fmt.Sprintf("（±%d%% 抖动）", s.JitterPercent)
		}
		return desc
	case "daily":
		times := parseTimes(s.Times)
		if len(times) == 0 {
			return "每天定时（未填写时刻）"
		}
		var parts []string
		for _, hm := range times {
			parts = append(parts, fmt.Sprintf("%02d:%02d", hm.h, hm.m))
		}
		prefix := "每天 "
		if len(s.Weekdays) > 0 {
			var ds []string
			for _, d := range s.Weekdays {
				if n, ok := weekdayNames[d]; ok {
					ds = append(ds, n)
				}
			}
			sort.Strings(ds)
			prefix = "每周" + strings.Join(ds, "、") + " "
		}
		desc := prefix + strings.Join(parts, "、")
		if s.JitterPercent > 0 {
			desc += "（抖动触发）"
		}
		return desc
	default:
		return "仅手动触发"
	}
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%d 小时", h)
		}
		return fmt.Sprintf("%d 小时 %d 分钟", h, m)
	default:
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	}
}
