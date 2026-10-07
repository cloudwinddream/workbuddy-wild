package smzdm

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// APIBaseURL 默认接口域名（SMZDM_BASE_URL 可覆盖）。
const APIBaseURL = "https://user-api.smzdm.com"

// APIError 是服务端返回的业务错误（error_code != 0）。
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("smzdm 接口错误(%d)：%s", e.Code, e.Msg)
	}
	return "smzdm 接口错误：" + e.Msg
}

// Client 是 SMZDM 签名 HTTP 客户端（只负责请求和签名）。
type Client struct {
	http        *http.Client
	baseURL     string
	cookie      string
	cookies     map[string]string
	platform    string
	profile     AppProfile
	version     string
	deviceID    string
	smzdmID     string
	securityKey string
	iphone      bool

	cookieMu       sync.Mutex
	onCookieUpdate func(string) // 服务端轮换 sess 后回调（存档更新）
}

// SetCookieUpdateHook 注册 Cookie 更新回调（服务端轮换 sess 时触发）。
func (c *Client) SetCookieUpdateHook(f func(string)) { c.onCookieUpdate = f }

var reSessPair = regexp.MustCompile(`(^|;\s*)sess=[^;]*`)

// captureRotatedSess 保存服务端轮换后的 sess：旧 sess 一旦过期签到
// 就会静默失效，这里把新值同步回内存与存档。
func (c *Client) captureRotatedSess(resp *http.Response) {
	for _, ck := range resp.Cookies() {
		if ck.Name != "sess" || ck.Value == "" {
			continue
		}
		c.cookieMu.Lock()
		if c.cookies["sess"] == ck.Value {
			c.cookieMu.Unlock()
			return
		}
		c.cookies["sess"] = ck.Value
		if reSessPair.MatchString(c.cookie) {
			c.cookie = reSessPair.ReplaceAllString(c.cookie, "${1}sess="+ck.Value)
		} else {
			c.cookie = "sess=" + ck.Value + "; " + c.cookie
		}
		updated := c.cookie
		c.cookieMu.Unlock()
		log.Printf("smzdm: 服务端轮换了 sess，已同步更新 Cookie")
		if c.onCookieUpdate != nil {
			c.onCookieUpdate(updated)
		}
		return
	}
}

// NewClient 用 Cookie 字符串创建客户端。Cookie 须包含 sess 字段；
// Android 协议还需要 smzdm_id + device_id 以动态生成 SK。
func NewClient(cookie, baseURL string) (*Client, error) {
	cookies := ParseCookieHeader(strings.TrimSpace(cookie))
	if cookies["sess"] == "" {
		return nil, fmt.Errorf("Cookie 缺少 sess 字段")
	}
	platform := cookies["device_smzdm"]
	if platform == "" {
		platform = "android"
	}
	profile := ResolveAppProfile(platform)
	iphone := IsIPhone(platform)
	version := cookies["device_smzdm_version"]
	if version == "" {
		version = cookies["v"]
	}
	if version == "" {
		version = profile.Version
	}
	c := &Client{
		http:     &http.Client{Timeout: 30 * time.Second},
		baseURL:  strings.TrimRight(baseURL, "/"),
		cookie:   strings.TrimSpace(cookie),
		cookies:  cookies,
		platform: platform,
		profile:  profile,
		version:  version,
		deviceID: cookies["device_id"],
		smzdmID:  cookies["smzdm_id"],
		iphone:   iphone,
	}
	if !iphone {
		sk, err := GenerateSecurityKey(c.smzdmID, c.deviceID, profile)
		if err != nil {
			return nil, fmt.Errorf("SK 生成失败：%w", err)
		}
		c.securityKey = sk
	}
	if c.baseURL == "" {
		c.baseURL = APIBaseURL
	}
	return c, nil
}

// IsIPhone 是否为 iPhone 协议。
func (c *Client) IsIPhone() bool { return c.iphone }

// SmzdmID 返回 Cookie 中的 smzdm_id（可作为账号标识展示）。
func (c *Client) SmzdmID() string { return c.smzdmID }

// appHeaders 构造 APP 请求头。
func (c *Client) appHeaders() http.Header {
	versionCode := c.cookies["device_smzdm_version_code"]
	if versionCode == "" {
		versionCode = c.profile.VersionCode
	}
	deviceModel := c.cookies["device_type"]
	if deviceModel == "" {
		deviceModel = "Redmi"
	}
	sysVer := c.cookies["device_system_version"]
	if sysVer == "" {
		sysVer = "10"
	}
	ua := fmt.Sprintf("smzdm_%s_V%s rv:%s (%s;%s%s;zh)smzdmapp",
		c.platform, c.version, versionCode, deviceModel,
		strings.ToUpper(c.platform[:1])+c.platform[1:], sysVer)
	h := http.Header{}
	h.Set("User-Agent", ua)
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("Cookie", c.cookie)
	h.Set("request_key", fmt.Sprintf("%d", 1000000000000000+rand.Int63n(9000000000000000)))
	return h
}

