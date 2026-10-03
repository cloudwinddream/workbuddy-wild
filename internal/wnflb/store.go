package wnflb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// account 存档账号（0600 权限；明文，与项目现有 auth 文件策略一致）。
type account struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// status 签到状态持久化（含最近一次首页探测的登录态与积分，
// 供签到中心列表直接回显，不必每次实时请求论坛）。
type status struct {
	LastCheckinAt string `json:"last_checkin_at"`
	LastCheckinOK bool   `json:"last_checkin_ok"`
	LastMsg       string `json:"last_msg"`
	LoggedIn      bool   `json:"logged_in"`
	Group         string `json:"group"`
	Credits       string `json:"credits"`
	Coins         string `json:"coins"`
	Streak        int    `json:"streak"`
	Total         int    `json:"total"`
	ProbedAt      string `json:"probed_at"`
}

func (s *Service) accountPath() string { return filepath.Join(s.dir, "account.json") }
func (s *Service) statusPath() string  { return filepath.Join(s.dir, "status.json") }

// saveAccount 保存账号（登录成功时调用）。
func (s *Service) saveAccount(username, password string) {
	raw, _ := json.Marshal(account{Username: username, Password: password})
	_ = os.WriteFile(s.accountPath(), raw, 0o600)
}

// loadAccount 读取存档账号。
func (s *Service) loadAccount() (account, bool) {
	raw, err := os.ReadFile(s.accountPath())
	if err != nil {
		return account{}, false
	}
	var acc account
	if err := json.Unmarshal(raw, &acc); err != nil || acc.Username == "" {
		return account{}, false
	}
	return acc, true
}

// clearAccount 删除存档账号（退出登录时调用）。
func (s *Service) clearAccount() {
	_ = os.Remove(s.accountPath())
}

// saveStatus 记录签到结果（合并写入，不覆盖探测字段）。
func (s *Service) saveStatus(ok bool, msg string) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	st := s.loadStatus()
	st.LastCheckinAt = time.Now().Format("2006-01-02 15:04:05")
	st.LastCheckinOK = ok
	st.LastMsg = msg
	s.writeStatus(st)
}

// saveProbe 记录一次状态探测结果（合并写入，不覆盖签到字段）。
func (s *Service) saveProbe(d ProbeData) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	st := s.loadStatus()
	st.LoggedIn = d.LoggedIn
	st.Group = d.Group
	st.Credits = d.Credits
	st.Coins = d.Coins
	st.Streak = d.Streak
	st.Total = d.Total
	st.ProbedAt = time.Now().Format("2006-01-02 15:04:05")
	s.writeStatus(st)
}

// writeStatus 落盘（调用方须持 statusMu）。
func (s *Service) writeStatus(st status) {
	raw, _ := json.Marshal(st)
	_ = os.WriteFile(s.statusPath(), raw, 0o600)
}

// loadStatus 读取签到状态。
func (s *Service) loadStatus() status {
	var st status
	raw, err := os.ReadFile(s.statusPath())
	if err != nil {
		return st
	}
	_ = json.Unmarshal(raw, &st)
	return st
}

// LoadStatusForAPI 给 webapi 用的状态读取。
func (s *Service) LoadStatusForAPI() status { return s.loadStatus() }

// SaveStatusForAPI 给 webapi 用的状态写入。
func (s *Service) SaveStatusForAPI(ok bool, msg string) { s.saveStatus(ok, msg) }

// Logout 退出登录：清 Cookie、删账号、清验证码会话。
func (s *Service) Logout() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client.clearCookies()
	s.clearAccount()
	s.challenges = make(map[string]*Challenge)
}

// ImportAccountFromEnv .env 引导：无存档账号且环境变量给了账号密码时导入。
// 返回是否导入了账号。
func (s *Service) ImportAccountFromEnv(username, password string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.loadAccount(); ok {
		return false
	}
	if username == "" || password == "" {
		return false
	}
	s.saveAccount(username, password)
	return true
}
