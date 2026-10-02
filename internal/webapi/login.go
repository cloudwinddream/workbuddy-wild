package webapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/login"
	logintrae "github.com/rockswang/workbuddy-wild/internal/login_trae"
	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// loginTimeout 单次登录流程的最长有效期（与桌面端一致）。
const loginTimeout = 5 * time.Minute

// loginSession 一次进行中的登录流程。
type loginSession struct {
	id        string // 32 hex 字符，回调/轮询凭据
	kind      provider.Kind
	client    *http.Client
	statePath string
	deadline  time.Time
}

// loginReaper 后台清理过期登录会话（每分钟一次）。
func (a *API) loginReaper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		a.loginMu.Lock()
		if a.active != nil && time.Now().After(a.active.deadline) {
			_ = os.Remove(a.active.statePath)
			log.Printf("webapi: 登录会话 %s 超时清理", a.active.id[:8])
			a.active = nil
		}
		a.loginMu.Unlock()
	}
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *API) traeAccountCount() int {
	if rt := a.runtime(provider.TraeWork); rt != nil && rt.Pool != nil {
		return len(rt.Pool.List())
	}
	return 0
}

// ---------------------------------------------------------------------------
// POST /api/login/start  {platform: workbuddy|traework}
// → {auth_url, state, expires_in, hint}
// ---------------------------------------------------------------------------

