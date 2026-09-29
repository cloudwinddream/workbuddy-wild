package traework

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/provider"
)

var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) provider.ErrKind {
	lower := strings.ToLower(body)
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return provider.ErrHardCredit
	}
	if status == http.StatusUnauthorized {
		for _, m := range sessionDeadMarkers {
			if strings.Contains(lower, strings.ToLower(m)) {
				return provider.ErrSessionDead
			}
		}
		return provider.ErrSessionDead
	}
	if status == http.StatusTooManyRequests {
		return provider.ErrSoftRate
	}
	if status == http.StatusNotFound {
		return provider.ErrNotFound
	}
	if status >= 500 {
		return provider.ErrServer
	}
	if status >= 400 {
		return provider.ErrClient
	}
	return provider.ErrNone
}

// CheckinRateLimitCode TraeWork 签到限流业务码（"当前参与用户太多，请稍后再试"）。
const CheckinRateLimitCode = 9074

// ErrCheckinRateLimited 签到在耗尽重试后仍被限流。
// 这是一个**可重试**的瞬时错误，不是永久失败——调用方（调度器）应安排稍后重试，
// 而不是把它当作账号异常（不应触发冷却或禁用）。
type ErrCheckinRateLimited struct {
	Attempts int
	Msg      string
}

func (e *ErrCheckinRateLimited) Error() string {
	return fmt.Sprintf("checkin rate limited after %d attempts: %s", e.Attempts, e.Msg)
}

// IsRateLimited 实现 scheduler 的 rateLimited 接口：标记为瞬时、可重试。
func (e *ErrCheckinRateLimited) IsRateLimited() bool { return true }

// IsCheckinRateLimited 报告错误是否为签到限流（可重试）。
func IsCheckinRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var target *ErrCheckinRateLimited
	return errors.As(err, &target)
}

// Client Trae SOLO 上游 HTTP 客户端。
type Client struct {
	HTTP          *http.Client
	StreamHTTP    *http.Client
	AgentHost     string
	UgHost        string
	OAuthHost     string
	ClientID      string
	CheckinRetry  time.Duration // 首次 9074 限流的等待基数；生产默认 8s
	CheckinMaxTry int           // 9074 限流的最大尝试次数；默认 4
}

func New() *Client {
	tr := &http.Transport{MaxIdleConns: 100, MaxIdleConnsPerHost: 20, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 120 * time.Second}
	return &Client{
		HTTP:          &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP:    &http.Client{Transport: tr},
		AgentHost:     AgentHost,
		UgHost:        UgHost,
		OAuthHost:     OAuthHost,
		ClientID:      ClientID,
		CheckinRetry:  8 * time.Second,
		CheckinMaxTry: 4,
	}
}

func (c *Client) agentBase() string { return c.AgentHost }
func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }

func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &provider.Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return raw, nil
}

func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	oldRefresh := a.RefreshToken
	log.Printf("traework refresh start uid=%s", a.UID)
	if strings.TrimSpace(a.RefreshToken) == "" {
		err := fmt.Errorf("no refreshToken")
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{"ClientID": c.ClientID, "RefreshToken": a.RefreshToken, "ClientSecret": "-", "UserID": ""}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	OAuthHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		err = fmt.Errorf("exchange parse: %w", err)
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	if resp.Result.Token == "" {
		err := fmt.Errorf("refresh_failed: no token in response — re-login required")
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	if resp.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		d := time.Duration(resp.Result.TokenExpireDuration)
		if resp.Result.TokenExpireDuration > 1e9 { // 上游通常是毫秒
			d *= time.Millisecond
		} else {
			d *= time.Second
		}
		a.ExpiresAt = time.Now().Add(d).Unix()
	}
	log.Printf("traework refresh success uid=%s refresh_rotated=%t expires_at=%d", a.UID, a.RefreshToken != oldRefresh, a.ExpiresAt)
	return nil
}

