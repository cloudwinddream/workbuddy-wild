package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestBarkSendKeyResolution(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		got = append(got, m)
		_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Update(Config{
		Server:     srv.URL,
		DefaultKey: "defaultkey",
		ModuleKeys: map[string]string{"wnflb": "wnflbkey", "multi": "k1,k2"},
	}); err != nil {
		t.Fatal(err)
	}

	// 模块覆盖 key
	if err := b.SendSync("wnflb", "标题", "正文"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["device_key"] != "wnflbkey" || got[0]["title"] != "标题" {
		t.Errorf("wnflb 推送 = %v", got)
	}

	// 未覆盖的模块用默认 key
	got = nil
	if err := b.SendSync("smzdm", "t", "b"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["device_key"] != "defaultkey" {
		t.Errorf("smzdm 默认 key = %v", got)
	}

	// 逗号分隔多 key 逐个发送
	got = nil
	if err := b.SendSync("multi", "t", "b"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("多 key 应发送 2 次，实际 %d", len(got))
	}

	// 未配置 key 时报错
	b2, _ := Open(t.TempDir())
	if err := b2.SendSync("wnflb", "t", "b"); err == nil {
		t.Error("无 key 应报错")
	}
	if b2.Enabled("wnflb") {
		t.Error("无 key 时 Enabled 应为 false")
	}

	// 配置落盘后重开可读
	b3, _ := Open(dir)
	if b3.KeyFor("wnflb") != "wnflbkey" || b3.KeyFor("other") != "defaultkey" {
		t.Errorf("重开后配置 = %+v", b3.RawConfig())
	}
	// 文件权限与位置
	if b3.path != filepath.Join(dir, "notify.json") {
		t.Errorf("path = %s", b3.path)
	}
}

func TestBarkHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":400,"message":"bad key"}`))
	}))
	defer srv.Close()
	b, _ := Open(t.TempDir())
	_ = b.Update(Config{Server: srv.URL, DefaultKey: "x", ModuleKeys: map[string]string{}})
	if err := b.SendSync("m", "t", "b"); err == nil {
		t.Error("HTTP 400 应报错")
	}
}

func TestBarkAccountKeyResolution(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		got = append(got, m)
		_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
	}))
	defer srv.Close()

	b, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Update(Config{
		Server:      srv.URL,
		DefaultKey:  "defaultkey",
		ModuleKeys:  map[string]string{"workbuddy": "wbplatformkey"},
		AccountKeys: map[string]string{"workbuddy:u1": "u1key"},
	}); err != nil {
		t.Fatal(err)
	}

	// 账号级 key 优先
	if err := b.SendAccountSync("workbuddy", "u1", "t", "b"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["device_key"] != "u1key" {
		t.Errorf("账号级 = %v", got)
	}
	// 同平台其他账号走平台 key
	got = nil
	if err := b.SendAccountSync("workbuddy", "u2", "t", "b"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["device_key"] != "wbplatformkey" {
		t.Errorf("平台级 = %v", got)
	}
	// 其他平台走默认 key
	got = nil
	if err := b.SendAccountSync("traework", "u9", "t", "b"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["device_key"] != "defaultkey" {
		t.Errorf("默认级 = %v", got)
	}
	// KeyForAccount 逐级回退
	if b.KeyForAccount("workbuddy", "u1") != "u1key" ||
		b.KeyForAccount("workbuddy", "u2") != "wbplatformkey" ||
		b.KeyForAccount("traework", "u9") != "defaultkey" {
		t.Error("KeyForAccount 回退链错误")
	}
	if !b.EnabledAccount("workbuddy", "u1") {
		t.Error("EnabledAccount 应为 true")
	}
}

func TestMask(t *testing.T) {
	if mask("") != "" || mask("short") != "****" {
		t.Error("mask 边界错误")
	}
	if mask("abcdefghijklmnopqrstuv") != "abcd****stuv" {
		t.Errorf("mask = %s", mask("abcdefghijklmnopqrstuv"))
	}
}
