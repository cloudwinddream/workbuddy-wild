package traework

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

func TestDailyCheckinClaimsWhenNotCheckedIn(t *testing.T) {
	var statusCalls, claimCalls atomic.Int32
	var checked atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT at" || r.Header.Get("X-User-Region") != "CN" {
			t.Errorf("missing Trae UG headers: auth=%q region=%q", r.Header.Get("Authorization"), r.Header.Get("X-User-Region"))
		}
		switch r.URL.Path {
		case EpCheckinStatus:
			statusCalls.Add(1)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"checked_in":%t,"credits":200,"enable":true}`, checked.Load())))
		case EpCheckinClaim:
			claimCalls.Add(1)
			checked.Store(true)
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New()
	c.CheckinRetry = 0
	c.CheckinMaxTry = 3
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at", DeviceID: "device"}); err != nil {
		t.Fatalf("daily checkin: %v", err)
	}
	if statusCalls.Load() != 2 || claimCalls.Load() != 1 {
		t.Fatalf("status calls=%d claim calls=%d", statusCalls.Load(), claimCalls.Load())
	}
}

func TestDailyCheckinSkipsClaimWhenAlreadyCheckedIn(t *testing.T) {
	var claimCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCheckinStatus {
			_, _ = w.Write([]byte(`{"checked_in":true,"credits":200,"enable":true}`))
			return
		}
		if r.URL.Path == EpCheckinClaim {
			claimCalls.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err == nil || err.Error() != "已签到" {
		t.Fatalf("err=%v, want 已签到", err)
	}
	if claimCalls.Load() != 0 {
		t.Fatalf("claim calls=%d", claimCalls.Load())
	}
}

func TestCheckinClaimBusinessError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCheckinClaim {
			calls.Add(1)
			_, _ = w.Write([]byte(`{"code":9074,"message":"operation too frequent"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.CheckinRetry = 0
	c.CheckinMaxTry = 3
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	err := c.CheckinClaim(&auth.Auth{AccessToken: "at"})
	// 修复后：限流耗尽重试应返回**可重试类型**（不再退化为普通错误），
	// 以便调度器识别为瞬时状态而非账号异常。
	if err == nil {
		t.Fatal("expected rate-limited error, got nil")
	}
	if !IsCheckinRateLimited(err) {
		t.Fatalf("err=%v, want ErrCheckinRateLimited", err)
	}
	var rl *ErrCheckinRateLimited
	if !errors.As(err, &rl) || rl.Attempts != 3 {
		t.Fatalf("Attempts=%v, want 3", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("claim calls=%d, want 3 (CheckinMaxTry)", calls.Load())
	}
}

// 限流持续时应重试到上限，而不是只试 1 次（修复前只重试 1 次导致高峰必失败）。
func TestCheckinClaimRetriesUpToMaxTry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCheckinClaim {
			calls.Add(1)
			_, _ = w.Write([]byte(`{"code":9074,"message":"当前参与用户太多，请稍后再试"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.CheckinRetry = 0 // 测试中不真正等待
	c.CheckinMaxTry = 5
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.CheckinClaim(&auth.Auth{AccessToken: "at"}); !IsCheckinRateLimited(err) {
		t.Fatalf("err=%v, want rate limited", err)
	}
	if calls.Load() != 5 {
		t.Fatalf("claim calls=%d, want 5", calls.Load())
	}
}

// 中间某次成功后应立即返回 nil，不再继续重试。
func TestCheckinClaimSucceedsMidRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCheckinClaim {
			if calls.Add(1) < 3 {
				_, _ = w.Write([]byte(`{"code":9074,"message":"busy"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.CheckinRetry = 0
	c.CheckinMaxTry = 5
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.CheckinClaim(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("want success at 3rd attempt, got %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("claim calls=%d, want 3", calls.Load())
	}
}

// 非限流业务错误应立即失败，不浪费重试（避免把真错误当限流反复试）。
func TestCheckinClaimNonRateLimitFailsFast(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCheckinClaim {
			calls.Add(1)
			_, _ = w.Write([]byte(`{"code":5001,"message":"some real error"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.CheckinRetry = 0
	c.CheckinMaxTry = 5
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	err := c.CheckinClaim(&auth.Auth{AccessToken: "at"})
	if err == nil || IsCheckinRateLimited(err) {
		t.Fatalf("err=%v, want non-rate-limit error", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("claim calls=%d, want 1 (fail fast)", calls.Load())
	}
}

// backoffDelay 应随次数增长并带上限。
func TestBackoffDelayGrowsAndCaps(t *testing.T) {
	base := 8 * time.Second
	var prev time.Duration
	for i := 0; i < 6; i++ {
		d := backoffDelay(base, i)
		if d <= 0 {
			t.Fatalf("attempt %d: delay=%v must be positive", i, d)
		}
		if d > 2*time.Minute+2*time.Minute/4+time.Second {
			t.Fatalf("attempt %d: delay=%v exceeds cap", i, d)
		}
		if i > 0 && d < prev/4 {
			// 抖动可能让相邻值波动，但不该出现数量级回退
			t.Fatalf("attempt %d: delay=%v regressed from %v", i, d, prev)
		}
		prev = d
	}
	// 大 attempt 必须被封顶在 2min 附近（含 ±25% 抖动）
	d := backoffDelay(base, 20)
	if d < 90*time.Second || d > 150*time.Second {
		t.Fatalf("attempt 20: delay=%v, want ~2min", d)
	}
}

func TestCheckinClaimRetriesRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpCheckinClaim {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"code":9074,"message":"operation too frequent"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
	}))
	defer srv.Close()

	c := New()
	c.CheckinRetry = 0
	c.CheckinMaxTry = 3
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.CheckinClaim(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("retry claim: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("claim calls=%d", calls.Load())
	}
}
