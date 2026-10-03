package checkin

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestNextRun(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 2, 13, 0, 0, 0, loc)
	next := NextRun([]string{"01:00", "22:00"}, now)
	want := time.Date(2026, 10, 2, 22, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Errorf("NextRun = %v, want %v", next, want)
	}
	// 已过的时刻顺延到明天
	next = NextRun([]string{"01:00"}, now)
	want = time.Date(2026, 10, 3, 1, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Errorf("NextRun = %v, want %v", next, want)
	}
	// 非法时刻忽略
	if !NextRun([]string{"garbage", "25:99"}, now).IsZero() {
		t.Error("全非法时刻应返回 zero")
	}
	if !NextRun(nil, now).IsZero() {
		t.Error("空时刻表应返回 zero")
	}
}

func TestParseTimes(t *testing.T) {
	got := ParseTimes("01:00, 22:00,,")
	if len(got) != 2 || got[0] != "01:00" || got[1] != "22:00" {
		t.Errorf("ParseTimes = %v", got)
	}
}

// 调度器必须在目标分钟真正触发，且能跟随运行中修改的时刻表
// （回归：睡醒后重算 NextRun 会过点顺延到明天，导致整轮漏执行）。
func TestRunSchedulerFiresAndFollowsTimeChange(t *testing.T) {
	var times atomic.Value
	times.Store([]string{"23:59"}) // 先给个远时间
	called := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunScheduler(ctx, func() []string {
			v, _ := times.Load().([]string)
			return v
		}, false, "test", func() string {
			select {
			case called <- struct{}{}:
			default:
			}
			return "ok"
		})
	}()
	// 把时刻表改到约 1 分钟后：调度器应跟随新时间并在到点时触发。
	target := time.Now().Add(65 * time.Second).Format("15:04")
	times.Store([]string{target})
	select {
	case <-called:
	case <-time.After(120 * time.Second):
		t.Fatalf("调度器未在目标时间 %s 触发", target)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后调度器未退出")
	}
}
