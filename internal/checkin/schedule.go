package checkin

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"
)

// NextRun 计算下一次执行时间：times 为 ["01:00","22:00"] 格式（当天已过的
// 时刻顺延到明天），返回最近的一个未来时刻。times 为空/全非法返回 zero。
func NextRun(times []string, now time.Time) time.Time {
	var best time.Time
	for _, t := range times {
		t = strings.TrimSpace(t)
		hm := strings.Split(t, ":")
		if len(hm) != 2 {
			continue
		}
		h, err1 := strconv.Atoi(strings.TrimSpace(hm[0]))
		m, err2 := strconv.Atoi(strings.TrimSpace(hm[1]))
		if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			continue
		}
		cand := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
		if !cand.After(now) {
			cand = cand.Add(24 * time.Hour)
		}
		if best.IsZero() || cand.Before(best) {
			best = cand
		}
	}
	return best
}

// ParseTimes 解析 "01:00,22:00" 为时刻表。
func ParseTimes(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// RunScheduler 每日定时调度：按 timesFn 提供的时刻表执行 fn（时刻表每次
// 循环都重新读取，修改后最迟 1 分钟生效）；runOnStartup 为 true 时启动后
// 先执行一次。ctx 取消时退出。
func RunScheduler(ctx context.Context, timesFn func() []string, runOnStartup bool, name string, fn func() string) {
	if timesFn == nil {
		timesFn = func() []string { return nil }
	}
	log.Printf("[%s] 定时调度启动（统一签到时间）", name)
	if runOnStartup {
		log.Printf("[%s] 启动后先执行一次", name)
		log.Printf("[%s] 结果: %s", name, fn())
	}
	var lastNext time.Time
	for {
		next := NextRun(timesFn(), time.Now())
		if next.IsZero() {
			log.Printf("[%s] 时刻表无效，1 小时后重试", name)
			if !sleepCtx(ctx, time.Hour) {
				return
			}
			continue
		}
		if !next.Equal(lastNext) {
			log.Printf("[%s] 下次执行：%s", name, next.Format("2006-01-02 15:04"))
			lastNext = next
		}
		wait := time.Until(next)
		if wait > time.Minute {
			// 还早：睡 1 分钟就重算，以便签到时间被修改时尽快跟随。
			if !sleepCtx(ctx, time.Minute) {
				log.Printf("[%s] 调度退出", name)
				return
			}
			continue
		}
		// 最后一段路：小步睡到点。每步先判是否到点（到点立即执行），
		// 再确认目标没被改掉（改掉就回外层重算）；顺序不能反——过点后
		// NextRun 会顺延到明天，先判目标变化会把这次执行漏掉。
		fired := false
		for {
			w := time.Until(next)
			if w <= 0 {
				log.Printf("[%s] 结果: %s", name, fn())
				fired = true
				break
			}
			if cur := NextRun(timesFn(), time.Now()); !cur.Equal(next) {
				break
			}
			step := w
			if step > 10*time.Second {
				step = 10 * time.Second
			}
			if !sleepCtx(ctx, step) {
				log.Printf("[%s] 调度退出", name)
				return
			}
		}
		if fired {
			lastNext = time.Time{}
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
