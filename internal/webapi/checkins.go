package webapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/checkin"
	"github.com/rockswang/workbuddy-wild/internal/wnflb"
)

// ---------------------------------------------------------------------------
// 签到中心（/api/checkins*）：统一展示所有签到模块。
// 每个模块实现 checkin.Module；福利吧是第一个（适配器见下）。
// ---------------------------------------------------------------------------

// wnflbModule 把 wnflb.Service 适配为 checkin.Module。
type wnflbModule struct {
	svc     *wnflb.Service
	timesFn func() []string // 统一签到时间提供者（与主账号共用一套）
}

func (m *wnflbModule) ID() string   { return "wnflb" }
func (m *wnflbModule) Name() string { return "福利吧" }
func (m *wnflbModule) Desc() string { return "福利吧论坛（wnflb2023.com）每日签到" }

// times 当前统一签到时间（展示与下次执行计算用）。
func (m *wnflbModule) times() []string {
	if m.timesFn == nil {
		return nil
	}
	return m.timesFn()
}

func (m *wnflbModule) Summary() checkin.Summary {
	username, hasAccount := m.svc.QuickStatus()
	st := m.svc.LoadStatusForAPI()
	var d wnflb.ProbeData
	if hasAccount {
		// 只读缓存，绝不在列表请求里实时打论坛（论坛慢时会拖死整个页面）；
		// 缓存过期由服务层后台异步刷新，页面自动刷新周期内即可看到新值。
		d = m.svc.CachedHomeStatus()
	}
	times := m.times()
	next := checkin.NextRun(times, time.Now())
	nextStr := ""
	if !next.IsZero() {
		nextStr = next.Format("2006-01-02 15:04")
	}
	// 附加信息行：等级 · 金币 · 连续/累计签到天数（有啥显示啥）。
	var parts []string
	if d.Group != "" {
		parts = append(parts, d.Group)
	}
	if d.Coins != "" {
		parts = append(parts, "金币 "+d.Coins)
	}
	if d.Streak > 0 {
		parts = append(parts, fmt.Sprintf("连续签到 %d 天", d.Streak))
	}
	if d.Total > 0 {
		parts = append(parts, fmt.Sprintf("累计 %d 天", d.Total))
	}
	return checkin.Summary{
		ID:         "wnflb",
		Name:       "福利吧",
		Desc:       "福利吧论坛（wnflb2023.com）每日签到",
		Configured: hasAccount,
		LoggedIn:   d.LoggedIn,
		Username:   username,
		Points:     d.Credits,
		PointsName: "积分",
		Detail:     strings.Join(parts, " · "),
		LastOK:     st.LastCheckinOK,
		LastMsg:    st.LastMsg,
		LastAt:     st.LastCheckinAt,
		NextAt:     nextStr,
		Times:      strings.Join(times, ","),
	}
}

func (m *wnflbModule) RunNow() (bool, string) {
	return m.svc.ManualCheckin()
}

// registerCheckins 注册签到中心路由。
func (a *API) registerCheckins(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/checkins", a.withAuth(a.handleCheckinList))
	mux.HandleFunc("POST /api/checkins/{id}/run", a.withAuth(a.handleCheckinRun))
	mux.HandleFunc("GET /api/checkins/accounts", a.withAuth(a.handleCheckinAccounts))
	mux.HandleFunc("GET /api/checkins/settings", a.withAuth(a.handleCheckinSettings))
}

// GET /api/checkins → {modules: [Summary...]}
func (a *API) handleCheckinList(w http.ResponseWriter, r *http.Request) {
	if a.checkins == nil {
		writeJSON(w, http.StatusOK, map[string]any{"modules": []any{}})
		return
	}
	mods := a.checkins.List()
	out := make([]checkin.Summary, 0, len(mods))
	for _, m := range mods {
		out = append(out, m.Summary())
	}
	writeJSON(w, http.StatusOK, map[string]any{"modules": out})
}

// POST /api/checkins/{id}/run → {ok, msg}
func (a *API) handleCheckinRun(w http.ResponseWriter, r *http.Request) {
	if a.checkins == nil {
		writeErr(w, http.StatusServiceUnavailable, "签到中心未启用")
		return
	}
	m := a.checkins.Get(r.PathValue("id"))
	if m == nil {
		writeErr(w, http.StatusNotFound, "未知的签到模块")
		return
	}
	ok, msg := m.RunNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}
