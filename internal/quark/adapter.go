package quark

import (
	"strings"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/checkin"
)

// Adapter 把 Service 接入签到中心注册表。
type Adapter struct {
	svc     *Service
	timesFn func() []string // 统一签到时间提供者（与主账号共用一套）
}

// NewAdapter 创建签到中心适配器。timesFn 提供统一签到时间（可为 nil）。
func NewAdapter(svc *Service, timesFn func() []string) *Adapter {
	return &Adapter{svc: svc, timesFn: timesFn}
}

func (a *Adapter) ID() string   { return "quark" }
func (a *Adapter) Name() string { return "夸克网盘" }
func (a *Adapter) Desc() string { return "夸克网盘（quark.cn）每日签到领空间" }

func (a *Adapter) Summary() checkin.Summary {
	st := a.svc.loadStatus()
	cfg := a.svc.Configured()
	var times []string
	if a.timesFn != nil {
		times = a.timesFn()
	}
	next := checkin.NextRun(times, time.Now())
	nextStr := ""
	if !next.IsZero() {
		nextStr = next.Format("2006-01-02 15:04")
	}
	p := st.Probe
	// 已配置但缓存为空/过期时，触发后台刷新（当次先返回旧值）。
	if cfg {
		p = a.svc.CachedProbe()
	}
	points := ""
	if p.Total > 0 {
		points = FormatBytes(p.Total)
	}
	return checkin.Summary{
		ID:         "quark",
		Name:       "夸克网盘",
		Desc:       "夸克网盘（quark.cn）每日签到领空间",
		Configured: cfg,
		LoggedIn:   cfg && (p.Total > 0 || p.Nickname != ""),
		Username:   p.Nickname,
		Points:     points,
		PointsName: "总空间",
		Detail:     p.Detail(),
		LastOK:     st.LastCheckinOK,
		LastMsg:    st.LastMsg,
		LastAt:     st.LastCheckinAt,
		NextAt:     nextStr,
		Times:      strings.Join(times, ","),
	}
}

func (a *Adapter) RunNow() (bool, string) {
	msg := a.svc.AutoCheckin()
	st := a.svc.loadStatus()
	return st.LastCheckinOK, msg
}
