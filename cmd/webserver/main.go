// Command webserver WorkBuddy-Wild 无头 Web 版入口。
//
// 单进程：OpenAI 兼容 HTTP 服务（/v1/*）+ 自动签到调度器（双平台）+
// REST 管理 API（/api/*）+ 静态管理页（/，复用桌面版前端 + 兼容垫片）。
//
// 与桌面版 main.go 的区别：无托盘、无 WebView2、无 winutil 调用，
// 可在 Linux/Docker 无头环境运行。装配逻辑（pool/scheduler/handler）
// 与桌面版保持一致。
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/checkin"
	"github.com/rockswang/workbuddy-wild/internal/config"
	"github.com/rockswang/workbuddy-wild/internal/notify"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
	"github.com/rockswang/workbuddy-wild/internal/scheduler"
	"github.com/rockswang/workbuddy-wild/internal/server"
	"github.com/rockswang/workbuddy-wild/internal/smzdm"
	"github.com/rockswang/workbuddy-wild/internal/traework"
	"github.com/rockswang/workbuddy-wild/internal/upstream"
	"github.com/rockswang/workbuddy-wild/internal/webapi"
	"github.com/rockswang/workbuddy-wild/internal/wnflb"
)

// version 管理页展示的版本号（构建时 -ldflags "-X main.version=..." 注入）。
var version = "0.6.9"

//go:embed shim.js
var shimJS string

