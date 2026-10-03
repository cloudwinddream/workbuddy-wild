package wnflb

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// blackholeServer 接受连接但永不响应（等客户端自己断开），
// 模拟论坛对服务器 IP 慢响应/ hang 住的情况。
func blackholeServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func withProbeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := probeTimeout
	probeTimeout = d
	t.Cleanup(func() { probeTimeout = old })
}

// 签到中心列表走的 CachedHomeStatus 绝不能被慢论坛拖住。
func TestCachedHomeStatusNeverBlocks(t *testing.T) {
	withProbeTimeout(t, 300*time.Millisecond)
	srv := blackholeServer(t)
	s := New(t.TempDir(), srv.URL)

	start := time.Now()
	d := s.CachedHomeStatus()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("CachedHomeStatus 阻塞了 %v，应立即返回", elapsed)
	}
	// 无任何缓存/落盘记录时返回零值即可（后台会异步刷新）
	if d.LoggedIn || d.Credits != "" {
		t.Fatalf("无缓存时应返回零值，got %+v", d)
	}
}

// 实时探测必须受短超时约束，论坛 hang 住时也不能无限等。
func TestHomeStatusRespectsProbeTimeout(t *testing.T) {
	withProbeTimeout(t, 300*time.Millisecond)
	srv := blackholeServer(t)
	s := New(t.TempDir(), srv.URL)

	start := time.Now()
	d := s.HomeStatus()
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("HomeStatus 阻塞了 %v，探测超时未生效", elapsed)
	}
	if d.LoggedIn || d.Credits != "" {
		t.Fatalf("探测超时应返回未登录，got %+v", d)
	}
}

// 探测结果要落盘：服务重启后（新 Service 实例）列表可直接回显，
// 不必等下一次实时探测。
func TestProbeResultPersistsAcrossRestart(t *testing.T) {
	withProbeTimeout(t, 2*time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><script>var discuz_uid = '9527';</script><body>积分：1,234</body></html>`))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	s := New(dir, srv.URL)
	d := s.HomeStatus()
	if !d.LoggedIn || d.Credits != "1234" {
		t.Fatalf("探测结果 = %+v, want loggedIn 积分 1234", d)
	}

	// 模拟重启：新实例指向黑洞（网络不可用），CachedHomeStatus 应读到落盘值
	blackhole := blackholeServer(t)
	s2 := New(dir, blackhole.URL)
	d = s2.CachedHomeStatus()
	if !d.LoggedIn || d.Credits != "1234" {
		t.Fatalf("重启后回显 = %+v, want loggedIn 积分 1234", d)
	}
	if _, err := os.Stat(filepath.Join(dir, "status.json")); err != nil {
		t.Fatalf("status.json 未落盘: %v", err)
	}
	// 等后台异步探测结束，避免与 TempDir 清理竞态（探测会回写 status.json）。
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&s2.probing) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
}

// 积分探测链（与现成脚本一致）：签到列表页提取 UID →
// 个人空间页取积分（统计信息块 <em>积分</em>122）。
func TestHomeStatusFallsBackToCreditPage(t *testing.T) {
	withProbeTimeout(t, 10*time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch {
		case strings.Contains(r.URL.String(), "fx_checkin:list"):
			// 签到列表页：无积分，但有连续/累计天数且能提取 UID
			_, _ = w.Write([]byte(`<html><head><title>签到</title></head><script>var discuz_uid = '9527';</script><body>已连续签到12天，累计签到34天</body></html>`))
		case strings.Contains(r.URL.Path, "space-uid-9527.html"):
			// 个人空间页：统计信息块（等级/积分/金币）
			_, _ = w.Write([]byte(`<html><head><title>个人空间</title></head><body><ul><li><em>积分</em> 888</li><li><em>金币</em> 10</li></ul><p><em>用户组: </em><a href="home.php?mod=spacecp&amp;ac=usergroup">Lv.8金别福禄娃</a></p></body></html>`))
		case strings.Contains(r.URL.Path, "forum-2-1.html"):
			_, _ = w.Write([]byte(`<html><body><a id="extcreditmenu">积分: 111</a></body></html>`))
		default:
			// 论坛首页：已登录、含版块链接，但没有积分
			_, _ = w.Write([]byte(`<html><script>var discuz_uid = '9527';</script><body><a href="forum-2-1.html">版块</a></body></html>`))
		}
	}))
	t.Cleanup(srv.Close)

	s := New(t.TempDir(), srv.URL)
	d := s.HomeStatus()
	if !d.LoggedIn || d.Credits != "888" {
		t.Fatalf("空间页回退 = %+v, want loggedIn 积分 888", d)
	}
	if d.Coins != "10" || d.Group != "Lv.8金别福禄娃" || d.Streak != 12 || d.Total != 34 {
		t.Fatalf("探测字段不全 = %+v, want 金币10 等级Lv.8 连续12 累计34", d)
	}
}

func TestExtractUID(t *testing.T) {
	if got := extractUID(`<a href="home.php?mod=space&amp;uid=42">我</a>`); got != "42" {
		t.Errorf("extractUID = %q, want 42", got)
	}
	if got := extractUID(`<script>var discuz_uid = '77';</script>`); got != "77" {
		t.Errorf("extractUID = %q, want 77", got)
	}
	if got := extractUID(`<script>var discuz_uid = '0';</script>`); got != "" {
		t.Errorf("游客应为空，got %q", got)
	}
}
