package checkin

import (
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