//go:embed checkin.html
var checkinHTML string

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json (不存在则用默认配置 + WB2A_* 环境变量)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行（常见于 Docker）：纯默认 + 环境变量，不落盘 config.json
			//（避免把环境变量"烤进"文件导致后续改 env 不生效）。
			log.Printf("config %s 不存在，使用默认配置 + 环境变量", *cfgPath)
			cfg, err = config.Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	stateDir := filepath.Dir(cfg.StateFile)
	_ = os.MkdirAll(cfg.AuthDir, 0o755)
	_ = os.MkdirAll(stateDir, 0o755)

	setupLogging(stateDir)

	// ---- 装配（与桌面版 main.go 一致）：双平台各自独立 pool + scheduler ----
	wbAuths, err := auth.LoadWorkBuddyDir(cfg.AuthDir, cfg.Region)
	if err != nil {
		log.Fatalf("读取 WorkBuddy 账号目录失败：%v", err)
	}
	trAuths, err := auth.LoadTraeDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("读取 TraeWork 账号目录失败：%v", err)
	}

	wbPool := pool.New(filepath.Join(stateDir, "state-workbuddy.json"))
	for _, a := range wbAuths {
		wbPool.Add(a)
	}
	trPool := pool.New(filepath.Join(stateDir, "state-traework.json"))
	for _, a := range trAuths {
		trPool.Add(a)
	}
	log.Printf("loaded accounts: workbuddy=%d %s, traework=%d from %s",
		len(wbAuths), cfg.Region, len(trAuths), cfg.AuthDir)

	if strat, ok := pool.ParseStrategy(cfg.Strategy); ok {
		wbPool.SetStrategy(strat)
		trPool.SetStrategy(strat)
		log.Printf("选号策略：%s（%s）", strat, strat.Label())
	}

	wbUp := upstream.New()
	wbUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	trUp := traework.New()
	trUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second

	checkinMinutes, err := config.ParseClockTimes(cfg.Schedule.CheckinTimes)
	if err != nil {
		log.Fatalf("解析签到时间失败：%v", err)
	}
	wbSch := scheduler.New(scheduler.Config{Pool: wbPool, Upstream: wbUp, Name: "workbuddy",
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours})
	trSch := scheduler.New(scheduler.Config{Pool: trPool, Upstream: trUp, Name: "traework",
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours})

	runtimes := map[provider.Kind]*server.Runtime{
		provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: wbUp, StaticModels: server.WorkBuddyStaticModels()},
		provider.TraeWork:  {Kind: provider.TraeWork, Pool: trPool, Upstream: trUp, StaticModels: server.TraeWorkStaticModels()},
	}
	h := server.NewHandler(server.Config{
		Runtimes: runtimes, APIKey: cfg.APIKey, MaxRotate: cfg.MaxRotate,
		HardCooldown: cfg.HardCreditDur, SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh, ErrCooldown: cfg.ErrCooldownDur,
	})

	// ---- 福利吧签到（独立模块，单账号，Go 原生实现） ----
	wnflbSvc := wnflb.New(filepath.Join(stateDir, "wnflb"), os.Getenv("WNFLB_BASE_URL"))
	wnflbTimes := strings.TrimSpace(os.Getenv("WNFLB_CHECKIN_TIMES"))
	if wnflbTimes == "" {
		wnflbTimes = "01:00,22:00"
	}
	// .env 引导：无存档账号且给了账号密码时自动导入（网页登录仍可覆盖）。
	if u, p := os.Getenv("FORUM_USERNAME"), os.Getenv("FORUM_PASSWORD"); u != "" && p != "" {
		if wnflbSvc.ImportAccountFromEnv(u, p) {
			log.Printf("wnflb: 已从环境变量导入论坛账号")
		}
	}

	// ---- 什么值得买签到（独立模块，单账号，Go 原生实现） ----
	// 协议逆向来自 https://github.com/enwaiax/smzdm-bot（Apache-2.0），仅移植签到部分。
	smzdmSvc := smzdm.New(filepath.Join(stateDir, "smzdm"), os.Getenv("SMZDM_BASE_URL"))
	smzdmTimes := strings.TrimSpace(os.Getenv("SMZDM_CHECKIN_TIMES"))
	if smzdmTimes == "" {
		smzdmTimes = "09:00"
	}
	// .env 引导：无存档 Cookie 且给了 Cookie 时自动导入（网页提交仍可覆盖）。
	if ck := strings.TrimSpace(os.Getenv("SMZDM_COOKIE")); ck != "" {
		if smzdmSvc.ImportCookieFromEnv(ck) {
			log.Printf("smzdm: 已从环境变量导入 Cookie")
		}
	}

	// ---- 签到结果 Bark 推送通知 ----
	bark, barkErr := notify.Open(filepath.Join(stateDir, "notify"))
	if barkErr != nil {
		log.Printf("notify: Bark 通知初始化失败（签到推送不可用）: %v", barkErr)
	} else {
		wnflbSvc.SetNotifier(bark)
		smzdmSvc.SetNotifier(bark)
	}

	api := webapi.New(webapi.Options{
		Config: cfg, ConfigPath: *cfgPath,
		Runtimes: map[provider.Kind]*webapi.Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: wbUp, Scheduler: wbSch},
			provider.TraeWork:  {Kind: provider.TraeWork, Pool: trPool, Upstream: trUp, Scheduler: trSch},
		},
		Handler: h, Version: version, StateDir: stateDir,
		PublicBase:       publicBase(cfg),
		SetListen:        setListen,
		Wnflb:            wnflbSvc,
		WnflbTimes:       wnflbTimes,
		Smzdm:            smzdmSvc,
		SmzdmTimes:       smzdmTimes,
		Notify:           bark,
		CatchupOnStartup: strings.ToLower(strings.TrimSpace(os.Getenv("WB2A_CATCHUP_ON_STARTUP"))) != "false",
	})
	wbSch.SetCheckinObserver(func(r scheduler.CheckinResult) { api.NotifyCheckin("workbuddy", r) })
	trSch.SetCheckinObserver(func(r scheduler.CheckinResult) { api.NotifyCheckin("traework", r) })
	wbSch.SetRefreshObserver(func(uid string, ok bool, msg string) { api.NotifyRefresh("workbuddy", uid, ok, msg) })
	trSch.SetRefreshObserver(func(uid string, ok bool, msg string) { api.NotifyRefresh("traework", uid, ok, msg) })

	// ---- 路由 ----
	mux := http.NewServeMux()
	// 原有 OpenAI 兼容接口（行为不变，仍走 handler 内部鉴权）。
	mux.Handle("/v1/", h)
	mux.Handle("/status", h)
	mux.Handle("/healthz", h)
	// 管理 API。
	api.Register(mux)
	// 静态管理页（复用桌面版前端 + 注入垫片）。
	mountFrontend(mux)

	// ---- 调度器后台运行 ----
	sctx, stop := context.WithCancel(context.Background())
	defer stop()
	go wbSch.Run(sctx)
	go trSch.Run(sctx)
	// 主账号兜底补签：启动后对"已错过签到时刻且今日未签"的账号补签
	//（WB2A_CATCHUP_ON_STARTUP=false 可关）。
	catchupOnStartup := strings.ToLower(strings.TrimSpace(os.Getenv("WB2A_CATCHUP_ON_STARTUP"))) != "false"
	if catchupOnStartup {
		go func() {
			select {
			case <-sctx.Done():
				return
			case <-time.After(15 * time.Second):
			}
			wbSch.CatchUpMissed()
			trSch.CatchUpMissed()
		}()
	}
	// 福利吧每日签到（独立模块）。
	wnflbRunOnStartup := strings.ToLower(strings.TrimSpace(os.Getenv("WNFLB_RUN_ON_STARTUP"))) != "false"
	go checkin.RunScheduler(sctx, checkin.ParseTimes(wnflbTimes), wnflbRunOnStartup, "福利吧签到", wnflbSvc.AutoCheckin)
	// 什么值得买每日签到（独立模块）。
	smzdmRunOnStartup := strings.ToLower(strings.TrimSpace(os.Getenv("SMZDM_RUN_ON_STARTUP"))) != "false"
	go checkin.RunScheduler(sctx, checkin.ParseTimes(smzdmTimes), smzdmRunOnStartup, "什么值得买签到", smzdmSvc.AutoCheckin)

	// ---- HTTP 服务 ----
	if err := setListen(cfg.Listen.Host, cfg.Listen.Port); err != nil {
		log.Fatalf("listen %s failed: %v", cfg.Listen.Addr(), err)
	}
	log.Printf("workbuddy-wild-web %s 已启动", version)
	log.Printf("管理页： http://<本机IP>:%d/  （API Key 为空则免鉴权，否则先在页面输入）", cfg.Listen.Port)
	log.Printf("OpenAI 兼容 API：http://<本机IP>:%d/v1", cfg.Listen.Port)

	// ---- 信号：优雅退出 ----
	ctx, sigStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer sigStop()
	<-ctx.Done()
	log.Printf("收到退出信号，优雅关闭…")
	stop()
	shutdownHTTP()
	log.Printf("bye")
}

