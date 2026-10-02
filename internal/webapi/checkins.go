package webapi

import (
	"net/http"
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
	svc   *wnflb.Service
	times string
}

func (m *wnflbModule) ID() string   { return "wnflb" }
func (m *wnflbModule) Name() string { return "福利吧" }
func (m *wnflbModule) Desc() string { return "福利吧论坛（wnflb2023.com）每日签到" }

func (m *wnflbModule) Summary() checkin.Summary {
	username, hasAccount := m.svc.QuickStatus()
	st := m.svc.LoadStatusForAPI()
	loggedIn, points := false, ""
	if hasAccount {
		loggedIn, points = m.svc.HomeStatus()
	}
	next := checkin.NextRun(checkin.ParseTimes(m.times), time.Now())
	nextStr := ""
	if !next.IsZero() {
		nextStr = next.Format("2006-01-02 15:04")
	}
	return checkin.Summary{
		ID:         "wnflb",
		Name:       "福利吧",
		Desc:       "福利吧论坛（wnflb2023.com）每日签到",
		Configured: hasAccount,
		LoggedIn:   loggedIn,
		Username:   username,
		Points:     points,
		PointsName: "积分",
		LastOK:     st.LastCheckinOK,
		LastMsg:    st.LastMsg,
		LastAt:     st.LastCheckinAt,
		NextAt:     nextStr,
		Times:      m.times,
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
