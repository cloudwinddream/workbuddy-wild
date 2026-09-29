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
		log.Printf("traework device: 从客户端读到真实设备号（%d 位，前缀 %s…）", len(id), id[:4])
		return id
	}

	// 兜底：某些版本可能把它放在值里（{"deviceId":"..."}）
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err == nil {
		if v, ok := raw["deviceId"].(string); ok && strings.TrimSpace(v) != "" {
			id := strings.TrimSpace(v)
			log.Printf("traework device: 从 deviceId 字段读到设备号（%d 位）", len(id))
			return id
		}
	}
	log.Printf("traework device: %s 中未找到 iCubeAuthInfo 设备号", p)
	return ""
}
