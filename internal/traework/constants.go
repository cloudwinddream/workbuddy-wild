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
// ⚠️ 文案说"设备"，但实测去重粒度是**账号级**，与设备号无关。
// 决定性矩阵实验（2026-09-30，每格都改 X-Device-Id）：
//
//	账号1（今天已签） + 真实设备号 -> 9095
//	账号1（今天已签） + 随机设备号 -> 9095
//	账号2（今天未签） + 真实设备号 -> success
//	账号2（今天未签） + 随机设备号 -> success
//
// ⇒ 同一账号无论用什么设备号结果都一样 ⇒ **决定因素是账号，不是设备**。
//
// 这也解释了用户的现象："同一个客户端手动切换账号，两个账号都能各签一次" ——
// 每个账号各自有一份每日名额，互不影响。
//
// 因此 9095 的正确语义是：**该账号今日已经领过**（幂等成功，不是失败）。
//
// ⚠️ 但要警惕一种异常：上游可能把账号标记为"已签"却**没有真正发额度包**。
// 实测账号1 全天 did_checked_in=true 却没有任何今日新建权益包。
// 因此对账必须查权益包的 start_time 是否落在今天，不能只信状态标志。
//
// 另注：claim 必须带 X-Device-Id，否则返回 9004（订单参数不正确）。
// 但设备号**只要是格式合法的 16 位数字即可**，不必须是本机真实注册的那个
// —— 实测随机 16 位号同样能让未签账号返回 success。
const CheckinAlreadyClaimedCode = 9095

// CheckinBadParamsCode 签到业务码 9004：缺少必需的订单参数（实测为缺 X-Device-Id）。
const CheckinBadParamsCode = 9004

// checkinIDPrefix 签到包的 entitlement_id 前缀。
//
// 实测上游给每个权益包都带自解释的 entitlement_id：
//
//	checkin_20260930_2222575719809915        ← 9/30 的签到奖励
//	checkin_20261001_2222575719809915        ← 10/1 的签到奖励
//	monthly_bonus_202610_2222575719809915    ← 2026年10月的月初奖励
//	367884760578                             ← 固定福利包（纯数字）
//
// 因此判断"今天是否真的发了签到额度"，直接读 entitlement_id 前缀 + 日期段
// 即可，无需依赖时间/金额等启发式（那套在 v0.6.5 用过，脆弱且易误判）。
const checkinIDPrefix = "checkin_"

// 以下两个常量保留作**兜底参考**：万一上游改了 entitlement_id 命名，
// 可退回"时间窗 + 金额"的启发式判定。当前主路径已改用 entitlement_id。
//
// checkinGrantMaxCredits 签到包的额度上限（用于与月度订阅包区分）。
// 实测签到包为 100 或 150；月度订阅包是 4000 / 500 这类大额。
const checkinGrantMaxCredits = 300

// scheduledGrantMaxHour 定时发放包的"凌晨整点"判定上界（不含）。
// 实测月初包 start_time 恰为 00:00:00，而签到不会落在凌晨整点。
const scheduledGrantMaxHour = 1

// CheckinClaimBody 签到 claim 接口的请求体。
//
// **不可用空对象 `{}`** —— 多个独立实现一致确认：空请求体会被服务端拒为 9074
// （与设备号校验失败共用同一业务码，文案都是"当前用户太多"，极易误判）。
// 必须与桌面端一致地带上 req_source。
const CheckinClaimBody = `{"req_source":1}`
