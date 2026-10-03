package wnflb

import (
	"context"
	"fmt"
	"log"
	"strings"
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

// probeTimeout 状态探测的网络超时：论坛对某些 IP 响应很慢，
// 探测必须有短超时兜底，不能拖住签到中心列表接口。
// （探测最多三次请求，只在后台执行，故给到 20 秒。）
var probeTimeout = 20 * time.Second

// probeStale 探测缓存过期阈值：超过后 Summary 触发后台刷新（不阻塞当次请求）。
const probeStale = 2 * time.Minute

// HomeStatus 实时探测：一次请求同时返回登录态与积分，并回写缓存与状态文件。
// 只供签到后通知等后台场景；状态接口请用 CachedHomeStatus。
//
// 积分位置实测（用户账号 + 现成脚本印证）：论坛首页与积分页都不一定
// 渲染积分；个人空间页（space-uid-{uid}.html）有"统计信息"块，
// 形如 <em>积分</em>122。探测链：首页 → 个人空间页 → 积分页。
func (s *Service) HomeStatus() (loggedIn bool, credits string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	logged, html := s.checkLoggedInCtx(ctx)
	if !logged {
		s.storeProbe(false, "")
		return false, ""
	}
	credits = parseCredits(html)
	src := "首页"
	if credits == "" {
		uid := discuzUID(html)
		if uid == "" {
			log.Printf("wnflb: 积分探测：首页未提取到 discuz_uid，跳过空间页")
		}
		// 候选页依次尝试：个人空间页 → 个人资料页 → 积分页。
		type cand struct{ name, url string }
		cands := []cand{{"积分页", s.creditURL()}}
		if uid != "" {
			cands = append([]cand{
				{"个人空间页", s.spaceURL(uid)},
				{"个人资料页", s.profileURL(uid)},
			}, cands...)
		}
		for _, cd := range cands {
			page, err := s.client.getTextCtx(ctx, cd.url, nil)
			if err != nil {
				log.Printf("wnflb: 积分探测：%s抓取失败：%v", cd.name, err)
				continue
			}
			if c := parseCredits(page); c != "" {
				credits, src = c, cd.name
				break
			}
			// 诊断：页面里有"积分"却没解析出来时，打印其上下文供补模式。
			if i := strings.Index(page, "积分"); i >= 0 {
				lo := max(0, i-50)
				hi := min(len(page), i+110)
				snip := strings.ReplaceAll(page[lo:hi], "\n", " ")
				log.Printf("wnflb: 积分探测：%s未解析，积分上下文：…%s…", cd.name, snip)
			} else {
				log.Printf("wnflb: 积分探测：%s未解析（%d 字节，无积分字样）", cd.name, len(page))
			}
		}
	}
	if credits == "" {
		log.Printf("wnflb: 积分探测：所有候选页都未解析到积分")
	} else {
		log.Printf("wnflb: 积分探测（%s）当前积分 %s", src, credits)
	}
	s.storeProbe(true, credits)
	return true, credits
}

// discuzUID 从页面脚本变量提取当前登录用户 ID。
func discuzUID(html string) string {
	if m := reDiscuzUID.FindStringSubmatch(html); m != nil && m[1] != "0" {
		return m[1]
	}
	return ""
}

// spaceURL 个人空间页地址（统计信息块含积分）。
func (s *Service) spaceURL(uid string) string {
	return s.baseURL + "/space-uid-" + uid + ".html"
}

// profileURL 个人资料页地址（同为积分候选页）。
func (s *Service) profileURL(uid string) string {
	return s.baseURL + "/home.php?mod=space&uid=" + uid + "&do=profile"
}

// creditURL 积分页地址：该页必有当前积分（论坛首页模板可能不渲染）。
func (s *Service) creditURL() string {
	return s.baseURL + "/home.php?mod=spacecp&ac=credit&showcredit=1"
}

// storeProbe 更新内存缓存并落盘探测结果。
func (s *Service) storeProbe(loggedIn bool, credits string) {
	s.probeMu.Lock()
	s.probeLoggedIn, s.probeCredits, s.probeAt = loggedIn, credits, time.Now()
	s.probeMu.Unlock()
	s.saveProbe(loggedIn, credits)
}

// CachedHomeStatus 只读缓存的登录态与积分，绝不打网络（签到中心列表用）。
// 缓存过期时后台异步刷新一次（单飞），当次仍立即返回旧值；
// 冷启动先用 status.json 里上次落盘的探测结果顶上。
func (s *Service) CachedHomeStatus() (loggedIn bool, credits string) {
	s.probeMu.Lock()
	loggedIn, credits, at := s.probeLoggedIn, s.probeCredits, s.probeAt
	s.probeMu.Unlock()
	if at.IsZero() {
		st := s.loadStatus()
		loggedIn, credits = st.LoggedIn, st.Credits
	}
	if time.Since(at) > probeStale {
		s.refreshHomeStatusAsync()
	}
	return loggedIn, credits
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