func (a *API) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Platform string `json:"platform"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	k := provider.Kind(strings.TrimSpace(req.Platform))
	if k == "" {
		k = provider.WorkBuddy
	}
	if k != provider.WorkBuddy && k != provider.TraeWork {
		writeErr(w, http.StatusBadRequest, "unknown platform: "+req.Platform)
		return
	}

	a.loginMu.Lock()
	if a.active != nil && time.Now().Before(a.active.deadline) {
		a.loginMu.Unlock()
		writeErr(w, http.StatusConflict, "已有登录流程进行中，请先完成或取消")
		return
	}
	if a.active != nil {
		_ = os.Remove(a.active.statePath)
		a.active = nil
	}
	sess := &loginSession{id: newSessionID(), kind: k, deadline: time.Now().Add(loginTimeout)}
	sess.statePath = filepath.Join(a.stateDir, "weblogin-"+sess.id+".json")
	// 先标记 busy 再调上游（上游可能耗时 30s+，锁不能跨上游调用，
	// 否则这段时间内 cancel/poll 会被 wedged）。
	a.active = sess
	a.loginMu.Unlock()

	var authURL, hint string
	var startErr error
	if k == provider.TraeWork {
		sess.client = logintrae.NewClient()
		// 设备号分配策略与桌面端一致：按已有 TraeWork 账号数轮转真实设备号，
		// 取不到时 PrepareHeadless 内部回退随机值。
		deviceID := logintrae.NextClientDeviceID(a.traeAccountCount())
		callbackURL := strings.TrimRight(a.pubBase, "/") + "/api/login/traecb?sid=" + sess.id
		authURL, startErr = logintrae.PrepareHeadless(sess.statePath, deviceID, callbackURL)
		if startErr == nil {
			hint = "请在浏览器中打开授权链接完成 TraeWork 登录；登录成功后页面会自动回调本服务，回调地址需能从你的浏览器访问。"
		}
	} else {
		sess.client = login.NewClient()
		authURL, startErr = login.Start(sess.client, sess.statePath)
		if startErr == nil {
			// 与桌面端一致：手动跟随跳转链，浏览器直接打开最终地址。
			if resolved, rerr := login.ResolveAuthURL(sess.client, authURL); rerr == nil && resolved != "" {
				authURL = resolved
			}
			hint = "请在浏览器中打开授权链接完成 WorkBuddy 登录，本页面会自动检测登录结果。"
		}
	}

	a.loginMu.Lock()
	if a.active != sess {
		// 发起过程中被取消/超时清理了。
		a.loginMu.Unlock()
		_ = os.Remove(sess.statePath)
		writeErr(w, http.StatusConflict, "登录已在发起过程中被取消")
		return
	}
	if startErr != nil {
		a.active = nil
		a.loginMu.Unlock()
		_ = os.Remove(sess.statePath)
		if k == provider.TraeWork {
			writeErr(w, http.StatusBadGateway, "发起 TraeWork 登录失败："+startErr.Error())
		} else {
			writeErr(w, http.StatusBadGateway, "发起 WorkBuddy 登录失败："+startErr.Error())
		}
		return
	}
	a.loginMu.Unlock()

	a.emit("login", map[string]any{"phase": "waiting", "msg": "登录流程已发起，等待浏览器完成登录…"})
	log.Printf("webapi: %s 登录流程已发起 sid=%s", k, sess.id[:8])
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_url": authURL, "state": sess.id, "expires_in": int(loginTimeout.Seconds()), "hint": hint,
	})
}

// ---------------------------------------------------------------------------
// GET /api/login/poll?state=  → {phase, msg, uid?, nickname?}
// phase: waiting | success | failed | cancelled
// ---------------------------------------------------------------------------

func (a *API) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("state")
	a.loginMu.Lock()
	sess := a.active
	if sess == nil || sess.id != sid {
		a.loginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"phase": "failed", "msg": "登录会话不存在或已结束"})
		return
	}
	if time.Now().After(sess.deadline) {
		_ = os.Remove(sess.statePath)
		a.active = nil
		a.loginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"phase": "failed", "msg": "登录超时，请重新发起"})
		return
	}
	a.loginMu.Unlock()

	if sess.kind == provider.TraeWork {
		res, err := logintrae.Poll(sess.client, sess.statePath)
		if err == nil {
			a.finishLogin(sess)
			uid, name := a.completeTraeLogin(res)
			writeJSON(w, http.StatusOK, map[string]any{
				"phase": "success", "msg": "TraeWork 登录成功：" + name + "（正在同步积分…）",
				"uid": uid, "nickname": name,
			})
			return
		}
		if errors.Is(err, logintrae.ErrPending) {
			writeJSON(w, http.StatusOK, map[string]any{"phase": "waiting", "msg": "等待 Trae 浏览器回调…"})
		} else {
			// 与桌面端一致：轮询异常视为瞬时失败，继续等待（5 分钟超时兜底）。
			log.Printf("webapi trae login poll failed: %v", err)
			writeJSON(w, http.StatusOK, map[string]any{"phase": "waiting", "msg": "Trae 登录轮询失败，自动重试：" + shortErr(err)})
		}
		return
	}

	res, err := login.Poll(sess.client, sess.statePath)
	if err == nil {
		a.finishLogin(sess)
		uid, name := a.completeWorkBuddyLogin(res)
		writeJSON(w, http.StatusOK, map[string]any{
			"phase": "success", "msg": "登录成功：" + name + "（正在同步积分…）",
			"uid": uid, "nickname": name,
		})
		return
	}
	if errors.Is(err, login.ErrPending) {
		writeJSON(w, http.StatusOK, map[string]any{"phase": "waiting", "msg": "等待浏览器完成登录…"})
	} else {
		log.Printf("webapi workbuddy login poll failed: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{"phase": "waiting", "msg": "登录轮询失败，自动重试：" + shortErr(err)})
	}
}

// finishLogin 释放登录会话（成功/取消时调用）。
func (a *API) finishLogin(sess *loginSession) {
	a.loginMu.Lock()
	if a.active == sess {
		a.active = nil
	}
	a.loginMu.Unlock()
	_ = os.Remove(sess.statePath)
}

// completeWorkBuddyLogin 登录成功：写 auth 文件、重载账号池、异步签到。
// （逻辑同 app.completeLogin，仅把面板事件换成事件总线。）
func (a *API) completeWorkBuddyLogin(r login.Result) (uid, name string) {
	log.Printf("webapi workbuddy 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t",
		r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	fp, err := login.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("webapi workbuddy 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		a.emit("login", map[string]any{"phase": "failed", "msg": "保存凭证失败: " + err.Error()})
		return r.UID, r.UID
	}
	log.Printf("webapi workbuddy 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	name = r.Nickname
	if name == "" && len(r.UID) >= 8 {
		name = r.UID[:8]
	}
	a.emit("login", map[string]any{"phase": "success", "msg": "登录成功：" + name + "（正在同步积分…）"})
	go a.checkinNewAccount(provider.WorkBuddy, r.UID, name)
	return r.UID, name
}

// completeTraeLogin TraeWork 登录成功：写 auth 文件、重载账号池、异步签到。
func (a *API) completeTraeLogin(r logintrae.Result) (uid, name string) {
	log.Printf("webapi traework 登录成功 uid=%s nickname=%s expires_at=%d refresh_token=%t",
		r.UID, r.Nickname, r.ExpiresAt, r.RefreshToken != "")
	fp, err := logintrae.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("webapi traework 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		a.emit("login", map[string]any{"phase": "failed", "msg": "保存凭证失败: " + err.Error()})
		return r.UID, r.UID
	}
	log.Printf("webapi traework 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	name = r.Nickname
	if name == "" && len(r.UID) >= 8 {
		name = r.UID[:8]
	}
	a.emit("login", map[string]any{"phase": "success", "msg": "TraeWork 登录成功：" + name + "（正在同步积分…）"})
	go a.checkinNewAccount(provider.TraeWork, r.UID, name)
	return r.UID, name
}

// checkinNewAccount 新账号登录后异步签到一次（慢网络不阻塞登录完成提示）。
func (a *API) checkinNewAccount(kind provider.Kind, uid, name string) {
	rt := a.runtime(kind)
	if rt == nil || rt.Scheduler == nil {
		return
	}
	res, err := rt.Scheduler.CheckinAccount(uid)
	if err != nil {
		log.Printf("webapi 新账号签到失败 %s: %v", name, err)
		return
	}
	remain := "-"
	if res.HasRemain {
		remain = strconv.FormatInt(res.Remain, 10)
	}
	log.Printf("webapi 新账号签到完成 %s：%s，剩余积分 %s", name, res.Msg, remain)
}

// ---------------------------------------------------------------------------
// POST /api/login/cancel  {state} → {ok:true}
// ---------------------------------------------------------------------------

func (a *API) handleLoginCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
	}
	_ = decodeJSON(r, &req)
	a.loginMu.Lock()
	sess := a.active
	if sess != nil && (req.State == "" || sess.id == req.State) {
		_ = os.Remove(sess.statePath)
		a.active = nil
	}
	a.loginMu.Unlock()
	a.emit("login", map[string]any{"phase": "cancelled", "msg": "登录已取消"})
	log.Printf("webapi: 登录已取消")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// GET /api/login/traecb?sid=  TraeWork OAuth 浏览器回调（无需 Bearer）。
// ---------------------------------------------------------------------------

func (a *API) handleTraeCallback(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	a.loginMu.Lock()
	sess := a.active
	a.loginMu.Unlock()
	if sess == nil || sess.id != sid || sess.kind != provider.TraeWork {
		http.Error(w, "登录会话不存在或已过期，请重新发起登录", http.StatusBadRequest)
		return
	}
	refreshToken := r.URL.Query().Get("refreshToken")
	host := r.URL.Query().Get("host")
	if err := logintrae.NoteCallback(sess.statePath, refreshToken, host); err != nil {
		log.Printf("webapi trae callback note failed: %v", err)
		http.Error(w, "记录回调失败："+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("webapi: traework 回调已记录 sid=%s", sid[:8])
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<html><body style='font-family:sans-serif;padding:24px'>" +
		"TraeWork 登录已完成，可以关闭此页面，回到管理页等待结果。</body></html>"))
}
