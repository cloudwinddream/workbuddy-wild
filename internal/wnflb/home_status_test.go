package wnflb

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	loggedIn, credits := s.CachedHomeStatus()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("CachedHomeStatus 阻塞了 %v，应立即返回", elapsed)
	}
	// 无任何缓存/落盘记录时返回零值即可（后台会异步刷新）
	if loggedIn || credits != "" {
		t.Fatalf("无缓存时应返回零值，got loggedIn=%v credits=%q", loggedIn, credits)
	}
}

// 实时探测必须受短超时约束，论坛 hang 住时也不能无限等。
func TestHomeStatusRespectsProbeTimeout(t *testing.T) {
	withProbeTimeout(t, 300*time.Millisecond)
	srv := blackholeServer(t)
	s := New(t.TempDir(), srv.URL)

	start := time.Now()
	loggedIn, credits := s.HomeStatus()
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("HomeStatus 阻塞了 %v，探测超时未生效", elapsed)
	}
	if loggedIn || credits != "" {
		t.Fatalf("探测超时应返回未登录，got loggedIn=%v credits=%q", loggedIn, credits)
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
	loggedIn, credits := s.HomeStatus()
	if !loggedIn || credits != "1234" {
		t.Fatalf("探测结果 = (%v, %q), want (true, 1234)", loggedIn, credits)
	}

	// 模拟重启：新实例指向黑洞（网络不可用），CachedHomeStatus 应读到落盘值
	blackhole := blackholeServer(t)
	s2 := New(dir, blackhole.URL)
	loggedIn, credits = s2.CachedHomeStatus()
	if !loggedIn || credits != "1234" {
		t.Fatalf("重启后回显 = (%v, %q), want (true, 1234)", loggedIn, credits)
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

// 部分模板论坛首页不渲染积分锚点（用户实测要点进版块页才有）：
// 首页解析不到积分时，探测应回退抓积分页拿到积分。
func TestHomeStatusFallsBackToCreditPage(t *testing.T) {
	withProbeTimeout(t, 5*time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Query().Get("ac") == "credit" {
			// 积分页：表格布局
			_, _ = w.Write([]byte(`<html><body><table><tr><th>积分</th><td>122</td></tr></table></body></html>`))
			return
		}
		// 论坛首页：已登录但没有积分锚点
		_, _ = w.Write([]byte(`<html><script>var discuz_uid = '9527';</script><body>欢迎回来</body></html>`))
	}))
	t.Cleanup(srv.Close)

	s := New(t.TempDir(), srv.URL)
	loggedIn, credits := s.HomeStatus()
	if !loggedIn || credits != "122" {
		t.Fatalf("回退探测 = (%v, %q), want (true, 122)", loggedIn, credits)
	}
}
