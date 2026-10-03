// Package checkin 为"签到中心"提供统一的签到模块注册表。
//
// 设计目标：所有第三方自动签到（福利吧论坛签到是第一个）都注册为 Module，
// 在唯一的 /checkin/ 页面里展示：每天自动签到是否成功、上次时间、积分，
// 并支持手动立即签到。新签到项只需实现 Module 接口并注册，无需加页面。
package checkin

import "sync"

// Summary 模块状态摘要（给签到中心页面用）。
type Summary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Desc       string `json:"desc"`
	Configured bool   `json:"configured"` // 是否已配置账号
	LoggedIn   bool   `json:"logged_in"`
	Username   string `json:"username"`
	Points     string `json:"points"`      // 积分（解析不到为空）
	PointsName string `json:"points_name"` // 积分名称，如"积分"
	Detail     string `json:"detail"`      // 附加信息行（等级·金币·签到天数等，可空）
	LastOK     bool   `json:"last_ok"`
	LastMsg    string `json:"last_msg"`
	LastAt     string `json:"last_at"`
	NextAt     string `json:"next_at"`
	Times      string `json:"times"`
}

// Module 一个签到模块。
type Module interface {
	ID() string
	Name() string
	Desc() string
	// Summary 返回当前状态摘要（允许做一次轻量远程探测）。
	Summary() Summary
	// RunNow 手动触发一次签到，返回（是否成功，结果文案）。
	RunNow() (bool, string)
}

// Registry 模块注册表。
type Registry struct {
	mu      sync.Mutex
	modules []Module
}

// New 新建注册表。
func New() *Registry { return &Registry{} }

// Register 注册模块（ID 重复时替换）。
func (r *Registry) Register(m Module) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, e := range r.modules {
		if e.ID() == m.ID() {
			r.modules[i] = m
			return
		}
	}
	r.modules = append(r.modules, m)
}

// List 按注册顺序返回模块。
func (r *Registry) List() []Module {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Module, len(r.modules))
	copy(out, r.modules)
	return out
}

// Get 按 ID 取模块。
func (r *Registry) Get(id string) Module {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.modules {
		if m.ID() == id {
			return m
		}
	}
	return nil
}
