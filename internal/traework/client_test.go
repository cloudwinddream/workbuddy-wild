package traework

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

func TestDailyCheckinClaimsWhenNotCheckedIn(t *testing.T) {
	var statusCalls, claimCalls atomic.Int32
	var didChecked atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT at" || r.Header.Get("X-User-Region") != "CN" {
			t.Errorf("missing Trae UG headers: auth=%q region=%q", r.Header.Get("Authorization"), r.Header.Get("X-User-Region"))
		}
		switch r.URL.Path {
		case EpCheckinStatus:
			statusCalls.Add(1)
			// 关键：checked_in 恒 false（API 调用的真实表现），
			// 签到成功的标志是 did_checked_in。用 checked_in 做验证会永远误判失败。
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"checked_in":false,"did_checked_in":%t,"credits":200,"enable":true}`, didChecked.Load())))
		case EpCheckinClaim:
			claimCalls.Add(1)
			didChecked.Store(true)
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at", DeviceID: "device"}); err != nil {
		t.Fatalf("daily checkin: %v", err)
	}
	// status 调用 2 次（前置查询 + 后置验证），claim 1 次
	if statusCalls.Load() != 2 || claimCalls.Load() != 1 {
		t.Fatalf("status calls=%d claim calls=%d", statusCalls.Load(), claimCalls.Load())
	}
}

// 回归（v0.5.5 之前的第二个 bug）：后置验证必须认 `did_checked_in`。
//
// 真实响应（2026-09-30 抓包）：签到成功后为
//
//	{"checked_in":false,"did_checked_in":true,"credits":100,...}
//
// `checked_in` 表示"用户当前处于已签到会话"，对 API 调用方恒为 false。
// 旧实现只读 checked_in → 每次签到都被判失败。
//
// 本测试模拟：前置 status 报未签（checked_in=false, did_checked_in=false），
// claim 成功，后置 status 才翻转 did_checked_in=true → 必须判成功。
func TestDailyCheckinAcceptsDidCheckedIn(t *testing.T) {
	var claimed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCheckinStatus:
			// checked_in 始终 false；did_checked_in 只在 claim 之后变 true
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"checked_in":false,"did_checked_in":%t,"credits":100,"enable":true}`, claimed.Load())))
		case EpCheckinClaim:
			claimed.Store(true)
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("did_checked_in=true 时必须判成功，得到 err=%v", err)
	}
}

// 前置 status 已报 did_checked_in=true 时应直接返回"已签到"，不再 claim。
func TestDailyCheckinAlreadyDidCheckedIn(t *testing.T) {
	var claimCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCheckinStatus:
			_, _ = w.Write([]byte(`{"checked_in":false,"did_checked_in":true,"credits":100,"enable":true}`))
		case EpCheckinClaim:
			claimCalls.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("已签到时必须返回错误（已签到），却成功了")
	}
	if claimCalls.Load() != 0 {
		t.Fatalf("已签到时不应再 claim，claim calls=%d", claimCalls.Load())
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

// 9074 现在被正确理解为**设备未注册**，因此：
//  1. 立即返回 ErrCheckinRateLimited（不重试——重试永不成功）
//  2. IsRateLimited() 返回 false，调度器不再安排无效的"稍后重试"
//  3. 只调用 1 次 claim
func TestCheckinClaimDeviceRejectedIsNotRateLimit(t *testing.T) {
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
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	err := c.CheckinClaim(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("expected ErrCheckinRateLimited, got nil")
	}
	if !IsCheckinRateLimited(err) {
		t.Fatalf("err=%v, want ErrCheckinRateLimited", err)
	}
	var rl *ErrCheckinRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("errors.As failed for %v", err)
	}
	// 关键：必须报告"不可重试"，否则调度器会做无效重试
	if rl.IsRateLimited() {
		t.Fatal("9074 是设备未注册，必须 IsRateLimited()==false（重试无用）")
	}
	// 只调一次，不再做指数退避
	if calls.Load() != 1 {
		t.Fatalf("claim calls=%d, want 1（9074 不重试）", calls.Load())
	}
}

