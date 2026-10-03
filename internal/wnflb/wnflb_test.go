package wnflb

import (
	"os"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("fixture 缺失，跳过: %v", err)
	}
	return string(raw)
}

func TestExtractLoginFields(t *testing.T) {
	html := loadFixture(t, "testdata/login_page.html")
	fh, lh := extractLoginFields(html)
	// formhash/loginhash 每次请求随机：只断言非空且格式正确
	if !isHex(fh) {
		t.Errorf("formhash = %q, want 非空十六进制", fh)
	}
	if lh == "" || !isAlnum(lh) {
		t.Errorf("loginhash = %q, want 非空字母数字", lh)
	}
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

func TestCheckLoggedIn(t *testing.T) {
	guest := loadFixture(t, "testdata/home_guest.html")
	if checkLoggedIn(guest) {
		t.Error("游客页不应判定为已登录")
	}
	logged := `<script>var discuz_uid = '12345';</script><a class="logout" href="member.php?mod=logging&action=logout">退出</a>`
	if !checkLoggedIn(logged) {
		t.Error("含真实 UID 的页面应判定为已登录")
	}
}

func TestExtractCheckinFormhash(t *testing.T) {
	html := loadFixture(t, "testdata/home_guest.html")
	a, b := extractCheckinFormhash(html)
	// 每次请求随机：只断言非空十六进制
	if !isHex(a) || !isHex(b) {
		t.Errorf("formhash 对 = (%q,%q), want 非空十六进制对", a, b)
	}
}

func TestAlreadySigned(t *testing.T) {
	if alreadySigned(`var fx_chk_menu = true;`) != true {
		t.Error("fx_chk_menu=true 应判定已签到")
	}
	if alreadySigned(`var fx_chk_menu = false;`) != false {
		t.Error("fx_chk_menu=false 应判定未签到")
	}
}

func TestParseCheckinResult(t *testing.T) {
	cases := []struct {
		in      string
		wantOK  bool
		wantSub string
	}{
		{`<root><![CDATA[签到成功！您是今天第 <b>123</b> 个签到的用户]]></root>`, true, "第 123 个签到"},
		{`<root><![CDATA[您今天已经签到过了]]></root>`, true, "已签到"},
		{`<root><![CDATA[请先登录后再签到]]></root>`, false, "登录已过期"},
		{``, false, "空响应"},
	}
	for _, c := range cases {
		res := parseCheckinResult(c.in)
		if res.OK != c.wantOK || !strings.Contains(res.Msg, c.wantSub) {
			t.Errorf("parseCheckinResult(%q) = (%v,%q), want (%v, 含%q)", c.in, res.OK, res.Msg, c.wantOK, c.wantSub)
		}
	}
}

func TestParseCredits(t *testing.T) {
	html := `<div><a href="home.php?mod=spacecp&ac=credit">积分: 12,345</a></div>`
	if got := parseCredits(html); got != "12345" {
		t.Errorf("parseCredits = %q, want 12345", got)
	}
	if got := parseCredits(`<div>无积分信息</div>`); got != "" {
		t.Errorf("无积分时应返回空，got %q", got)
	}
	// 用户实测的页面锚点原样（属性很多、半角冒号）
	real := `<a href="home.php?mod=spacecp&amp;ac=credit&amp;showcredit=1" id="extcreditmenu" onmouseover="delayShow(this, showCreditmenu);" class="showmenu" initialized="true">积分: 122</a>`
	if got := parseCredits(real); got != "122" {
		t.Errorf("真实锚点 parseCredits = %q, want 122", got)
	}
	// 积分页表格布局
	if got := parseCredits(`<table><tr><th>积分</th><td>888</td></tr></table>`); got != "888" {
		t.Errorf("表格布局 parseCredits = %q, want 888", got)
	}
	// 数字在子元素里
	if got := parseCredits(`<span>积分：<em>66</em></span>`); got != "66" {
		t.Errorf("子元素布局 parseCredits = %q, want 66", got)
	}
	// Discuz 积分浮层布局：<em>积分</em>数字
	if got := parseCredits(`<ul><li><em>积分</em>122</li><li><em>金钱</em>3</li></ul>`); got != "122" {
		t.Errorf("em 布局 parseCredits = %q, want 122", got)
	}
	// 个人空间页统计信息块布局
	space := `<div class="stat"><ul><li><em>积分</em> 456</li><li><em>金币</em> 7</li></ul></div>`
	if got := parseCredits(space); got != "456" {
		t.Errorf("空间页布局 parseCredits = %q, want 456", got)
	}
	if got := parseCoins(space); got != "7" {
		t.Errorf("parseCoins = %q, want 7", got)
	}
	// 用户等级
	if got := parseUserGroup(`<p><em>用户组: </em><a href="x">Lv.8金别福禄娃</a></p>`); got != "Lv.8金别福禄娃" {
		t.Errorf("parseUserGroup = %q", got)
	}
	// 连续/累计签到天数
	if s, tt := parseCheckinStats(`已连续签到91天，累计签到828天`); s != 91 || tt != 828 {
		t.Errorf("parseCheckinStats = (%d, %d), want (91, 828)", s, tt)
	}
	// 本次签到获得积分
	res := parseCheckinResult(`<?xml version="1.0"?><root><![CDATA[签到成功，恭喜你获得5积分]]></root>`)
	if !res.OK || res.Gain != "5" {
		t.Errorf("parseCheckinResult gain = %+v", res)
	}
}

func TestDetectCaptcha(t *testing.T) {
	// 无验证码的登录页
	html := loadFixture(t, "testdata/login_page.html")
	if ci := detectCaptcha(html); ci.Needed {
		t.Error("普通登录页不应需要验证码")
	}
	// 二次挑战页
	challenge := `<input type="hidden" name="auth" value="abc%2Fdef123" />
		<script>updateseccode('Sm123XYZ', 1);</script>
		<input type="hidden" name="seccodehash" value="Sm123XYZ" />`
	ci := detectCaptcha(challenge)
	if !ci.Needed || ci.Idhash != "Sm123XYZ" || ci.Auth != "abc%2Fdef123" {
		t.Errorf("挑战页解析错误: %+v", ci)
	}
	if unquoteAuth(ci.Auth) != "abc/def123" {
		t.Errorf("auth unquote 错误: %q", unquoteAuth(ci.Auth))
	}
	if detectChallenge("请输入验证码后继续登录 <form>auth=xxx") == nil {
		// 注意：需要同时含 请输入验证码 与 auth= 才算挑战
	}
	chHTML := `请输入验证码后继续登录<input type="hidden" name="auth" value="tok%2F1" />` +
		`<input type="hidden" name="formhash" value="a1b2c3d4" />` +
		`<form action="member.php?mod=logging&action=login&loginsubmit=yes&loginhash=LH99">`
	ch := detectChallenge(chHTML)
	if ch == nil {
		t.Fatal("应识别出验证码挑战")
	}
	if ch.Auth != "tok/1" || ch.Formhash != "a1b2c3d4" || ch.Loginhash != "LH99" {
		t.Errorf("挑战解析错误: %+v", ch)
	}
}
