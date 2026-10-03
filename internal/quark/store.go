package quark

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/notify"
)

// credential 存档凭证：整段 Cookie 明文（0600；与项目现有凭证策略一致）。
type credential struct {
	Cookie  string `json:"cookie"`
	SavedAt string `json:"saved_at"`
}

// Probe 缓存的账号/容量信息（卡片展示与 Bark 推送共用）。
type Probe struct {
	Nickname  string `json:"nickname"`
	Member    string `json:"member"`
	Total     int64  `json:"total"`      // 总容量字节
	Used      int64  `json:"used"`       // 已用字节（接口未给时为 0）
	AccReward int64  `json:"acc_reward"` // 签到累计获得字节
	Progress  int    `json:"progress"`   // 连签进度
	Target    int    `json:"target"`     // 连签目标
	Signed    bool   `json:"signed"`     // 今日是否已签到
	Reward    int64  `json:"reward"`     // 今日签到奖励字节
	ProbedAt  string `json:"probed_at"`
}

// status 签到状态 + 探测缓存持久化。
type status struct {
	LastCheckinAt string `json:"last_checkin_at"`
	LastCheckinOK bool   `json:"last_checkin_ok"`
	LastMsg       string `json:"last_msg"`
	Probe         Probe  `json:"probe"`
}

// Detail 卡片附加信息行（签到累计 · 连签进度 · 已用）。
func (p Probe) Detail() string {
	var parts []string
	if p.AccReward > 0 {
		parts = append(parts, "签到累计 "+FormatBytes(p.AccReward))
	}
	if p.Target > 0 {
		parts = append(parts, fmt.Sprintf("连签进度 %d/%d", p.Progress, p.Target))
	}
	if p.Used > 0 {
		parts = append(parts, "已用 "+FormatBytes(p.Used))
	}
	return strings.Join(parts, " · ")
}

// Service 夸克网盘签到服务。
type Service struct {
	dir         string
	growthBase  string
	accountBase string

	mu       sync.Mutex
	client   *Client
	probing  int32 // 原子标志：后台探测进行中（单飞）
	notifier *notify.Bark
}

// New 创建服务。dir 为数据目录（如 <stateDir>/quark）；
// baseURL 非空时同时覆盖成长/账号接口域名（测试注入）。
func New(dir, baseURL string) *Service {
	_ = os.MkdirAll(dir, 0o700)
	s := &Service{dir: dir}
	if baseURL != "" {
		s.growthBase, s.accountBase = baseURL, baseURL
	}
	return s
}

// SetNotifier 设置签到结果推送器（Bark）。
func (s *Service) SetNotifier(b *notify.Bark) { s.notifier = b }

func (s *Service) cookiePath() string { return filepath.Join(s.dir, "cookie.json") }
func (s *Service) statusPath() string { return filepath.Join(s.dir, "status.json") }

// SaveCookie 保存 Cookie（网页提交/环境变量导入时调用，构造校验）。
func (s *Service) SaveCookie(cookie string) error {
	c, err := NewClient(cookie, s.growthBase, s.accountBase)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(credential{
		Cookie:  strings.TrimSpace(cookie),
		SavedAt: time.Now().Format("2006-01-02 15:04:05"),
	})
	if err := os.WriteFile(s.cookiePath(), raw, 0o600); err != nil {
		return err
	}
	s.mu.Lock()
	s.client = c
	s.mu.Unlock()
	return nil
}

// ImportCookieFromEnv 从环境变量导入（仅当尚未存档时）。
func (s *Service) ImportCookieFromEnv(cookie string) bool {
	if cookie == "" {
		return false
	}
	if _, err := os.Stat(s.cookiePath()); err == nil {
		return false
	}
	return s.SaveCookie(cookie) == nil
}

// ClearCookie 删除存档 Cookie（退出时调用）。
func (s *Service) ClearCookie() {
	s.mu.Lock()
	s.client = nil
	s.mu.Unlock()
	_ = os.Remove(s.cookiePath())
	raw, _ := json.Marshal(status{})
	_ = os.WriteFile(s.statusPath(), raw, 0o600)
}

func (s *Service) loadCredential() (credential, bool) {
	raw, err := os.ReadFile(s.cookiePath())
	if err != nil {
		return credential{}, false
	}
	var cred credential
	if err := json.Unmarshal(raw, &cred); err != nil || cred.Cookie == "" {
		return credential{}, false
	}
	return cred, true
}

// Configured 是否已配置 Cookie。
func (s *Service) Configured() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		return true
	}
	_, ok := s.loadCredential()
	return ok
}

var errNoCredential = errors.New("未配置 Cookie，请先在网页填入")

// getClient 获取可用客户端（内存缓存 → 存档重建）。
func (s *Service) getClient() (*Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		return s.client, nil
	}
	cred, ok := s.loadCredential()
	if !ok {
		return nil, errNoCredential
	}
	c, err := NewClient(cred.Cookie, s.growthBase, s.accountBase)
	if err != nil {
		return nil, err
	}
	s.client = c
	return c, nil
}

// saveStatus 记录签到结果（保留探测缓存）。
func (s *Service) saveStatus(ok bool, msg string) {
	st := s.loadStatus()
	st.LastCheckinAt = time.Now().Format("2006-01-02 15:04:05")
	st.LastCheckinOK = ok
	st.LastMsg = msg
	s.writeStatus(st)
}

// loadStatus 读取状态（含探测缓存）。
func (s *Service) loadStatus() status {
	raw, err := os.ReadFile(s.statusPath())
	if err != nil {
		return status{}
	}
	var st status
	_ = json.Unmarshal(raw, &st)
	return st
}

func (s *Service) writeStatus(st status) {
	raw, _ := json.Marshal(st)
	_ = os.WriteFile(s.statusPath(), raw, 0o600)
}

