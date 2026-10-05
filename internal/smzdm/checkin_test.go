package smzdm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSMZDM 按路径返回预置响应，可附带 Set-Cookie。
func fakeSMZDM(t *testing.T, checkinData map[string]any, setSess string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/checkin", func(w http.ResponseWriter, r *http.Request) {
		if setSess != "" {
			http.SetCookie(w, &http.Cookie{Name: "sess", Value: setSess, Path: "/"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error_code": 0, "error_msg": "", "data": checkinData,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func iphoneClient(t *testing.T, base string) *Client {
	t.Helper()
	c, err := NewClient("sess=old-sess; device_smzdm=iphone; smzdm_id=u1", base)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// 空结果必须报错，不能假报"签到成功 · 第0天"。
func TestCheckinEmptyResultIsError(t *testing.T) {
	srv := fakeSMZDM(t, map[string]any{}, "")
	c := iphoneClient(t, srv.URL)
	if _, err := PerformDailyCheckin(c); err == nil {
		t.Fatal("空结果应报错")
	}
}

// 正常结果解析。
func TestCheckinParsesResult(t *testing.T) {
	srv := fakeSMZDM(t, map[string]any{
		"daily_num": 262.0, "cgold": 5.0, "cpoints": 3.0, "cexperience": 2.0,
	}, "")
	c := iphoneClient(t, srv.URL)
	res, err := PerformDailyCheckin(c)
	if err != nil {
		t.Fatalf("PerformDailyCheckin: %v", err)
	}
	if res.ConsecutiveDays != 262 || res.GoldEarned != 5 {
		t.Fatalf("解析错误：%+v", res)
	}
	if !strings.Contains(res.Summary(), "第262天") {
		t.Fatalf("摘要不对：%s", res.Summary())
	}
}

// 服务端轮换 sess 时，客户端 Cookie 与回调都要更新。
func TestSessRotationUpdatesCookie(t *testing.T) {
	srv := fakeSMZDM(t, map[string]any{"daily_num": 1.0}, "new-sess")
	c := iphoneClient(t, srv.URL)
	var updated string
	c.SetCookieUpdateHook(func(nc string) { updated = nc })
	if _, err := PerformDailyCheckin(c); err != nil {
		t.Fatalf("PerformDailyCheckin: %v", err)
	}
	if c.cookies["sess"] != "new-sess" {
		t.Fatalf("内存 sess 未更新：%q", c.cookies["sess"])
	}
	if !strings.Contains(updated, "sess=new-sess") {
		t.Fatalf("回调 Cookie 未更新：%q", updated)
	}
	if !strings.Contains(c.cookie, "sess=new-sess") {
		t.Fatalf("请求头 Cookie 未更新：%q", c.cookie)
	}
}

// 已签到业务错误识别。
func TestIsAlreadySigned(t *testing.T) {
	if !IsAlreadySigned(&APIError{Code: 1, Msg: "今日已签到，请勿重复"}) {
		t.Fatal("已签到错误应被识别")
	}
	if IsAlreadySigned(&APIError{Code: 1, Msg: "登录已过期"}) {
		t.Fatal("其他错误不应被识别为已签到")
	}
}
