// Package scheduler 定时任务：每日签到 + token keepalive。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
// 签到时间支持分钟精度运行中更新（SetCheckinMinutes），由 GUI 面板写入 config.json 后调用。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// Config 调度器依赖。
type Config struct {
	Pool           *pool.Pool
	Upstream       provider.Upstream
	Name           string // 日志中的平台名
	CheckinHours   []int  // 旧配置兼容，整点小时
	CheckinMinutes []int  // 当天分钟数，优先于 CheckinHours
	KeepaliveHours []int  // 默认 [22]
}

// 限流重试参数。
//
// 背景：TraeWork 的签到 claim 接口在高峰会返回 9074（"当前参与用户太多"）。
// 单次调用内虽然已有指数退避重试（CheckinClaim 内 4 次 / 约 58 秒），
// 但高峰可能持续更久。原实现在耗尽后**只是标记 Retryable 而无人消费**，
// 用户看到的"稍后自动重试"实际要等到下一个定时点（可能间隔数小时）。
//
// 现在：耗尽后由调度器安排**独立的延迟重试**，不依赖下一次定时签到。
const (
	// rateLimitRetryDelay 限流耗尽后的首次延迟重试间隔。
	// 取 10 分钟：足够跨过一波瞬时高峰，又不会让用户等太久。
	rateLimitRetryDelay = 10 * time.Minute

	// rateLimitRetryMax 单个账号每天最多安排多少次限流延迟重试。
	// 防止上游长时间异常时无限重试刷屏。
	rateLimitRetryMax = 6
)

// retryState 单个账号的限流重试状态。
type retryState struct {
	count    int       // 已安排的延迟重试次数（当天）
	lastDay  int       // 归属日期（一年中的第几天），跨天重置
	nextAt   time.Time // 下次重试时刻
	pending  bool      // 是否有待执行的重试
}

// Scheduler 调度器。
type Scheduler struct {
	mu        sync.Mutex                 // 保护 cfg 中的小时配置
	cfg       Config
	wake      chan struct{}              // 配置变更唤醒 Run 循环重算下次触发
	onCheckin func(CheckinResult)        // 结果观察器，供 GUI 接收自动签到结果
	onRefresh func(string, bool, string) // token 刷新结果观察器

	retryMu sync.Mutex            // 保护 retries
	retries map[string]*retryState // uid → 限流重试状态
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinMinutes) == 0 {
		if len(cfg.CheckinHours) > 0 {
			cfg.CheckinMinutes = make([]int, 0, len(cfg.CheckinHours))
			for _, h := range cfg.CheckinHours {
				if h >= 0 && h <= 23 {
					cfg.CheckinMinutes = append(cfg.CheckinMinutes, h*60)
				}
			}
		} else {
			cfg.CheckinMinutes = []int{9 * 60, 21 * 60}
		}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	return &Scheduler{cfg: cfg, wake: make(chan struct{}, 1), retries: map[string]*retryState{}}
}

// markRetryable 记录某账号需要延迟重试，并返回安排的时刻。
//
// 仅在限流耗尽时调用。返回 (下次重试时刻, 是否还能重试)：
//   - 跨天自动重置计数
//   - 超过 rateLimitRetryMax 后不再安排（返回 ok=false）
func (s *Scheduler) markRetryable(uid string, now time.Time) (time.Time, bool) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	st := s.retries[uid]
	day := now.YearDay()
	if st == nil || st.lastDay != day {
		st = &retryState{lastDay: day}
		s.retries[uid] = st
	}
	if st.count >= rateLimitRetryMax {
		return time.Time{}, false
	}
	st.count++
	// 递增退避：10min, 20min, 30min, ...（线性增长，避免像指数那样很快跨过小时级）
	st.nextAt = now.Add(rateLimitRetryDelay * time.Duration(st.count))
	st.pending = true
	return st.nextAt, true
}

// dueRetries 取出所有已到期的重试账号，并清除其 pending 标记。
func (s *Scheduler) dueRetries(now time.Time) []string {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	var out []string
	for uid, st := range s.retries {
		if st.pending && !now.Before(st.nextAt) {
			st.pending = false
			out = append(out, uid)
		}
	}
	return out
}

// nextRetryAt 返回最近一次待执行重试的时刻（无则返回零值）。
func (s *Scheduler) nextRetryAt() time.Time {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	var best time.Time
	for _, st := range s.retries {
		if !st.pending {
			continue
		}
		if best.IsZero() || st.nextAt.Before(best) {
			best = st.nextAt
		}
	}
	return best
}

// clearRetry 账号签到成功后清除其重试状态。
func (s *Scheduler) clearRetry(uid string) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	delete(s.retries, uid)
}

