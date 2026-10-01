package traework

import (
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
// 前置 status 已报 did_checked_in=true 时，**仍然会发一次 claim**。
//
// 为什么不再跳过：实测（2026-10-01）存在 did_checked_in=true 但当天
// 其实没拿到额度的状态（checked_in=false）。若前置直接 return，
// 就永远救不回这类账号。现在改为"照发 claim"：
//   已领 -> 9095（幂等，无副作用）
//   被误标但未领 -> 0 且真正入账
func TestDailyCheckinStillClaimsWhenMarkedCheckedIn(t *testing.T) {
	var claimCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCheckinStatus:
			_, _ = w.Write([]byte(`{"checked_in":false,"did_checked_in":true,"credits":100,"enable":true}`))
		case EpCheckinClaim:
			claimCalls.Add(1)
			_, _ = w.Write([]byte(`{"code":9095,"message":"当前设备今日已经签到"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	_ = c.DailyCheckin(&auth.Auth{AccessToken: "at", DeviceID: "4484256452647802"})
	if claimCalls.Load() == 0 {
		t.Fatal("前置已标记已签时仍应发 claim（否则救不回被误标的账号）")
	}
}

func TestCheckinClaim9095IsAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCheckinClaim {
			_, _ = w.Write([]byte(`{"code":9095,"message":"当前设备今日已经签到，请明日再来哦～"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	err := c.CheckinClaim(&auth.Auth{AccessToken: "at", DeviceID: "4484256452647802"})
	if err == nil {
		t.Fatal("9095 必须返回 ErrCheckinAlreadyClaimed，以便上层按\"今日已签\"展示")
	}
	if !IsCheckinAlreadyClaimed(err) {
		t.Fatalf("err=%v, want ErrCheckinAlreadyClaimed", err)
	}
	// 它不该被误认为 9074（那是设备未注册，需换设备号）
	if IsCheckinRateLimited(err) {
		t.Fatal("9095 不是 9074（设备未注册），不可混淆")
	}
}

// claim 缺少 X-Device-Id 时上游返回 9004（订单参数不正确）。
// 这解释了为什么"不带设备号"不能作为绕过手段。
func TestCheckinClaimWithoutDeviceRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Device-Id") == "" {
			_, _ = w.Write([]byte(`{"code":9004,"message":"The submitted order parameters are incorrect."}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL

	// 不带设备号 → 9004
	err := c.CheckinClaim(&auth.Auth{AccessToken: "at"})
	if err == nil || IsCheckinAlreadyClaimed(err) || IsCheckinRateLimited(err) {
		t.Fatalf("缺设备号应报普通错误，得到 %v", err)
	}
	// 带真实设备号 → 成功
	if err := c.CheckinClaim(&auth.Auth{AccessToken: "at", DeviceID: "4484256452647802"}); err != nil {
		t.Fatalf("带设备号应成功，得到 %v", err)
	}
}

// ---- CurrentDayGrant：据 entitlement_id 精确识别"今天的签到到账" ----
//
// 实测上游给每个包带自解释 ID：
//   checkin_<YYYYMMDD>_<uid>        签到奖励
//   monthly_bonus_<YYYYMM>_<uid>    月初奖励
//   纯数字                          固定福利包
//
// 该识别方式取代了 v0.6.5 的"时间窗+金额"启发式（那套脆弱、易误判）。

func todayYYYYMMDD() string { return time.Now().Format("20060102") }

// 今天的签到包存在 -> 判定已到账，且金额只算签到包。
func TestCurrentDayGrantDetectsCheckinPack(t *testing.T) {
	today := todayYYYYMMDD()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
			{"entitlement_base_info":{"entitlement_id":"367884760578","quota":{"credits_limit":4000}},"usage":{}},
			{"entitlement_base_info":{"entitlement_id":"monthly_bonus_202610_u1","quota":{"credits_limit":500}},"usage":{}},
			{"entitlement_base_info":{"entitlement_id":"checkin_20260930_u1","quota":{"credits_limit":100}},"usage":{}},
			{"entitlement_base_info":{"entitlement_id":"checkin_` + today + `_u1","quota":{"credits_limit":100}},"usage":{}}
		]}`))
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	granted, amt, err := c.CurrentDayGrant(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !granted {
		t.Fatal("存在今天的 checkin_ 包，应判为已到账")
	}
	if amt != 100 {
		t.Fatalf("amount=%v, want 100（只算今天的签到包；4000/500/往日签到都不算）", amt)
	}
}

// 只有月初包、没有今天的签到包 -> 必须判为未到账。
//
// 这是 2026-10-01 的真实 bug：月初 500 包的 start_time 也在今天，
// 旧实现（按时间判定）把它误算成"签到到账 500"。
func TestCurrentDayGrantIgnoresMonthlyBonus(t *testing.T) {
	today := todayYYYYMMDD()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
			{"entitlement_base_info":{"entitlement_id":"367884760578","quota":{"credits_limit":4000}},"usage":{}},
			{"entitlement_base_info":{"entitlement_id":"monthly_bonus_202610_u2","quota":{"credits_limit":500}},"usage":{}},
			{"entitlement_base_info":{"entitlement_id":"checkin_20260930_u2","quota":{"credits_limit":100}},"usage":{}}
		]}`))
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	granted, amt, err := c.CurrentDayGrant(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if granted {
		t.Fatalf("只有月初包+往日签到包，不得判为今天已到账（amt=%v）", amt)
	}
	_ = today
}

// 完全没有今天的包（连月初包都没有）-> 未到账。
func TestCurrentDayGrantMissingWhenNoTodayPack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
			{"entitlement_base_info":{"entitlement_id":"367884760578","quota":{"credits_limit":4000}},"usage":{}},
			{"entitlement_base_info":{"entitlement_id":"checkin_20260928_u3","quota":{"credits_limit":150}},"usage":{}}
		]}`))
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	granted, amt, err := c.CurrentDayGrant(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if granted || amt != 0 {
		t.Fatalf("无今日签到包时应判未到账，得 granted=%t amt=%v", granted, amt)
	}
}

// 兼容性：上游若改掉 entitlement_id，至少不能崩（返回未到账而非 panic）。
func TestCurrentDayGrantHandlesMissingIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[
			{"entitlement_base_info":{"quota":{"credits_limit":100}},"usage":{}},
			{"entitlement_base_info":{},"usage":{}}
		]}`))
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	granted, amt, err := c.CurrentDayGrant(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("缺 entitlement_id 不应报错，err=%v", err)
	}
	if granted || amt != 0 {
		t.Fatalf("无 ID 时保守判未到账，得 granted=%t amt=%v", granted, amt)
	}
}
