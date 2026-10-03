package wnflb

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	reFormhash      = regexp.MustCompile(`name="formhash"\s+value="([a-f0-9]+)"`)
	reLoginhash     = regexp.MustCompile(`loginhash=([A-Za-z0-9]+)`)
	reDiscuzUID     = regexp.MustCompile(`discuz_uid\s*=\s*'(\d+)'`)
	reBoardLink     = regexp.MustCompile(`href="(forum-\d+-1\.html)"`)
	reTitle         = regexp.MustCompile(`(?i)<title>([^<]*)</title>`)
	reSpaceUID      = regexp.MustCompile(`mod=space&amp;uid=(\d+)`)
	reSpaceUIDPath  = regexp.MustCompile(`space-uid-(\d+)\.html`)
	reAuth          = regexp.MustCompile(`name="auth"\s+value="([A-Za-z0-9%_./=+]+)"`)
	reUpdateseccode = regexp.MustCompile(`updateseccode\(\s*['"]([A-Za-z0-9]+)['"]`)
	reSeccodeSpan   = regexp.MustCompile(`id="seccode_([A-Za-z0-9]+)"`)
	reSeccodeImg    = regexp.MustCompile(`misc\.php\?mod=seccode&update=([^&\"']+)&idhash=([A-Za-z0-9]+)`)
	reSeccodeInput  = regexp.MustCompile(`id="seccodeverify_([A-Za-z0-9]+)"`)
	reSeccodeHash   = regexp.MustCompile(`name="seccodehash"\s+value="([A-Za-z0-9]+)"`)
	reFxChkMenu     = regexp.MustCompile(`fx_chk_menu\s*=\s*(true|false)`)
	reCheckinFH     = regexp.MustCompile(`fx_checkin:checkin&formhash=([a-f0-9]+)&([a-f0-9]+)`)
	reCDATA         = regexp.MustCompile(`<!\[CDATA\[(.*?)\]\]>`)
	reMsgText       = regexp.MustCompile(`id="messagetext"[^>]*>(.*?)</div>\s*</div>`)
	reAlert         = regexp.MustCompile(`class="alert_(?:right|error|info)"[^>]*>(.*?)</div>`)
	reTags          = regexp.MustCompile(`<[^>]+>`)
	reSpaces        = regexp.MustCompile(`\s+`)
	reRank          = regexp.MustCompile(`第\s*(\d+)\s*个`)
)

// extractLoginFields 从登录页提取 formhash / loginhash。
func extractLoginFields(html string) (formhash, loginhash string) {
	if m := reFormhash.FindStringSubmatch(html); m != nil {
		formhash = m[1]
	}
	if m := reLoginhash.FindStringSubmatch(html); m != nil {
		loginhash = m[1]
	}
	return formhash, loginhash
}

// checkLoggedIn 检测是否已登录：discuz_uid 为真实 UID（游客为 '0'）。
func checkLoggedIn(html string) bool {
	if m := reDiscuzUID.FindStringSubmatch(html); m != nil {
		return m[1] != "0"
	}
	if strings.Contains(html, `class="logout"`) || strings.Contains(html, "mod=logging&action=logout") {
		return true
	}
	if strings.Contains(html, `name="username"`) && strings.Contains(html, `name="password"`) {
		return false
	}
	return false
}

// captchaInfo 验证码挑战参数。
type captchaInfo struct {
	Needed      bool
	Idhash      string
	SeccodeHash string
	Auth        string
}

// detectCaptcha 识别页面是否需要验证码并提取参数（登录页 / 二次挑战页）。
func detectCaptcha(html string) captchaInfo {
	var ci captchaInfo
	if m := reAuth.FindStringSubmatch(html); m != nil {
		ci.Auth = m[1]
	}
	if m := reUpdateseccode.FindStringSubmatch(html); m != nil {
		ci.Idhash = m[1]
	} else if m := reSeccodeSpan.FindStringSubmatch(html); m != nil {
		ci.Idhash = m[1]
	} else if m := reSeccodeImg.FindStringSubmatch(html); m != nil {
		ci.Idhash = m[2]
	} else if m := reSeccodeInput.FindStringSubmatch(html); m != nil {
		ci.Idhash = m[1]
	} else if m := reSeccodeHash.FindStringSubmatch(html); m != nil {
		ci.Idhash = m[1]
	} else if strings.Contains(html, `name="seccodeverify"`) {
		ci.Idhash = "SkyV" // 极端兜底（与上游一致）
	}
	if m := reSeccodeHash.FindStringSubmatch(html); m != nil {
		ci.SeccodeHash = m[1]
	} else {
		ci.SeccodeHash = ci.Idhash
	}
	ci.Needed = ci.Idhash != "" || ci.Auth != ""
	return ci
}

// extractMessage 提取 Discuz 提示信息页正文。
func extractMessage(html string) string {
	if m := reMsgText.FindStringSubmatch(html); m != nil {
		if t := stripTags(m[1]); t != "" {
			return t
		}
	}
	if m := reAlert.FindStringSubmatch(html); m != nil {
		return stripTags(m[1])
	}
	return ""
}

func stripTags(s string) string {
	return strings.TrimSpace(reTags.ReplaceAllString(s, ""))
}

// alreadySigned 首页是否已签到（fx_chk_menu=true）。
func alreadySigned(html string) bool {
	if m := reFxChkMenu.FindStringSubmatch(html); m != nil {
		return m[1] == "true"
	}
	return false
}