// ---------------------------------------------------------------------------
// HTTP 监听（热切换，语义同 app.serveLocked：失败保持原监听）
// ---------------------------------------------------------------------------

var (
	srvMu   sync.Mutex
	httpSrv *http.Server
	httpMux *http.ServeMux
)

func setListen(host string, port int) error {
	addr := config.Listen{Host: host, Port: port}.Addr()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: httpMux, ReadHeaderTimeout: 30 * time.Second}
	srvMu.Lock()
	old := httpSrv
	httpSrv = srv
	srvMu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("http serve %s: %v", addr, err)
		}
	}()
	if old != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = old.Shutdown(shutdownCtx)
	}
	log.Printf("listening on %s", addr)
	return nil
}

func shutdownHTTP() {
	srvMu.Lock()
	srv := httpSrv
	httpSrv = nil
	srvMu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// ---------------------------------------------------------------------------
// 静态管理页
// ---------------------------------------------------------------------------

// frontendDir 管理页静态文件目录（默认 ./frontend/dist，可用 WB2A_FRONTEND_DIR 覆盖）。
func frontendDir() string {
	if d := os.Getenv("WB2A_FRONTEND_DIR"); d != "" {
		return d
	}
	return "./frontend/dist"
}

func mountFrontend(mux *http.ServeMux) {
	dir := frontendDir()
	httpMux = mux

	// /shim.js：兼容垫片（内嵌，独立于前端目录）。
	mux.HandleFunc("/shim.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		_, _ = io.WriteString(w, shimJS)
	})

	// /app.js /style.css：桌面版前端原文件（不修改）。
	mux.HandleFunc("/app.js", serveFile(filepath.Join(dir, "app.js"), "application/javascript; charset=utf-8"))
	mux.HandleFunc("/style.css", serveFile(filepath.Join(dir, "style.css"), "text/css; charset=utf-8"))

	// /checkin/：签到中心（所有签到模块统一管理，内嵌）。
	mux.HandleFunc("/checkin/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, checkinHTML)
	})
	// /wnflb/：旧地址，跳转到签到中心。
	mux.HandleFunc("/wnflb/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/checkin/", http.StatusFound)
	})

	// /：index.html + 注入垫片（在 app.js 之前加载，保证 window.go 先就绪）。
	indexRaw, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		log.Printf("警告：前端目录 %s 缺少 index.html，管理页不可用（API 正常）: %v", dir, err)
		indexRaw = []byte("<html><body>frontend not found</body></html>")
	}
	indexHTML := injectShim(string(indexRaw))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, indexHTML)
	})
}

