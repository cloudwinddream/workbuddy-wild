package webapi

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/rockswang/workbuddy-wild/internal/config"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/scheduler"
)

// ---------------------------------------------------------------------------
// GET /api/state  → State（字段与 app.GetState 一致）
// ---------------------------------------------------------------------------

func (a *API) handleState(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	listenHost, listenPort, apiKey := a.cfg.Listen.Host, a.cfg.Listen.Port, a.cfg.APIKey
	a.mu.Unlock()
	st := State{
		Accounts:       a.accountViews(),
		CheckinHours:   a.checkinHours(),
		CheckinTimes:   a.checkinTimes(),
		KeepaliveHours: a.keepaliveHours(),
		ListenHost:     listenHost,
		ListenPort:     listenPort,
		APIKey:         apiKey,
		LoginBusy:      a.loginBusy(),
		NextCheckin:    fmtTime(a.nextFire()),
		Version:        a.version,
		Autostart:      false, // Web 版无开机自启概念（用容器 restart 策略）
		Running:        true,
		Strategy:       string(a.currentStrategy()),
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *API) checkinHours() []int {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.CheckinHours()
	}
	return nil
}

// GET /api/accounts → []AccountView（供前端 1.5s 轮询）
func (a *API) handleAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.accountViews())
}

// ---------------------------------------------------------------------------
// 签到 / 刷新
// ---------------------------------------------------------------------------

// POST /api/checkin/account  {uid} → CheckinResult
func (a *API) handleCheckinAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	if err := decodeJSON(r, &req); err != nil || req.UID == "" {
		writeErr(w, http.StatusBadRequest, "missing uid")
		return
	}
	rt, _ := a.findRuntimeAuth(req.UID)
	if rt == nil || rt.Scheduler == nil {
		writeErr(w, http.StatusNotFound, "unknown account "+shortUID(req.UID))
		return
	}
	res, err := rt.Scheduler.CheckinAccount(req.UID)
	if err != nil {
		log.Printf("webapi checkin failed uid=%s err=%v", req.UID, err)
		res = scheduler.CheckinResult{UID: req.UID, Msg: err.Error()}
	} else {
		log.Printf("webapi checkin uid=%s ok=%t msg=%s remain=%d has_remain=%t",
			req.UID, res.OK, res.Msg, res.Remain, res.HasRemain)
	}
	writeJSON(w, http.StatusOK, res)
}

// POST /api/checkin/all → []CheckinResult（逻辑同 app.CheckinAll）
func (a *API) handleCheckinAll(w http.ResponseWriter, r *http.Request) {
	results := make([]scheduler.CheckinResult, 0)
	for _, rt := range a.runtimes {
		if rt == nil || rt.Pool == nil || rt.Scheduler == nil {
			continue
		}
		for _, st := range rt.Pool.List() {
			if st.Disabled {
				res := scheduler.CheckinResult{UID: st.UID, Msg: "账号已禁用"}
				results = append(results, res)
				log.Printf("webapi checkin result platform=%s uid=%s ok=false msg=%s", rt.Kind, st.UID, res.Msg)
				continue
			}
			res, err := rt.Scheduler.CheckinAccount(st.UID)
			if err != nil {
				res = scheduler.CheckinResult{UID: st.UID, Msg: err.Error()}
			}
			log.Printf("webapi checkin result platform=%s uid=%s ok=%t msg=%s", rt.Kind, st.UID, res.OK, res.Msg)
			results = append(results, res)
		}
	}
	ok := 0
	for _, res := range results {
		if res.OK {
			ok++
		}
	}
	log.Printf("webapi 批量签到完成：total=%d ok=%d failed=%d", len(results), ok, len(results)-ok)
	writeJSON(w, http.StatusOK, results)
}

// POST /api/refresh/account  {uid} → {uid, credits}
func (a *API) handleRefreshAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	if err := decodeJSON(r, &req); err != nil || req.UID == "" {
		writeErr(w, http.StatusBadRequest, "missing uid")
		return
	}
	rt, au := a.findRuntimeAuth(req.UID)
	if rt == nil || au == nil {
		writeErr(w, http.StatusNotFound, "unknown account "+shortUID(req.UID))
		return
	}
	log.Printf("webapi credits refresh start platform=%s uid=%s", rt.Kind, req.UID)
	remain, err := rt.Upstream.UserResource(au)
	if err != nil {
		log.Printf("webapi credits refresh failed platform=%s uid=%s err=%v", rt.Kind, req.UID, err)
		writeErr(w, http.StatusBadGateway, "刷新积分失败："+shortErr(err))
		return
	}
	rt.Pool.SetCredits(req.UID, remain)
	log.Printf("webapi credits refresh success platform=%s uid=%s remain=%d", rt.Kind, req.UID, remain)
	writeJSON(w, http.StatusOK, map[string]any{"uid": req.UID, "credits": remain})
}

