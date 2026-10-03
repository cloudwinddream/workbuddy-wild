package wnflb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"
)

// newChallengeID 生成验证码会话 ID。
func newChallengeID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Login 用账号密码登录。
//   - 成功：返回 (nil, nil)，Cookie 已落盘；
//   - 触发验证码挑战：返回 (challenge, ErrCaptchaRequired)，调用方展示
//     验证码图片后调 SubmitCaptcha；
//   - 失败：返回 (nil, err)。
func (s *Service) Login(username, password string) (*Challenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(username) == "" || password == "" {
		return nil, fmt.Errorf("请输入账号和密码")
	}
	log.Printf("wnflb: 开始登录 username=%s", username)

	_, formhash, loginhash, err := s.fetchLoginPage()
	if err != nil {
		return nil, fmt.Errorf("获取登录页失败: %w", err)
	}
	if formhash == "" || loginhash == "" {
		return nil, fmt.Errorf("无法解析登录页（formhash/loginhash 缺失），论坛可能改版")
	}

	// 首次提交：不带验证码（已信任 IP 通常直接成功）。
	respHTML, err := s.submitLogin(formhash, loginhash, username, password)
	if err != nil {
		return nil, err
	}
	if ch := detectChallenge(respHTML); ch != nil {
		log.Printf("wnflb: 触发验证码挑战")
		return s.registerChallenge(ch), ErrCaptchaRequired
	}
	if msg, ok := checkLoginResult(s, respHTML); ok {
		s.saveAccount(username, password)
		return nil, nil
	} else if msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	return nil, fmt.Errorf("登录失败（未进入登录态）")
}

// detectChallenge 从首次提交的响应中识别二次验证码挑战。
// 判定信号：挑战页特有的 name="auth" 隐藏域（一次性令牌）。
func detectChallenge(respHTML string) *Challenge {
	m := reAuth.FindStringSubmatch(respHTML)
	if m == nil {
		return nil
	}
	fh, lh := extractLoginFields(respHTML)
	ci := detectCaptcha(respHTML)
	return &Challenge{
		Auth:        unquoteAuth(m[1]),
		Formhash:    fh,
		Loginhash:   lh,
		Idhash:      ci.Idhash,
		SeccodeHash: ci.SeccodeHash,
	}
}

// registerChallenge 登记验证码会话并返回。
func (s *Service) registerChallenge(ch *Challenge) *Challenge {
	// 挑战页字段没解析全时，带 auth 重拉一次挑战页。
	if ch.Formhash == "" || ch.Loginhash == "" || !detectCaptchaNeedsID(ch) {
		if rch, err := s.refetchChallenge(ch.Auth); err == nil {
			ch = rch
		}
	}
	ch.ID = newChallengeID()
	ch.Created = time.Now()
	s.challenges[ch.ID] = ch
	// 顺手清理过期会话
	for id, c := range s.challenges {
		if time.Since(c.Created) > 5*time.Minute {
			delete(s.challenges, id)
		}
	}
	return ch
}

func detectCaptchaNeedsID(ch *Challenge) bool {
	return ch.Idhash != ""
}

// refetchChallenge 带 auth 重拉挑战页，获取最新的表单字段。
func (s *Service) refetchChallenge(auth string) (*Challenge, error) {
	u := s.baseURL + "/member.php?" + url.Values{
		"mod":    {"logging"},
		"action": {"login"},
		"auth":   {auth},
	}.Encode()
	html, err := s.client.getText(u, map[string]string{"Referer": s.loginPageURL()})
	if err != nil {
		return nil, err
	}
	fh, lh := extractLoginFields(html)
	ci := detectCaptcha(html)
	if ci.Auth != "" {
		auth = unquoteAuth(ci.Auth)
	}
	return &Challenge{
		Auth:        auth,
		Formhash:    fh,
		Loginhash:   lh,
		Idhash:      ci.Idhash,
		SeccodeHash: ci.SeccodeHash,
	}, nil
}

// fetchLoginPage 拉取登录页并解析字段。
func (s *Service) fetchLoginPage() (html, formhash, loginhash string, err error) {
	html, err = s.client.getText(s.loginPageURL(), nil)
	if err != nil {
		return "", "", "", err
	}
	formhash, loginhash = extractLoginFields(html)
	return html, formhash, loginhash, nil
}

// submitLogin 执行一次账号密码登录 POST（首次提交，不带验证码）。
// 返回响应 HTML。
func (s *Service) submitLogin(formhash, loginhash, username, password string) (string, error) {
	form := url.Values{
		"formhash":   {formhash},
		"referer":    {s.baseURL + "/"},
		"loginfield": {"username"},
		"username":   {username},
		"password":   {password},
		"questionid": {"0"},
		"answer":     {""},
		"cookietime": {"2592000"},
	}
	loginURL := s.baseURL + "/member.php?mod=logging&action=login&loginsubmit=yes&loginhash=" + loginhash
	return s.client.postForm(loginURL, form, map[string]string{"Referer": s.loginPageURL()})
}

