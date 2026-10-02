package webapi

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/wnflb"
)

// ---------------------------------------------------------------------------
// 福利吧论坛签到（/api/wnflb/*）：独立管理页用的接口。
// 账号为单账号（存档于 stateDir/wnflb/account.json），与 AI 账号池互不干扰。
// ---------------------------------------------------------------------------

func (a *API) wnflbSvc() *wnflb.Service { return a.wnflb }

func (a *API) registerWnflb(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/wnflb/status", a.withAuth(a.handleWnflbStatus))
	mux.HandleFunc("POST /api/wnflb/login", a.withAuth(a.handleWnflbLogin))
	mux.HandleFunc("GET /api/wnflb/captcha", a.withAuth(a.handleWnflbCaptcha))
	mux.HandleFunc("POST /api/wnflb/captcha", a.withAuth(a.handleWnflbSubmitCaptcha))
	mux.HandleFunc("POST /api/wnflb/checkin", a.withAuth(a.handleWnflbCheckin))
	mux.HandleFunc("POST /api/wnflb/logout", a.withAuth(a.handleWnflbLogout))
}

// GET /api/wnflb/status → 登录态、积分、上次/下次签到。
func (a *API) handleWnflbStatus(w http.ResponseWriter, r *http.Request) {
	svc := a.wnflbSvc()
	if svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "福利吧签到模块未启用")
		return
	}
	username, hasAccount := svc.QuickStatus()
	st := svc.LoadStatusForAPI()
	loggedIn := false
	credits := ""
	if hasAccount {
		// 一次请求同时探测登录态与积分；失败不报错，只标记未登录
		loggedIn, credits = svc.HomeStatus()
	}
	a.mu.Lock()
	times := a.wnflbTimes
	a.mu.Unlock()
	next := wnflb.NextRun(wnflb.ParseTimes(times), time.Now())
	nextStr := ""
	if !next.IsZero() {
		nextStr = next.Format("2006-01-02 15:04")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"has_account":   hasAccount,
		"logged_in":     loggedIn,
		"username":      username,
		"credits":       credits,
		"last_checkin":  st.LastCheckinAt,
		"last_ok":       st.LastCheckinOK,
		"last_msg":      st.LastMsg,
		"next_checkin":  nextStr,
		"checkin_times": times,
	})
}

// POST /api/wnflb/login {username, password}
// → {ok:true} | {ok:false, need_captcha:true, captcha_id} | {ok:false, msg}
func (a *API) handleWnflbLogin(w http.ResponseWriter, r *http.Request) {
	svc := a.wnflbSvc()
	if svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "福利吧签到模块未启用")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	ch, err := svc.Login(strings.TrimSpace(req.Username), req.Password)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "登录成功"})
		return
	}
	if errors.Is(err, wnflb.ErrCaptchaRequired) && ch != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "need_captcha": true, "captcha_id": ch.ID,
			"msg": "本次登录需要验证码，请识别下图并提交",
		})
		return
	}
	log.Printf("webapi wnflb 登录失败: %v", err)
	writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
}

// GET /api/wnflb/captcha?id= → 验证码图片。
func (a *API) handleWnflbCaptcha(w http.ResponseWriter, r *http.Request) {
	svc := a.wnflbSvc()
	if svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "福利吧签到模块未启用")
		return
	}
	raw, err := svc.CaptchaImage(r.URL.Query().Get("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ct := "image/png"
	if len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xD8 {
		ct = "image/jpeg"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

// POST /api/wnflb/captcha {id, code, username, password}
// → {ok:true} | {ok:false, need_retry:true, captcha_id, msg} | {ok:false, msg}
func (a *API) handleWnflbSubmitCaptcha(w http.ResponseWriter, r *http.Request) {
	svc := a.wnflbSvc()
	if svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "福利吧签到模块未启用")
		return
	}
	var req struct {
		ID       string `json:"id"`
		Code     string `json:"code"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	ch, err := svc.SubmitCaptcha(req.ID, req.Code, req.Username, req.Password)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "登录成功"})
		return
	}
	if errors.Is(err, wnflb.ErrCaptchaRetry) && ch != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "need_retry": true, "captcha_id": ch.ID,
			"msg": "验证码不正确，已换一张，请重试",
		})
		return
	}
	log.Printf("webapi wnflb 验证码提交失败: %v", err)
	writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
}

// POST /api/wnflb/checkin → {ok, msg}
func (a *API) handleWnflbCheckin(w http.ResponseWriter, r *http.Request) {
	svc := a.wnflbSvc()
	if svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "福利吧签到模块未启用")
		return
	}
	if err := svc.EnsureLoggedIn(); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	ok, msg := svc.Checkin()
	svc.SaveStatusForAPI(ok, msg)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

// POST /api/wnflb/logout → {ok:true}
func (a *API) handleWnflbLogout(w http.ResponseWriter, r *http.Request) {
	svc := a.wnflbSvc()
	if svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "福利吧签到模块未启用")
		return
	}
	svc.Logout()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "已退出登录"})
}
