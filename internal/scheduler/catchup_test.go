package scheduler

import (
	"testing"
	"time"
)

func TestNeedsCatchUp(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 3, 10, 0, 0, 0, loc) // 已过 09:00 签到时刻
	minutes := []int{9 * 60}

	// 从未签到 → 需要补签
	if !needsCatchUp(time.Time{}, now, minutes) {
		t.Error("从未签到应补签")
	}
	// 昨天签过 → 需要补签
	yesterday := time.Date(2026, 10, 2, 9, 5, 0, 0, loc)
	if !needsCatchUp(yesterday, now, minutes) {
		t.Error("昨天签过、今天没签应补签")
	}
	// 今天已签 → 不补
	today := time.Date(2026, 10, 3, 9, 5, 0, 0, loc)
	if needsCatchUp(today, now, minutes) {
		t.Error("今天已签不应补签")
	}
	// 还没到签到时刻 → 不补
	early := time.Date(2026, 10, 3, 8, 0, 0, 0, loc)
	if needsCatchUp(time.Time{}, early, minutes) {
		t.Error("未到签到时刻不应补签")
	}
	// 无签到时间配置 → 不补
	if needsCatchUp(time.Time{}, now, nil) {
		t.Error("无时刻表不应补签")
	}
	// 多个时刻取最早的一个
	multi := []int{20 * 60, 8 * 60}
	morning := time.Date(2026, 10, 3, 8, 30, 0, 0, loc)
	if !needsCatchUp(time.Time{}, morning, multi) {
		t.Error("已过最早时刻（08:00）应补签")
	}
}