// probeStale 探测缓存过期阈值：超过后 Summary 触发后台刷新。
const probeStale = 2 * time.Minute

// CachedProbe 只读缓存的探测结果（签到中心列表用，绝不打网络）；
// 过期时后台异步刷新一次（单飞），当次仍立即返回旧值。
func (s *Service) CachedProbe() Probe {
	st := s.loadStatus()
	if st.Probe.ProbedAt != "" {
		if t, err := time.Parse("2006-01-02 15:04:05", st.Probe.ProbedAt); err == nil &&
			time.Since(t) > probeStale {
			s.refreshProbeAsync()
		}
	} else if s.Configured() {
		s.refreshProbeAsync()
	}
	return st.Probe
}

// refreshProbeAsync 后台刷新探测缓存（同时只跑一个）。
func (s *Service) refreshProbeAsync() {
	if !atomic.CompareAndSwapInt32(&s.probing, 0, 1) {
		return
	}
	go func() {
		defer atomic.StoreInt32(&s.probing, 0)
		_, _ = s.refreshProbe()
	}()
}

// refreshProbe 实时探测（成长信息 + 昵称），成功后落盘并返回缓存。
func (s *Service) refreshProbe() (Probe, error) {
	c, err := s.getClient()
	if err != nil {
		return Probe{}, err
	}
	info, err := c.GrowthInfo()
	if err != nil {
		return Probe{}, err
	}
	p := Probe{
		Nickname:  c.Nickname(),
		Member:    info.MemberLabel(),
		Total:     info.TotalCapacity,
		Used:      info.UseCapacity,
		AccReward: info.CapComposition.SignReward,
		Progress:  info.CapSign.SignProgress,
		Target:    info.CapSign.SignTarget,
		Signed:    info.CapSign.SignDaily,
		Reward:    info.CapSign.SignDailyReward,
		ProbedAt:  time.Now().Format("2006-01-02 15:04:05"),
	}
	st := s.loadStatus()
	st.Probe = p
	s.writeStatus(st)
	return p, nil
}

// ProbeNow 立即同步探测一次（保存 Cookie 后验证用）。
func (s *Service) ProbeNow() (Probe, error) { return s.refreshProbe() }

// AutoCheckin 定时/手动触发的完整签到流程，返回人类可读结果。
func (s *Service) AutoCheckin() string {
	c, err := s.getClient()
	if err != nil {
		msg := errNoCredential.Error()
		s.saveStatus(false, msg)
		s.notifyResult(false, msg, Probe{})
		return msg
	}
	info, err := c.GrowthInfo()
	if err != nil {
		msg := "签到失败：" + err.Error()
		s.saveStatus(false, msg)
		s.notifyResult(false, msg, Probe{})
		return msg
	}
	var msg string
	ok := true
	if info.CapSign.SignDaily {
		msg = fmt.Sprintf("今日已签到 +%s，连签进度（%d/%d）",
			FormatBytes(info.CapSign.SignDailyReward),
			info.CapSign.SignProgress, info.CapSign.SignTarget)
	} else {
		reward, serr := c.Sign()
		if serr != nil {
			msg = "签到失败：" + serr.Error()
			ok = false
		} else {
			msg = fmt.Sprintf("签到成功 +%s，连签进度（%d/%d）",
				FormatBytes(reward),
				info.CapSign.SignProgress+1, info.CapSign.SignTarget)
			// 签到后容量/进度可能变化，刷新一次探测缓存。
			if fresh, ferr := c.GrowthInfo(); ferr == nil {
				info = fresh
			}
		}
	}
	// 落盘探测缓存（卡片与推送共用同一份新鲜数据）。
	p := Probe{
		Nickname:  c.Nickname(),
		Member:    info.MemberLabel(),
		Total:     info.TotalCapacity,
		Used:      info.UseCapacity,
		AccReward: info.CapComposition.SignReward,
		Progress:  info.CapSign.SignProgress,
		Target:    info.CapSign.SignTarget,
		Signed:    true,
		Reward:    info.CapSign.SignDailyReward,
		ProbedAt:  time.Now().Format("2006-01-02 15:04:05"),
	}
	if !ok {
		p.Signed = info.CapSign.SignDaily
	}
	st := s.loadStatus()
	st.Probe = p
	st.LastCheckinAt = time.Now().Format("2006-01-02 15:04:05")
	st.LastCheckinOK = ok
	st.LastMsg = msg
	s.writeStatus(st)
	s.notifyResult(ok, msg, p)
	return msg
}

// notifyResult 发送签到结果通知：结果文案 + 账号信息行
// （用户名/会员/总空间/已用/签到累计/连签进度）。
func (s *Service) notifyResult(ok bool, msg string, p Probe) {
	if s.notifier == nil {
		return
	}
	title := "夸克网盘签到成功 ✅"
	if !ok {
		title = "夸克网盘签到失败 ❌"
	}
	lines := []string{msg}
	if p.Nickname != "" {
		lines = append(lines, "👤 用户名: "+p.Nickname)
	}
	if p.Member != "" {
		lines = append(lines, "👑 会员: "+p.Member)
	}
	if p.Total > 0 {
		line := "💾 总空间: " + FormatBytes(p.Total)
		if p.Used > 0 {
			line += "（已用 " + FormatBytes(p.Used) + "）"
		}
		lines = append(lines, line)
	}
	if p.AccReward > 0 {
		lines = append(lines, "🎁 签到累计: "+FormatBytes(p.AccReward))
	}
	if p.Target > 0 {
		lines = append(lines, fmt.Sprintf("📅 连签进度: %d/%d", p.Progress, p.Target))
	}
	s.notifier.Send("quark", title, strings.Join(lines, "\n"))
}
