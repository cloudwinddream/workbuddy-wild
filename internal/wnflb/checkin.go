package wnflb

import (
	"fmt"
	"log"
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
	log.Printf("wnflb: 签到结果 ok=%v msg=%s", res.OK, res.Msg)
	return res.OK, res.Msg
}

// Credits 查询当前积分。未登录返回 ""。
func (s *Service) Credits() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	logged, html := s.checkLoggedIn()
	if !logged {
		return ""
	}
	return parseCredits(html)
}

// HomeStatus 一次请求同时返回登录态与积分（状态接口用，避免两次请求）。
func (s *Service) HomeStatus() (loggedIn bool, credits string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	logged, html := s.checkLoggedIn()
	if !logged {
		return false, ""
	}
	return true, parseCredits(html)
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

// AutoCheckin 定时任务入口：确保登录后签到，并更新状态文件。
// 返回人类可读的结果文案（成功/失败都记录）。
func (s *Service) AutoCheckin() string {
	if err := s.EnsureLoggedIn(); err != nil {
		msg := "自动签到跳过: " + err.Error()
		log.Printf("wnflb: %s", msg)
		s.saveStatus(false, msg)
		return msg
	}
	ok, msg := s.Checkin()
	s.saveStatus(ok, msg)
	return msg
}

// QuickStatus 轻量状态（不请求论坛）：用于调度器日志与状态文件回显。
func (s *Service) QuickStatus() (username string, hasAccount bool) {
	if acc, ok := s.loadAccount(); ok {
		return acc.Username, true
	}
	return "", false
}
