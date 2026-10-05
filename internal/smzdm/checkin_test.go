package smzdm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAPI 模拟 smzdm 接口：/robot/token 换 token、/checkin 按 token
// 区分 robot 签到（token=RT1）与档案读取，其余路径回档案数据。
type fakeAPI struct {
	profile  map[string]any
	tokenErr string // 非空时 /robot/token 返回该业务错误
	signErr  string // 非空时 robot /checkin 返回该业务错误
	setSess  string // 非空时在档案响应里轮换 sess
}

func (f *fakeAPI) handler(t *testing.T) http.Handler {
	t.Helper()
	write := func(w http.ResponseWriter, code int, msg string, data map[string]any) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error_code": code, "error_msg": msg, "data": data,
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/robot/token", func(w http.ResponseWriter, r *http.Request) {
		if f.tokenErr != "" {
			write(w, 1, f.tokenErr, nil)
			return
		}
		write(w, 0, "", map[string]any{"token": "RT1"})
	})
	mux.HandleFunc("/checkin", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("token") == "RT1" { // robot 签到
			if f.signErr != "" {
				write(w, 1, f.signErr, nil)
				return
			}
			write(w, 0, "签到成功", map[string]any{})
			return
		}
		// 档案读取
		if f.setSess != "" {
			http.SetCookie(w, &http.Cookie{Name: "sess", Value: f.setSess, Path: "/"})
		}
		write(w, 0, "", f.profile)
	})
	return mux
}

func fakeClient(t *testing.T, f *fakeAPI) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	c, err := NewClient("sess=old-sess; smzdm_id=u1; device_id=d1", srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

var profileSample = map[string]any{
	"daily_num": 264.0, "cgold": 569.0, "cpoints": 68169.0, "cexperience": 79736.0,
}

// robot 流程签到成功 + 档案展示。
func TestPerformCheckinRobotSigns(t *testing.T) {
	c := fakeClient(t, &fakeAPI{profile: profileSample})
	msg, prof, err := PerformDailyCheckin(c)
	if err != nil {
		t.Fatalf("PerformDailyCheckin: %v", err)
	}
	if !strings.Contains(msg, "签到成功") || !strings.Contains(msg, "连签第264天") {
		t.Fatalf("文案不对：%s", msg)
	}
	if prof.GoldEarned != 569 || prof.PointsEarned != 68169 {
		t.Fatalf("档案解析不对：%+v", prof)
	}
}

// robot 签到回"今日已签到"按成功处理。
func TestPerformCheckinAlreadySigned(t *testing.T) {
	c := fakeClient(t, &fakeAPI{profile: profileSample, signErr: "今日已签到，请勿重复签到"})
	msg, _, err := PerformDailyCheckin(c)
	if err != nil {
		t.Fatalf("已签到不应报错：%v", err)
	}
	if !strings.Contains(msg, "今日已签到") {
		t.Fatalf("文案不对：%s", msg)
	}
}

// token 阶段失败（登录过期）必须暴露为失败。
func TestPerformCheckinTokenFailure(t *testing.T) {
	c := fakeClient(t, &fakeAPI{profile: profileSample, tokenErr: "登录已过期"})
	if _, _, err := PerformDailyCheckin(c); err == nil || !strings.Contains(err.Error(), "登录已过期") {
		t.Fatalf("应暴露登录过期，got %v", err)
	}
}

// 档案为空要报错（Cookie 失效），不能假报成功。
func TestProfileEmptyIsError(t *testing.T) {
	c := fakeClient(t, &fakeAPI{profile: map[string]any{}})
	if _, err := FetchCheckinProfile(c); err == nil {
		t.Fatal("空档案应报错")
	}
}

// 服务端轮换 sess 时，客户端 Cookie 与回调都要更新。
func TestSessRotationUpdatesCookie(t *testing.T) {
	c := fakeClient(t, &fakeAPI{profile: profileSample, setSess: "new-sess"})
	var updated string
	c.SetCookieUpdateHook(func(nc string) { updated = nc })
	if _, err := FetchCheckinProfile(c); err != nil {
		t.Fatalf("FetchCheckinProfile: %v", err)
	}
	if c.cookies["sess"] != "new-sess" {
		t.Fatalf("内存 sess 未更新：%q", c.cookies["sess"])
	}
	if !strings.Contains(updated, "sess=new-sess") || !strings.Contains(c.cookie, "sess=new-sess") {
		t.Fatalf("Cookie 未同步：hook=%q header=%q", updated, c.cookie)
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
