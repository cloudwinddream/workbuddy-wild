package traework

import (
	"encoding/json"
	"testing"
)

// 直接验证 shape 日志能展开嵌套结构（这是修复后定位真实字段名的关键手段）
func TestLogResourceShapeExpandsNested(t *testing.T) {
	body := `{"user_entitlement_pack_list":[
		{"entitlement_base_info":{"quota":{"credits_limit":4000,"credits_used":3690},"pack_name":"daily"}},
		{"entitlement_base_info":{"quota":{"credits_limit":500,"credits_remain":310}}}
	]}`
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	pairs := make([]string, 0, 16)
	collectShape(raw, "", &pairs, 0)

	joined := ""
	for _, p := range pairs {
		joined += p + " "
	}
	t.Logf("shape: %s", joined)

	// 必须能看到嵌套到 quota 层的字段名与数值
	// 注意路径前缀是完整键名（如 user_entitlement_pack_list[0]....）
	for _, want := range []string{
		"user_entitlement_pack_list.len=2",
		"user_entitlement_pack_list[0].entitlement_base_info.quota.credits_limit=4000",
		"user_entitlement_pack_list[0].entitlement_base_info.quota.credits_used=3690",
		"user_entitlement_pack_list[0].entitlement_base_info.pack_name=daily",
		"user_entitlement_pack_list[1].entitlement_base_info.quota.credits_remain=310",
	} {
		found := false
		for _, p := range pairs {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("shape 缺少 %q\n实际: %s", want, joined)
		}
	}
}

// 解析路径也验证一遍：直接调纯函数，绕开 HTTP
func TestSumRemainFromEntitlementsDirect(t *testing.T) {
	body := `{"user_entitlement_pack_list":[
		{"entitlement_base_info":{"quota":{"credits_limit":4000,"credits_used":3690}}},
		{"entitlement_base_info":{"quota":{"credits_limit":500,"credits_remain":210}}}
	]}`
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	got, ok := sumRemainFromEntitlements(raw)
	if !ok {
		t.Fatal("sumRemainFromEntitlements 未命中任何包")
	}
	// 包0: 4000-3690=310；包1: 明确 remain=210 → 合计 520
	if got != 520 {
		t.Fatalf("remain=%d want 520 (310+210)", got)
	}
}

// 关键回归：字段名不含 "remain" 时，绝不能把 credits_limit 当余额。
// 这是「显示 4050 但实际 310」的根因场景。
// 语义变更（v0.5.3）：只有 credits_limit、没有 usage 时，
// 表示该包**完全未使用**，剩余 = 上限（而不是"拒绝计算"）。
//
// 依据：真实响应中最新签到的权益包 usage 为 `{}`，若判为无法计算会漏算额度。
// 但要警惕的是**纯上限累加**（那是 v0.5.1 的病根）——区别在于
// 这里每个包都参与了 "上限 - 已用(0)" 的计算，而不是把上限直接当余额求和。
func TestSumRemainTreatsBareLimitAsUnused(t *testing.T) {
	body := `{"user_entitlement_pack_list":[
		{"entitlement_base_info":{"quota":{"credits_limit":4000}}},
		{"entitlement_base_info":{"quota":{"credits_limit":500}}}
	]}`
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	got, ok := sumRemainFromEntitlements(raw)
	if !ok {
		t.Fatal("缺 usage 应按未使用处理（ok=true）")
	}
	if got != 4500 {
		t.Fatalf("remain=%d want 4500（两包均未使用）", got)
	}
}

// 但**完全没有任何余额线索**（既无上限也无 usage）时必须拒绝，
// 避免把无关字段当成余额（v0.5.2 曾误用 checkin credits=150）。
func TestSumRemainRefusesNoBalanceInfo(t *testing.T) {
	body := `{"user_entitlement_pack_list":[
		{"entitlement_base_info":{"quota":{"enable_solo_agent":true}},"usage":{}},
		{"entitlement_base_info":{"quota":{"no_bonus_quota":true}}}
	]}`
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	got, ok := sumRemainFromEntitlements(raw)
	if ok || got != 0 {
		t.Fatalf("无任何余额字段时必须拒绝（ok=false），实际 got=%d ok=%v", got, ok)
	}
}