// submitChallenge 二次验证码挑战提交（字段对齐浏览器真实成功包）。
func (s *Service) submitChallenge(ch *Challenge, code string) (string, error) {
	form := url.Values{
		"formhash":      {ch.Formhash},
		"referer":       {s.baseURL + "/"},
		"auth":          {ch.Auth},
		"questionid":    {"0"},
		"answer":        {""},
		"seccodehash":   {ch.SeccodeHash},
		"seccodemodid":  {"member::logging"},
		"seccodeverify": {code},
	}
	loginURL := s.baseURL + "/member.php?mod=logging&action=login&loginsubmit=yes&loginhash=" + ch.Loginhash + "&inajax=1"
	return s.client.postForm(loginURL, form, map[string]string{"Referer": s.loginPageURL()})
}

// checkLoginResult 按上游逻辑判定登录 POST 是否成功：先看响应消息，
// 再以首页 discuz_uid 为权威校验。返回 (msg, ok)。
func checkLoginResult(s *Service, respHTML string) (string, bool) {
	msg := extractMessage(respHTML)
	if msg == "" {
		if m := reCDATA.FindStringSubmatch(respHTML); m != nil {
			msg = stripTags(m[1])
		}
	}
	logged, _ := s.checkLoggedIn()
	if logged {
		return "", true
	}
	if msg != "" && (strings.Contains(msg, "密码") || strings.Contains(msg, "用户名")) {
		return "登录失败: " + msg, false
	}
	if msg != "" {
		return msg, false
	}
	return "", false
}

// checkLoggedIn 访问首页判定登录态，返回 (是否登录, 首页HTML)。
func (s *Service) checkLoggedIn() (bool, string) {
	return s.checkLoggedInCtx(context.Background())
}

// checkLoggedInCtx 同 checkLoggedIn，但受 ctx 约束（状态探测时用短超时）。
func (s *Service) checkLoggedInCtx(ctx context.Context) (bool, string) {
	html, err := s.client.getTextCtx(ctx, s.forumURL(), nil)
	if err != nil {
		return false, ""
	}
	return checkLoggedIn(html), html
}

// CaptchaImage 拉取验证码图片（PNG/JPEG）。非图片（WAF 拦截页）返回错误。
func (s *Service) CaptchaImage(id string) ([]byte, error) {
	s.mu.Lock()
	ch, ok := s.challenges[id]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("验证码会话已过期，请重新登录")
	}
	update := fmt.Sprintf("%d", time.Now().UnixMilli())
	imgURL := fmt.Sprintf("%s/misc.php?mod=seccode&update=%s&idhash=%s", s.baseURL, update, ch.Idhash)
	raw, err := s.client.getBytes(imgURL, map[string]string{"Referer": s.loginPageURL()})
	if err != nil {
		return nil, fmt.Errorf("拉取验证码图片失败: %w", err)
	}
	if len(raw) < 100 || !isImage(raw) {
		return nil, fmt.Errorf("验证码图片被拦截（返回非图片），请稍后重试")
	}
	return raw, nil
}

func isImage(raw []byte) bool {
	if len(raw) < 4 {
		return false
	}
	magic := string(raw[:4])
	return magic == "\x89PNG" || magic == "\xff\xd8\xff\xe0" || magic == "\xff\xd8\xff\xe1"
}

// ErrCaptchaRetry 表示验证码不正确，已换一张新图，可重试。
var ErrCaptchaRetry = fmt.Errorf("captcha retry")

// SubmitCaptcha 提交验证码完成登录。
//   - 成功：(nil, nil)，账号已保存；
//   - 验证码错误：返回新 challenge 与 ErrCaptchaRetry，调用方用新 ID 重新拉图；
//   - 其他失败：(nil, err)。
func (s *Service) SubmitCaptcha(id, code string, username, password string) (*Challenge, error) {
	s.mu.Lock()
	ch, ok := s.challenges[id]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("验证码会话已过期，请重新登录")
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, fmt.Errorf("请输入验证码")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 先调 Discuz 验证码校验接口（与浏览器行为一致；失败则换图重试）。
	if !s.verifySecCode(ch, code) {
		log.Printf("wnflb: 验证码校验未通过，换图重试")
		return s.registerChallenge(ch), ErrCaptchaRetry
	}
	respHTML, err := s.submitChallenge(ch, code)
	if err != nil {
		return nil, fmt.Errorf("提交登录失败: %w", err)
	}
	// 仍被要求验证码 → 验证码不正确，换图重试
	if strings.Contains(respHTML, "请输入验证码") && strings.Contains(respHTML, "auth=") {
		log.Printf("wnflb: 验证码不正确，换图重试")
		return s.registerChallenge(ch), ErrCaptchaRetry
	}
	if msg, ok := checkLoginResult(s, respHTML); ok {
		s.saveAccount(username, password)
		delete(s.challenges, id)
		return nil, nil
	} else if msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	return nil, fmt.Errorf("登录失败（未进入登录态）")
}

// verifySecCode 调用验证码校验接口（action=check），成功写入 seccode cookie。
func (s *Service) verifySecCode(ch *Challenge, code string) bool {
	u := fmt.Sprintf("%s/misc.php?mod=seccode&action=check&inajax=1&modid=member::logging&idhash=%s&secverify=%s",
		s.baseURL, url.QueryEscape(ch.Idhash), url.QueryEscape(code))
	txt, err := s.client.getText(u, map[string]string{
		"Referer":          s.loginPageURL(),
		"X-Requested-With": "XMLHttpRequest",
	})
	if err != nil {
		return false
	}
	return strings.Contains(txt, "succeed")
}
