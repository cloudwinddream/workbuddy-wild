package login_trae

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// deviceIDKeyRe 从客户端 storage.json 的**键名**中提取设备号。
//
// TraeWork 桌面端把设备号明文写在键名上，形如：
//
//	"iCubeAuthInfo://icube-dc:4484256452647802": { ... }
//	                          ^^^^^^^^^^^^^^^^ 16 位数字 = 注册设备号
//
// 这是**唯一可靠**的设备号来源。实测依据（多位研究者独立复现）：
//   - 服务端按「注册指纹」校验 device id，自己随机的 16 位数字不被认作注册设备
//   - 同一账号同一时刻，只改设备号一个变量：
//     随机值 → claim 返回 9074；客户端真实值 → 返回 9095（通过设备检查）
//   - 走完整 OAuth 登录**不会**把设备号注册成可信设备，必须来自装过客户端的机器
//
// 注意：UUID 形式的 telemetry.devDeviceId **不是**这个值，用它会被更严格限流。
var deviceIDKeyRe = regexp.MustCompile(`iCubeAuthInfo://icube-dc:(\d{8,32})`)

// traeClientDirs TraeWork 桌面端的用户数据目录候选（Windows）。
// 覆盖 CN 版与 SOLO 版；与 VS Code 系一致，globalStorage 在 User 子目录下。
func traeClientDirs() []string {
	var dirs []string
	appData := os.Getenv("APPDATA")
	home, _ := os.UserHomeDir()
	if appData != "" {
		dirs = append(dirs,
			filepath.Join(appData, "TRAE SOLO CN", "User", "globalStorage"),
			filepath.Join(appData, "Trae CN", "User", "globalStorage"),
			filepath.Join(appData, "Trae", "User", "globalStorage"),
			filepath.Join(appData, "TraeWork", "User", "globalStorage"),
		)
	}
	if home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".trae-cn", "User", "globalStorage"),
			filepath.Join(home, ".trae", "User", "globalStorage"),
		)
	}
	return dirs
}

// findClientStorageJSON 返回第一个存在的客户端 storage.json 路径。
func findClientStorageJSON() string {
	for _, dir := range traeClientDirs() {
		p := filepath.Join(dir, "storage.json")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// ReadClientDeviceID 从本机 TraeWork 客户端的 storage.json 读取真实注册设备号。
//
// 返回 "" 表示本机没有可用的客户端设备号（未装客户端，或格式不符）。
// 调用方应在这种情况下回退到随机值，并提示用户签到可能失败。
//
// ⚠️ 重要：多账号场景下**必须对每个账号用同一个真实设备号**。
// 签到额度是按「设备」而非「账号」发放的（同一设备一天只能签一个账号），
// 但重点在于**服务端是否认这个设备号**：
//
//	随机生成的 32 位 hex            → claim 恒返 9074（设备校验失败）
//	客户端真实注册的 16 位数字       → claim 返 0 / 9095（成功或今日已签）
//
// 2026-09-30 单变量实测（同账号同 token，只改 X-Device-Id）：
//
//	5a4fc621c06a42300ba78afd67e74ad9 + {"req_source":1} → 9074
//	4484256452647802                 + {"req_source":1} → 成功
//	5a4fc621c06a42300ba78afd67e74ad9 + {}               → 成功
//	4484256452647802                 + {}               → 成功（9095 今日已签）
//
// 注意最后两行：请求体为空时服务端**跳过**设备校验，所以"空体也成功"曾
// 误导过前一轮修复。但带 `req_source` 才是桌面端的真实行为，因此设备号
// 仍必须正确。两者都要对，不能只对其中一个。
func ReadClientDeviceID() string {
	p := findClientStorageJSON()
	if p == "" {
		log.Printf("traework device: 未找到客户端 storage.json（已检查候选目录）")
		return ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		log.Printf("traework device: 读取 %s 失败 err=%v", p, err)
		return ""
	}

	// 设备号写在**键名**上，因此直接在原始文本里搜，不需要解析 JSON
	// （storage.json 体积可能很大，且格式随版本变化，正则更稳）。
	if m := deviceIDKeyRe.FindSubmatch(data); len(m) == 2 {
		id := string(m[1])
		log.Printf("traework device: 从客户端读到真实设备号（%d 位，前缀 %s…）src=%s", len(id), id[:4], p)
		return id
	}

	// 兜底：某些版本可能把它放在值里（{"deviceId":"..."}）
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err == nil {
		if v, ok := raw["deviceId"].(string); ok && strings.TrimSpace(v) != "" {
			id := strings.TrimSpace(v)
			log.Printf("traework device: 从 deviceId 字段读到设备号（%d 位）src=%s", len(id), p)
			return id
		}
	}
	log.Printf("traework device: %s 中未找到 iCubeAuthInfo 设备号", p)
	return ""
}

// ListClientDeviceIDs 返回本机所有可用的客户端注册设备号（去重，保持发现顺序）。
//
// 多账号用户常见做法是「装多个客户端实例，每个目录登一个账号」，此时每个
// storage.json 里都有一个真实注册设备号。为每个账号分配一个**不同的真实设备号**
// 既能让服务端通过校验，又能天然避开「同设备一天只能签一个账号」的限制。
//
// 返回空切片表示本机没有任何客户端设备号（此时调用方只能回退随机值，
// 并应提示用户签到很可能失败）。
func ListClientDeviceIDs() []string {
	seen := map[string]bool{}
	var out []string
	for _, dir := range traeClientDirs() {
		p := filepath.Join(dir, "storage.json")
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		m := deviceIDKeyRe.FindSubmatch(data)
		if len(m) != 2 {
			continue
		}
		id := string(m[1])
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		log.Printf("traework device: 发现客户端设备号 %s…（共 %d 个）src=%s", id[:4], len(out), p)
	}
	return out
}

// NextClientDeviceID 为第 idx 个账号挑一个设备号。
//
// 策略：优先按序号分配不同的真实设备号（idx < 可用数量时），
// 超出数量后复用一个真实设备号——此时同设备只能签一个账号，
// 但至少不会误报 9074（其余账号会拿到 9095「今日已签」，语义清晰）。
//
// 完全没有客户端设备号时返回 ""，由调用方回退。
func NextClientDeviceID(idx int) string {
	ids := ListClientDeviceIDs()
	if len(ids) == 0 {
		return ""
	}
	if idx < 0 {
		idx = 0
	}
	return ids[idx%len(ids)]
}
