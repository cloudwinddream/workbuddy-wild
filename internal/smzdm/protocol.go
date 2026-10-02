// Package smzdm 是"什么值得买"签到模块的 Go 原生实现。
//
// 协议逆向来自上游开源项目 https://github.com/enwaiax/smzdm-bot（Apache-2.0），
// 仅移植其中的签到部分：APP 请求签名、SK 生成、每日签到及签到奖励领取。
// 本包未使用上游的抽奖、每日任务、通知等其他功能。
package smzdm

import (
	"crypto/des"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// AppProfile 是版本化的 APP 协议配置。
type AppProfile struct {
	Version     string
	VersionCode string
	SignKey     string
	SKKey       string
}

// DefaultAppProfile 经 com.smzdm.client.android 11.1.90（versionCode 1190）验证。
var DefaultAppProfile = AppProfile{
	Version:     "11.1.90",
	VersionCode: "1190",
	SignKey:     "apr1$AwP!wRRT$gJ/q.X24poeBInlUJC",
	SKKey:       "geZm53XAspb02exN",
}

// IPhoneAppProfile 经解密后的 com.smzdm.client.ios 11.1.92 及配套 HAR 验证。
var IPhoneAppProfile = AppProfile{
	Version:     "11.1.92",
	VersionCode: "11.1.92",
	SignKey:     "zok5JtAq3$QixaA%mncn*jGWlEpSL3E1",
	SKKey:       "",
}

// ResolveAppProfile 按抓包平台名返回对应的协议配置。
func ResolveAppProfile(platform string) AppProfile {
	if strings.ToLower(strings.TrimSpace(platform)) == "iphone" ||
		strings.ToLower(strings.TrimSpace(platform)) == "ios" {
		return IPhoneAppProfile
	}
	return DefaultAppProfile
}

// IsIPhone 判断是否为 iPhone 协议。
func IsIPhone(platform string) bool {
	p := strings.ToLower(strings.TrimSpace(platform))
	return p == "iphone" || p == "ios"
}

var cookiePairRe = regexp.MustCompile(`([^=;]+)=([^;]*);`)

// ParseCookieHeader 把 Cookie 头解析为解码后的键值对。
func ParseCookieHeader(header string) map[string]string {
	h := header
	if !strings.HasSuffix(h, ";") {
		h += ";"
	}
	out := map[string]string{}
	for _, m := range cookiePairRe.FindAllStringSubmatch(h, -1) {
		k := strings.TrimSpace(m[1])
		v, _ := url.QueryUnescape(strings.TrimSpace(m[2]))
		out[k] = v
	}
	return out
}

// ComputeRequestSignature 生成 Android APP 用的大写 MD5 请求签名：
// 按字段名排序，取"去空格后非空"的 name=value，用 & 连接，
// 末尾追加 &key={sign_key}，MD5 后转大写十六进制。
func ComputeRequestSignature(fields map[string]string, profile AppProfile) string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	var parts []string
	for _, name := range names {
		v := strings.ReplaceAll(fields[name], " ", "")
		if v == "" {
			continue
		}
		parts = append(parts, name+"="+v)
	}
	payload := strings.Join(parts, "&") + "&key=" + profile.SignKey
	sum := md5.Sum([]byte(payload))
	return fmt.Sprintf("%X", sum)
}

// pkcs7Pad 按 PKCS#7（8 字节块时等同 PKCS#5）填充。
func pkcs7Pad(data []byte, blockSize int) []byte {
	padLen := blockSize - len(data)%blockSize
	out := make([]byte, len(data)+padLen)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(padLen)
	}
	return out
}

// desECBEncrypt DES-ECB 分组加密（data 长度须为 8 的倍数）。
func desECBEncrypt(key, data []byte) ([]byte, error) {
	block, err := des.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += des.BlockSize {
		block.Encrypt(out[i:i+des.BlockSize], data[i:i+des.BlockSize])
	}
	return out, nil
}

// GenerateSecurityKey 生成 SK：Base64(DES-ECB-PKCS5(smzdm_id + device_id))。
func GenerateSecurityKey(smzdmID, deviceID string, profile AppProfile) (string, error) {
	if smzdmID == "" {
		return "", fmt.Errorf("smzdm_id 不能为空")
	}
	if deviceID == "" {
		return "", fmt.Errorf("device_id 不能为空")
	}
	key := []byte(profile.SKKey)
	if len(key) > des.BlockSize {
		key = key[:des.BlockSize]
	}
	enc, err := desECBEncrypt(key, pkcs7Pad([]byte(smzdmID+deviceID), des.BlockSize))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}
