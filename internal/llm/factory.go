// Package llm 是 D08 的 LLM 层：角色化模型工厂 + ReAct Reasoner。
// LLM 只读取脱敏证据、产出 RCA/Plan，不直接持有变更工具（GC-08）。
package llm

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"

	"oncall-agent/internal/config"
)

// 角色名。配置里只允许这两个，多一个角色就是一处没审计过的密钥出口。
const (
	RoleReasoner   = "reasoner"
	RoleSummarizer = "summarizer"
)

// Factory 按角色创建并缓存模型客户端。进程内复用（GC：不重复建连接池），
// 密钥只进 client 配置，不进日志、不进错误文本。
type Factory struct {
	cfg   config.LLMConfig
	mu    sync.Mutex
	cache map[string]model.ToolCallingChatModel
}

func NewFactory(cfg config.LLMConfig) *Factory {
	return &Factory{cfg: cfg, cache: make(map[string]model.ToolCallingChatModel)}
}

// Validate 启动期校验：reasoner 是 D08 的硬依赖，配置不全就 fail-fast。
// 错误文本只带角色名和缺什么字段，绝不带 api_key 的值。
func (f *Factory) Validate() error {
	role := f.cfg.Roles.Reasoner
	var missing []string
	if strings.TrimSpace(role.BaseURL) == "" {
		missing = append(missing, "base_url")
	}
	if strings.TrimSpace(role.APIKey) == "" {
		missing = append(missing, "api_key")
	}
	if strings.TrimSpace(role.Model) == "" {
		missing = append(missing, "model")
	}
	if len(missing) > 0 {
		return fmt.Errorf("llm: role %s missing config: %s", RoleReasoner, strings.Join(missing, ", "))
	}
	return nil
}

// Build 按角色取模型，进程内缓存。
func (f *Factory) Build(role string) (model.ToolCallingChatModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cached, ok := f.cache[role]; ok {
		return cached, nil
	}
	roleCfg, err := f.roleConfig(role)
	if err != nil {
		return nil, err
	}
	maxTokens := roleCfg.MaxTokens
	chatModel, err := openai.NewChatModel(context.Background(), &openai.ChatModelConfig{
		BaseURL:   roleCfg.BaseURL,
		APIKey:    roleCfg.APIKey,
		Model:     roleCfg.Model,
		MaxTokens: &maxTokens,
	})
	if err != nil {
		// 不把错误原文透出去：SDK 错误可能带请求头里的密钥。
		return nil, fmt.Errorf("llm: build role %s model failed", role)
	}
	f.cache[role] = chatModel
	return chatModel, nil
}

func (f *Factory) roleConfig(role string) (config.RoleConfig, error) {
	switch role {
	case RoleReasoner:
		return f.cfg.Roles.Reasoner, nil
	case RoleSummarizer:
		return f.cfg.Roles.Summarizer, nil
	default:
		return config.RoleConfig{}, fmt.Errorf("llm: unknown role %q", role)
	}
}
