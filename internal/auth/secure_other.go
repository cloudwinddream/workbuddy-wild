//go:build !windows

package auth

// EncryptSecret 非 Windows 平台的空实现：直接返回原文（明文落盘）。
//
// 与 secure.go（Windows/DPAPI）的"解密失败降级原样"语义一致：
// DecryptSecret 对无 dpapi: 前缀的值本就原样返回，因此跨平台读旧明文文件无碍。
// 注意：Windows 桌面端写入的 dpapi: 前缀文件在本平台无法解密，
// 会被当作字面 token 使用而导致上游鉴权失败——迁移账号请使用明文 auth 文件。
func EncryptSecret(plain string) string { return plain }

// DecryptSecret 非 Windows 平台的空实现：直接返回原文。
func DecryptSecret(s string) string { return s }
