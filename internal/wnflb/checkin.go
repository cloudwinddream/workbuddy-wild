package wnflb

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"
)

// Checkin 执行一次签到。要求已登录；未登录返回错误。
// 返回 (是否成功, 结果文案)。
func (s *Service) Checkin() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	logged, html := s.checkLoggedIn()
	if !logged {
		return false, "未登录，请先登录"
	}
	if alreadySigned(html) {
		return true, "今日已签到，无需重复操作"
	}
	a, b := extractCheckinFormhash(html)
	if a == "" {
		return false, "无法提取签到参数（页面结构可能变化）"
	}
	checkURL := fmt.Sprintf("%s/plugin.php?id=fx_checkin:checkin&formhash=%s&%s&inajax=1", s.baseURL, a, b)
	text, err := s.client.getText(checkURL, map[string]string{
		"Referer":          s.forumURL(),
		"X-Requested-With": "XMLHttpRequest",
	})
	if err != nil {
		return false, "签到请求失败: " + err.Error()
	}
	res := parseCheckinResult(text)
	if res.OK && res.Gain != "" {
		res.Msg += " · 本次积分+" + res.Gain
	}
	log.Printf("wnflb: 签到结果 ok=%v msg=%s", res.OK, res.Msg)
	return res.OK, res.Msg
}

// Credits 查询当前积分。未登录返回 ""。
func (s *Service) Credits() string {
	return s.HomeStatus().Credits
}

// probeTimeout 状态探测的网络超时：论坛对某些 IP 响应很慢，
// 探测必须有短超时兜底，不能拖住签到中心列表接口。
// （探测最多三次请求，只在后台执行，故给到 20 秒。）
var probeTimeout = 20 * time.Second

// probeStale 探测缓存过期阈值：超过后 Summary 触发后台刷新（不阻塞当次请求）。
const probeStale = 2 * time.Minute

// ProbeData 一次状态探测的结果（卡片展示与 Bark 推送共用）。
type ProbeData struct {
	LoggedIn bool
	Group    string // 用户等级/用户组，如 Lv.8金别福禄娃
	Credits  string // 积分
	Coins    string // 金币
	Streak   int    // 连续签到天数（0 = 未知）
	Total    int    // 累计签到天数（0 = 未知）
}

// HomeStatus 实时探测：登录态 + 等级/积分/金币/连续·累计签到天数，
// 并回写缓存与状态文件。只供签到后通知等后台场景；状态接口请用
// CachedHomeStatus。采集顺序：论坛首页 → 签到列表页（天数/UID）→
// 个人空间页（等级/金币），缺什么补什么。
func (s *Service) HomeStatus() ProbeData {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	logged, html := s.checkLoggedInCtx(ctx)
	if !logged {
		d := ProbeData{}
		s.storeProbe(d)
		return d
	}
	d := ProbeData{LoggedIn: true}
	d.Credits = parseCredits(html)
	d.Group = parseUserGroup(html)
	d.Coins = parseCoins(html)
	uid := extractUID(html)
	// 签到列表页：连续/累计签到天数（顺带补 UID 与积分）。
	if page, err := s.client.getTextCtx(ctx, s.checkinListURL(), nil); err != nil {
		log.Printf("wnflb: 积分探测：签到列表页抓取失败：%v", err)
	} else {
		d.Streak, d.Total = parseCheckinStats(page)
		if d.Credits == "" {
			d.Credits = parseCredits(page)
		}
		if uid == "" {
			uid = extractUID(page)
		}
	}
	// 个人空间页：等级/金币/积分的统计信息块。
	if uid != "" && (d.Group == "" || d.Coins == "" || d.Credits == "") {
		if page, err := s.client.getTextCtx(ctx, s.spaceURL(uid), nil); err != nil {
			log.Printf("wnflb: 积分探测：个人空间页抓取失败：%v", err)
		} else {
			if d.Group == "" {
				d.Group = parseUserGroup(page)
			}
			if d.Credits == "" {
				d.Credits = parseCredits(page)
			}
			if d.Coins == "" {
				d.Coins = parseCoins(page)
			}
		}
	}
	log.Printf("wnflb: 积分探测：积分=%q 金币=%q 等级=%q 连续=%d 累计=%d",
		d.Credits, d.Coins, d.Group, d.Streak, d.Total)
	s.storeProbe(d)
	return d
}

// checkinListURL 签到列表页地址（插件页，带签到天数统计）。
func (s *Service) checkinListURL() string {
	return s.baseURL + "/plugin.php?id=fx_checkin:list"
}