// POST /api/refresh/all → {ok, refreshed, failed}
func (a *API) handleRefreshAll(w http.ResponseWriter, r *http.Request) {
	refreshed, failed := 0, 0
	for _, rt := range a.runtimes {
		if rt == nil || rt.Pool == nil || rt.Upstream == nil {
			continue
		}
		for _, st := range rt.Pool.List() {
			au := rt.Pool.AuthByUID(st.UID)
			if au == nil || au.AccessToken == "" {
				continue
			}
			if remain, err := rt.Upstream.UserResource(au); err == nil {
				rt.Pool.SetCredits(st.UID, remain)
				refreshed++
			} else {
				failed++
				log.Printf("webapi credits refresh failed platform=%s uid=%s err=%v", rt.Kind, st.UID, err)
			}
		}
	}
	log.Printf("webapi 刷新全部积分完成：refreshed=%d failed=%d", refreshed, failed)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "refreshed": refreshed, "failed": failed})
}

// ---------------------------------------------------------------------------
// DELETE /api/account/{uid}
// ---------------------------------------------------------------------------

func (a *API) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeErr(w, http.StatusBadRequest, "missing uid")
		return
	}
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil {
		writeErr(w, http.StatusNotFound, "unknown account "+shortUID(uid))
		return
	}
	if au.FilePath != "" {
		_ = os.Remove(au.FilePath)
	}
	rt.Pool.Remove(uid)
	log.Printf("webapi 已删除账号 %s", shortUID(uid))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 策略
// ---------------------------------------------------------------------------

// GET /api/strategy → {strategy}
func (a *API) handleGetStrategy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"strategy": string(a.currentStrategy())})
}

