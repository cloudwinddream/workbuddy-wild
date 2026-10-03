package wnflb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

const requestTimeout = 30 * time.Second

var defaultHeaders = map[string]string{
	"User-Agent":      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
}

// Client 带 Cookie 持久化的论坛 HTTP 客户端。
type Client struct {
	base       string
	cookieFile string
	http       *http.Client
	mu         sync.Mutex
}

type cookieEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func newClient(base, dir string) *Client {
	jar, _ := cookiejar.New(nil)
	c := &Client{
		base:       base,
		cookieFile: filepath.Join(dir, "cookies.json"),
		http:       &http.Client{Timeout: requestTimeout, Jar: jar},
	}
	c.loadCookies()
	return c
}

func (c *Client) baseURL() *url.URL {
	u, _ := url.Parse(c.base)
	return u
}

// loadCookies 从文件恢复 Cookie（上游脚本同款 name→value 字典语义）。
func (c *Client) loadCookies() {
	raw, err := os.ReadFile(c.cookieFile)
	if err != nil {
		return
	}
	var entries []cookieEntry
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		return
	}
	cookies := make([]*http.Cookie, 0, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		cookies = append(cookies, &http.Cookie{Name: e.Name, Value: e.Value})
	}
	c.http.Jar.SetCookies(c.baseURL(), cookies)
}

// saveCookies 把当前 Cookie 落盘（每次请求后调用）。
func (c *Client) saveCookies() {
	cookies := c.http.Jar.Cookies(c.baseURL())
	entries := make([]cookieEntry, 0, len(cookies))
	for _, k := range cookies {
		entries = append(entries, cookieEntry{Name: k.Name, Value: k.Value})
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return
	}
	_ = os.WriteFile(c.cookieFile, raw, 0o600)
}

// clearCookies 清空内存与文件中的 Cookie。
func (c *Client) clearCookies() {
	c.mu.Lock()
	defer c.mu.Unlock()
	jar, _ := cookiejar.New(nil)
	c.http.Jar = jar
	_ = os.Remove(c.cookieFile)
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	for k, v := range defaultHeaders {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.http.Do(req)
	if err == nil {
		c.saveCookies()
	}
	return resp, err
}

// getText GET 并按 UTF-8/GBK 解码为文本。
func (c *Client) getText(url string, headers map[string]string) (string, error) {
	return c.getTextCtx(context.Background(), url, headers)
}

// getTextCtx 同 getText，但受 ctx 超时/取消约束（状态探测用短超时，
// 避免论坛响应慢时拖死调用方）。
func (c *Client) getTextCtx(ctx context.Context, url string, headers map[string]string) (string, error) {
	body, _, _, err := c.getDiag(ctx, url, headers)
	return body, err
}

// getDiag 探测专用抓取：额外返回 HTTP 状态与重定向后的最终 URL，
// 用于日志定位"实际拿到了什么页面"。
func (c *Client) getDiag(ctx context.Context, rawurl string, headers map[string]string) (body string, status int, finalURL string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return "", 0, "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.do(req)
	if err != nil {
		return "", 0, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", resp.StatusCode, "", err
	}
	final := rawurl
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return decodeBody(raw), resp.StatusCode, final, nil
}

// postForm POST 表单并解码为文本。
func (c *Client) postForm(url string, form url.Values, headers map[string]string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	return decodeBody(raw), nil
}

// getBytes GET 返回原始字节（验证码图片用）。
func (c *Client) getBytes(url string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// decodeBody 优先按 UTF-8 解码（实测论坛返回 UTF-8），否则按 GBK 解码
// （上游脚本注明论坛为 GBK，历史页面/代理可能返回 GBK）。
// decodeBody 把论坛页面解码为 UTF-8 文本。
//
// 编码策略（实测教训）：不能"整页 utf8.Valid 才按 UTF-8、否则整页按
// GBK"——已登录页面常混有个别 GBK 编码的历史内容（老帖标题等），
// 一个坏字节就会让整页被 GBK 转码，全页中文变乱码（积分锚点也随之
// 丢失，而 ASCII 的 discuz_uid 等不受影响，极具迷惑性）。
// 故：页面声明 GBK 才按 GBK；否则按 UTF-8 容错解码（坏字节变 �，
// 只坏那几个字，不伤全页）。
func decodeBody(raw []byte) string {
	if declaresGBK(raw) {
		if out, _, err := transform.Bytes(simplifiedchinese.GBK.NewDecoder(), raw); err == nil {
			return string(out)
		}
	}
	if utf8.Valid(raw) {
		return string(raw)
	}
	return strings.ToValidUTF8(string(raw), "�")
}

// declaresGBK 页面是否声明 GBK 系编码（看头部 meta charset）。
var reDeclaredGBK = regexp.MustCompile(`(?i)charset\s*=\s*"?gb(?:k|2312|18030)`)

func declaresGBK(raw []byte) bool {
	head := raw
	if len(head) > 4096 {
		head = head[:4096]
	}
	return reDeclaredGBK.Match(head)
}