// 非 9074 业务错误应立即失败，且不属于 9074 类型。
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
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	err := c.CheckinClaim(&auth.Auth{AccessToken: "at"})
	if err == nil || IsCheckinRateLimited(err) {
		t.Fatalf("err=%v, want non-9074 error", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("claim calls=%d, want 1 (fail fast)", calls.Load())
	}
}

// success 明确为 false 时报错。
func TestCheckinClaimSuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCheckinClaim {
			_, _ = w.Write([]byte(`{"code":0,"success":false,"message":"denied"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.CheckinClaim(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("want error when success=false")
	}
}

// 9074 之外的其它 code 不得被当成 9074。
func TestIsCheckinRateLimitedOnly9074(t *testing.T) {
	if IsCheckinRateLimited(nil) {
		t.Fatal("nil 不应是 9074")
	}
	if IsCheckinRateLimited(errors.New("checkin claim code=9095 msg=already")) {
		t.Fatal("9095（今日已签）不应被当成 9074")
	}
}

// ---------------------------------------------------------------------------
// 积分余额解析（修复「显示 4050 但实际只有 310」）
// ---------------------------------------------------------------------------

// 核心回归：`credits_limit` 是**额度上限**，绝不能直接当剩余积分。
//
// 历史病根（v0.5.0 及以前）：把各包 credits_limit 简单累加 → 显示 4050。
// 正确做法是每包算 `上限 - 已用`（已用缺失按 0 计），本测试锁定这一算法。
func TestUserResourceDoesNotTreatLimitAsRemain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCurrentEntList {
			// 两个包：一个全用完（上限 4000、已用 4000），一个完全没用（上限 500）
			// 正确剩余 = 0 + 500 = 500；若把上限直接累加会得到 4500（旧 bug）
			_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
				{"entitlement_base_info":{"quota":{"credits_limit":4000}},"usage":{"credits_amount":4000}},
				{"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{}}
			]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	got, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got == 4500 {
		t.Fatal("把 credits_limit 直接累加当余额（旧 bug 复现）")
	}
	if got != 500 {
		t.Fatalf("remain=%d want 500（4000-4000 + 500-0）", got)
	}
}

// 有 remain 字段时直接用该字段。
func TestUserResourcePrefersRemainField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCurrentEntList {
			_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
				{"entitlement_base_info":{"quota":{"credits_limit":4000,"credits_remain":310}}}
			]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	got, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != 310 {
		t.Fatalf("remain=%d want 310", got)
	}
}

// 无 remain 字段时用 上限 - 已用 计算。
func TestUserResourceComputesLimitMinusUsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCurrentEntList {
			_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
				{"entitlement_base_info":{"quota":{"credits_limit":4000,"credits_used":3690}}}
			]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	got, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != 310 {
		t.Fatalf("remain=%d want 310 (4000-3690)", got)
	}
}

// 多包时各自算剩余再求和，而不是把上限求和。
func TestUserResourceSumsRemainAcrossPacks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCurrentEntList {
			_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
				{"entitlement_base_info":{"quota":{"credits_limit":1000,"credits_used":900}}},
				{"entitlement_base_info":{"quota":{"credits_limit":500,"credits_remain":210}}}
			]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	got, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// (1000-900) + 210 = 310
	if got != 310 {
		t.Fatalf("remain=%d want 310", got)
	}
}

// 已用超过上限时应钳到 0，不返回负数。
func TestUserResourceClampsNegative(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCurrentEntList {
			_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
				{"entitlement_base_info":{"quota":{"credits_limit":100,"credits_used":250}}}
			]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	got, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != 0 {
		t.Fatalf("remain=%d want 0 (clamped)", got)
	}
}

// ent_usage 无可用字段时应回退到 checkin/status 的 credits。
// 回归：解析不出余额时**不得**回退到 checkin/status 的 credits。
//
// 这是 v0.5.1 引入的缺陷：checkin/status 的 credits 实测恒为 150（签到奖励固定值），
// 拿它冒充余额会让面板显示 150，并让「优先积分」策略把账号当成恒定满额。
// 现在要求明确失败（返回 error），由调用方把该账号积分标为不可用。
func TestUserResourceDoesNotFallBackToCheckinStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCurrentEntList:
			// 权益包里**没有任何额度信息**（无 credits_limit、无 usage）
			// → 无法得出余额，必须报错，而不能去别的接口找数
			_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
				{"entitlement_base_info":{"quota":{"no_bonus_quota":true}},"usage":{}}
			]}`))
		case EpCheckinStatus:
			// 即使这里有值，也不允许被采用
			_, _ = w.Write([]byte(`{"checked_in":false,"credits":150,"enable":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	got, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatalf("解析不出余额时必须报错，却返回了 remain=%d（疑似采用了 checkin credits）", got)
	}
	if got != 0 {
		t.Fatalf("出错时 remain 应为 0，得到 %d", got)
	}
}

// 回归：裸 "credits" 字段不得被当作余额。
// 实测该字段恒为 150，是签到奖励值而非余额。
func TestUserResourceIgnoresBareCreditsField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
			{"entitlement_base_info":{"quota":{"credits":150}}}
		]}`))
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	got, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatalf("裸 credits=150 不得当余额，却返回 remain=%d", got)
	}
}

