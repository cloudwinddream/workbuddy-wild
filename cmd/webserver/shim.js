/* WorkBuddy-Wild Web 版兼容垫片。
 *
 * 桌面版前端（frontend/dist/app.js）通过 window.go.app.App.*（wails 绑定）
 * 与 window.runtime.*（wails runtime）跟后端交互。本文件在浏览器里实现
 * 同名接口，底层桥接到本服务的 /api/* HTTP 接口，让同一套前端无需修改
 * 即可在 Web 版运行。
 *
 * 由服务端注入到 index.html（<script src="/shim.js">，位于 app.js 之前）。
 */
/* global window, document, fetch, sessionStorage, navigator */
(function () {
  "use strict";

  var LS_KEY = "wbw_api_key";
  var apiKey = "";
  try { apiKey = sessionStorage.getItem(LS_KEY) || ""; } catch (e) {}
  // authed：至少一次 /api 调用成功（或服务端未设 key）后才启动后台轮询，
  // 避免未登录时轮询触发反复弹框。
  var authed = false;

  function setKey(v) {
    apiKey = v || "";
    try { sessionStorage.setItem(LS_KEY, apiKey); } catch (e) {}
  }

  function escHtml(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  // ---- API Key 输入框（401 时弹出） ----
  var keyPrompt = null;
  function promptApiKey() {
    if (keyPrompt) return keyPrompt;
    keyPrompt = new Promise(function (resolve) {
      var ov = document.createElement("div");
      ov.style.cssText = "position:fixed;inset:0;background:rgba(0,0,0,.45);z-index:9999;" +
        "display:flex;align-items:center;justify-content:center;";
      var box = document.createElement("div");
      box.style.cssText = "background:#fff;border-radius:12px;padding:24px;width:320px;" +
        "box-shadow:0 8px 32px rgba(0,0,0,.25);font-family:sans-serif;";
      box.innerHTML =
        "<div style='font-size:16px;font-weight:600;margin-bottom:8px;'>需要 API Key</div>" +
        "<div style='font-size:13px;color:#666;margin-bottom:12px;'>管理接口需要鉴权。" +
        "请输入服务的 API Key（服务端 WB2A_API_KEY 或面板“API-Key”处设置的值）。</div>" +
        "<input type='password' id='wbwKeyInput' placeholder='API Key' " +
        "style='width:100%;box-sizing:border-box;padding:8px 10px;border:1px solid #ccc;" +
        "border-radius:8px;font-size:14px;margin-bottom:12px;' value='" + escHtml(apiKey) + "'>" +
        "<div style='display:flex;gap:8px;justify-content:flex-end;'>" +
        "<button id='wbwKeyCancel' style='padding:6px 14px;border:1px solid #ccc;background:#fff;" +
        "border-radius:8px;cursor:pointer;'>取消</button>" +
        "<button id='wbwKeyOk' style='padding:6px 14px;border:none;background:#1677ff;color:#fff;" +
        "border-radius:8px;cursor:pointer;'>确定</button></div>";
      ov.appendChild(box);
      document.body.appendChild(ov);
      var input = box.querySelector("#wbwKeyInput");
      input.focus();
      function done(v) {
        document.body.removeChild(ov);
        keyPrompt = null;
        resolve(v);
      }
      box.querySelector("#wbwKeyOk").onclick = function () { done(input.value.trim()); };
      box.querySelector("#wbwKeyCancel").onclick = function () { done(null); };
      input.addEventListener("keydown", function (e) {
        if (e.key === "Enter") done(input.value.trim());
        if (e.key === "Escape") done(null);
      });
    });
    return keyPrompt;
  }

  async function apiFetch(path, opts) {
    opts = opts || {};
    var headers = {};
    if (opts.headers) {
      for (var k in opts.headers) headers[k] = opts.headers[k];
    }
    if (apiKey) headers["Authorization"] = "Bearer " + apiKey;
    var r = await fetch(path, {
      method: opts.method || "GET",
      headers: headers,
      body: opts.body
    });
    if (r.status === 401 && !opts._retried) {
      var k2 = await promptApiKey();
      if (k2 === null || k2 === "") throw new Error("需要 API Key 才能访问管理接口");
      setKey(k2);
      opts._retried = true;
      return apiFetch(path, opts);
    }
    if (!r.ok) {
      var msg = "HTTP " + r.status;
      try {
        var j = await r.clone().json();
        if (j && (j.error || j.msg)) msg = j.error || j.msg;
      } catch (e) {}
      throw new Error(msg);
    }
    authed = true;
    var ct = r.headers.get("content-type") || "";
    if (ct.indexOf("application/json") >= 0) return r.json();
    return r.text();
  }

  function post(path, body) {
    return apiFetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body || {})
    });
  }
  function put(path, body) {
    return apiFetch(path, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body || {})
    });
  }
  function del(path) {
    return apiFetch(path, { method: "DELETE" });
  }

  // ---- 事件分发（模拟 window.runtime.EventsOn） ----
  var evtHandlers = {};
  var evtSeq = 0;
  var evtTimer = null;

  function dispatch(name, data) {
    var list = evtHandlers[name] || [];
    for (var i = 0; i < list.length; i++) {
      try { list[i](data); } catch (e) { console.error(e); }
    }
  }

  function ensureEvtPoll() {
    if (evtTimer) return;
    evtTimer = setInterval(async function () {
      if (!authed || document.hidden) return;
      try {
        var j = await apiFetch("/api/events?since=" + evtSeq);
        evtSeq = j.seq || evtSeq;
        (j.events || []).forEach(function (ev) { dispatch(ev.name, ev.data); });
      } catch (e) { /* 瞬时失败忽略 */ }
    }, 3000);
  }

  // ---- 登录轮询 ----
  var loginSid = null;
  var loginPollTimer = null;
  function stopLoginPoll() {
    if (loginPollTimer) { clearInterval(loginPollTimer); loginPollTimer = null; }
  }
  function startLoginPoll() {
    stopLoginPoll();
    loginPollTimer = setInterval(async function () {
      if (!loginSid) { stopLoginPoll(); return; }
      try {
        var j = await apiFetch("/api/login/poll?state=" + encodeURIComponent(loginSid));
        dispatch("login", { phase: j.phase, msg: j.msg });
        if (j.phase === "success" || j.phase === "failed" || j.phase === "cancelled") {
          loginSid = null;
          stopLoginPoll();
        }
      } catch (e) { /* 网络瞬断则继续等下一轮 */ }
    }, 2000);
  }

  // ---- window.go.app.App 桥接 ----
  var App = {
    GetState: function () { return apiFetch("/api/state"); },
    GetAccounts: function () { return apiFetch("/api/accounts"); },

    SetAPIKey: async function (key) {
      await put("/api/config", { api_key: key });
      setKey(key); // 服务端 key 已换，垫片同步换 key
    },
    SetCheckinTimes: function (times) { return put("/api/config", { checkin_times: times }); },
    SetListen: function (host, port) {
      return put("/api/config", { listen_host: host, listen_port: port });
    },
    SetStrategy: function (val) { return put("/api/strategy", { strategy: val }); },
    SetAutostart: function () {
      return Promise.reject(new Error("Web 版不支持开机自启，请使用 Docker 的 restart 策略保持常驻"));
    },

    HidePanel: function () { return Promise.resolve(); }, // Web 无面板可藏
    QuitAll: function () {
      return Promise.reject(new Error("Web 版运行在容器中，请用 docker compose stop 停止服务"));
    },
    OpenLogFile: async function () {
      var j = await apiFetch("/api/logs?lines=500");
      var w = window.open("", "_blank");
      if (!w) throw new Error("弹窗被拦截，请允许本页弹窗后重试");
      w.document.write("<html><head><meta charset='utf-8'><title>运行日志</title></head>" +
        "<body><pre style='font-size:12px;'>" + escHtml((j.lines || []).join("\n")) +
        "</pre></body></html>");
      w.document.close();
    },
    SavePanelPos: function () { return Promise.resolve(); }, // Web 无需记忆窗口位置

    CheckinAll: function () { return post("/api/checkin/all"); },
    CheckinAccount: function (uid) { return post("/api/checkin/account", { uid: uid }); },
    RefreshAll: function () { return post("/api/refresh/all"); },
    RefreshCredits: function (uid) { return post("/api/refresh/account", { uid: uid }); },
    RemoveAccount: function (uid) { return del("/api/account/" + encodeURIComponent(uid)); },

    StartLogin: function () { return App.StartLoginFor("workbuddy"); },
    StartLoginFor: async function (kind) {
      var j = await post("/api/login/start", { platform: kind || "workbuddy" });
      loginSid = j.state;
      // 先发一条 waiting 事件，让前端把遮罩文案换成 Web 版提示
      //（桌面版文案是"已打开无痕浏览器"，Web 版需要用户自己打开链接）。
      dispatch("login", {
        phase: "waiting",
        msg: (j.hint || "请在浏览器新标签页打开授权链接完成登录…") +
          (kind === "traework" ? "（授权链接可点击右上角复制按钮复制）" : "")
      });
      startLoginPoll();
      return j.auth_url;
    },
    CancelLogin: async function () {
      var sid = loginSid;
      loginSid = null;
      stopLoginPoll();
      if (sid) {
        try { await post("/api/login/cancel", { state: sid }); } catch (e) {}
      }
    }
  };

  // ---- window.runtime 桥接 ----
  var runtime = {
    EventsOn: function (name, cb) {
      (evtHandlers[name] = evtHandlers[name] || []).push(cb);
      if (name === "checkin" || name === "refresh" || name === "login" || name === "accounts") {
        ensureEvtPoll();
      }
    },
    ClipboardSetText: function (t) {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        return navigator.clipboard.writeText(t);
      }
      return Promise.reject(new Error("clipboard unavailable"));
    },
    BrowserOpenURL: function (u) { window.open(u, "_blank"); },
    WindowGetPosition: function () { return Promise.resolve(null); }
  };

  window.go = window.go || {};
  window.go.app = window.go.app || {};
  window.go.app.App = App;
  window.runtime = runtime;
})();
