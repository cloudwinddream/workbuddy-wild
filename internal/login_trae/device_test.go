package login_trae

import (
	"os"
	"path/filepath"
	"testing"
)

// 回归：必须能从客户端 storage.json 的**键名**里提取设备号。
//
// 格式（TraeWork 桌面端）：
//
//	"iCubeAuthInfo://icube-dc:4484256452647802": {...}
//
// 这是唯一可靠的设备号来源；随机值会导致签到恒返回 9074。
func TestExtractDeviceIDFromKeyName(t *testing.T) {
	body := `{
	  "telemetry.devDeviceId": "a1b2c3d4-0000-1111-2222-333344445555",
	  "iCubeAuthInfo://icube-dc:4484256452647802": {"foo":"bar"},
	  "iCubeAuthInfo://usertag": {"x":1}
	}`
	m := deviceIDKeyRe.FindStringSubmatch(body)
	if len(m) != 2 {
		t.Fatal("未能从键名提取设备号")
	}
	if m[1] != "4484256452647802" {
		t.Fatalf("提取到 %q，want 4484256452647802", m[1])
	}
}

// 必须**忽略** UUID 形式的 telemetry.devDeviceId。
// 资料明确：UUID 不被识别为注册设备，用它会被更严格限流。
func TestExtractIgnoresUUIDDeviceID(t *testing.T) {
	body := `{"telemetry.devDeviceId":"a1b2c3d4-0000-1111-2222-333344445555"}`
	if m := deviceIDKeyRe.FindStringSubmatch(body); m != nil {
		t.Fatalf("UUID 不应被当作设备号，却匹配到 %q", m[1])
	}
}

// 位数太短的（明显不是设备号）不应匹配。
func TestExtractRejectsTooShort(t *testing.T) {
	body := `{"iCubeAuthInfo://icube-dc:12345":{}}`
	if m := deviceIDKeyRe.FindStringSubmatch(body); m != nil {
		t.Fatalf("5 位数字不应匹配，却得到 %q", m[1])
	}
}

// ReadClientDeviceID 在真实客户端目录不存在时应返回空串而不是 panic。
func TestReadClientDeviceIDMissingFile(t *testing.T) {
	// 临时改 APPDATA 指向空目录，确保找不到
	old := os.Getenv("APPDATA")
	oldHome := os.Getenv("HOME")
	tmp := t.TempDir()
	os.Setenv("APPDATA", tmp)
	os.Setenv("HOME", tmp) // 让 .trae-cn 候选也落空
	defer func() {
		os.Setenv("APPDATA", old)
		os.Setenv("HOME", oldHome)
	}()

	if got := ReadClientDeviceID(); got != "" {
		t.Fatalf("目录不存在时应返回空串，得到 %q", got)
	}
}

// 存在合法 storage.json 时应能读到设备号。
func TestReadClientDeviceIDFromTempDir(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "TRAE SOLO CN", "User", "globalStorage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"iCubeAuthInfo://icube-dc:9999888877776666":{"a":1}}`
	if err := os.WriteFile(filepath.Join(dir, "storage.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	old := os.Getenv("APPDATA")
	os.Setenv("APPDATA", tmp)
	defer os.Setenv("APPDATA", old)

	if got := ReadClientDeviceID(); got != "9999888877776666" {
		t.Fatalf("得到 %q，want 9999888877776666", got)
	}
}