// 回归：裸 "credits" 与 "used" 同现时也不得做减法。
// 二者语义都不确定，做减法同样可能得出假余额。
func TestUserResourceIgnoresBareCreditsMinusUsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
			{"entitlement_base_info":{"quota":{"credits":500,"used":400}}}
		]}`))
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	got, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatalf("裸 credits/used 不得做减法，却返回 remain=%d", got)
	}
}

// 部分包可解析、部分不可解析时：只要有一个包给出可信余额就采用，
// 不可解析的包被跳过（而不是当成 0 参与求和，那会虚低）。
// 部分包缺少 usage 时：按"未使用"处理（剩余 = 上限），与有 usage 的包一起求和。
//
// 语义变更（v0.5.3）：`usage` 缺失或为 `{}` 表示**该包未被使用**，
// 剩余即全额上限。此前 v0.5.1/v0.5.2 把它当成"无法判断"而跳过，
// 导致余额虚低；实测真实响应里最新的签到包正是 `usage:{}`。
func TestUserResourceTreatsMissingUsageAsUnused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
			{"entitlement_base_info":{"quota":{"credits_limit":4000}},"usage":{}},
			{"entitlement_base_info":{"quota":{"credits_limit":500,"credits_remain":310}}}
		]}`))
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	got, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// 包0: 4000 - 0 = 4000；包1: 取 credits_remain = 310 → 合计 4310
	if got != 4310 {
		t.Fatalf("remain=%d want 4310（无 usage 的包按未使用计）", got)
	}
}

// 字段名容错：嵌套包装与不同命名都应能解析。
func TestUserResourceTolerantFieldNames(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{"嵌套 data 包装", `{"data":{"entitlement_pack_list":[{"quota":{"credits_limit":500,"credits_remain":77}}]}}`, 77},
		{"credits_available 命名", `{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":500,"credits_available":88}}}]}`, 88},
		{"credits_balance 命名", `{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":500,"credits_balance":99}}}]}`, 99},
		{"used 命名变体", `{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":500,"used":400}}}]}`, 100},
		{"复数包装 result", `{"result":{"pack_list":[{"quota":{"credits_limit":900,"credits_remain":123}}]}}`, 123},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := New()
			c.HTTP = srv.Client()
			c.UgHost = srv.URL
			got, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if got != tc.want {
				t.Fatalf("remain=%d want %d", got, tc.want)
			}
		})
	}
}
