// Package webapi 为无头 Web 版提供 REST 管理 API（/api/*）。
//
// 设计来源：internal/app（桌面托盘版 wails 绑定层）的面板逻辑——
// GetState/GetAccounts 的数据组装、登录编排、签到/刷新/删号、配置热更新，
// 全部照搬其语义，仅把"wails 绑定 + 系统弹窗/托盘"换成 HTTP。
//
// 本包只依赖标准库与现有 internal 包，不引用 internal/app、winutil、wails。
package webapi

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/checkin"
	"github.com/rockswang/workbuddy-wild/internal/config"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
	"github.com/rockswang/workbuddy-wild/internal/scheduler"
	"github.com/rockswang/workbuddy-wild/internal/server"
	"github.com/rockswang/workbuddy-wild/internal/wnflb"
)

// Runtime 是一个平台的运行时资源（与 app.Runtime 同构，避免引用 internal/app）。
type Runtime struct {
	Kind      provider.Kind
	Pool      *pool.Pool
	Upstream  provider.Upstream
	Scheduler *scheduler.Scheduler
}

// Options 构建 API 的依赖。
type Options struct {
	Config     *config.Config
	ConfigPath string
	Runtimes   map[provider.Kind]*Runtime
	Handler    *server.Handler // /v1 的 handler：SetAPIKey 时同步更新
	Version    string
	StateDir   string // state 文件所在目录：登录态文件、日志都放这里
	PublicBase string // TraeWork OAuth 回调的公开基址，如 http://公网IP:7863

	// Wnflb 福利吧签到服务（nil 表示未启用）；WnflbTimes 为签到时刻表展示。
	Wnflb      *wnflb.Service
	WnflbTimes string

	// SetListen 由宿主提供：热切换 HTTP 监听（语义同 app.SetListen，
	// 失败保持原监听）。
	SetListen func(host string, port int) error
}

// API 无头 Web 版的管理接口。
type API struct {
	cfg       *config.Config
	cfgPath   string
	runtimes  map[provider.Kind]*Runtime
	handler   *server.Handler
	version   string
	stateDir  string
	pubBase   string
	setListen func(host string, port int) error

	mu     sync.Mutex // 保护 cfg 与 apiKey
	apiKey string

	evMu   sync.Mutex
	events []apiEvent
	seq    uint64

	loginMu sync.Mutex
	active  *loginSession // 同一时间只允许一个登录流程（与桌面端 loginBusy 语义一致）

	wnflb      *wnflb.Service // 福利吧签到（可为 nil 表示未启用）
	wnflbTimes string         // 签到时刻表展示，如 "01:00,22:00"

	checkins *checkin.Registry // 签到中心模块注册表
}

// apiEvent 推送给前端垫片的事件（垫片轮询 /api/events 拉取）。
type apiEvent struct {
	Seq  uint64         `json:"seq"`
	Name string         `json:"name"` // checkin | refresh | login
	Data map[string]any `json:"data"`
	At   string         `json:"at"`
}

// maxEvents 事件环形缓冲上限。
const maxEvents = 200

// New 构建 API，并清理上次残留的登录态文件。
func New(opts Options) *API {
	a := &API{
		cfg:        opts.Config,
		cfgPath:    opts.ConfigPath,
		runtimes:   opts.Runtimes,
		handler:    opts.Handler,
		version:    opts.Version,
		stateDir:   opts.StateDir,
		pubBase:    opts.PublicBase,
		setListen:  opts.SetListen,
		apiKey:     opts.Config.APIKey,
		wnflb:      opts.Wnflb,
		wnflbTimes: opts.WnflbTimes,
	}
	// 清理上次异常退出残留的登录态文件。
	if fps, _ := filepath.Glob(filepath.Join(opts.StateDir, "weblogin-*.json")); len(fps) > 0 {
		for _, fp := range fps {
			_ = os.Remove(fp)
		}
		log.Printf("webapi: 清理残留登录态文件 %d 个", len(fps))
	}
	go a.loginReaper()
	// 签到中心：注册所有签到模块（福利吧为第一个）。
	a.checkins = checkin.New()
	if opts.Wnflb != nil {
		a.checkins.Register(&wnflbModule{svc: opts.Wnflb, times: opts.WnflbTimes})
	}
	return a
}

