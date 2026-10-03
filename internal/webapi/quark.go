package webapi

import (
	"log"
	"net/http"
	"strings"

	"github.com/rockswang/workbuddy-wild/internal/quark"
)

// ---------------------------------------------------------------------------
// 夸克网盘签到——模块专属的凭证接口（Cookie 提交/退出）。
// 通用展示与手动签到走签到中心 /api/checkins*。
// ---------------------------------------------------------------------------

func (a *API) quarkSvc() *quark.Service { return a.quark }

func (a *API) registerQuark(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/quark/cookie", a.withAuth(a.handleQuarkCookie))
	mux.HandleFunc("POST /api/quark/logout", a.withAuth(a.handleQuarkLogout))
	mux.HandleFunc("GET /api/quark/status", a.withAuth(a.handleQuarkStatus))
}

// POST /api/quark/cookie {cookie}
// → {ok:true, nickname} | {ok:false, msg}
// Cookie 从夸克网盘网页版获取，须包含 kps/sign/vcode。
func (a *API) handleQuarkCookie(w http.ResponseWriter, r *http.Request) {
	svc := a.quarkSvc()
	if svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "夸克网盘签到模块未启用")
		return
	}
	var req struct {
		Cookie string `json:"cookie"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	cookie := strings.TrimSpace(req.Cookie)
	if cookie == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "Cookie 不能为空"})
		return
	}
	if err := svc.SaveCookie(cookie); err != nil {
		log.Printf("webapi quark Cookie 保存失败: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	// 保存后立即探测一次验证可用性（失败也保留 Cookie，仅提示）。
	nickname := ""
	msg := "Cookie 已保存"
	if p, err := svc.ProbeNow(); err != nil {
		log.Printf("webapi quark Cookie 保存后探测失败: %v", err)
		msg = "Cookie 已保存（暂时拉不到账号信息，请检查 Cookie 是否完整）"
	} else {
		nickname = p.Nickname
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "msg": msg, "nickname": nickname,
	})
}

// POST /api/quark/logout → {ok:true}
func (a *API) handleQuarkLogout(w http.ResponseWriter, r *http.Request) {
	svc := a.quarkSvc()
	if svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "夸克网盘签到模块未启用")
		return
	}
	svc.ClearCookie()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "已清除 Cookie"})
}

// GET /api/quark/status → {configured, nickname}
func (a *API) handleQuarkStatus(w http.ResponseWriter, r *http.Request) {
	svc := a.quarkSvc()
	if svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "夸克网盘签到模块未启用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": svc.Configured(),
		"nickname":   svc.CachedProbe().Nickname,
	})
}