// injectShim 在 app.js 引用之前插入垫片 script 标签。
func injectShim(html string) string {
	tag := `<script src="/shim.js"></script>`
	if strings.Contains(html, `<script src="./app.js">`) {
		return strings.Replace(html, `<script src="./app.js">`, tag+`<script src="./app.js">`, 1)
	}
	if strings.Contains(html, "</body>") {
		return strings.Replace(html, "</body>", tag+"</body>", 1)
	}
	return tag + html
}

func serveFile(fp, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		http.ServeFile(w, r, fp)
	}
}

// ---------------------------------------------------------------------------
// TraeWork OAuth 回调的公开基址。
// 默认为 http://127.0.0.1:端口（浏览器与服务端同机时直接可用，如 SSH 端口转发场景）；
// 跨机器浏览器访问时，请设置 WB2A_PUBLIC_URL（如 http://服务器IP:7863）。
// ---------------------------------------------------------------------------

func publicBase(cfg *config.Config) string {
	if v := strings.TrimRight(os.Getenv("WB2A_PUBLIC_URL"), "/"); v != "" {
		return v
	}
	return fmt.Sprintf("http://127.0.0.1:%d", cfg.Listen.Port)
}

// ---------------------------------------------------------------------------
// 日志：同时写 stdout（docker logs）与 data/app.log（5MB 轮转，语义同桌面版）
// ---------------------------------------------------------------------------

func setupLogging(stateDir string) {
	fp := filepath.Join(stateDir, "app.log")
	rw := &rotatingWriter{fp: fp}
	if f, err := os.OpenFile(fp, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		rw.f = f
	} else {
		log.Printf("打开日志文件失败 %s: %v（仅输出到 stdout）", fp, err)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(io.MultiWriter(os.Stdout, rw))
}

// rotatingWriter 5MB 轮转的文件 writer（app.log → app.log.1）。
type rotatingWriter struct {
	mu      sync.Mutex
	fp      string
	f       *os.File
	maxSize int64
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		if w.maxSize <= 0 {
			w.maxSize = 5 << 20
		}
		if fi, err := w.f.Stat(); err == nil && fi.Size() >= w.maxSize {
			_ = w.f.Close()
			_ = os.Remove(w.fp + ".1")
			_ = os.Rename(w.fp, w.fp+".1")
			if f, err := os.OpenFile(w.fp, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
				w.f = f
			} else {
				w.f = nil
			}
			log.Printf("日志已轮转（旧日志见 app.log.1）")
		}
	}
	if w.f == nil {
		return len(p), nil
	}
	return w.f.Write(p)
}
