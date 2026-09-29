package scheduler

import (
	"testing"
	"time"
)

// 回归：限流耗尽后必须**真的安排延迟重试**，而不是只打个标记。
//
// 缺陷背景：原实现仅在 CheckinResult 上设 Retryable=true，但没有任何代码
// 消费该字段 —— 面板提示"稍后自动重试"，实际要等到下一个定时点（可能数小时）。
func TestMarkRetryableSchedulesRetry(t *testing.T) {
	s := New(Config{Name: "test"})
	now := time.Date(2026, 9, 30, 5, 53, 0, 0, time.Local)

	at, ok := s.markRetryable("uid-1", now)
	if !ok {
		t.Fatal("首次标记应成功安排重试")
	}
	if !at.After(now) {
		t.Fatalf("重试时刻 %v 应晚于当前 %v", at, now)
	}
	// 首次延迟应为 rateLimitRetryDelay * 1
	want := now.Add(rateLimitRetryDelay)
	if !at.Equal(want) {
		t.Fatalf("首次重试时刻=%v want %v", at, want)
	}

	// 到期前不应被取出
	if got := s.dueRetries(now.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("未到期就被取出: %v", got)
	}
	// 到期后应被取出
	if got := s.dueRetries(want); len(got) != 1 || got[0] != "uid-1" {
		t.Fatalf("到期未取出: %v", got)
	}
	// 取出后 pending 清除，不应重复取出
	if got := s.dueRetries(want.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("重复取出: %v", got)
	}
}

// 多次重试应递增退避，且超过上限后停止。
func TestMarkRetryableBackoffAndCap(t *testing.T) {
	s := New(Config{Name: "test"})
	base := time.Date(2026, 9, 30, 5, 0, 0, 0, time.Local)

	var last time.Time
	for i := 1; i <= rateLimitRetryMax; i++ {
		at, ok := s.markRetryable("uid-1", base)
		if !ok {
			t.Fatalf("第 %d 次应仍可安排", i)
		}
		if !last.IsZero() && !at.After(last) {
			t.Fatalf("第 %d 次退避未递增: %v <= %v", i, at, last)
		}
		last = at
		// 推进时间，模拟重试时刻到达
		base = at
		s.dueRetries(base)
	}

	// 第 rateLimitRetryMax+1 次应被拒
	if _, ok := s.markRetryable("uid-1", base); ok {
		t.Fatalf("超过上限(%d)后不应再安排", rateLimitRetryMax)
	}
}

// 跨天应重置计数（否则昨天失败过今天就没法重试了）。
func TestMarkRetryableResetsNextDay(t *testing.T) {
	s := New(Config{Name: "test"})
	day1 := time.Date(2026, 9, 30, 5, 0, 0, 0, time.Local)

	for i := 0; i < rateLimitRetryMax; i++ {
		s.markRetryable("uid-1", day1)
	}
	if _, ok := s.markRetryable("uid-1", day1); ok {
		t.Fatal("当天应已耗尽")
	}

	day2 := day1.Add(24 * time.Hour)
	if _, ok := s.markRetryable("uid-1", day2); !ok {
		t.Fatal("跨天应重置计数，允许重新安排")
	}
}

// 不同账号的重试状态互不干扰。
func TestRetryStateIsPerAccount(t *testing.T) {
	s := New(Config{Name: "test"})
	now := time.Date(2026, 9, 30, 5, 0, 0, 0, time.Local)

	s.markRetryable("uid-a", now)
	s.markRetryable("uid-b", now.Add(5*time.Minute))

	// 只有 a 到期
	got := s.dueRetries(now.Add(rateLimitRetryDelay))
	if len(got) != 1 || got[0] != "uid-a" {
		t.Fatalf("应只取出 uid-a，实际 %v", got)
	}
}

// 签到成功后应清除该账号的重试状态。
func TestClearRetryOnSuccess(t *testing.T) {
	s := New(Config{Name: "test"})
	now := time.Date(2026, 9, 30, 5, 0, 0, 0, time.Local)

	s.markRetryable("uid-1", now)
	s.clearRetry("uid-1")

	if got := s.dueRetries(now.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("清除后不应再取出: %v", got)
	}
	if !s.nextRetryAt().IsZero() {
		t.Fatal("清除后 nextRetryAt 应为零值")
	}
}

// nextRetryAt 应返回最近的待执行时刻。
func TestNextRetryAtPicksEarliest(t *testing.T) {
	s := New(Config{Name: "test"})
	now := time.Date(2026, 9, 30, 5, 0, 0, 0, time.Local)

	if !s.nextRetryAt().IsZero() {
		t.Fatal("无待执行时应为零值")
	}
	at1, _ := s.markRetryable("uid-a", now)
	at2, _ := s.markRetryable("uid-b", now)
	if got := s.nextRetryAt(); !got.Equal(at1) && !got.Equal(at2) {
		t.Fatalf("nextRetryAt=%v 应为 %v 或 %v", got, at1, at2)
	}
	// b 用更晚的基准 → 更晚到期；a 仍应是最早
	s2 := New(Config{Name: "test"})
	a1, _ := s2.markRetryable("uid-a", now)
	b1, _ := s2.markRetryable("uid-b", now.Add(time.Hour))
	want := a1
	if b1.Before(a1) {
		want = b1
	}
	if got := s2.nextRetryAt(); !got.Equal(want) {
		t.Fatalf("nextRetryAt=%v want %v", got, want)
	}
}
