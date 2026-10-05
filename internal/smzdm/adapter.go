package smzdm

import (
	"fmt"
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

func (a *Adapter) ID() string   { return "smzdm" }
func (a *Adapter) Name() string { return "什么值得买" }
func (a *Adapter) Desc() string { return "什么值得买（smzdm.com）APP 每日签到" }

func (a *Adapter) Summary() checkin.Summary {
	st := a.svc.loadStatus()
	cfg := a.svc.Configured()
	points := ""
	if st.LastGold > 0 {
		points = fmt.Sprintf("%d", st.LastGold)
	}
	detail := ""
	if st.LastDays > 0 {
		detail = fmt.Sprintf("连签第%d天", st.LastDays)
		if st.LastPoints > 0 {
			detail += fmt.Sprintf(" · 总积分%d", st.LastPoints)
		}
	}
	var times []string
	if a.timesFn != nil {
		times = a.timesFn()
	}
	next := checkin.NextRun(times, time.Now())
	nextStr := ""
	if !next.IsZero() {
		nextStr = next.Format("2006-01-02 15:04")
	}
	return checkin.Summary{
		ID:         "smzdm",
		Name:       "什么值得买",
		Desc:       "什么值得买（smzdm.com）APP 每日签到",
		Configured: cfg,
		LoggedIn:   cfg, // Cookie 即登录态
		Username:   a.svc.SmzdmID(),
		Points:     points,
		PointsName: "金币",
		Detail:     detail,
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