func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(PrepareBody(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	SOLOHeaders(req, a, true)
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("traework chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("traework chat_stream uid=%s: upstream %d %s body=%s", a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	// traework 上游 llm_utils_chat 强制 stream=true（见 PrepareBody），
	// 所有模型均为流式模式；非流式请求由本地 Aggregate() 缓冲 SSE 后聚合。
	// mode_type=nil 返回全部配置，按 config_name 去重避免流式/非流式重复。
	body := map[string]any{"function": Function, "config_names": nil, "need_prompt": false, "current_config_info": nil, "poly_prompt": true, "mode_type": nil, "agent_type": nil}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []struct {
			ConfigName    string `json:"config_name"`
			DisplayConfig struct {
				DisplayName string `json:"display_name"`
			} `json:"display_config"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	// 按 config_name 去重：上游可能为同一模型返回流式/非流式两条配置。
	seen := make(map[string]bool, len(resp.ConfigInfoList))
	out := make([]provider.ModelInfo, 0, len(resp.ConfigInfoList))
	for _, cfg := range resp.ConfigInfoList {
		name := strings.TrimSpace(cfg.ConfigName)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, provider.ModelInfo{ID: name, Name: cfg.DisplayConfig.DisplayName})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, 0, false, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, 0, false, err
	}
	var resp struct {
		CheckedIn bool   `json:"checked_in"`
		Credits   int64  `json:"credits"`
		Enable    bool   `json:"enable"`
		Code      int    `json:"code"`
		Message   string `json:"message"`
		Msg       string `json:"msg"`
		Success   *bool  `json:"success"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		err = fmt.Errorf("checkin status parse: %w", err)
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, 0, false, err
	}
	if resp.Code != 0 {
		err := fmt.Errorf("checkin status code=%d msg=%s", resp.Code, checkinResponseMessage(resp.Message, resp.Msg))
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, 0, false, err
	}
	if resp.Success != nil && !*resp.Success {
		err := fmt.Errorf("checkin status failed: %s", checkinResponseMessage(resp.Message, resp.Msg))
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, 0, false, err
	}
	log.Printf("traework checkin status uid=%s checked_in=%t credits=%d enable=%t", a.UID, resp.CheckedIn, resp.Credits, resp.Enable)
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

// CheckinClaim 领取签到额度。
//
// 上游在高峰时段会对 claim 接口返回业务码 9074（"当前参与用户太多，请稍后再试"），
// 这是**瞬时**限流而非错误。原实现只重试 1 次（等待 8s 固定），在高峰时段几乎必然
// 失败——实测连续 57 次签到全部因此失败。
//
// 现改为：最多 CheckinMaxTry 次尝试，等待时间按指数退避（base, 2×base, 4×base…）
// 并叠加抖动，避免多账号同时重试再次撞上高峰。耗尽后返回 *ErrCheckinRateLimited，
// 交由调度器安排稍后重试（而非判定账号异常）。
func (c *Client) CheckinClaim(a *auth.Auth) error {
	maxTry := c.CheckinMaxTry
	if maxTry <= 0 {
		maxTry = 4
	}
	base := c.CheckinRetry
	if base <= 0 {
		base = 8 * time.Second
	}

	var lastMsg string
	for attempt := 0; attempt < maxTry; attempt++ {
		req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte("{}")))
		if err != nil {
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		UgHeaders(req, a)
		data, err := c.doJSON(req)
		if err != nil {
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		code, msg, success, err := parseCheckinResponse(data)
		if err != nil {
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		lastMsg = msg

		// 9074 限流：非最后一次则退避等待后重试。
		if code == CheckinRateLimitCode {
			if attempt == maxTry-1 {
				break // 耗尽重试，交由调度器稍后重试
			}
			delay := backoffDelay(base, attempt)
			log.Printf("traework checkin claim rate-limited uid=%s code=%d attempt=%d/%d retry_after=%s",
				a.UID, code, attempt+1, maxTry, delay)
			time.Sleep(delay)
			continue
		}
		if code != 0 {
			err := fmt.Errorf("checkin claim code=%d msg=%s", code, msg)
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		if success != nil && !*success {
			err := fmt.Errorf("checkin claim failed: %s", msg)
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		log.Printf("traework checkin claim response uid=%s code=%d msg=%s", a.UID, code, msg)
		return nil // 9095 等业务无害响应交给后置 status 验证最终状态
	}

	log.Printf("traework checkin claim rate-limited-exhausted uid=%s attempts=%d last_msg=%s",
		a.UID, maxTry, lastMsg)
	return &ErrCheckinRateLimited{Attempts: maxTry, Msg: lastMsg}
}

// backoffDelay 计算第 attempt 次（0 基）重试的等待时间：
// base × 2^attempt，叠加 ±25% 抖动，上限 2 分钟。
// 抖动用于打散多账号的重试时刻，避免同步撞上同一波高峰。
func backoffDelay(base time.Duration, attempt int) time.Duration {
	const maxDelay = 2 * time.Minute
	d := base
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= maxDelay {
			d = maxDelay
			break
		}
	}
	// ±25% 抖动
	jitter := time.Duration(float64(d) * 0.25 * (rand.Float64()*2 - 1))
	out := d + jitter
	if out < time.Second {
		out = time.Second
	}
	return out
}

func (c *Client) DailyCheckin(a *auth.Auth) error {
	log.Printf("traework checkin start uid=%s", a.UID)
	checked, _, enable, err := c.CheckinStatus(a)
	if err != nil {
		return err
	}
	if checked {
		log.Printf("traework checkin already uid=%s", a.UID)
		return fmt.Errorf("已签到")
	}
	if !enable {
		err := fmt.Errorf("checkin disabled")
		log.Printf("traework checkin rejected uid=%s err=%v", a.UID, err)
		return err
	}
	if err := c.CheckinClaim(a); err != nil {
		return err
	}
	checked, _, _, err = c.CheckinStatus(a)
	if err != nil {
		return fmt.Errorf("checkin verification: %w", err)
	}
	if !checked {
		err := fmt.Errorf("checkin verification failed: checked_in=false")
		log.Printf("traework checkin failed uid=%s err=%v", a.UID, err)
		return err
	}
	log.Printf("traework checkin verified uid=%s", a.UID)
	return nil
}

func parseCheckinResponse(data []byte) (code int, msg string, success *bool, err error) {
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Success *bool  `json:"success"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, "", nil, fmt.Errorf("checkin response parse: %w", err)
	}
	return resp.Code, checkinResponseMessage(resp.Message, resp.Msg), resp.Success, nil
}

func checkinResponseMessage(message, msg string) string {
	if strings.TrimSpace(message) != "" {
		return strings.TrimSpace(message)
	}
	return strings.TrimSpace(msg)
}

func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) { return c.UserEntUsage(a) }

// UserEntUsage 查询 TraeWork 账号的**剩余**可用积分。
//
// 修订记录：
//
//	v0.5.1 修复「显示 4050 但实际只有 310」
//	  原实现把所有权益包的 `quota.credits_limit` **累加**后当剩余积分返回。
//	  `credits_limit` 是**额度上限**（发放总量），不是剩余量：
//	    1) 未减去已用量，导致虚高
//	    2) 多个权益包（签到礼包/试用/活动）累加，数字进一步膨胀
//	    3) 上限随发放变化，与"当前能用多少"无关
//
//	v0.5.2 修复「显示 150 也不对」（v0.5.1 引入的回退缺陷）
//	  v0.5.1 在权益包解析不出余额时，回退读取 checkin/status 的 `credits` 字段，
//	  并假定其语义是"当前可用积分"。**该假定是错的**：
//	  `credits` 在 16 次采样中恒为 150，不随天数/消耗变化——它是**签到奖励固定值**
//	  （对照 WorkBuddy 的 remain 有 2100/2200/2300/2400/2500 五种值，那才是真余额）。
//	  用固定值冒充余额，比返回错误更糟：会让「优先积分」策略把账号当成恒定满额。
//
//	  现在：**只认权益包里的真实余额**。解析不出就返回错误，绝不用其它接口的
//	  近似字段顶替。面板会显示"不可用"，这比显示一个假数字诚实。
//
// 同时把原始响应的 key 结构写入日志，便于确认真实字段名（一次刷新即可定位）。
func (c *Client) UserEntUsage(a *auth.Auth) (remain int64, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, err
	}

	// 用 map 解析：字段名可能随上游调整，按候选列表逐个尝试，避免硬编码单一字段名。
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0, fmt.Errorf("ent usage parse: %w", err)
	}
	logResourceShape(a.UID, raw)

	if v, ok := sumRemainFromEntitlements(raw); ok {
		log.Printf("traework credits uid=%s source=entitlements remain=%d", a.UID, v)
		return v, nil
	}

	// 解析失败：把权益包里所有**数值型**字段及其路径单独再打一遍。
	// shape 日志有 40 条上限，若响应字段很多可能截断；这里只挑数值字段，
	// 数量少、信息密度高——真实余额字段几乎必然是数值型。
	logBalanceCandidates(a.UID, raw)

	// 不再回退到 checkin/status（其 credits 是签到奖励值，非余额）。
	// 明确失败，让面板显示"积分不可用"，避免用假数字误导选号策略。
	log.Printf("traework credits uid=%s no remaining-credit field; checkin/status NOT used as fallback (its credits is a fixed checkin reward, not balance)", a.UID)
	return 0, fmt.Errorf("ent usage: no usable remaining-credit field in response")
}

// logBalanceCandidates 在解析失败时，打印权益包里全部数值型字段的路径与值。
//
// 目的：让用户刷新一次就能把真实字段名反馈回来。只打数值字段，
// 不含 token/uid 等敏感信息，也不会因为响应字段多而被截断。
func logBalanceCandidates(uid string, raw map[string]any) {
	packs := findPackList(raw)
	if len(packs) == 0 {
		log.Printf("traework credits uid=%s balance-candidates: <未找到权益包数组>", uid)
		return
	}
	seen := map[string]bool{}
	var out []string
	for i, pack := range packs {
		flat := map[string]any{}
		flattenInto(pack, flat, 0)
		keys := make([]string, 0, len(flat))
		for k := range flat {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			n, ok := toInt64(flat[k])
			if !ok {
				continue
			}
			entry := fmt.Sprintf("pack%d.%s=%d", i, k, n)
			if !seen[entry] {
				seen[entry] = true
				out = append(out, entry)
			}
		}
	}
	if len(out) == 0 {
		log.Printf("traework credits uid=%s balance-candidates: <权益包内无数值字段>", uid)
		return
	}
	sort.Strings(out)
	log.Printf("traework credits uid=%s balance-candidates: %s", uid, strings.Join(out, " "))
}

// 字段名清单按**特异性从高到低**排列，先命中的优先。
//
// 采用扁平 key 精确匹配（flattenInto 已统一转小写），所以不用考虑大小写，
// 但要警惕**过于宽泛**的字段名——它们会误匹配到无关的同名字段：
//   - "credits" 单独出现时，既可能是"剩余"也可能是"发放总量"，语义不定，故
//     只在最末位候补，且必须与 limit/used 同现时才参与计算
//   - "available" / "balance" 在部分上游里表示"可提现余额"而非"积分余额"，
//     排在具体名称之后
var remainFieldNames = []string{
	// 最明确：带 credits/credit 前缀且含 remain
	"credits_remain", "credit_remain", "remain_credits",
	// 较明确
	"credits_available", "available_credits", "credits_balance", "credits_surplus",
	// 宽泛（可能与其它业务字段重名），放最后
	"remain", "available", "balance", "surplus",
}

// usedFieldNames 可能表示"已用积分"的字段名。
var usedFieldNames = []string{
	"credits_used", "credit_used", "used_credits",
	"credits_consume", "credit_consume", "credits_cost",
	"used", "consume", "cost",
}

// limitFieldNames 可能表示"额度上限"的字段名（**不可**直接当剩余量）。
//
// 刻意**不收录**裸 "credits" / "total" / "quota" 这类宽泛名：
//   - "credits" 单独出现时语义不定（可能是签到奖励值，实测恒为 150）
//   - "quota" 通常是**容器对象**而非标量，收进来只会在 toInt64 时失败
//   - "total" 太泛，可能命中"总记录数"之类的分页字段
// 宁可少认字段、把包判为"解析失败"，也不要认错字段得出一个假余额。
var limitFieldNames = []string{
	"credits_limit", "credit_limit", "limit_credits",
	"credits_total", "credit_total", "total_credits",
}

// sumRemainFromEntitlements 从权益包里求"剩余积分"之和。
//
// 对每个权益包：
//   - 优先取明确的"剩余"字段
//   - 否则用 上限 - 已用 计算
//   - 两者都没有则该包 **解析失败**
//
// 返回 (总值, 是否至少命中一个包)。
//
// 注意第二个返回值的语义：它是"是否至少有一个包提供了可信的剩余量"，
// 而不是"是否找到了权益包"。若全部包都解析不出，返回 false 让调用方失败，
// 而不是返回一个 0 或残缺的和——残缺的和会让用户以为余额变少了。
func sumRemainFromEntitlements(raw map[string]any) (int64, bool) {
	packs := findPackList(raw)
	if len(packs) == 0 {
		return 0, false
	}
	var total int64
	hit := false
	for i, pack := range packs {
		bal := extractBalance(pack)
		if bal == nil {
			// 记下是哪个包解析失败，方便对照 shape 日志定位字段名
			if v, ok := pack["entitlement_base_info"]; ok {
				_ = v
			}
			log.Printf("traework ent pack[%d] skipped: no remain/limit-used fields", i)
			continue
		}
		total += *bal
		hit = true
	}
	return total, hit
}

// findPackList 在响应里定位权益包数组（字段名容错：多层级候选）。
func findPackList(raw map[string]any) []map[string]any {
	candidates := []string{
		"user_entitlement_pack_list", "entitlement_pack_list",
		"user_entitlement_packs", "pack_list", "packs",
	}
	// 先看顶层
	for _, key := range candidates {
		if v, ok := raw[key]; ok {
			if list := asMapList(v); len(list) > 0 {
				return list
			}
		}
	}
	// 再递归一层（常见包装：data / result / {"data":{"..."}}）
	for _, wrapper := range []string{"data", "result", "Result", "Data", "response", "Response"} {
		if sub, ok := raw[wrapper].(map[string]any); ok {
			if list := findPackList(sub); len(list) > 0 {
				return list
			}
		}
	}
	return nil
}

func asMapList(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// extractBalance 从一个权益包里提取"剩余积分"。
// 找不到任何余额/用量信息时返回 nil（表示无法判断，调用方跳过该包）。
func extractBalance(pack map[string]any) *int64 {
	// 递归收集该包里所有 key→值（含嵌套 quota / entitlement_base_info）
	flat := map[string]any{}
	flattenInto(pack, flat, 0)

	// 1) 明确的"剩余"字段
	for _, k := range remainFieldNames {
		if v, ok := flat[k]; ok {
			if n, ok := toInt64(v); ok {
				return &n
			}
		}
	}
	// 2) 上限 - 已用
	var limit, used *int64
	for _, k := range limitFieldNames {
		if v, ok := flat[k]; ok {
			if n, ok := toInt64(v); ok {
				limit = &n
				break
			}
		}
	}
	for _, k := range usedFieldNames {
		if v, ok := flat[k]; ok {
			if n, ok := toInt64(v); ok {
				used = &n
				break
			}
		}
	}
	if limit != nil && used != nil {
		remain := *limit - *used
		if remain < 0 {
			remain = 0
		}
		return &remain
	}
	// 3) 只有上限没有已用 → 无法判断剩余，返回 nil
	//    （这是本次修复的关键：旧实现直接拿上限当余额，导致虚高）
	return nil
}

// flattenInto 递归展开嵌套 map（深度上限 4，避免异常结构导致栈问题）。
func flattenInto(m map[string]any, out map[string]any, depth int) {
	if depth > 4 {
		return
	}
	for k, v := range m {
		lk := strings.ToLower(k)
		if _, exists := out[lk]; !exists {
			out[lk] = v
		}
		if sub, ok := v.(map[string]any); ok {
			flattenInto(sub, out, depth+1)
		}
	}
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// logResourceShape 打印 ent_usage 响应的字段结构（只打 key 与数值，不含 token）。
//
// 用于确认上游真实字段名：积分字段名一旦变化/新增，看这行日志即可定位。
// 采用**扁平路径**输出（如 entitlement_base_info.quota.credits_remain=310），
// 这样嵌套在哪一层、字段叫什么都能直接看出来，不受响应包装层数影响。
func logResourceShape(uid string, raw map[string]any) {
	pairs := make([]string, 0, 16)
	collectShape(raw, "", &pairs, 0)
	sort.Strings(pairs)
	// 控制长度，避免超长响应刷爆日志
	const maxPairs = 40
	if len(pairs) > maxPairs {
		pairs = append(pairs[:maxPairs], fmt.Sprintf("...(+%d)", len(pairs)-maxPairs))
	}
	log.Printf("traework ent_usage shape uid=%s %s", uid, strings.Join(pairs, " "))
}

// collectShape 递归收集 "路径=值" 对；只输出标量与非空容器摘要。
// 深度上限放宽到 8：上游响应常有 data/result 多层包装，过浅会看不到 quota 层。
func collectShape(v any, prefix string, out *[]string, depth int) {
	const maxDepth = 8
	if depth > maxDepth {
		*out = append(*out, prefix+"=<max-depth>")
		return
	}
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			collectShape(t[k], p, out, depth+1)
		}
	case []any:
		if len(t) == 0 {
			*out = append(*out, prefix+"=[]")
			return
		}
		*out = append(*out, fmt.Sprintf("%s.len=%d", prefix, len(t)))
		// 展开前几个元素：单个包结构相同，但不同包的字段名可能不同
		// （例如有的包给 credits_remain、有的只给 limit+used），
		// 因此至少展开 3 个，确保能看出各包字段差异。
		limit := len(t)
		if limit > 3 {
			limit = 3
		}
		for i := 0; i < limit; i++ {
			collectShape(t[i], fmt.Sprintf("%s[%d]", prefix, i), out, depth+1)
		}
		if len(t) > limit {
			*out = append(*out, fmt.Sprintf("%s[%d..]=<省略 %d 项>", prefix, limit, len(t)-limit))
		}
	default:
		// 标量：数值 / 字符串 / 布尔 / null
		*out = append(*out, fmt.Sprintf("%s=%v", prefix, t))
	}
}

func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	OAuthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.JWT())
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, nil
}

func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }
func (c *Client) Stream(w http.ResponseWriter, r io.Reader) error   { return Stream(w, r) }
func (c *Client) Aggregate(r io.Reader) (map[string]any, error)     { return Aggregate(r) }

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
