package wnflb

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDiscuz 模拟带登录页验证码的 Discuz 论坛：
//   - 登录页自带验证码（updateseccode TESTID），裸提交一律拒绝；
//   - 验证码校验接口只认 "1234"；
//   - 带正确验证码的完整登录提交才发登录 Cookie。
type fakeDiscuz struct {
	loginPosts []map[string]string
}

func (f *fakeDiscuz) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/member.php", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method == http.MethodPost && q.Get("loginsubmit") == "yes" {
			_ = r.ParseForm()
			rec := map[string]string{}
			for k := range r.Form {
				rec[k] = r.Form.Get(k)
			}
			f.loginPosts = append(f.loginPosts, rec)
			if rec["seccodeverify"] == "1234" && rec["username"] == "alice" {
				http.SetCookie(w, &http.Cookie{Name: "pfx_2132_auth", Value: "1", Path: "/"})
				_, _ = w.Write([]byte(`<?xml version="1.0"?><root><![CDATA[欢迎您回来，alice]]></root>`))
				return
			}
			_, _ = w.Write([]byte(`<div id="messagetext"><p class="alert_error">验证码错误，请重新填写</p></div>`))
			return
		}
		_, _ = w.Write([]byte(`<html><body>
<form method="post" action="member.php?mod=logging&action=login&loginsubmit=yes&loginhash=LHtest">
<input type="hidden" name="formhash" value="abcd1234" />
<input type="text" name="username" /><input type="password" name="password" />
<span id="seccode_TESTID"></span>
<script>updateseccode('TESTID', 'tpl', 'member::logging');</script>
</form></body></html>`))
	})
	mux.HandleFunc("/misc.php", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("action") == "check" {
			if q.Get("secverify") == "1234" {
				_, _ = w.Write([]byte(`<root><![CDATA[succeed]]></root>`))
			} else {
				_, _ = w.Write([]byte(`<root><![CDATA[invalid]]></root>`))
			}
			return
		}
		// 验证码图片：PNG 魔数 + 填充
		_, _ = w.Write(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 128)...))
	})
	mux.HandleFunc("/forum.php", func(w http.ResponseWriter, r *http.Request) {
		uid := "0"
		if c, err := r.Cookie("pfx_2132_auth"); err == nil && c.Value == "1" {
			uid = "94140"
		}
		_, _ = w.Write([]byte(`<script>var discuz_uid = '` + uid + `';</script>`))
	})
	return mux
}

// 登录页带验证码时：Login 应先返回挑战（不提交密码），
// 输错换图重试，输对后带验证码完整提交并登录成功。
func TestLoginWithPageCaptcha(t *testing.T) {
	f := &fakeDiscuz{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	svc := New(t.TempDir(), srv.URL)

	ch, err := svc.Login("alice", "pw123")
	if !errors.Is(err, ErrCaptchaRequired) || ch == nil {
		t.Fatalf("Login 应返回验证码挑战，got ch=%v err=%v", ch, err)
	}
	if ch.Auth != "" || ch.Idhash != "TESTID" {
		t.Fatalf("挑战字段不对：%+v", ch)
	}
	if len(f.loginPosts) != 0 {
		t.Fatalf("出验证码前不应提交密码，已提交 %d 次", len(f.loginPosts))
	}

	img, err := svc.CaptchaImage(ch.ID)
	if err != nil || len(img) < 100 {
		t.Fatalf("验证码图片拉取失败：%v len=%d", err, len(img))
	}

	// 输错：校验接口不过 → 换图重试
	ch2, err := svc.SubmitCaptcha(ch.ID, "9999", "alice", "pw123")
	if !errors.Is(err, ErrCaptchaRetry) || ch2 == nil {
		t.Fatalf("输错应返回 ErrCaptchaRetry，got %v", err)
	}

	// 输对：完整提交（账号密码 + 验证码）→ 登录成功
	ch3, err := svc.SubmitCaptcha(ch2.ID, "1234", "alice", "pw123")
	if err != nil || ch3 != nil {
		t.Fatalf("正确验证码应登录成功，got ch=%v err=%v", ch3, err)
	}
	if len(f.loginPosts) != 1 {
		t.Fatalf("应只完整提交一次，got %d", len(f.loginPosts))
	}
	post := f.loginPosts[0]
	if post["username"] != "alice" || post["password"] != "pw123" ||
		post["seccodeverify"] != "1234" || post["seccodemodid"] != "member::logging" {
		t.Fatalf("完整提交字段不对：%v", post)
	}
	if logged, _ := svc.checkLoggedIn(); !logged {
		t.Fatal("登录后应处于登录态")
	}
	if acc, ok := svc.loadAccount(); !ok || acc.Username != "alice" {
		t.Fatal("登录成功应保存账号")
	}
}

// Cookie 导入：含 *_auth 且有效的直接登录；缺票据/无效的报错。
func TestImportCookie(t *testing.T) {
	f := &fakeDiscuz{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	svc := New(t.TempDir(), srv.URL)

	if err := svc.ImportCookie("pfx_2132_saltkey=xyz; other=1"); err == nil ||
		!strings.Contains(err.Error(), "_auth") {
		t.Fatalf("缺 *_auth 应报错，got %v", err)
	}
	if err := svc.ImportCookie("pfx_2132_auth=wrong; pfx_2132_saltkey=xyz"); err == nil ||
		!strings.Contains(err.Error(), "无效或已过期") {
		t.Fatalf("无效 Cookie 应报错，got %v", err)
	}
	if err := svc.ImportCookie("pfx_2132_auth=1; pfx_2132_saltkey=xyz"); err != nil {
		t.Fatalf("有效 Cookie 应导入成功，got %v", err)
	}
	if logged, _ := svc.checkLoggedIn(); !logged {
		t.Fatal("导入后应处于登录态")
	}
}

// 自动登录遇登录页验证码：明确报 ErrCaptchaRequired，不裸提交。
func TestEnsureLoggedInPageCaptcha(t *testing.T) {
	f := &fakeDiscuz{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	svc := New(t.TempDir(), srv.URL)
	svc.saveAccount("alice", "pw123")

	err := svc.EnsureLoggedIn()
	if !errors.Is(err, ErrCaptchaRequired) {
		t.Fatalf("应返回 ErrCaptchaRequired，got %v", err)
	}
	if !strings.Contains(err.Error(), "手动登录") {
		t.Fatalf("错误文案应提示手动登录，got %v", err)
	}
	if len(f.loginPosts) != 0 {
		t.Fatalf("自动登录不应裸提交密码，已提交 %d 次", len(f.loginPosts))
	}
}
