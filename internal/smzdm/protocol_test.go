package smzdm

import "testing"

// 以下向量由上游原版 Python 实现（smzdm_bot/protocol.py）生成，
// 用于验证 Go 移植与原版逐字节一致。

func TestComputeRequestSignature(t *testing.T) {
	form := map[string]string{
		"weixin": "1", "basic_v": "0", "f": "android", "v": "11.1.90",
		"time": "1727865600000", "token": "sess_abc123", "sk": "PLACEHOLDER_SK",
	}
	if got := ComputeRequestSignature(form, DefaultAppProfile); got != "108D18FDB24F30DCED434E61A30CEBEB" {
		t.Errorf("Android 签名 = %s", got)
	}

	formIP := map[string]string{
		"weixin": "1", "basic_v": "0", "f": "iphone", "v": "11.1.92",
		"time": "1727865600000", "zhuanzai_ab": "d",
	}
	if got := ComputeRequestSignature(formIP, IPhoneAppProfile); got != "B1709BCF66BD8BFA69B1D2E240375162" {
		t.Errorf("iPhone 签名 = %s", got)
	}
}

func TestGenerateSecurityKey(t *testing.T) {
	got, err := GenerateSecurityKey("12345678", "abcdef1234567890", DefaultAppProfile)
	if err != nil {
		t.Fatal(err)
	}
	if got != "vdAAnkDyJakvTHI9HsGLVu8YKjtiu1bvgy4jk0VdVoE=" {
		t.Errorf("SK = %s", got)
	}
	if _, err := GenerateSecurityKey("", "x", DefaultAppProfile); err == nil {
		t.Error("空 smzdm_id 应报错")
	}
}

func TestParseCookieHeader(t *testing.T) {
	c := ParseCookieHeader("sess=tok123; smzdm_id=12345678; device_id=abcdef1234567890; device_smzdm=android; other=x%20y;")
	if c["sess"] != "tok123" || c["smzdm_id"] != "12345678" {
		t.Errorf("Cookie 解析 = %v", c)
	}
	if c["other"] != "x y" {
		t.Errorf("URL 解码 = %q", c["other"])
	}
	// 末尾无分号也能解析
	c2 := ParseCookieHeader("sess=abc")
	if c2["sess"] != "abc" {
		t.Errorf("无分号结尾解析 = %v", c2)
	}
}

func TestResolveAppProfile(t *testing.T) {
	if ResolveAppProfile("iPhone").Version != "11.1.92" {
		t.Error("iPhone profile 解析错误")
	}
	if ResolveAppProfile("android").Version != "11.1.90" {
		t.Error("Android profile 解析错误")
	}
	if ResolveAppProfile("").Version != "11.1.90" {
		t.Error("默认应为 Android profile")
	}
}

func TestNewClientRequiresSess(t *testing.T) {
	if _, err := NewClient("smzdm_id=1; device_id=2;", ""); err == nil {
		t.Error("缺少 sess 应报错")
	}
	// Android 缺 device_id 无法生成 SK
	if _, err := NewClient("sess=x; smzdm_id=1;", ""); err == nil {
		t.Error("Android 缺 device_id 应报错")
	}
	// iPhone 不需要 SK
	c, err := NewClient("sess=x; device_smzdm=iphone;", "")
	if err != nil {
		t.Fatalf("iPhone client 创建失败: %v", err)
	}
	if !c.IsIPhone() {
		t.Error("应识别为 iPhone")
	}
}
