package smzdm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/notify"
)

// credential 存档凭证：Cookie 明文（0600 权限；与项目现有 auth 文件策略一致）。
// Cookie 从 APP 抓包获取，须包含 sess 字段；Android 还需要 smzdm_id + device_id
// 用于动态生成 SK（iPhone 协议不需要 SK）。
type credential struct {
	Cookie  string `json:"cookie"`
	SmzdmID string `json:"smzdm_id"`
	SavedAt string `json:"saved_at"`
}

// status 签到状态持久化。
type status struct {
	LastCheckinAt string `json:"last_checkin_at"`
	LastCheckinOK bool   `json:"last_checkin_ok"`
	LastMsg       string `json:"last_msg"`
	LastGold      int    `json:"last_gold"` // 上次签到获得的金币
}

// Service 是什么值得买签到服务。
type Service struct {
	dir     string
	baseURL string

	mu     sync.Mutex
	client *Client

	notifier *notify.Bark // 签到结果推送（可为 nil 表示不推送）
}

// SetNotifier 设置签到结果推送器（Bark）。
func (s *Service) SetNotifier(b *notify.Bark) { s.notifier = b }

// notifyResult 发送签到结果通知（结果文案已包含本次金币/积分明细）。
func (s *Service) notifyResult(ok bool, msg string) {
	if s.notifier == nil {
		return
	}
	title := "什么值得买签到成功 ✅"
	if !ok {
		title = "什么值得买签到失败 ❌"
	}
	s.notifier.Send("smzdm", title, msg)
}

// New 创建服务。dir 为数据目录（如 <stateDir>/smzdm）。
func New(dir, baseURL string) *Service {
	_ = os.MkdirAll(dir, 0o700)
	if baseURL == "" {
		baseURL = APIBaseURL
	}
	return &Service{dir: dir, baseURL: baseURL}
}

func (s *Service) cookiePath() string { return filepath.Join(s.dir, "cookie.json") }
func (s *Service) statusPath() string { return filepath.Join(s.dir, "status.json") }

// SaveCookie 保存 Cookie（网页提交/环境变量导入时调用）。
func (s *Service) SaveCookie(cookie string) error {
	c, err := NewClient(cookie, s.baseURL)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(credential{
		Cookie:  cookie,
		SmzdmID: c.SmzdmID(),
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
}

// loadCredential 读取存档凭证。
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

// SmzdmID 返回存档账号标识。
func (s *Service) SmzdmID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		return s.client.SmzdmID()
	}
	cred, ok := s.loadCredential()
	if !ok {
		return ""
	}
	return cred.SmzdmID
}

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
	c, err := NewClient(cred.Cookie, s.baseURL)
	if err != nil {
		return nil, err
	}
	s.client = c
	return c, nil
}

var errNoCredential = errorString("未配置 Cookie，请先在网页填入")

type errorString string

func (e errorString) Error() string { return string(e) }

// saveStatus 记录签到结果。
func (s *Service) saveStatus(ok bool, msg string, gold int) {
	raw, _ := json.Marshal(status{
		LastCheckinAt: time.Now().Format("2006-01-02 15:04:05"),
		LastCheckinOK: ok,
		LastMsg:       msg,
		LastGold:      gold,
	})
	_ = os.WriteFile(s.statusPath(), raw, 0o600)
}

// loadStatus 读取签到状态。
func (s *Service) loadStatus() status {
	raw, err := os.ReadFile(s.statusPath())
	if err != nil {
		return status{}
	}
	var st status
	_ = json.Unmarshal(raw, &st)
	return st
}

// AutoCheckin 定时/手动触发的完整签到流程，返回人类可读结果。
func (s *Service) AutoCheckin() string {
	c, err := s.getClient()
	if err != nil {
		msg := "未配置 Cookie，请先在网页填入"
		s.saveStatus(false, msg, 0)
		s.notifyResult(false, msg)
		return msg
	}
	res, err := PerformDailyCheckin(c)
	if err != nil {
		msg := "签到失败：" + err.Error()
		s.saveStatus(false, msg, 0)
		s.notifyResult(false, msg)
		return msg
	}
	parts := res.Summary()
	summary := parts
	if reward := FetchNormalReward(c); reward != "" {
		summary += "；奖励：" + reward
	}
	if claimed, err := ClaimExtraReward(c); err != nil {
		summary += "；额外奖励领取失败：" + err.Error()
	} else if claimed {
		summary += "；连续签到额外奖励已领取"
	}
	s.saveStatus(true, summary, res.GoldEarned)
	s.notifyResult(true, summary)
	return summary
}
