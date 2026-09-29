package traework

import (
	"encoding/json"
	"testing"
)

// 用**真实抓包响应**验证积分口径。
//
// 数据来源：2026-09-29 用户在 www.trae.cn 抓到的
// `POST /trae/api/v2/pay/user_current_entitlement_list` 响应（已精简为关键字段）。
//
// 官网个人中心显示剩余 ≈ 310，响应里：
//
//	usage_summary.total_amount    = 4050
//	usage_summary.consumed_amount = 3751.3
//	4050 - 3751.3 = 298.7
//
// 这是**唯一权威口径**。此前 v0.5.1/v0.5.2 用其它字段（credits_limit 累加、
// checkin credits）导致显示 4050 / 150，均错误。
const realEntitlementListJSON = `{
  "is_credits_billing": true,
  "usage_summary": {
    "consumed_amount": 3751.3,
    "consumption_ratio": 0.9262469135802469,
    "total_amount": 4050
  },
  "user_entitlement_pack_list": [
    {"display_desc":"免费","entitlement_base_info":{"quota":{"no_bonus_quota":true}},"usage":{}},
    {"display_desc":"每月登录赠送","group_name":"每月登录积分",
     "entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":500}},
    {"display_desc":"签到奖励","group_name":"每日签到",
     "entitlement_base_info":{"quota":{"credits_limit":200}},"usage":{"credits_amount":200}},
    {"display_desc":"签到奖励","group_name":"每日签到",
     "entitlement_base_info":{"quota":{"credits_limit":150}},"usage":{"credits_amount":150}},
    {"display_desc":"签到奖励","group_name":"每日签到",
     "entitlement_base_info":{"quota":{"credits_limit":150}},"usage":{"credits_amount":1.304}},
    {"display_desc":"签到奖励","group_name":"每日签到",
     "entitlement_base_info":{"quota":{"credits_limit":150}},"usage":{}}
  ]
}`

// 核心回归：必须走 usage_summary，返回 299（298.7 四舍五入）。
func TestRealResponseUsesUsageSummary(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(realEntitlementListJSON), &raw); err != nil {
		t.Fatal(err)
	}
	total, consumed, ok := parseUsageSummary(raw)
	if !ok {
		t.Fatal("未能解析 usage_summary")
	}
	if total != 4050 {
		t.Errorf("total=%v want 4050", total)
	}
	if consumed != 3751.3 {
		t.Errorf("consumed=%v want 3751.3", consumed)
	}
	remain := total - consumed
	if want := 298.7; remain < want-0.01 || remain > want+0.01 {
		t.Errorf("remain=%v want %v", remain, want)
	}
}

// 逐包计算也应与 usage_summary 一致（互为佐证）。
//
//	500-500 + 200-200 + 150-150 + 150-1.304 + 150-0 = 298.696 ≈ 298.7
//
// 关键点：`usage:{}` 的包必须按**已用 0** 计入（剩余 150），
// 不能因"usage 为空"被当成解析失败而跳过，否则会少算 150。
func TestRealResponsePackSumMatchesSummary(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(realEntitlementListJSON), &raw); err != nil {
		t.Fatal(err)
	}
	got, ok := sumRemainFromEntitlements(raw)
	if !ok {
		t.Fatal("逐包求和无命中")
	}
	// 500-500=0, 200-200=0, 150-150=0, 150-1.304=148.696, 150-0=150
	// （"免费"包无 credits_limit，跳过）
	// 合计 298.696 → int64 截断为 298
	if got < 298 || got > 299 {
		t.Errorf("逐包求和=%d want ≈298", got)
	}
}

// 空 usage 必须按"未使用"处理（剩余 = 上限），而不是跳过。
func TestEmptyUsageMeansUnused(t *testing.T) {
	packs := []map[string]any{
		{"entitlement_base_info": map[string]any{"quota": map[string]any{"credits_limit": float64(150)}},
			"usage": map[string]any{}},
	}
	got := extractBalance(packs[0])
	if got == nil {
		t.Fatal("usage 为空时应按未使用处理，返回剩余 150，而不是 nil")
	}
	if *got != 150 {
		t.Errorf("remain=%d want 150", *got)
	}
}

// 无 credits_limit 的包（如"免费"包）应返回 nil，不参与计算。
func TestPackWithoutLimitSkipped(t *testing.T) {
	pack := map[string]any{
		"entitlement_base_info": map[string]any{"quota": map[string]any{"no_bonus_quota": true}},
		"usage":                 map[string]any{},
	}
	if got := extractBalance(pack); got != nil {
		t.Errorf("无额度上限的包应跳过，得到 %d", *got)
	}
}
