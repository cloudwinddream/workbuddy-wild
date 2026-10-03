// Package quark 为夸克网盘提供 Cookie 登录的每日签到（领签到空间）。
//
// 协议逆向来自开源项目 Cp0204/quark-auto-save 与 Liu8Can/Quark_Auto_Check_In
// 的签到部分：
//   - 认证：用户粘贴整段 Cookie，其中包含 kps/sign/vcode 三个参数；
//     成长接口以这三个参数作查询参数调用，账号信息接口用整段 Cookie。
//   - 成长信息：GET drive-m.quark.cn/1/clouddrive/capacity/growth/info
//   - 签到：POST .../capacity/growth/sign，JSON {"sign_cyclic":true}
//   - 账号信息：GET pan.quark.cn/account/info（整段 Cookie）取昵称。
package quark

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// GrowthBaseURL 移动端成长接口域名。
	GrowthBaseURL = "https://drive-m.quark.cn"
	// AccountBaseURL 网页版账号接口域名。
	AccountBaseURL = "https://pan.quark.cn"

	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/3.14.2 Chrome/112.0.5615.165 Electron/24.1.3.8 Safari/537.36 Channel/pckk_other_ch"
)

// 凭证参数提取：Cookie 串或抓包 URL 中以 ; & 空格分隔的 kps/sign/vcode。
// 值允许的字符集与上游脚本一致（[a-zA-Z0-9%+/=]），且 %25 需还原为 %。
var (
	reKps   = regexp.MustCompile(`(?:^|[;\s&])kps=([a-zA-Z0-9%+/=]+)`)
	reSign  = regexp.MustCompile(`(?:^|[;\s&])sign=([a-zA-Z0-9%+/=]+)`)
	reVcode = regexp.MustCompile(`(?:^|[;\s&])vcode=([a-zA-Z0-9%+/=]+)`)
)

// ExtractParams 从 Cookie 串或抓包 URL 中提取 kps/sign/vcode。
// 缺任何一个返回错误（提示用户粘贴完整 Cookie）。
func ExtractParams(raw string) (kps, sign, vcode string, err error) {
	get := func(re *regexp.Regexp) string {
		m := re.FindStringSubmatch(raw)
		if m == nil {
			return ""
		}
		return strings.ReplaceAll(m[1], "%25", "%")
	}
	kps, sign, vcode = get(reKps), get(reSign), get(reVcode)
	var missing []string
	if kps == "" {
		missing = append(missing, "kps")
	}
	if sign == "" {
		missing = append(missing, "sign")
	}
	if vcode == "" {
		missing = append(missing, "vcode")
	}
	if len(missing) > 0 {
		return "", "", "", fmt.Errorf("没找到 %s：网页版 Cookie 里没有这三个参数，请从夸克 APP 抓包复制 drive-m.quark.cn 请求的完整 URL", strings.Join(missing, "/"))
	}
	return kps, sign, vcode, nil
}

// GrowthInfo 成长信息（总容量、签到累计、连签进度等）。
type GrowthInfo struct {
	TotalCapacity  int64  `json:"total_capacity"`
	UseCapacity    int64  `json:"use_capacity"`
	MemberType     string `json:"member_type"`
	Is88VIP        bool   `json:"88VIP"`
	CapComposition struct {
		SignReward int64 `json:"sign_reward"`
	} `json:"cap_composition"`
	CapSign struct {
		SignDaily       bool  `json:"sign_daily"`
		SignDailyReward int64 `json:"sign_daily_reward"`
		SignProgress    int   `json:"sign_progress"`
		SignTarget      int   `json:"sign_target"`
	} `json:"cap_sign"`
}

// MemberLabel 会员类型中文名。
func (g *GrowthInfo) MemberLabel() string {
	switch g.MemberType {
	case "NORMAL":
		return "普通用户"
	case "EXP_SVIP":
		return "88VIP"
	case "SUPER_VIP":
		return "SVIP"
	case "Z_VIP":
		return "SVIP+"
	}
	if g.MemberType != "" {
		return g.MemberType
	}
	if g.Is88VIP {
		return "88VIP"
	}
	return "普通用户"
}

// Client 夸克网盘成长接口客户端。
type Client struct {
	cookie string
	kps    string
	sign   string
	vcode  string

	growthBase  string
	accountBase string
	http        *http.Client
}

// NewClient 从整段 Cookie 构造客户端（校验 kps/sign/vcode 齐备）。
// growthBase/accountBase 为空时用官方域名（测试可注入假服务器）。
func NewClient(cookie, growthBase, accountBase string) (*Client, error) {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return nil, fmt.Errorf("Cookie 不能为空")
	}
	kps, sign, vcode, err := ExtractParams(cookie)
	if err != nil {
		return nil, err
	}
	if growthBase == "" {
		growthBase = GrowthBaseURL
	}
	if accountBase == "" {
		accountBase = AccountBaseURL
	}
	return &Client{
		cookie: cookie, kps: kps, sign: sign, vcode: vcode,
		growthBase: growthBase, accountBase: accountBase,
		http: &http.Client{Timeout: 20 * time.Second},
	}, nil
}

func (c *Client) params() url.Values {
	q := url.Values{}
	q.Set("pr", "ucpro")
	q.Set("fr", "android")
	q.Set("kps", c.kps)
	q.Set("sign", c.sign)
	q.Set("vcode", c.vcode)
	return q
}

type envelope struct {
	Status  int             `json:"status"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// do 执行一次请求并拆信封；data 非空才算成功（与上游脚本判定一致）。
func (c *Client) do(method, rawurl string, body any, withCookie bool) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, rawurl, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	if withCookie {
		req.Header.Set("Cookie", c.cookie)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求夸克接口失败：%w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("夸克接口 HTTP %d", resp.StatusCode)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("夸克接口返回无法解析：%w", err)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		msg := env.Message
		if msg == "" {
			msg = fmt.Sprintf("code=%d", env.Code)
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return env.Data, nil
}

// GrowthInfo 拉取成长信息。
func (c *Client) GrowthInfo() (*GrowthInfo, error) {
	u := c.growthBase + "/1/clouddrive/capacity/growth/info?" + c.params().Encode()
	data, err := c.do(http.MethodGet, u, nil, false)
	if err != nil {
		return nil, err
	}
	var info GrowthInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("成长信息解析失败：%w", err)
	}
	return &info, nil
}

// Sign 执行今日签到，返回本次获得的空间字节数。
func (c *Client) Sign() (int64, error) {
	u := c.growthBase + "/1/clouddrive/capacity/growth/sign?" + c.params().Encode()
	data, err := c.do(http.MethodPost, u, map[string]any{"sign_cyclic": true}, false)
	if err != nil {
		return 0, err
	}
	var out struct {
		SignDailyReward int64 `json:"sign_daily_reward"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, fmt.Errorf("签到结果解析失败：%w", err)
	}
	return out.SignDailyReward, nil
}

// Nickname 用整段 Cookie 查账号昵称（查不到返回 ""，不算错误）。
func (c *Client) Nickname() string {
	u := c.accountBase + "/account/info?fr=pc&platform=pc"
	data, err := c.do(http.MethodGet, u, nil, true)
	if err != nil {
		return ""
	}
	var out struct {
		Nickname string `json:"nickname"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return ""
	}
	return out.Nickname
}

// FormatBytes 字节数转人类可读（1.50 GB / 512 MB / 0 B）。
func FormatBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	s := fmt.Sprintf("%.2f", f)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + " " + units[i]
}
