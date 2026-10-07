package smzdm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAPI 模拟 smzdm 接口：/checkin 按是否带 zhuanzai_ab 字段
// 区分 APP 签到与档案读取。
type fakeAPI struct {
	profile map[string]any
	signErr string // 非空时 APP 签到返回该业务错误
	setSess string // 非空时在档案响应里轮换 sess
}

func (f *fakeAPI) handler(t *testing.T) http.Handler {
	t.Helper()
	write := func(w http.ResponseWriter, code int, msg string, data map[string]any) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error_code": code, "error_msg": msg, "data": data,
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/checkin/show_view_v2", func(w http.ResponseWriter, r *http.Request) {
		write(w, 0, "", map[string]any{"rows": []any{}})
	})
	mux.HandleFunc("/checkin", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		// 签到用 APP 原生 UA（"smzdm 11.1.95 rv:173"），档案读取用旧 UA：
		// 两者字段相同，靠 UA 区分，记录我们押注的差异点。
		if strings.HasPrefix(r.UserAgent(), "smzdm ") { // APP 签到（抓包原方）
			// 抓包原方：签到请求不带 token/sk/touchstone_event/captcha
			for _, k := range []string{"token", "sk", "touchstone_event", "captcha"} {
				if _, ok := r.Form[k]; ok {
					t.Errorf("签到请求不应带字段 %s", k)
				}
			}
			if _, ok := r.Form["zhuanzai_ab"]; !ok {
				t.Errorf("签到请求应带 zhuanzai_ab 字段")
			}
			if f.signErr != "" {
				write(w, 1, f.signErr, nil)
				return
			}
			write(w, 0, "签到成功", f.profile)
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
	c, err := NewClient("sess=old-sess; smzdm_id=u1; device_id=d1; device_smzdm=iphone; device_smzdm_version=11.1.92; v=11.1.92", srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

var profileSample = map[string]any{
	"daily_num": 264.0, "cgold": 569.0, "cpoints": 68169.0, "cexperience": 79736.0,
}

// APP 原方签到成功，签到响应自带档案。
func TestPerformCheckinAppSigns(t *testing.T) {
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

// 签到回"今日已签到"按成功处理，并展示档案。
func TestPerformCheckinAlreadySigned(t *testing.T) {
	c := fakeClient(t, &fakeAPI{profile: profileSample, signErr: "今日已签到，请勿重复签到"})
	msg, _, err := PerformDailyCheckin(c)
	if err != nil {
		t.Fatalf("已签到不应报错：%v", err)
	}
	if !strings.Contains(msg, "今日已签到") || !strings.Contains(msg, "连签第264天") {
		t.Fatalf("文案不对：%s", msg)
	}
}

// 签到阶段失败（登录过期）必须暴露为失败。
func TestPerformCheckinSignFailure(t *testing.T) {
	c := fakeClient(t, &fakeAPI{profile: profileSample, signErr: "登录已过期"})
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
