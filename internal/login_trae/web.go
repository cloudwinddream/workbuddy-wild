package login_trae

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/rockswang/workbuddy-wild/internal/traework"
)

// 本文件为无头/Web 场景提供 TraeWork 登录支持（与桌面端 Start 的区别见下）。
//
// 桌面端 Start() 会在 127.0.0.1 上起一个随机端口的临时监听，把
// auth_callback_url 指过去——这要求浏览器必须跑在同一台机器上。
// Docker/服务器场景下浏览器在用户电脑上，回调必须走 Web 服务的主端口，
// 因此这里把"拼授权 URL"与"接收回调"拆开：
//   - PrepareHeadless：只生成 machineID/deviceID、写 state 文件、返回授权 URL；
//   - NoteCallback：由调用方的 HTTP 服务在收到浏览器回调时调用，把
//     refreshToken 写回 state 文件；后续 Poll() 复用不变。

// PrepareHeadless 为无头场景准备 TraeWork 登录，返回拼好 auth_callback_url
// 的授权 URL。deviceID 为空时自动尝试读取客户端真实设备号，取不到回退随机值
// （与 Start 一致；此时签到可能因设备校验返回 9074，但积分查询与 API 代理可用）。
func PrepareHeadless(statePath, deviceID, callbackURL string) (string, error) {
	machineID := randHex(16)
	if strings.TrimSpace(deviceID) == "" {
		deviceID = ReadClientDeviceID()
	}
	if deviceID == "" {
		deviceID = randHex(16)
		log.Printf("traework device: 未取到客户端设备号，回退随机值；签到可能因设备校验失败（9074）")
	}
	st := state{MachineID: machineID, DeviceID: deviceID}
	if err := writeState(statePath, st); err != nil {
		return "", err
	}

	u, _ := url.Parse(traework.ConsoleHost + "/authorization")
	v := u.Query()
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", "2.3.62834")
	v.Set("auth_type", "local")
	v.Set("client_id", traework.ClientID)
	v.Set("redirect", "0")
	v.Set("login_trace_id", randHex(8))
	v.Set("auth_callback_url", callbackURL)
	v.Set("machine_id", machineID)
	v.Set("device_id", deviceID)
	v.Set("x_device_id", deviceID)
	v.Set("x_machine_id", machineID)
	v.Set("x_device_brand", "PC")
	v.Set("x_device_type", "PC")
	v.Set("x_os_version", "1.0")
	v.Set("x_app_version", traework.IdeVersion)
	v.Set("x_app_type", "stable")
	u.RawQuery = v.Encode()
	return u.String(), nil
}

// NoteCallback 记录浏览器 OAuth 回调带回的 refreshToken，供 Poll() 完成后交换。
// refreshToken 为空时记为回调错误（与桌面端临时监听的行为一致）。
func NoteCallback(statePath, refreshToken, host string) error {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return fmt.Errorf("read login state: %w", err)
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("parse login state: %w", err)
	}
	st.RefreshToken = refreshToken
	st.Host = host
	if st.Host == "" {
		st.Host = traework.OAuthHost
	}
	if refreshToken == "" {
		st.Err = "missing refreshToken in callback"
	}
	return writeState(statePath, st)
}
