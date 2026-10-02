package webapi

import (
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// 签到中心：主账号（WorkBuddy/TraeWork）统一管理接口。
// 账号签到状态复用主池子数据；Bark 设备按账号单独配置（notify 账号级 key）。
// ---------------------------------------------------------------------------

// GET /api/checkins/accounts → {accounts: [{uid, nickname, group, credits,
// disabled, last_checkin_ok/at/msg, bark_key(脱敏), bark_set}]}
func (a *API) handleCheckinAccounts(w http.ResponseWriter, r *http.Request) {
	views := a.accountViews()
	out := make([]map[string]any, 0, len(views))
	for _, v := range views {
		barkKey, barkSet := "", false
		if a.notify != nil {
			k := a.notify.KeyForAccount(v.Group, v.UID)
			barkSet = k != ""
			if mv, ok := a.notify.View()["accounts"].(map[string]any)[v.Group+":"+v.UID].(map[string]any); ok {
				barkKey, _ = mv["key"].(string)
			}
		}
		out = append(out, map[string]any{
			"uid": v.UID, "nickname": v.Nickname, "group": v.Group,
			"credits": v.Credits, "disabled": v.Disabled,
			"last_checkin_ok": v.LastCheckinOK, "last_checkin_at": v.LastCheckinAt,
			"last_checkin_msg": v.LastCheckinMsg,
			"bark_key":         barkKey, "bark_set": barkSet,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// GET /api/checkins/settings → 统一签到设置（签到时间两平台共用一套配置）。
func (a *API) handleCheckinSettings(w http.ResponseWriter, r *http.Request) {
	next := ""
	if nf := a.nextFire(); !nf.IsZero() {
		next = nf.Format("2006-01-02 15:04")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"checkin_times":      a.checkinTimes(),
		"keepalive_hours":    a.keepaliveHours(),
		"next_fire":          next,
		"catchup_on_startup": a.catchupOnStartup,
		"server_now":         time.Now().Format("2006-01-02 15:04"),
		"retry_policy":       "签到失败（限流）自动延迟重试：10 分钟起递增，单账号每天最多 6 次；服务启动时对已错过签到时刻且今日未签的账号兜底补签",
	})
}