// signedForm 构造签名表单。
func (c *Client) signedForm(extra map[string]string) map[string]string {
	fields := map[string]string{
		"weixin":  "1",
		"basic_v": "0",
		"f":       c.platform,
		"v":       c.version,
		"time":    fmt.Sprintf("%d000", time.Now().Unix()),
	}
	if c.iphone {
		fields["zhuanzai_ab"] = "d"
	} else {
		fields["token"] = c.cookies["sess"]
		if c.securityKey != "" {
			fields["sk"] = c.securityKey
		}
	}
	for k, v := range extra {
		fields[k] = v
	}
	fields["sign"] = ComputeRequestSignature(fields, c.profile)
	return fields
}

// Post 发送签名 POST 请求并返回 data 层 payload。
func (c *Client) Post(path string, extra map[string]string) (map[string]any, error) {
	form := url.Values{}
	for k, v := range c.signedForm(extra) {
		form.Set(k, v)
	}
	return c.doPost(path, form, c.appHeaders())
}

// doPost 发送表单 POST 并拆信封（error_code != 0 转 APIError）。
func (c *Client) doPost(path string, form url.Values, headers http.Header) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header = headers
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	c.captureRotatedSess(resp)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var payload struct {
		ErrorCode any            `json:"error_code"`
		ErrorMsg  string         `json:"error_msg"`
		Data      map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("响应解析失败：%w", err)
	}
	codeStr := fmt.Sprintf("%v", payload.ErrorCode)
	if payload.ErrorCode != nil && codeStr != "0" && codeStr != "<nil>" {
		code := 0
		_, _ = fmt.Sscanf(codeStr, "%d", &code)
		return nil, &APIError{Code: code, Msg: payload.ErrorMsg}
	}
	if payload.Data == nil {
		payload.Data = map[string]any{}
	}
	return payload.Data, nil
}

// ---------------------------------------------------------------------------
// APP 签到（2026-10-07 用户 Reqable 抓包当前 iPhone APP 11.1.95 的真实请求）
//
// 实测教训：gen3（token+SK）、robot 流程、hex-ci 原方（touchstone_event
// + 强制安卓身份）回的都是"假成功"——error_code 0 但 APP 里仍可再签，
// 次日 APP 还会推送签到提醒。真实 APP 的签到请求是：
//   POST /checkin
//   basic_v=0&f=iphone&v=11.1.95&weixin=1&time=<毫秒>&zhuanzai_ab=d&sign=<MD5>
// 即：不带 token / sk / touchstone_event / captcha，Cookie 原样发送
// （iPhone 身份），签名覆盖 6 个字段。zhuanzai_ab 疑似签到页的 A/B
// 分桶标识（抓包值为 d），可用 SMZDM_ZHUANZAI_AB 覆盖。
// ---------------------------------------------------------------------------

func (c *Client) appSignHeaders() http.Header {
	rv := c.cookies["device_smzdm_version_code"]
	if rv == "" {
		if c.version == "11.1.95" {
			rv = "173" // 2026-10-07 抓包：11.1.95 的 rv 以 173 开头
		} else {
			rv = c.profile.VersionCode
		}
	}
	h := http.Header{}
	h.Set("User-Agent", fmt.Sprintf("smzdm %s rv:%s", c.version, rv))
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "zh-Hans-CN;q=1")
	h.Set("Cookie", c.cookie)
	h.Set("request_key", fmt.Sprintf("%018d", rand.Int63n(1e18)))
	return h
}

func zhuanzaiAB() string {
	if v := strings.TrimSpace(os.Getenv("SMZDM_ZHUANZAI_AB")); v != "" {
		return v
	}
	return "d"
}

// AppSign 执行真正的每日签到，返回签到响应 data（内含档案字段）。
// 对齐 APP 行为：先调 show_view_v2 拉签到页（best-effort），再调
// /checkin 签到。服务端以业务错误表示"今日已签到"时返回 APIError
// （IsAlreadySigned 识别）。
func (c *Client) AppSign() (map[string]any, error) {
	prefetch := map[string]string{
		"basic_v":     "0",
		"f":           signPlatform(c),
		"v":           c.version,
		"weixin":      "1",
		"time":        fmt.Sprintf("%d000", time.Now().Unix()),
		"zhuanzai_ab": zhuanzaiAB(),
	}
	prefetch["sign"] = ComputeRequestSignature(prefetch, c.profile)
	// 预取失败不影响签到（纯读页面）。
	_, _ = c.doPost("/checkin/show_view_v2", formOf(prefetch), c.appSignHeaders())

	fields := map[string]string{
		"basic_v":     "0",
		"f":           signPlatform(c),
		"v":           c.version,
		"weixin":      "1",
		"time":        fmt.Sprintf("%d000", time.Now().Unix()),
		"zhuanzai_ab": zhuanzaiAB(),
	}
	fields["sign"] = ComputeRequestSignature(fields, c.profile)
	return c.doPost("/checkin", formOf(fields), c.appSignHeaders())
}

func signPlatform(c *Client) string {
	if f := c.cookies["f"]; f != "" {
		return f
	}
	if c.platform != "" {
		return c.platform
	}
	return "iphone"
}

func formOf(fields map[string]string) url.Values {
	form := url.Values{}
	for k, v := range fields {
		form.Set(k, v)
	}
	return form
}