// Register 把 /api/* 路由挂到 mux（调用方同时挂载 /v1/* 与静态文件）。
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/state", a.withAuth(a.handleState))
	mux.HandleFunc("GET /api/accounts", a.withAuth(a.handleAccounts))
	mux.HandleFunc("POST /api/login/start", a.withAuth(a.handleLoginStart))
	mux.HandleFunc("GET /api/login/poll", a.withAuth(a.handleLoginPoll))
	mux.HandleFunc("POST /api/login/cancel", a.withAuth(a.handleLoginCancel))
	// TraeWork OAuth 浏览器回调：浏览器直接 GET，无 Bearer（sid 为 128bit 随机，
	// 且仅在对应登录会话有效期内可写该会话的 state 文件）。
	mux.HandleFunc("GET /api/login/traecb", a.handleTraeCallback)
	mux.HandleFunc("POST /api/checkin/account", a.withAuth(a.handleCheckinAccount))
	mux.HandleFunc("POST /api/checkin/all", a.withAuth(a.handleCheckinAll))
	mux.HandleFunc("POST /api/refresh/account", a.withAuth(a.handleRefreshAccount))
	mux.HandleFunc("POST /api/refresh/all", a.withAuth(a.handleRefreshAll))
	mux.HandleFunc("DELETE /api/account/{uid}", a.withAuth(a.handleDeleteAccount))
	mux.HandleFunc("GET /api/strategy", a.withAuth(a.handleGetStrategy))
	mux.HandleFunc("PUT /api/strategy", a.withAuth(a.handleSetStrategy))
	mux.HandleFunc("GET /api/config", a.withAuth(a.handleGetConfig))
	mux.HandleFunc("PUT /api/config", a.withAuth(a.handleSetConfig))
	mux.HandleFunc("GET /api/logs", a.withAuth(a.handleLogs))
	mux.HandleFunc("GET /api/events", a.withAuth(a.handleEvents))
	a.registerWnflb(mux)
	a.registerCheckins(mux)
}

// ---------------------------------------------------------------------------
// 鉴权（语义与 server.Handler.withAuth 一致：key 为空 = 不鉴权）
// ---------------------------------------------------------------------------