// PUT /api/strategy  {strategy} → {ok, strategy}（逻辑同 app.SetStrategy）
func (a *API) handleSetStrategy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Strategy string `json:"strategy"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	strat, ok := pool.ParseStrategy(req.Strategy)
	if !ok {
		writeErr(w, http.StatusBadRequest, "未知策略："+req.Strategy)
		return
	}
	for _, rt := range a.runtimes {
		if rt != nil && rt.Pool != nil {
			rt.Pool.SetStrategy(strat)
		}
	}
	a.mu.Lock()
	a.cfg.Strategy = string(strat)
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存配置失败："+err.Error())
		return
	}
	log.Printf("webapi 选号策略已切换：%s（%s）", strat, strat.Label())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "strategy": string(strat)})
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

// GET /api/config → 当前可在线修改的配置子集
func (a *API) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"listen_host":     a.cfg.Listen.Host,
		"listen_port":     a.cfg.Listen.Port,
		"api_key":         a.cfg.APIKey,
		"checkin_times":   a.cfg.Schedule.CheckinTimes,
		"keepalive_hours": a.cfg.Schedule.KeepaliveHours,
		"max_rotate":      a.cfg.MaxRotate,
		"strategy":        a.cfg.Strategy,
		"version":         a.version,
	})
}

// PUT /api/config  部分更新：checkin_times / api_key / max_rotate /
// listen_host / listen_port。checkin_times 与 api_key 即时生效；
// max_rotate 需重启生效（handler 启动时快照）；listen 热切换。
func (a *API) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CheckinTimes *[]string `json:"checkin_times"`
		APIKey       *string   `json:"api_key"`
		MaxRotate    *int      `json:"max_rotate"`
		ListenHost   *string   `json:"listen_host"`
		ListenPort   *int      `json:"listen_port"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}

	notes := []string{}

	// 签到时间：校验 → 写回 → 唤醒两个调度器（逻辑同 app.SetCheckinTimes）。
	if req.CheckinTimes != nil {
		minutes, err := config.ParseClockTimes(*req.CheckinTimes)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "checkin_times 非法："+err.Error())
			return
		}
		clean := normalizeMinutes(minutes)
		if len(clean) == 0 {
			writeErr(w, http.StatusBadRequest, "请至少保留一个签到时间")
			return
		}
		times := make([]string, 0, len(clean))
		for _, m := range clean {
			times = append(times, fmt.Sprintf("%02d:%02d", m/60, m%60))
		}
		a.mu.Lock()
		a.cfg.Schedule.CheckinTimes = times
		a.cfg.Schedule.CheckinHours = nil
		err = config.Save(a.cfg, a.cfgPath)
		a.mu.Unlock()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "保存配置失败："+err.Error())
			return
		}
		for _, rt := range a.runtimes {
			if rt != nil && rt.Scheduler != nil {
				rt.Scheduler.SetCheckinMinutes(clean)
			}
		}
		// 同步第三方模块（福利吧 / 什么值得买）的调度时间源：全部签到
		// 共用同一套签到时间。
		if a.checkinTimesChanged != nil {
			a.checkinTimesChanged(times)
		}
		log.Printf("webapi 自动签到时间已更新：%s", strings.Join(times, "、"))
	}

	// API-Key：写回 + /v1 与 /api 同时即时生效。
	if req.APIKey != nil {
		key := strings.TrimSpace(*req.APIKey)
		a.mu.Lock()
		a.cfg.APIKey = key
		err := config.Save(a.cfg, a.cfgPath)
		a.mu.Unlock()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "保存配置失败："+err.Error())
			return
		}
		a.applyAPIKey(key)
		log.Printf("webapi API-Key 已更新")
	}

	// max_rotate：仅写回，重启生效（handler 在 NewHandler 时快照了该值）。
	if req.MaxRotate != nil {
		n := *req.MaxRotate
		if n <= 0 {
			n = 3
		}
		if n > 20 {
			n = 20
		}
		a.mu.Lock()
		a.cfg.MaxRotate = n
		err := config.Save(a.cfg, a.cfgPath)
		a.mu.Unlock()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "保存配置失败："+err.Error())
			return
		}
		notes = append(notes, "max_rotate 已保存，重启后生效")
		log.Printf("webapi max_rotate 已更新为 %d（重启生效）", n)
	}

	// 监听地址：热切换（失败保持原样），逻辑同 app.SetListen。
	if req.ListenHost != nil || req.ListenPort != nil {
		a.mu.Lock()
		host, port := a.cfg.Listen.Host, a.cfg.Listen.Port
		a.mu.Unlock()
		if req.ListenHost != nil {
			host = *req.ListenHost
		}
		if req.ListenPort != nil {
			port = *req.ListenPort
		}
		if port <= 0 || port > 65535 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("端口无效：%d", port))
			return
		}
		if a.setListen == nil {
			writeErr(w, http.StatusInternalServerError, "当前不支持热切换监听")
			return
		}
		if err := a.setListen(host, port); err != nil {
			writeErr(w, http.StatusBadGateway, fmt.Sprintf("监听 %s:%d 失败（可能被占用）：%v", host, port, err))
			return
		}
		a.mu.Lock()
		a.cfg.Listen = config.Listen{Host: host, Port: port}
		serr := config.Save(a.cfg, a.cfgPath)
		a.mu.Unlock()
		if serr != nil {
			log.Printf("webapi save config after listen change: %v", serr)
		}
		log.Printf("webapi API 监听已切换至 %s", config.Listen{Host: host, Port: port}.Addr())
		notes = append(notes, "监听地址已切换，注意浏览器地址栏也要换")
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "notes": notes})
}

// normalizeMinutes 分钟去重/排序/合法性校验（逻辑同 app.normalizeMinutes）。
func normalizeMinutes(minutes []int) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, m := range minutes {
		if m >= 0 && m < 24*60 && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Ints(out)
	return out
}

// ---------------------------------------------------------------------------
// GET /api/logs?lines=200 → {lines:[...]}
// ---------------------------------------------------------------------------

func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			n = p
		}
	}
	if n > 2000 {
		n = 2000
	}
	fp := filepath.Join(a.stateDir, "app.log")
	raw, err := os.ReadFile(fp)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}})
		return
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// ---------------------------------------------------------------------------
// GET /api/events?since=0 → {events:[...], seq}
// 前端垫片轮询此接口，模拟 wails 的 EventsOn("checkin"/"refresh"/"login")。
// ---------------------------------------------------------------------------

func (a *API) handleEvents(w http.ResponseWriter, r *http.Request) {
	var since uint64
	if v := r.URL.Query().Get("since"); v != "" {
		since, _ = strconv.ParseUint(v, 10, 64)
	}
	events, seq := a.eventsSince(since)
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "seq": seq})
}
