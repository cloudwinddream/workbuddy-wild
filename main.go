//go:build windows

// main.go WorkBuddy-Wild 托盘 GUI 入口。
// 单进程集成：OpenAI 兼容 HTTP 服务 + 自动签到调度器 + 系统托盘管理面板。
// 依赖：Windows 10 21H2+（WebView2 运行时）或无 WebView2 时自动安装；无 Go/Python/Docker/Bash。
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/energye/systray"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/rockswang/workbuddy-wild/internal/app"
	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/config"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
	"github.com/rockswang/workbuddy-wild/internal/scheduler"
	"github.com/rockswang/workbuddy-wild/internal/server"
	"github.com/rockswang/workbuddy-wild/internal/traework"
	"github.com/rockswang/workbuddy-wild/internal/upstream"
	"github.com/rockswang/workbuddy-wild/internal/winutil"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/trayicon.ico
var trayIconICO []byte

func main() {
	// 工作目录固定为 exe 所在目录，保证相对路径配置（./auths ./data）稳定
	if exe, err := os.Executable(); err == nil {
		_ = os.Chdir(filepath.Dir(exe))
	}

	// ⚠️ 启动自检：确认本 exe 在磁盘上真的可读。
	//
	// 背景（v0.5.7 线上事故）：更新程序下载新版 exe 覆盖运行时，目标文件会被
	// 拒绝写入或被截断/部分写入。此时磁盘上留下一个**损坏或半截的 exe**：
	//   - 已运行的旧进程不受影响（它已映射到内存）
	//   - 系统仍允许"启动"它（PE 头可能完好），于是日志里能看到"已启动"
	//   - 但后续一切 GUI 初始化全部失败，窗口/托盘都不出现，且**几乎没有日志**
	// 表现为「窗口弹不出来」，且极难从日志定位。
	//
	// 本自检在"已启动"日志前后都不依赖，能第一时间给出可操作的错误提示。
	if err := verifySelfReadable(); err != nil {
		msg := "程序文件可能已损坏或不完整（常见于更新过程中被中断）。\n\n" +
			"错误：" + err.Error() + "\n\n" +
			"请重新下载完整版本，并确保更新时先退出旧版本再替换文件。"
		log.Printf("启动自检失败: %v", err)
		winutil.MessageBox(msg)
		os.Exit(1)
	}

	// 进程级单实例锁：必须在 HTTP 服务/调度器/WebView2 之前检查。
	// 否则第二个实例会 bind 失败 + 残留僵尸进程（旧实例卡死退不出时尤其危险）。
	if !winutil.AcquireSingleInstance() {
		log.Printf("已有实例在运行，本实例退出")
		os.Exit(0)
	}

	cfgPath := "config.json"
	cfg, err := config.Load(cfgPath)
	// --autostart 由开机自启注册表项附带：开机启动不弹“已启动”提示
	autostart := false
	for _, arg := range os.Args[1:] {
		if arg == "--autostart" {
			autostart = true
		}
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：生成默认配置
			log.Printf("config.json 不存在，使用默认配置并生成")
			cfg = config.Default()
			if err := config.Save(cfg, cfgPath); err != nil {
				log.Printf("write default config: %v", err)
			}
		} else {
			fatal("加载配置失败：%v\n\n请检查 config.json 后重新启动", err)
		}
	}
	_ = os.MkdirAll(cfg.AuthDir, 0o755)
	_ = os.MkdirAll(filepath.Dir(cfg.StateFile), 0o755)

	// 双平台运行时：workbuddy / traework 各自独立 pool + state + scheduler。
	stateDir := filepath.Dir(cfg.StateFile)
	wbAuths, err := auth.LoadWorkBuddyDir(cfg.AuthDir, cfg.Region)
	if err != nil {
		fatal("读取 WorkBuddy 账号目录失败：%v", err)
	}
	trAuths, err := auth.LoadTraeDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 TraeWork 账号目录失败：%v", err)
	}
	log.Printf("loaded accounts: workbuddy=%d %s, traework=%d from %s", len(wbAuths), cfg.Region, len(trAuths), cfg.AuthDir)

	wbPool := pool.New(filepath.Join(stateDir, "state-workbuddy.json"))
	for _, a := range wbAuths {
		wbPool.Add(a)
	}
	trPool := pool.New(filepath.Join(stateDir, "state-traework.json"))
	for _, a := range trAuths {
		trPool.Add(a)
	}
	// 全局策略：两个平台共用一个选号策略（面板可运行时切换）。
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
		fatal("解析签到时间失败：%v", err)
	}

	wbSch := scheduler.New(scheduler.Config{Pool: wbPool, Upstream: wbUp, Name: "workbuddy", CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours})
	trSch := scheduler.New(scheduler.Config{Pool: trPool, Upstream: trUp, Name: "traework", CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours})

	runtimes := map[provider.Kind]*server.Runtime{
		provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: wbUp, StaticModels: server.WorkBuddyStaticModels()},
		provider.TraeWork:  {Kind: provider.TraeWork, Pool: trPool, Upstream: trUp, StaticModels: server.TraeWorkStaticModels()},
	}
	appRuntimes := map[provider.Kind]*app.Runtime{
		provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: wbUp, Scheduler: wbSch},
		provider.TraeWork:  {Kind: provider.TraeWork, Pool: trPool, Upstream: trUp, Scheduler: trSch},
	}

	h := server.NewHandler(server.Config{
		Runtimes:     runtimes,
		APIKey:       cfg.APIKey,
		MaxRotate:    cfg.MaxRotate,
		HardCooldown: cfg.HardCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
	})

	appInst, err := app.New(app.Options{
		ConfigPath: cfgPath,
		Config:     cfg,
		Runtimes:   appRuntimes,
		Handler:    h,
	})
	if err != nil {
		fatal("初始化失败：%v", err)
	}
	// 定时签到结果实时推送面板；手动签到也复用同一回调。
	wbSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("workbuddy", r) })
	trSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("traework", r) })
	wbSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("workbuddy", uid, ok, msg) })
	trSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("traework", uid, ok, msg) })
	if err := appInst.StartServer(); err != nil {
		log.Printf("listen %s failed: %v（面板中将提示）", cfg.Listen.Addr(), err)
	}

	// 交互式启动（非 --autostart）：立即弹“已启动 + API 地址”提示，不等待窗口/托盘就绪
	if !autostart {
		appInst.ShowStartupNotice()
	}
	// 默认 API-Key 对外监听时弹安全提示（autostart 场景同样提示，防裸奔）
	appInst.WarnWeakAPIKey()

	// 调度器后台运行
	sctx, stop := context.WithCancel(context.Background())
	defer stop()
	go wbSch.Run(sctx)
	go trSch.Run(sctx)

	// 系统托盘：单击 / 双击 / 右击 均弹主面板（无原生右键菜单）
	go runTray(appInst)

	// WebView2 用户数据目录固定在 data/webview：不随 exe 改名变化，
	// 且启动前若有孤儿进程占用（强杀残留）先清理，避免白窗口长时间等待。
	webviewPath := filepath.Join(filepath.Dir(mustExecutable()), "data", "webview")
	if winutil.IsWebViewProfileLocked(webviewPath) {
		log.Printf("检测到孤儿 WebView2 进程占用 %s，正在清理", webviewPath)
		winutil.KillOrphanWebViews(webviewPath)
	}

	runGUI(appInst, webviewPath)
}