// spaceURL 个人空间页地址（统计信息块含等级/积分/金币）。
func (s *Service) spaceURL(uid string) string {
	return s.baseURL + "/space-uid-" + uid + ".html"
}

// storeProbe 更新内存缓存并落盘探测结果。
func (s *Service) storeProbe(d ProbeData) {
	s.probeMu.Lock()
	s.probe, s.probeAt = d, time.Now()
	s.probeMu.Unlock()
	s.saveProbe(d)
}

// CachedHomeStatus 只读缓存的探测结果，绝不打网络（签到中心列表用）。
// 缓存过期时后台异步刷新一次（单飞），当次仍立即返回旧值；
// 冷启动先用 status.json 里上次落盘的探测结果顶上。
func (s *Service) CachedHomeStatus() ProbeData {
	s.probeMu.Lock()
	d, at := s.probe, s.probeAt
	s.probeMu.Unlock()
	if at.IsZero() {
		st := s.loadStatus()
		d = ProbeData{
			LoggedIn: st.LoggedIn, Group: st.Group, Credits: st.Credits,
			Coins: st.Coins, Streak: st.Streak, Total: st.Total,
		}
	}
	if time.Since(at) > probeStale {
		s.refreshHomeStatusAsync()
	}
	return d
}

// refreshHomeStatusAsync 后台刷新探测缓存（同时只跑一个）。
func (s *Service) refreshHomeStatusAsync() {
	if !atomic.CompareAndSwapInt32(&s.probing, 0, 1) {
		return
	}
	go func() {
		defer atomic.StoreInt32(&s.probing, 0)
		s.HomeStatus()
	}()
}

// EnsureLoggedIn 确保登录态：已有 Cookie 则直接用；否则用存档账号
// 自动登录。自动登录触发验证码时返回 ErrCaptchaRequired（需手动登录一次）。
func (s *Service) EnsureLoggedIn() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if logged, _ := s.checkLoggedIn(); logged {
		return nil
	}
	acc, ok := s.loadAccount()
	if !ok || acc.Username == "" {
		return fmt.Errorf("未登录且无存档账号，请先在网页登录")
	}
	// 内联登录流程（避免与 s.Login 的锁重入）
	log.Printf("wnflb: Cookie 失效，用存档账号自动登录 username=%s", acc.Username)
	_, formhash, loginhash, err := s.fetchLoginPage()
	if err != nil {
		return fmt.Errorf("获取登录页失败: %w", err)
	}
	if formhash == "" || loginhash == "" {
		return fmt.Errorf("无法解析登录页")
	}
	respHTML, err := s.submitLogin(formhash, loginhash, acc.Username, acc.Password)
	if err != nil {
		return err
	}
	if ch := detectChallenge(respHTML); ch != nil {
		return fmt.Errorf("%w：自动登录触发验证码，请在网页手动登录一次", ErrCaptchaRequired)
	}
	if msg, ok := checkLoginResult(s, respHTML); ok {
		return nil
	} else if msg != "" {
		return fmt.Errorf("%s", msg)
	}
	return fmt.Errorf("自动登录失败（未进入登录态）")
}

// runCheckin 统一签到流程：确保登录 → 签到 → 保存状态 → 推送通知。
func (s *Service) runCheckin() (bool, string) {
	if err := s.EnsureLoggedIn(); err != nil {
		msg := "签到跳过: " + err.Error()
		log.Printf("wnflb: %s", msg)
		s.saveStatus(false, msg)
		s.notifyResult(false, msg)
		return false, msg
	}
	ok, msg := s.Checkin()
	s.saveStatus(ok, msg)
	s.notifyResult(ok, msg)
	return ok, msg
}

// AutoCheckin 定时任务入口：确保登录后签到，并更新状态文件与通知。
// 返回人类可读的结果文案（成功/失败都记录）。
func (s *Service) AutoCheckin() string {
	_, msg := s.runCheckin()
	return msg
}

// ManualCheckin 管理页手动触发，与定时任务同一流程（含通知）。
func (s *Service) ManualCheckin() (bool, string) { return s.runCheckin() }

// QuickStatus 轻量状态（不请求论坛）：用于调度器日志与状态文件回显。
func (s *Service) QuickStatus() (username string, hasAccount bool) {
	if acc, ok := s.loadAccount(); ok {
		return acc.Username, true
	}
	return "", false
}
