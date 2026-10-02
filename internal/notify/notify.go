// Package notify 提供签到结果的 Bark 推送通知。
//
// Bark 是 iOS 推送应用：向 {server}/push POST JSON 即可推送到设备。
// 支持：全局默认设备（server + key）、按签到模块覆盖 key——
// 不同的签到账号可走不同的 Bark 设备。配置保存在
// <stateDir>/notify/notify.json（0600），环境变量 BARK_SERVER/BARK_KEY
// 仅作首次引导默认值。
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultServer Bark 官方公共服务器。
const DefaultServer = "https://api.day.app"

// Config 通知配置。
type Config struct {
	Server     string            `json:"server"`      // Bark 服务器，如 https://api.day.app（可自建）
	DefaultKey string            `json:"default_key"` // 默认设备 key
	ModuleKeys map[string]string `json:"module_keys"` // 模块ID → 设备 key（覆盖默认）
}

// Bark Bark 推送客户端（线程安全，配置可热更新）。
type Bark struct {
	mu   sync.RWMutex
	cfg  Config
	path string
	http *http.Client
}

// Open 读取配置；文件不存在时用 env 引导值（BARK_SERVER/BARK_KEY）初始化。
// dir 为配置目录（如 <stateDir>/notify）。
func Open(dir string) (*Bark, error) {
	_ = os.MkdirAll(dir, 0o700)
	b := &Bark{
		path: filepath.Join(dir, "notify.json"),
		http: &http.Client{Timeout: 15 * time.Second},
	}
	b.cfg = Config{
		Server:     strings.TrimRight(strings.TrimSpace(os.Getenv("BARK_SERVER")), "/"),
		DefaultKey: strings.TrimSpace(os.Getenv("BARK_KEY")),
		ModuleKeys: map[string]string{},
	}
	if b.cfg.Server == "" {
		b.cfg.Server = DefaultServer
	}
	raw, err := os.ReadFile(b.path)
	if err == nil {
		var c Config
		if json.Unmarshal(raw, &c) == nil {
			b.cfg = c
			if b.cfg.Server == "" {
				b.cfg.Server = DefaultServer
			}
			if b.cfg.ModuleKeys == nil {
				b.cfg.ModuleKeys = map[string]string{}
			}
		}
	}
	return b, nil
}

// snapshot 返回配置副本。
func (b *Bark) snapshot() Config {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := b.cfg
	out.ModuleKeys = make(map[string]string, len(b.cfg.ModuleKeys))
	for k, v := range b.cfg.ModuleKeys {
		out.ModuleKeys[k] = v
	}
	return out
}

// RawConfig 返回当前原始配置副本（供管理接口合并更新用）。
func (b *Bark) RawConfig() Config { return b.snapshot() }

// Update 热更新配置并落盘（0600）。
func (b *Bark) Update(c Config) error {
	if c.Server == "" {
		c.Server = DefaultServer
	}
	c.Server = strings.TrimRight(strings.TrimSpace(c.Server), "/")
	if c.ModuleKeys == nil {
		c.ModuleKeys = map[string]string{}
	}
	b.mu.Lock()
	b.cfg = c
	b.mu.Unlock()
	raw, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(b.path, raw, 0o600)
}

// KeyFor 返回模块生效的 key（模块覆盖 → 默认）。空表示未配置。
func (b *Bark) KeyFor(moduleID string) string {
	c := b.snapshot()
	if k := strings.TrimSpace(c.ModuleKeys[moduleID]); k != "" {
		return k
	}
	return strings.TrimSpace(c.DefaultKey)
}

// Enabled 模块是否已配置推送。
func (b *Bark) Enabled(moduleID string) bool { return b.KeyFor(moduleID) != "" }

// View 返回给管理页的配置视图（key 脱敏）。
func (b *Bark) View() map[string]any {
	c := b.snapshot()
	mods := map[string]any{}
	for id, k := range c.ModuleKeys {
		mods[id] = map[string]any{"key": mask(k), "set": k != ""}
	}
	return map[string]any{
		"server":      c.Server,
		"default_key": mask(c.DefaultKey),
		"default_set": c.DefaultKey != "",
		"modules":     mods,
	}
}

func mask(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return "****"
	}
	return k[:4] + "****" + k[len(k)-4:]
}

// Send 向模块对应的 Bark 设备推送（未配置则跳过）。
// body 支持多行；title 为通知标题。异步发送，不阻塞签到流程。
func (b *Bark) Send(moduleID, title, body string) {
	go func() {
		if err := b.send(moduleID, title, body); err != nil {
			// 仅记录，不影响签到主流程
			fmt.Fprintf(os.Stderr, "notify: bark 推送失败 module=%s: %v\n", moduleID, err)
		}
	}()
}

// SendSync 同步发送（测试推送用，需要把结果告诉用户）。
func (b *Bark) SendSync(moduleID, title, body string) error {
	return b.send(moduleID, title, body)
}

func (b *Bark) send(moduleID, title, body string) error {
	c := b.snapshot()
	key := b.KeyFor(moduleID)
	if key == "" {
		return fmt.Errorf("未配置 Bark key")
	}
	// 支持逗号分隔的多个设备 key，逐个发送。
	keys := strings.Split(key, ",")
	var firstErr error
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if err := b.push(c.Server, k, title, body); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *Bark) push(server, key, title, body string) error {
	payload := map[string]any{
		"device_key": key,
		"title":      title,
		"body":       body,
		"group":      "签到中心",
		"ttl":        600,
	}
	raw, _ := json.Marshal(payload)
	url := strings.TrimRight(server, "/") + "/push"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	// Bark 成功返回 {"code":200,...}
	var ack struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
	}
	if json.Unmarshal(respBody, &ack) == nil && ack.Code != 0 && ack.Code != 200 {
		return fmt.Errorf("bark 返回 code=%d %s", ack.Code, ack.Msg)
	}
	return nil
}