// mustExecutable 返回当前 exe 的绝对路径（失败时退化为相对路径）。
func mustExecutable() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return os.Args[0]
}

// verifySelfReadable 校验本 exe 在磁盘上可完整读取（防"半截更新文件"）。
//
// 判据（任一不满足即判定损坏）：
//   - 文件存在且可打开
//   - 大小 ≥ 1 MiB（本程序 12MB+，远大于此；截断文件会明显偏小）
//   - 头部是合法 PE（"MZ"）
//
// 这是对 v0.5.7「窗口弹不出来」事故的直接防护：更新中断会留下损坏文件，
// 它能让进程"启动"却在 GUI 初始化阶段静默失败，日志几乎为空。
func verifySelfReadable() error {
	exe := mustExecutable()
	fi, err := os.Stat(exe)
	if err != nil {
		return fmt.Errorf("无法读取程序文件 %s: %w", filepath.Base(exe), err)
	}
	const minSize = 1 << 20 // 1 MiB
	if fi.Size() < minSize {
		return fmt.Errorf("程序文件仅 %d 字节（预期 ≥1MiB），疑似不完整", fi.Size())
	}
	f, err := os.Open(exe)
	if err != nil {
		return fmt.Errorf("无法打开程序文件: %w", err)
	}
	defer f.Close()
	var magic [2]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return fmt.Errorf("读取程序头部失败: %w", err)
	}
	if magic[0] != 'M' || magic[1] != 'Z' {
		return fmt.Errorf("程序头部非法（%q，非 PE 可执行文件）", string(magic[:]))
	}
	return nil
}

