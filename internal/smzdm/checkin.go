package smzdm

import (
	"fmt"
	"strings"
)

// CheckinResult 每日签到结果。
type CheckinResult struct {
	ConsecutiveDays int // daily_num：连续签到天数
	GoldEarned      int // cgold：本次获得金币
	PointsEarned    int // cpoints：本次获得积分
	ExpEarned       int // cexperience：本次获得经验
	AccountRank     int // rank：等级
	MakeupCards     int // cards：补签卡
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		var i int
		_, _ = fmt.Sscanf(fmt.Sprintf("%v", v), "%d", &i)
		return i
	}
}

func parseCheckinResult(data map[string]any) CheckinResult {
	return CheckinResult{
		ConsecutiveDays: toInt(data["daily_num"]),
		GoldEarned:      toInt(data["cgold"]),
		PointsEarned:    toInt(data["cpoints"]),
		ExpEarned:       toInt(data["cexperience"]),
		AccountRank:     toInt(data["rank"]),
		MakeupCards:     toInt(data["cards"]),
	}
}

// Summary 生成签到结果摘要。
func (r CheckinResult) Summary() string {
	return fmt.Sprintf("签到成功 · 第%d天 · +%d金币 +%d积分 +%d经验",
		r.ConsecutiveDays, r.GoldEarned, r.PointsEarned, r.ExpEarned)
}

// PerformDailyCheckin 执行每日签到。
func PerformDailyCheckin(c *Client) (CheckinResult, error) {
	data, err := c.Post("/checkin", nil)
	if err != nil {
		return CheckinResult{}, err
	}
	return parseCheckinResult(data), nil
}

// FetchNormalReward 获取今日签到普通奖励（尽力而为）。
func FetchNormalReward(c *Client) string {
	data, err := c.Post("/checkin/all_reward", nil)
	if err != nil {
		return ""
	}
	gift, _ := data["normal_reward"].(map[string]any)["gift"].(map[string]any)
	if gift == nil {
		if nr, ok := data["normal_reward"].(map[string]any); ok {
			gift, _ = nr["gift"].(map[string]any)
		}
	}
	if gift == nil {
		return ""
	}
	title, _ := gift["title"].(string)
	content, _ := gift["content_str"].(string)
	if title == "" {
		title = content
	}
	return strings.TrimSpace(title)
}

// ClaimExtraReward 领取连续签到额外奖励；无可领奖励时返回 false。
func ClaimExtraReward(c *Client) (bool, error) {
	extra := map[string]string{}
	if c.IsIPhone() {
		extra["is_install_zhangdama"] = "0"
	}
	data, err := c.Post("/checkin/show_view_v2", extra)
	if err != nil {
		return false, nil // 尽力而为：查不到就不领
	}
	rows, _ := data["rows"].([]any)
	for _, row := range rows {
		rm, _ := row.(map[string]any)
		if rm == nil {
			continue
		}
		if fmt.Sprintf("%v", rm["cell_type"]) != "18001" {
			continue
		}
		cellData, _ := rm["cell_data"].(map[string]any)
		cc, _ := cellData["checkin_continue"].(map[string]any)
		if cc == nil {
			continue
		}
		if show, _ := cc["continue_checkin_reward_show"].(bool); show {
			if _, err := c.Post("/checkin/extra_reward", nil); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// RunCheckinFlow 执行完整签到流程：签到 → 普通奖励 → 连续签到额外奖励。
// 返回人类可读的结果摘要。
func RunCheckinFlow(c *Client) (string, error) {
	res, err := PerformDailyCheckin(c)
	if err != nil {
		return "", err
	}
	parts := []string{res.Summary()}
	if reward := FetchNormalReward(c); reward != "" {
		parts = append(parts, "奖励："+reward)
	}
	if claimed, err := ClaimExtraReward(c); err != nil {
		parts = append(parts, "额外奖励领取失败："+err.Error())
	} else if claimed {
		parts = append(parts, "连续签到额外奖励已领取")
	}
	return strings.Join(parts, "；"), nil
}
