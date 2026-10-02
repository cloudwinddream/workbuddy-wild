package checkin

import (
	"context"
	"fmt"
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

// RunScheduler 每日定时调度：按 times 执行 fn；runOnStartup 为 true 时
// 启动后先执行一次。ctx 取消时退出。
func RunScheduler(ctx context.Context, times []string, runOnStartup bool, name string, fn func() string) {
	log.Printf("[%s] 定时调度启动，时刻表 %v", name, times)
	if runOnStartup {
		log.Printf("[%s] 启动后先执行一次", name)
		log.Printf("[%s] 结果: %s", name, fn())
	}
	for {
		next := NextRun(times, time.Now())
		if next.IsZero() {
			log.Printf("[%s] 时刻表无效，1 小时后重试", name)
			if !sleepCtx(ctx, time.Hour) {
				return
			}
			continue
		}
		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		log.Printf("[%s] 下次执行：%s（%s后）", name, next.Format("2006-01-02 15:04"), fmtDur(wait))
		if !sleepCtx(ctx, wait) {
			log.Printf("[%s] 调度退出", name)
			return
		}
		log.Printf("[%s] 结果: %s", name, fn())
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

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if h > 0 {
		return fmt.Sprintf("%d小时%d分", h, m)
	}
	return fmt.Sprintf("%d分", m)
}