// fatal 记录日志并弹出 MessageBox 后退出（GUI 无控制台，错误必须可见）。
func fatal(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("%s", msg)
	winutil.MessageBox(msg)
	os.Exit(1)
}

// runGUI 启动 wails 窗口；若窗口创建失败（无交互桌面/WebView2 异常），
// 捕获 panic 后保持 HTTP 服务与调度器继续运行（无头兜底）。
func runGUI(a *app.App, webviewPath string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("GUI 启动失败，以无头模式继续运行: %v", r)
			select {} // 保持进程存活（HTTP 服务 + 调度器仍在跑）
		}
	}()
	err := wails.Run(&options.App{
		Title:             "WorkBuddy-Wild",
		Width:             760,
		Height:            560,
		MinWidth:          680,
		MinHeight:         420,
		Frameless:         true,
		StartHidden:       true,
		AlwaysOnTop:       true,
		HideWindowOnClose: true,
		BackgroundColour:  options.NewRGB(246, 246, 248),
		AssetServer:       &assetserver.Options{Assets: assets},
		Windows: &windows.Options{
			WebviewUserDataPath: webviewPath,
			// 禁用 WebView2 GPU 加速：老显卡/驱动下 GPU 渲染进程是 WebView2
			// 卡死与崩溃的高发原因，软件渲染更稳（面板为轻量 UI，性能无影响）。
			WebviewGpuIsDisabled: true,
		},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "workbuddy-wild-gui-v1",
			OnSecondInstanceLaunch: func(d options.SecondInstanceData) {
				a.ShowSecondInstanceNotice() // 已在运行：双击 exe 询问打开面板或退出
			},
		},
		OnStartup:  func(ctx context.Context) { a.OnStartup(ctx) },
		OnDomReady: func(ctx context.Context) { a.OnDomReady(ctx) },
		OnShutdown: func(ctx context.Context) { a.OnShutdown(ctx) },
		Bind:       []interface{}{a},
	})
	if err != nil {
		log.Printf("wails: %v（以无头模式继续运行）", err)
		select {}
	}
}

// runTray 托盘生命周期：单击/双击/右击均弹主面板（不提供原生右键菜单）。
// 说明：energye/systray 的原生菜单（AddMenuItem + ShowMenu）的 TrackPopupMenu
// 模态循环会跑在托盘消息循环线程上，菜单异常不关闭时托盘永久卡死；
// goroutine 里弹菜单又不可靠（无线程消息队列）。权衡后取消右键菜单，
// 右键与单击一致直接弹面板。关闭程序请在面板右上角 ✕ 或底部“退出”（带确认）。
// 回调全部 go 化：托盘消息循环线程只做投递，绝不执行重量级逻辑。
func runTray(a *app.App) {
	started := make(chan struct{})
	systray.Run(func() {
		systray.SetIcon(trayIconICO)
		systray.SetTooltip("WorkBuddy-Wild — 托盘管理面板")
		systray.SetOnClick(func(systray.IMenu) { go a.ShowPanel() })
		systray.SetOnDClick(func(systray.IMenu) { go a.ShowPanel() })
		systray.SetOnRClick(func(systray.IMenu) { go a.ShowPanel() })
		close(started)
	}, func() {})
	// ⚠️ 托盘是打开面板的**唯一入口**。若 systray 初始化失败，onReady 永不执行、
	// 图标永不出现、窗口也永远弹不出来 —— 而日志此前一片空白，用户只能看到
	// "程序好像启动了但没反应"。
	//
	// 注意：systray.Run 在正常退出时也会返回，因此这里只报告"从未完成初始化"
	// 这一种情况（started 未关闭），避免关闭程序时误报。
	select {
	case <-started:
		// 正常情况下走到这里说明 systray 已退出（进程正在关闭），无需提示
	default:
		log.Printf("托盘初始化失败：systray 在显示图标前退出，面板入口不可用")
		winutil.InfoBox("托盘图标未能显示",
			"系统托盘初始化失败，点击托盘打开面板的入口不可用。\n\n"+
				"HTTP 服务与自动签到仍在正常运行。\n\n"+
				"可尝试：\n"+
				"1) 重启程序\n"+
				"2) 检查是否有安全软件拦截了托盘操作\n\n"+
				"API 地址：http://"+a.ListenAddr())
	}
}
