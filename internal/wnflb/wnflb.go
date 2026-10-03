// Package wnflb 为福利吧论坛（Discuz! X3.4，fx_checkin 插件）提供
// 网页版登录 / 签到 / 积分查询。
//
// 协议逆向结论来自上游开源项目 fmdxx1991/wnflb-checkin（Python）：
//   - 登录：GET member.php?mod=logging&action=login 取 formhash/loginhash，
//     POST 提交账号密码；风控 IP 会下发二次验证码挑战（auth 一次性令牌），
//     此时改走手动输入验证码完成登录（不依赖 OCR）。
//   - 签到：GET plugin.php?id=fx_checkin:checkin&formhash=..&..&inajax=1。
//   - 登录态判定：首页 HTML 里 discuz_uid != '0'。
package wnflb

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/notify"
)

// ErrCaptchaRequired 表示登录触发了验证码挑战，需走验证码流程。
var ErrCaptchaRequired = errors.New("captcha required")

// Service 福利吧签到服务（单账号）。
type Service struct {
	dir     string // 数据目录，如 /data/wnflb
	baseURL string // 论坛基址，如 https://www.wnflb2023.com

	mu         sync.Mutex
	client     *Client
	challenges map[string]*Challenge // 进行中的验证码会话

	// 首页状态探测缓存：Summary（状态接口）只读缓存、不打网络；
	// 实际探测由 HomeStatus 执行并回写缓存 + status.json 落盘。
	probeMu sync.Mutex
	probeAt time.Time // 上次探测完成时间
	probe   ProbeData // 上次探测结果
	probing int32     // 原子标志：后台刷新进行中（单飞）

	statusMu sync.Mutex // status.json 的读-改-写串行化

	notifier *notify.Bark // 签到结果推送（可为 nil 表示不推送）
}

// SetNotifier 设置签到结果推送器（Bark）。
func (s *Service) SetNotifier(b *notify.Bark) { s.notifier = b }

// notifyResult 发送签到结果通知：结果文案 + 账号信息行
// （用户名/等级/积分/金币/连续·累计签到天数，签到后实时探测）。
func (s *Service) notifyResult(ok bool, msg string) {
	if s.notifier == nil {
		return
	}
	title := "福利吧签到成功 ✅"
	if !ok {
		title = "福利吧签到失败 ❌"
	}
	lines := []string{msg}
	d := s.HomeStatus()
	if username, has := s.QuickStatus(); has && username != "" {
		lines = append(lines, "👤 用户名: "+username)
	}
	if d.Group != "" {
		lines = append(lines, "👑 用户等级: "+d.Group)
	}
	if d.Credits != "" {
		lines = append(lines, "📊 积分: "+d.Credits)
	}
	if d.Coins != "" {
		lines = append(lines, "💰 金币: "+d.Coins)
	}
	if d.Streak > 0 {
		lines = append(lines, "📅 已连续签到"+itoa(d.Streak)+"天")
	}
	if d.Total > 0 {
		lines = append(lines, "📆 累计签到"+itoa(d.Total)+"天")
	}
	s.notifier.Send("wnflb", title, strings.Join(lines, "\n"))
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// Challenge 一次验证码挑战的服务端状态（内存，5 分钟有效）。
type Challenge struct {
	ID          string
	Auth        string // 已做 URL-unquote，提交时由表单编码
	Formhash    string
	Loginhash   string
	Idhash      string
	SeccodeHash string
	Created     time.Time
}

// Status 对外状态（给网页管理页用）。
type Status struct {
	HasAccount    bool   `json:"has_account"`
	LoggedIn      bool   `json:"logged_in"`
	Username      string `json:"username"`
	Credits       string `json:"credits"` // 解析不到时为空
	LastCheckin   string `json:"last_checkin_at"`
	LastCheckinOK bool   `json:"last_checkin_ok"`
	LastMsg       string `json:"last_msg"`
	NextCheckin   string `json:"next_checkin_at"`
	CheckinTimes  string `json:"checkin_times"`
}

// New 创建服务。baseURL 为空时用默认论坛地址。
func New(dir, baseURL string) *Service {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://www.wnflb2023.com"
	}
	_ = os.MkdirAll(dir, 0o700)
	s := &Service{
		dir:        dir,
		baseURL:    baseURL,
		challenges: make(map[string]*Challenge),
	}
	s.client = newClient(baseURL, dir)
	return s
}

// loginPageURL 登录页地址。
func (s *Service) loginPageURL() string {
	return s.baseURL + "/member.php?mod=logging&action=login"
}

// forumURL 论坛首页地址。
func (s *Service) forumURL() string {
	return s.baseURL + "/forum.php"
}

// unquoteAuth 把挑战页 auth 令牌做 URL-unquote，还原 %2F 这类转义，
// 再交由表单编码统一处理（与上游 Python 脚本一致，否则服务端认不出令牌）。
func unquoteAuth(auth string) string {
	if u, err := url.QueryUnescape(auth); err == nil {
		return u
	}
	return auth
}