// schedule 返回当前签到分钟/保活小时配置的副本。
func (s *Scheduler) schedule() (checkinMinutes, keepaliveHours []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int{}, s.cfg.CheckinMinutes...), append([]int{}, s.cfg.KeepaliveHours...)
}

// CheckinHours 保留旧 API，返回整点签到小时。
func (s *Scheduler) CheckinHours() []int {
	minutes, _ := s.schedule()
	out := make([]int, 0, len(minutes))
	for _, m := range minutes {
		if m%60 == 0 {
			out = append(out, m/60)
		}
	}
	return out
}

// CheckinMinutes 当前签到时间，单位为当天分钟数（0..1439）。
func (s *Scheduler) CheckinMinutes() []int {
	minutes, _ := s.schedule()
	return minutes
}

// CheckinTimes 当前签到时间，格式为 HH:MM。
func (s *Scheduler) CheckinTimes() []string {
	minutes := s.CheckinMinutes()
	out := make([]string, 0, len(minutes))
	for _, m := range minutes {
		out = append(out, fmt.Sprintf("%02d:%02d", m/60, m%60))
	}
	return out
}

// KeepaliveHours 当前保活时间（副本）。
func (s *Scheduler) KeepaliveHours() []int {
	_, kh := s.schedule()
	return kh
}

// SetCheckinMinutes 运行中更新签到分钟并唤醒调度循环。
func (s *Scheduler) SetCheckinMinutes(minutes []int) {
	clean := make([]int, 0, len(minutes))
	seen := map[int]bool{}
	for _, m := range minutes {
		if m >= 0 && m < 24*60 && !seen[m] {
			seen[m] = true
			clean = append(clean, m)
		}
	}
	sort.Ints(clean)
	s.mu.Lock()
	s.cfg.CheckinMinutes = clean
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// SetCheckinHours 保留旧 API，将整点小时转换为当天分钟数。
func (s *Scheduler) SetCheckinHours(hours []int) {
	minutes := make([]int, 0, len(hours))
	for _, h := range hours {
		minutes = append(minutes, h*60)
	}
	s.SetCheckinMinutes(minutes)
}

// SetCheckinObserver 设置签到结果观察器；用于把定时任务结果推送到 GUI。
func (s *Scheduler) SetCheckinObserver(fn func(CheckinResult)) {
	s.mu.Lock()
	s.onCheckin = fn
	s.mu.Unlock()
}

func (s *Scheduler) notifyCheckin(r CheckinResult) {
	s.mu.Lock()
	fn := s.onCheckin
	s.mu.Unlock()
	if fn != nil {
		fn(r)
	}
}

// SetRefreshObserver 设置 token 刷新结果观察器。
func (s *Scheduler) SetRefreshObserver(fn func(uid string, ok bool, msg string)) {
	s.mu.Lock()
	s.onRefresh = fn
	s.mu.Unlock()
}

func (s *Scheduler) notifyRefresh(uid string, ok bool, msg string) {
	s.mu.Lock()
	fn := s.onRefresh
	s.mu.Unlock()
	if fn != nil {
		fn(uid, ok, msg)
	}
}

// NextFire 返回最近的一次触发时间（签到 + 保活合并）。
func (s *Scheduler) NextFire() time.Time {
	ch, kh := s.schedule()
	all := append(append([]int{}, ch...), hoursToMinutes(kh)...)
	return nextFireMinutes(time.Now(), all)
}

// nextFire 保留旧测试/API语义：hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	return nextFireMinutes(now, hoursToMinutes(hours))
}

func hoursToMinutes(hours []int) []int {
	out := make([]int, 0, len(hours))
	for _, h := range hours {
		out = append(out, h*60)
	}
	return out
}

// nextFireMinutes 返回 now 之后最近的触发时间；输入为当天分钟数。
func nextFireMinutes(now time.Time, minutes []int) time.Time {
	var earliest time.Time
	for _, m := range minutes {
		if m < 0 || m >= 24*60 {
			continue
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), m/60, m%60, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		ch, kh := s.schedule()
		all := append(append([]int{}, ch...), hoursToMinutes(kh)...)
		next := nextFireMinutes(time.Now(), all)
		// 若有待执行的限流重试且早于下次定时点，则以重试时刻为准唤醒。
		if ra := s.nextRetryAt(); !ra.IsZero() && ra.Before(next) {
			next = ra
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop() // 配置变更，重算
		case <-timer.C:
			now := time.Now()
			minute := now.Hour()*60 + now.Minute()
			if containsMinute(ch, minute) {
				s.RunCheckinNow()
			}
			if contains(kh, now.Hour()) {
				s.RunKeepaliveNow()
			}
			// 到期重试：只签这些账号，不影响其它账号
			s.runDueRetries(now)
		}
	}
}

