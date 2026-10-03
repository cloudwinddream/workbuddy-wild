package wnflb

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

func gbkBytes(t *testing.T, s string) []byte {
	t.Helper()
	out, _, err := transform.String(simplifiedchinese.GBK.NewEncoder(), s)
	if err != nil {
		t.Fatalf("gbk encode: %v", err)
	}
	return []byte(out)
}

// UTF-8 页面里混入个别 GBK 编码的历史内容时，不能整页被转成乱码：
// 积分锚点（正常 UTF-8）必须存活。根因回归（2026-10-03 实测）。
func TestDecodeBodyMixedContent(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<html><head><meta charset="utf-8"><title>发现之门</title></head><body>`)
	b.WriteString(`<a id="extcreditmenu">积分: 122</a>`)
	// 历史帖子标题：GBK 编码字节混入
	b.Write(gbkBytes(t, `<span>老帖子标题</span>`))
	b.WriteString(`</body></html>`)

	got := decodeBody([]byte(b.String()))
	if !strings.Contains(got, "积分: 122") {
		t.Fatalf("混合编码页积分锚点丢失：%.120q", got)
	}
	if got := parseCredits(got); got != "122" {
		t.Fatalf("parseCredits = %q, want 122", got)
	}
	if !strings.Contains(got, "发现之门") {
		t.Fatalf("标题被解坏：%.80q", got)
	}
}

// 真 GBK 页（meta 声明 gbk）仍按 GBK 正确解码。
func TestDecodeBodyGBKPage(t *testing.T) {
	page := `<html><head><meta charset="gbk"><title>积分</title></head><body><a>积分: 66</a></body></html>`
	got := decodeBody(gbkBytes(t, page))
	if !strings.Contains(got, "积分: 66") {
		t.Fatalf("GBK 页解码错误：%.120q", got)
	}
	if c := parseCredits(got); c != "66" {
		t.Fatalf("parseCredits = %q, want 66", c)
	}
}
