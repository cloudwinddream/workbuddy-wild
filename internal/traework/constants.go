// Package traework 封装 Trae SOLO 免费通道上游协议。
package traework

const (
	AgentHost      = "https://trae-api-cn.mchost.guru"
	UgHost         = "https://api.trae.cn"
	OAuthHost      = "https://api.trae.com.cn"
	ConsoleHost    = "https://www.trae.cn"
	ClientID       = "en1oxy7wnw8j9n"
	AppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	IdeVersion     = "0.1.43"
	IdeVersionCode = "20260716"
	DeviceBrand    = "83DG"
	OSVersion      = "Windows 11 Pro"
	Function       = "solo_work_lite"

	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"

	// EpCurrentEntList 当前权益列表 —— 这是**唯一**能拿到真实「剩余积分」的接口。
	// 它返回 usage_summary{total_amount, consumed_amount}，剩余 = total - consumed。
	// （2026-09-29 抓包确认：4050 - 3751.3 = 298.7，与官网显示一致）
	EpCurrentEntList = "/trae/api/v2/pay/user_current_entitlement_list"
)

const DefaultConfigName = "glm-5.2"

// CheckinAlreadyClaimedCode 签到业务码 9095：「当前设备今日已经签到，请明日再来哦～」。
//
// ⚠️ 关键语义：这是**设备级**去重，不是"本账号已签到"。
// 上游按设备计发签到额度，一天一份；同一设备号下的多个账号只有第一个能领到，
// 其余账号 claim 都返 9095。
//
// 麻烦之处在于 status 的 `did_checked_in` 也是**设备级**的 —— 拿到 9095 的账号
// 查 status 一样显示 did_checked_in=true，因此**无法用 status 区分到底谁领到了**。
// 唯一可靠的判据是查权益包有没有**今天新建的**（start_time 落在今天）。
//
// 因此本码必须当作"本账号没领到"来处理（返回 ErrCheckinAlreadyClaimed），
// 而不能当成幂等成功 —— 否则会误报"签到成功"而积分其实一分未增。
const CheckinAlreadyClaimedCode = 9095

// CheckinClaimBody 签到 claim 接口的请求体。
//
// **不可用空对象 `{}`** —— 多个独立实现一致确认：空请求体会被服务端拒为 9074
// （与设备号校验失败共用同一业务码，文案都是"当前用户太多"，极易误判）。
// 必须与桌面端一致地带上 req_source。
const CheckinClaimBody = `{"req_source":1}`
