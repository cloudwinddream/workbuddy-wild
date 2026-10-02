package webapi

import (
	"log"
	"net/http"
	"strings"

	"github.com/rockswang/workbuddy-wild/internal/smzdm"
)

// ---------------------------------------------------------------------------
// 什么值得买签到——模块专属的凭证接口（Cookie 提交/退出）。
// 通用展示与手动签到走签到中心 /api/checkins*。
// ---------------------------------------------------------------------------

func (a *API) smzdmSvc() *smzdm.Service { return a.smzdm }

func (a *API) registerSmzdm(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/smzdm/cookie", a.withAuth(a.handleSmzdmCookie))
	mux.HandleFunc("POST /api/smzdm/logout", a.withAuth(a.handleSmzdmLogout))
	mux.HandleFunc("GET /api/smzdm/status", a.withAuth(a.handleSmzdmStatus))
}

// POST /api/smzdm/cookie {cookie}
// → {ok:true, smzdm_id} | {ok:false, msg}
// Cookie 从什么值得买 APP 抓包获取，须包含 sess 字段。
func (a *API) handleSmzdmCookie(w http.ResponseWriter, r *http.Request) {
	svc := a.smzdmSvc()
	if svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "什么值得买签到模块未启用")
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
		log.Printf("webapi smzdm Cookie 保存失败: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "msg": "Cookie 已保存", "smzdm_id": svc.SmzdmID(),
	})
}

// POST /api/smzdm/logout → {ok:true}
func (a *API) handleSmzdmLogout(w http.ResponseWriter, r *http.Request) {
	svc := a.smzdmSvc()
	if svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "什么值得买签到模块未启用")
		return
	}
	svc.ClearCookie()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "已清除 Cookie"})
}

// GET /api/smzdm/status → {configured, smzdm_id}
func (a *API) handleSmzdmStatus(w http.ResponseWriter, r *http.Request) {
	svc := a.smzdmSvc()
	if svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "什么值得买签到模块未启用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": svc.Configured(),
		"smzdm_id":   svc.SmzdmID(),
	})
}
