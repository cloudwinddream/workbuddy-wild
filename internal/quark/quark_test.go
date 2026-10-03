package quark

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExtractParams(t *testing.T) {
	// Cookie 串形式
	kps, sign, vcode, err := ExtractParams("foo=1; kps=ABC+/=; sign=xyz%2525; vcode=123; other=2")
	if err != nil {
		t.Fatalf("ExtractParams: %v", err)
	}
	if kps != "ABC+/=" || sign != "xyz%25" || vcode != "123" {
		t.Fatalf("提取错误 kps=%q sign=%q vcode=%q", kps, sign, vcode)
	}
	// 抓包 URL 形式（& 分隔）
	if _, _, _, err := ExtractParams("https://drive-m.quark.cn/1/clouddrive/x?pr=ucpro&kps=K&sign=S&vcode=V"); err != nil {
		t.Fatalf("URL 形式提取失败: %v", err)
	}
	// 缺参数报错
	if _, _, _, err := ExtractParams("kps=K; sign=S"); err == nil {
		t.Fatal("缺 vcode 应报错")
	}
	// design= 不应误匹配为 sign
	if _, _, _, err := ExtractParams("design=1; kps=K; vcode=V"); err == nil {
		t.Fatal("design 不应被当作 sign")
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		0:                "0 B",
		512:              "512 B",
		1024:             "1 KB",
		1536:             "1.5 KB",
		10 * 1024 * 1024: "10 MB",
		10737418240:      "10 GB",
	}
	for in, want := range cases {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// fakeQuark 模拟夸克成长接口：info 前两次未签到、sign 后切已签到。
type fakeQuark struct {
	srv    *httptest.Server
	signed int32
}

func newFakeQuark(t *testing.T) *fakeQuark {
	t.Helper()
	f := &fakeQuark{}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, data any) {
		raw, _ := json.Marshal(map[string]any{"status": 200, "code": 0, "message": "ok", "data": data})
		_, _ = w.Write(raw)
	}
	mux.HandleFunc("/1/clouddrive/capacity/growth/info", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("kps") != "KPS" || q.Get("sign") != "SIGN" || q.Get("vcode") != "VC" {
			write(w, nil)
			return
		}
		signed := atomic.LoadInt32(&f.signed) == 1
		progress := 2
		if signed {
			progress = 3
		}
		write(w, map[string]any{
			"total_capacity":  10 * 1024 * 1024 * 1024,
			"use_capacity":    3 * 1024 * 1024 * 1024,
			"member_type":     "EXP_SVIP",
			"cap_composition": map[string]any{"sign_reward": 512 * 1024 * 1024},
			"cap_sign": map[string]any{
				"sign_daily":        signed,
				"sign_daily_reward": 10 * 1024 * 1024,
				"sign_progress":     progress,
				"sign_target":       7,
			},
		})
	})
	mux.HandleFunc("/1/clouddrive/capacity/growth/sign", func(w http.ResponseWriter, r *http.Request) {
		atomic.StoreInt32(&f.signed, 1)
		write(w, map[string]any{"sign_daily_reward": 10 * 1024 * 1024})
	})
	mux.HandleFunc("/account/info", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "kps=KPS") {
			write(w, nil)
			return
		}
		write(w, map[string]any{"nickname": "夸克测试号"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestAutoCheckinFlow(t *testing.T) {
	f := newFakeQuark(t)
	svc := New(t.TempDir(), f.srv.URL)
	if err := svc.SaveCookie("kps=KPS; sign=SIGN; vcode=VC; other=x"); err != nil {
		t.Fatalf("SaveCookie: %v", err)
	}
	// 第一次：未签到 → 执行签到
	msg := svc.AutoCheckin()
	if !strings.Contains(msg, "签到成功 +10 MB") || !strings.Contains(msg, "3/7") {
		t.Fatalf("首次签到文案不对：%s", msg)
	}
	st := svc.loadStatus()
	if !st.LastCheckinOK {
		t.Fatalf("状态未记成功：%+v", st)
	}
	p := st.Probe
	if p.Nickname != "夸克测试号" || p.Member != "88VIP" {
		t.Fatalf("探测信息不对：%+v", p)
	}
	if p.Total != 10*1024*1024*1024 || p.AccReward != 512*1024*1024 {
		t.Fatalf("容量信息不对：%+v", p)
	}
	if got := p.Detail(); !strings.Contains(got, "签到累计 512 MB") || !strings.Contains(got, "连签进度 3/7") {
		t.Fatalf("Detail 不对：%s", got)
	}
	// 第二次：已签到
	msg = svc.AutoCheckin()
	if !strings.Contains(msg, "今日已签到") {
		t.Fatalf("重复签到文案不对：%s", msg)
	}
	// 适配器摘要
	sum := NewAdapter(svc, func() []string { return []string{"02:00"} }).Summary()
	if !sum.Configured || !sum.LoggedIn || sum.Points != "10 GB" || sum.PointsName != "总空间" {
		t.Fatalf("Summary 不对：%+v", sum)
	}
	if sum.NextAt == "" || sum.Times != "02:00" {
		t.Fatalf("Summary 时间不对：%+v", sum)
	}
	fmt.Println("quark flow ok:", msg)
}
