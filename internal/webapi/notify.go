package webapi

import (
	"net/http"
	"strings"

	"github.com/rockswang/workbuddy-wild/internal/notify"
)

// ---------------------------------------------------------------------------
// 签到结果 Bark 推送通知设置（/api/notify*）。
// 配置存于 <stateDir>/notify/notify.json；模块可单独覆盖 key。
// ---------------------------------------------------------------------------

func (a *API) notifySvc() *notify.Bark { return a.notify }

func (a *API) registerNotify(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/notify/config", a.withAuth(a.handleNotifyGet))
	mux.HandleFunc("PUT /api/notify/config", a.withAuth(a.handleNotifyPut))
	mux.HandleFunc("POST /api/notify/test", a.withAuth(a.handleNotifyTest))
}

// GET /api/notify/config → {server, default_key(脱敏), default_set, modules: [...], accounts: [...]}
func (a *API) handleNotifyGet(w http.ResponseWriter, r *http.Request) {
	n := a.notifySvc()
	if n == nil {
		writeErr(w, http.StatusServiceUnavailable, "通知模块未启用")
		return
	}
	view := n.View()
	mods := []any{}
	if a.checkins != nil {
		for _, m := range a.checkins.List() {
			mv, _ := view["modules"].(map[string]any)[m.ID()].(map[string]any)
			set, key := false, ""
			if mv != nil {
				set, _ = mv["set"].(bool)
				key, _ = mv["key"].(string)
			}
			mods = append(mods, map[string]any{
				"id": m.ID(), "name": m.Name(), "key": key, "set": set,
			})
		}
	}
	accts := []any{}
	for _, v := range a.accountViews() {
		av, _ := view["accounts"].(map[string]any)[v.Group+":"+v.UID].(map[string]any)
		set, key := false, ""
		if av != nil {
			set, _ = av["set"].(bool)
			key, _ = av["key"].(string)
		}
		accts = append(accts, map[string]any{
			"platform": v.Group, "uid": v.UID, "nickname": v.Nickname,
			"key": key, "set": set,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server":      view["server"],
		"default_key": view["default_key"],
		"default_set": view["default_set"],
		"modules":     mods,
		"accounts":    accts,
	})
}

// PUT /api/notify/config {server, default_key, modules: {id: key}, accounts: {"platform:uid": key}}
// key 语义：省略/空 = 保持不变；"CLEAR" = 清除；其他 = 设置为新值。
func (a *API) handleNotifyPut(w http.ResponseWriter, r *http.Request) {
	n := a.notifySvc()
	if n == nil {
		writeErr(w, http.StatusServiceUnavailable, "通知模块未启用")
		return
	}
	var req struct {
		Server     string            `json:"server"`
		DefaultKey string            `json:"default_key"`
		Modules    map[string]string `json:"modules"`
		Accounts   map[string]string `json:"accounts"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	cur := n.RawConfig()
	if s := strings.TrimSpace(req.Server); s != "" {
		cur.Server = s
	}
	if req.DefaultKey == "CLEAR" {
		cur.DefaultKey = ""
	} else if req.DefaultKey != "" {
		cur.DefaultKey = req.DefaultKey
	}
	if cur.ModuleKeys == nil {
		cur.ModuleKeys = map[string]string{}
	}
	for id, k := range req.Modules {
		if k == "CLEAR" {
			delete(cur.ModuleKeys, id)
		} else if k != "" {
			cur.ModuleKeys[id] = k
		}
	}
	if cur.AccountKeys == nil {
		cur.AccountKeys = map[string]string{}
	}
	for id, k := range req.Accounts {
		if k == "CLEAR" {
			delete(cur.AccountKeys, id)
		} else if k != "" {
			cur.AccountKeys[id] = k
		}
	}
	if err := n.Update(cur); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "通知设置已保存"})
}

// POST /api/notify/test {module: "wnflb"|"smzdm"|""} 或 {account: "workbuddy:uid"}
// → 用对应 key 发测试推送。
func (a *API) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	n := a.notifySvc()
	if n == nil {
		writeErr(w, http.StatusServiceUnavailable, "通知模块未启用")
		return
	}
	var req struct {
		Module  string `json:"module"`
		Account string `json:"account"`
	}
	_ = decodeJSON(r, &req)
	var err error
	if acct := strings.TrimSpace(req.Account); acct != "" {
		platform, uid, found := strings.Cut(acct, ":")
		if !found || uid == "" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "account 格式应为 platform:uid"})
			return
		}
		err = n.SendAccountSync(platform, uid, "签到中心", "Bark 通知测试：账号推送已接通 ✅")
	} else {
		err = n.SendSync(strings.TrimSpace(req.Module), "签到中心", "Bark 通知测试：签到中心推送已接通 ✅")
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "发送失败：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "测试推送已发送"})
}
