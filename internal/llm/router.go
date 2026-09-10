package llm

import (
	"fmt"
	"sync"
)

// Router 简单的 provider 路由器：根据 model 字符串找对应 provider
type Router struct {
	mu        sync.RWMutex
	providers map[string]Provider
	// model → provider 名 的映射表（显式路由）
	modelIndex map[string]string
}

// NewRouter 新建 router
func NewRouter() *Router {
	return &Router{
		providers:  make(map[string]Provider),
		modelIndex: make(map[string]string),
	}
}

// Register 注册一个 provider
func (r *Router) Register(p Provider, models ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.Name()] = p
	for _, m := range models {
		r.modelIndex[m] = p.Name()
	}
}

// Pick 根据模型名选 provider
func (r *Router) Pick(model string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// 1. 先查显式映射
	if provName, ok := r.modelIndex[model]; ok {
		if p, ok := r.providers[provName]; ok {
			return p, nil
		}
	}

	// 2. 再看有没有 provider 能处理（这里简化：用第一个注册的）
	if len(r.providers) == 0 {
		return nil, fmt.Errorf("no provider registered")
	}
	// 返回第一个（map 遍历无序，生产环境应做更精确的 fallback）
	for _, p := range r.providers {
		return p, nil
	}
	return nil, fmt.Errorf("provider not found for model: %s", model)
}

// RegisteredProviders 返回所有已注册的 provider 名
func (r *Router) RegisteredProviders() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for name := range r.providers {
		out = append(out, name)
	}
	return out
}

// RegisteredModels 返回所有已注册的 model 名（显式注册的）
func (r *Router) RegisteredModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.modelIndex))
	for m := range r.modelIndex {
		out = append(out, m)
	}
	return out
}
