package webapi

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/rockswang/workbuddy-wild/internal/wnflb"
)

// ---------------------------------------------------------------------------
// 福利吧论坛签到——模块专属的账号接口（登录/验证码/退出）。
// 通用展示与手动签到走签到中心 /api/checkins*。
// ---------------------------------------------------------------------------

func (a *API) wnflbSvc() *wnflb.Service { return a.wnflb }

func (a *API) registerWnflb(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/wnflb/login", a.withAuth(a.handleWnflbLogin))
	mux.HandleFunc("GET /api/wnflb/captcha", a.withAuth(a.handleWnflbCaptcha))
	mux.HandleFunc("POST /api/wnflb/captcha", a.withAuth(a.handleWnflbSubmitCaptcha))
	mux.HandleFunc("POST /api/wnflb/logout", a.withAuth(a.handleWnflbLogout))
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