// extractCheckinFormhash 从首页提取签到插件的 formhash 对。
func extractCheckinFormhash(html string) (a, b string) {
	if m := reCheckinFH.FindStringSubmatch(html); m != nil {
		return m[1], m[2]
	}
	return "", ""
}

// checkinResult 签到结果。
type checkinResult struct {
	OK   bool
	Msg  string
	Gain string // 本次签到获得的积分（解析不到为 ""）
}

var reGain = regexp.MustCompile(`获得\s*(\d+)\s*积分`)

// parseCheckinResult 解析签到接口返回（CDATA 包裹的 HTML）。
func parseCheckinResult(text string) checkinResult {
	content := text
	if m := reCDATA.FindStringSubmatch(text); m != nil {
		content = m[1]
	}
	clean := reSpaces.ReplaceAllString(stripTags(content), " ")
	clean = strings.TrimSpace(clean)
	gain := ""
	if m := reGain.FindStringSubmatch(clean); m != nil {
		gain = m[1]
	}

	switch {
	case strings.Contains(clean, "签到成功"):
		if m := reRank.FindStringSubmatch(clean); m != nil {
			return checkinResult{true, "签到成功！今日第 " + m[1] + " 个签到", gain}
		}
		return checkinResult{true, "签到成功！", gain}
	case strings.Contains(clean, "已经签到") || strings.Contains(clean, "已签到"):
		return checkinResult{true, "今日已签到（重复签到）", gain}
	case strings.Contains(clean, "先登录") || strings.Contains(clean, "请登录"):
		return checkinResult{false, "登录已过期，请重新登录", ""}
	case strings.Contains(clean, "补签") && strings.Contains(clean, "成功"):
		return checkinResult{true, "补签成功", gain}
	}
	if len(clean) > 200 {
		clean = clean[:200]
	}
	if clean == "" {
		clean = "空响应"
	}
	return checkinResult{false, "未知响应: " + clean, gain}
}

var reCredits = []*regexp.Regexp{
	// 积分: 1234 / 积分：1,234（含总积分： 122）
	regexp.MustCompile(`积分\s*[：:]\s*([\d,]+)`),
	// >积分 1234< 之类的紧凑写法
	regexp.MustCompile(`>积分\s*([\d,]+)<`),
	// 标签与数字相邻：<em>积分</em>122 / 积分：</em>122
	regexp.MustCompile(`积分\s*[：:]?\s*</[^>]+>\s*([\d,]+)`),
	// 下划线分隔写法：_积分_ 122
	regexp.MustCompile(`_积分_\s*([\d,]+)`),
	// 表格布局：<th>积分</th><td>1234</td>
	regexp.MustCompile(`积分\s*</[^>]+>\s*<[^>]+>\s*([\d,]+)`),
	// 积分锚点后跟子元素：积分：<span>1234</span>
	regexp.MustCompile(`积分\s*[：:]\s*<[^>]+>\s*([\d,]+)`),
}

// extractUID 从页面提取当前用户 ID（空间链接 / discuz_uid 变量 / 空间路径）。
func extractUID(html string) string {
	for _, re := range []*regexp.Regexp{reSpaceUID, reDiscuzUID, reSpaceUIDPath} {
		if m := re.FindStringSubmatch(html); m != nil && m[1] != "0" {
			return m[1]
		}
	}
	return ""
}

// parseCredits 从已登录首页 HTML 解析当前积分，解析不到返回 ""。
func parseCredits(html string) string {
	for _, re := range reCredits {
		if m := re.FindStringSubmatch(html); m != nil {
			return strings.ReplaceAll(m[1], ",", "")
		}
	}
	return ""
}

var reCoins = []*regexp.Regexp{
	regexp.MustCompile(`_金币_\s*([\d,]+)`),
	regexp.MustCompile(`金币\s*[：:]\s*([\d,]+)`),
	regexp.MustCompile(`金币\s*[：:]?\s*</[^>]+>\s*([\d,]+)`),
	regexp.MustCompile(`>金币\s*([\d,]+)<`),
}

// parseCoins 解析当前金币，解析不到返回 ""。
func parseCoins(html string) string {
	for _, re := range reCoins {
		if m := re.FindStringSubmatch(html); m != nil {
			return strings.ReplaceAll(m[1], ",", "")
		}
	}
	return ""
}

var reUserGroup = []*regexp.Regexp{
	// <em>用户组: </em><a href="...">Lv.8金别福禄娃</a>
	regexp.MustCompile(`用户组\s*[：:]?\s*</[^>]+>\s*<a[^>]*>([^<]+)</a>`),
	regexp.MustCompile(`_用户组_\s*([^<\s]+)`),
	regexp.MustCompile(`用户组\s*[：:]\s*([^<\s，,；;]+)`),
}

// parseUserGroup 解析用户等级/用户组，解析不到返回 ""。
func parseUserGroup(html string) string {
	for _, re := range reUserGroup {
		if m := re.FindStringSubmatch(html); m != nil {
			if g := strings.TrimSpace(m[1]); g != "" {
				return g
			}
		}
	}
	return ""
}

var (
	reStreak = regexp.MustCompile(`连续签到\s*(\d+)\s*天`)
	reTotal  = regexp.MustCompile(`累计签到\s*(\d+)\s*天`)
)

// parseCheckinStats 从签到列表页解析连续/累计签到天数（解析不到为 0）。
func parseCheckinStats(html string) (streak, total int) {
	if m := reStreak.FindStringSubmatch(html); m != nil {
		streak, _ = strconv.Atoi(m[1])
	}
	if m := reTotal.FindStringSubmatch(html); m != nil {
		total, _ = strconv.Atoi(m[1])
	}
	return streak, total
}