func (a *API) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		key := a.apiKey
		a.mu.Unlock()
		if key != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != key {
				writeErr(w, http.StatusUnauthorized, "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

// applyAPIKey 更新运行时鉴权密钥（/v1 与 /api 同时生效）。
func (a *API) applyAPIKey(key string) {
	a.mu.Lock()
	a.apiKey = key
	a.mu.Unlock()
	if a.handler != nil {
		a.handler.SetAPIKey(key)
	}
}

// ---------------------------------------------------------------------------
// 事件总线（调度器/登录进度 → 前端垫片轮询）
// ---------------------------------------------------------------------------

// emit 记录一个事件（保留最近 maxEvents 条）。
func (a *API) emit(name string, data map[string]any) {
	a.evMu.Lock()
	defer a.evMu.Unlock()
	a.seq++
	a.events = append(a.events, apiEvent{Seq: a.seq, Name: name, Data: data, At: time.Now().Format(time.RFC3339)})
	if len(a.events) > maxEvents {
		a.events = a.events[len(a.events)-maxEvents:]
	}
}

// eventsSince 返回 seq 之后的新事件与当前 seq。
func (a *API) eventsSince(since uint64) ([]apiEvent, uint64) {
	a.evMu.Lock()
	defer a.evMu.Unlock()
	out := make([]apiEvent, 0)
	for _, e := range a.events {
		if e.Seq > since {
			out = append(out, e)
		}
	}
	return out, a.seq
}

// NotifyCheckin 签到结果推送（供 scheduler.SetCheckinObserver 使用）。
func (a *API) NotifyCheckin(platform string, r scheduler.CheckinResult) {
	log.Printf("webapi checkin platform=%s uid=%s ok=%t retryable=%t msg=%s remain=%d has_remain=%t",
		platform, r.UID, r.OK, r.Retryable, r.Msg, r.Remain, r.HasRemain)
	a.emit("checkin", map[string]any{
		"platform": platform, "uid": r.UID, "ok": r.OK, "retryable": r.Retryable,
		"msg": r.Msg, "remain": r.Remain, "has_remain": r.HasRemain,
	})
}

// NotifyRefresh token 刷新结果推送（供 scheduler.SetRefreshObserver 使用）。
func (a *API) NotifyRefresh(platform, uid string, ok bool, msg string) {
	log.Printf("webapi refresh platform=%s uid=%s ok=%t msg=%s", platform, uid, ok, msg)
	if !ok {
		a.emit("refresh", map[string]any{"platform": platform, "uid": uid, "ok": ok, "msg": msg})
	}
}

// ---------------------------------------------------------------------------
// 账号视图（数据组装照抄 internal/app：AccountView / State JSON 字段保持一致，
// 前端垫片无需改动即可复用）
// ---------------------------------------------------------------------------

// AccountView 面板展示的账号（脱敏）。JSON 字段与 app.AccountView 一致。
type AccountView struct {
	UID             string `json:"uid"`
	Group           string `json:"group"`
	Nickname        string `json:"nickname"`
	Credits         int64  `json:"credits"`
	Cooling         bool   `json:"cooling"`
	Until           string `json:"until"`
	Reason          string `json:"reason"`
	Disabled        bool   `json:"disabled"`
	ErrCount        int    `json:"err_count"`
	LastCheckinOK   bool   `json:"last_checkin_ok"`
	LastCheckinAt   string `json:"last_checkin_at"`
	LastCheckinMsg  string `json:"last_checkin_msg"`
	LastUsedAt      string `json:"last_used_at"`
	LastCallCredits int64  `json:"last_call_credits"`
	InUse           bool   `json:"in_use"`
	ExpiresAt       int64  `json:"expires_at"`
}

// State 管理页初始数据。JSON 字段与 app.State 一致。
type State struct {
	Accounts       []AccountView `json:"accounts"`
	CheckinHours   []int         `json:"checkin_hours"`
	CheckinTimes   []string      `json:"checkin_times"`
	KeepaliveHours []int         `json:"keepalive_hours"`
	ListenHost     string        `json:"listen_host"`
	ListenPort     int           `json:"listen_port"`
	APIKey         string        `json:"api_key"`
	LoginBusy      bool          `json:"login_busy"`
	NextCheckin    string        `json:"next_checkin"`
	Version        string        `json:"version"`
	Autostart      bool          `json:"autostart"`
	Running        bool          `json:"running"`
	Strategy       string        `json:"strategy"`
}

// inUseWindow 「正在调用」的时间窗（与桌面端一致）。
const inUseWindow = 5 * time.Second

func (a *API) runtime(kind provider.Kind) *Runtime {
	if a.runtimes == nil {
		return nil
	}
	return a.runtimes[kind]
}

func (a *API) firstRuntime() *Runtime {
	for _, k := range []provider.Kind{provider.WorkBuddy, provider.TraeWork} {
		if rt := a.runtime(k); rt != nil {
			return rt
		}
	}
	return nil
}

func (a *API) allStatuses() []pool.Status {
	out := []pool.Status{}
	for _, rt := range a.runtimes {
		if rt != nil && rt.Pool != nil {
			out = append(out, rt.Pool.List()...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

func (a *API) findRuntimeAuth(uid string) (*Runtime, *auth.Auth) {
	for _, rt := range a.runtimes {
		if rt == nil || rt.Pool == nil {
			continue
		}
		if au := rt.Pool.AuthByUID(uid); au != nil {
			return rt, au
		}
	}
	return nil, nil
}

// accountGroup 账号所属分组（逻辑同 app.accountGroup）。
func (a *API) accountGroup(uid string) string {
	_, au := a.findRuntimeAuth(uid)
	if au != nil {
		if au.Kind != "" {
			return au.Kind
		}
		if au.FilePath != "" && strings.HasPrefix(filepath.Base(au.FilePath), "trae-") {
			return "traework"
		}
	}
	return "workbuddy"
}

func (a *API) accountViews() []AccountView {
	statuses := a.allStatuses()
	now := time.Now()
	out := make([]AccountView, 0, len(statuses))
	for _, s := range statuses {
		_, au := a.findRuntimeAuth(s.UID)
		var expires int64
		if au != nil {
			expires = au.ExpiresAt
		}
		inUse := !s.LastUsedAt.IsZero() && now.Sub(s.LastUsedAt) < inUseWindow
		out = append(out, AccountView{
			UID: s.UID, Group: a.accountGroup(s.UID), Nickname: s.Nickname,
			Credits: s.Credits, Cooling: s.Cooling, Until: fmtTime(s.Until),
			Reason: s.Reason, Disabled: s.Disabled, ErrCount: s.ErrCount,
			LastCheckinOK: s.LastCheckinOK, LastCheckinAt: fmtTime(s.LastCheckinAt),
			LastCheckinMsg: s.LastCheckinMsg, LastUsedAt: fmtTime(s.LastUsedAt),
			LastCallCredits: s.LastCallCredits, InUse: inUse, ExpiresAt: expires,
		})
	}
	return out
}

func (a *API) checkinTimes() []string {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.CheckinTimes()
	}
	return nil
}

func (a *API) keepaliveHours() []int {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.KeepaliveHours()
	}
	return nil
}

func (a *API) nextFire() time.Time {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.NextFire()
	}
	return time.Time{}
}

func (a *API) currentStrategy() pool.Strategy {
	if rt := a.firstRuntime(); rt != nil && rt.Pool != nil {
		return rt.Pool.GetStrategy()
	}
	return pool.StrategyCredits
}

func (a *API) loginBusy() bool {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	return a.active != nil && time.Now().Before(a.active.deadline)
}

// reloadAccounts 用 auths 目录最新文件对齐账号池（逻辑同 app.reloadAccounts）。
func (a *API) reloadAccounts() {
	a.mu.Lock()
	authDir, region := a.cfg.AuthDir, a.cfg.Region
	a.mu.Unlock()
	if rt := a.runtime(provider.WorkBuddy); rt != nil && rt.Pool != nil {
		if auths, err := auth.LoadWorkBuddyDir(authDir, region); err != nil {
			log.Printf("webapi reload workbuddy accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.TraeWork); rt != nil && rt.Pool != nil {
		if auths, err := auth.LoadTraeDir(authDir); err != nil {
			log.Printf("webapi reload traework accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("01-02 15:04")
}

func shortUID(uid string) string {
	if len(uid) <= 10 {
		return uid
	}
	return uid[:10] + "…"
}

func shortErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if len(s) > 160 {
		return s[:160]
	}
	return s
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