// runDueRetries 对已到期的限流账号单独重试签到。
func (s *Scheduler) runDueRetries(now time.Time) {
	uids := s.dueRetries(now)
	if len(uids) == 0 {
		return
	}
	name := s.name()
	log.Printf("checkin retry start platform=%s accounts=%d uids=%v", name, len(uids), uids)
	for _, uid := range uids {
		if s.cfg.Pool.AuthByUID(uid) == nil {
			s.clearRetry(uid) // 账号已被删除
			continue
		}
		r := s.checkinOne(uid)
		if r.OK {
			s.clearRetry(uid) // 成功，不再重试
		} else if r.Retryable {
			if at, ok := s.markRetryable(uid, now); ok {
				log.Printf("checkin retry scheduled platform=%s uid=%s at=%s",
					name, uid, at.Format("15:04:05"))
			} else {
				log.Printf("checkin retry exhausted-for-today platform=%s uid=%s",
					name, uid)
			}
		}
	}
}

func containsMinute(minutes []int, minute int) bool {
	for _, v := range minutes {
		if v == minute {
			return true
		}
	}
	return false
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// CheckinResult 单账号签到结果（GUI 面板展示）。
type CheckinResult struct {
	UID string `json:"uid"`
	OK  bool   `json:"ok"`
	Msg string `json:"msg"`
	// Retryable 表示失败原因是上游瞬时状态（如高峰限流），
	// 不是账号本身的问题——不应触发冷却/禁用，稍后重试即可。
	Retryable bool  `json:"retryable,omitempty"`
	Remain    int64 `json:"remain"`
	HasRemain bool  `json:"has_remain"`
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	name := s.name()
	log.Printf("checkin batch start platform=%s accounts=%d", name, len(s.cfg.Pool.List()))
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			log.Printf("checkin skip platform=%s uid=%s reason=disabled", name, st.UID)
			continue
		}
		r := s.checkinOne(st.UID)
		log.Printf("checkin result platform=%s uid=%s ok=%t msg=%s remain=%d has_remain=%t", name, st.UID, r.OK, r.Msg, r.Remain, r.HasRemain)
	}
	log.Printf("checkin batch done platform=%s", name)
}

// CheckinAccount 单个账号立即签到（禁用跳过），返回该账号结果。
func (s *Scheduler) CheckinAccount(uid string) (CheckinResult, error) {
	st, ok := s.cfg.Pool.Status(uid)
	if !ok {
		return CheckinResult{}, fmt.Errorf("unknown account %s", uid)
	}
	if st.Disabled {
		return CheckinResult{}, fmt.Errorf("account %s disabled", uid)
	}
	return s.checkinOne(uid), nil
}

// checkinOne 单账号签到 + 余额刷新 + 解冻 + 记录签到状态。
func (s *Scheduler) checkinOne(uid string) CheckinResult {
	name := s.name()
	log.Printf("checkin start platform=%s uid=%s", name, uid)
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshToken == "" {
		r := CheckinResult{UID: uid, Msg: "no refresh token"}
		s.cfg.Pool.RecordCheckin(uid, false, r.Msg)
		s.notifyCheckin(r)
		return r
	}
	// 签到前保证 access token 有效；否则仅依赖晚间 keepalive 时，早上的签到可能拿过期 token。
	if a.NeedsRefresh(2 * time.Hour) {
		if err := s.refreshForCheckin(a, uid); err != nil {
			r := CheckinResult{UID: uid, Msg: "refresh: " + shortErr(err)}
			s.cfg.Pool.RecordCheckin(uid, false, r.Msg)
			s.notifyCheckin(r)
			return r
		}
	}
	r := CheckinResult{UID: uid}
	checkinErr := s.cfg.Upstream.DailyCheckin(a)
	// status 接口本身就是令牌有效性验证；若返回 session dead，刷新一次后重试整套签到。
	if checkinErr != nil && isSessionDead(checkinErr) {
		log.Printf("checkin token invalid platform=%s uid=%s, refreshing and retrying", name, uid)
		if err := s.refreshForCheckin(a, uid); err != nil {
			checkinErr = fmt.Errorf("token invalid; refresh failed: %w", err)
		} else {
			checkinErr = s.cfg.Upstream.DailyCheckin(a)
		}
	}
	if checkinErr != nil {
		log.Printf("checkin failed platform=%s uid=%s err=%v", name, uid, checkinErr)
		r.Msg = shortErr(checkinErr)
		if isAlready(checkinErr) {
			r.OK = true // 已签到时视为成功状态
			r.Msg = "已签到"
		} else if isRateLimited(checkinErr) {
			// 上游高峰限流（如 TraeWork 9074）：瞬时状态，不是账号问题。
			// 不触发冷却/禁用。此处**安排一次延迟重试**——原实现只打标记无人消费，
			// 导致"稍后自动重试"实际要等到下一个定时点（可能间隔数小时）。
			r.Retryable = true
			r.Msg = "上游繁忙，稍后自动重试"
			log.Printf("checkin rate-limited platform=%s uid=%s（可重试，不视为账号异常）", name, uid)
			if at, ok := s.markRetryable(uid, time.Now()); ok {
				log.Printf("checkin retry scheduled platform=%s uid=%s at=%s",
					name, uid, at.Format("15:04:05"))
			} else {
				log.Printf("checkin retry exhausted-for-today platform=%s uid=%s", name, uid)
			}
		}
	} else {
		r.OK = true
		r.Msg = "ok"
		s.clearRetry(uid) // 签到成功，清除待重试状态
	}
	// 无论签到成败都查余额（已签到等业务错误下余额刷新仍有效）
	remain, rerr := s.cfg.Upstream.UserResource(a)
	if rerr != nil {
		log.Printf("checkin credits failed platform=%s uid=%s err=%v", name, uid, rerr)
		r.OK = false // 签到后的积分确认失败，整次操作向 GUI 报告失败
		if r.Msg == "" {
			r.Msg = "余额查询失败"
		} else {
			r.Msg += "；余额查询失败"
		}
	} else {
		r.Remain, r.HasRemain = remain, true
		log.Printf("checkin credits platform=%s uid=%s remain=%d", name, uid, remain)
		s.cfg.Pool.ReenableIfCredits(uid, remain)
	}
	s.cfg.Pool.RecordCheckin(uid, r.OK, r.Msg)
	s.notifyCheckin(r)
	return r
}

// rateLimited 由上游客户端实现的"可重试瞬时错误"标记。
// 用接口探测而非直接 import 具体平台包，避免 scheduler 与各上游耦合。
type rateLimited interface {
	IsRateLimited() bool
}

// isRateLimited 判断错误是否为上游高峰限流（瞬时、可重试，非账号异常）。
// 同时兼容两类表达：
//   - 上游客户端定义的错误类型实现了 IsRateLimited() bool
//   - provider.Error 被分类为 ErrSoftRate（429 类软限流）
func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var rl rateLimited
	if errors.As(err, &rl) {
		return rl.IsRateLimited()
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe.Kind == provider.ErrSoftRate
	}
	return false
}

// isAlready 只匹配明确的“今日已签到”，不能因错误文本包含 checkin 就判成功。
func isAlready(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already check") ||
		strings.Contains(s, "already checked") ||
		strings.Contains(s, "code=9095")
}

func shortErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

func (s *Scheduler) name() string {
	if s.cfg.Name != "" {
		return s.cfg.Name
	}
	return "unknown"
}

func (s *Scheduler) refreshForCheckin(a *auth.Auth, uid string) error {
	name := s.name()
	log.Printf("refresh start platform=%s uid=%s reason=checkin", name, uid)
	if err := s.cfg.Upstream.RefreshToken(a); err != nil {
		log.Printf("refresh failed platform=%s uid=%s err=%v", name, uid, err)
		return err
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("refresh save failed platform=%s uid=%s err=%v", name, uid, err)
		return fmt.Errorf("refresh save: %w", err)
	}
	log.Printf("refresh success platform=%s uid=%s expires_at=%d", name, uid, a.ExpiresAt)
	return nil
}

func isSessionDead(err error) bool {
	var ue *provider.Error
	return errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
func (s *Scheduler) RunKeepaliveNow() {
	name := s.name()
	log.Printf("refresh batch start platform=%s", name)
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			log.Printf("refresh skip platform=%s uid=%s reason=disabled", name, st.UID)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			msg := "no refresh token"
			log.Printf("refresh skip platform=%s uid=%s reason=%s", name, st.UID, msg)
			s.notifyRefresh(st.UID, false, msg)
			continue
		}
		log.Printf("refresh start platform=%s uid=%s reason=keepalive", name, st.UID)
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("refresh failed platform=%s uid=%s err=%v", name, st.UID, err)
			var ue *provider.Error
			if errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "12153 session dead")
				log.Printf("refresh disabled platform=%s uid=%s reason=session_dead", name, st.UID)
			}
			s.notifyRefresh(st.UID, false, err.Error())
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("refresh save failed platform=%s uid=%s err=%v", name, st.UID, err)
			s.notifyRefresh(st.UID, false, "refresh save: "+err.Error())
			continue
		}
		log.Printf("refresh success platform=%s uid=%s expires_at=%d", name, st.UID, a.ExpiresAt)
		s.notifyRefresh(st.UID, true, "ok")
	}
	log.Printf("refresh batch done platform=%s", name)
}
