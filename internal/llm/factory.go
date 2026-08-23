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

// SelectModel applies a configured model profile to every LLM role while
// preserving role-specific endpoint, credential and token limits. Existing
// in-flight callers retain their model; later Build calls receive a new client.
func (f *Factory) SelectModel(profile config.ModelProfile) error {
	if f == nil {
		return fmt.Errorf("llm: model factory is unavailable")
	}
	profile.ID = strings.TrimSpace(profile.ID)
	if profile.ID == "" {
		return fmt.Errorf("llm: model id is required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg.Roles.Reasoner.Model = profile.ID
	f.cfg.Roles.Reasoner.Thinking = profile.Thinking
	f.cfg.Roles.Summarizer.Model = profile.ID
	f.cfg.Roles.Summarizer.Thinking = profile.Thinking
	f.cache = make(map[string]model.ToolCallingChatModel)
	return nil
}

// CurrentModel returns the model that future Build calls will use.
func (f *Factory) CurrentModel() string {
	if f == nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg.Roles.Reasoner.Model
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
	modelCfg := &openai.ChatModelConfig{
		BaseURL:   roleCfg.BaseURL,
		APIKey:    roleCfg.APIKey,
		Model:     roleCfg.Model,
		MaxTokens: &maxTokens,
		ExtraFields: map[string]any{
			"thinking": map[string]any{"type": thinkingType(roleCfg.Thinking.Enabled)},
		},
	}
	if roleCfg.Thinking.Enabled {
		modelCfg.ReasoningEffort = thinkingEffort(roleCfg.Thinking.Effort)
	}
	chatModel, err := openai.NewChatModel(context.Background(), modelCfg)
	if err != nil {
		// 不把错误原文透出去：SDK 错误可能带请求头里的密钥。
		return nil, fmt.Errorf("llm: build role %s model failed", role)
	}
	var built model.ToolCallingChatModel = chatModel
	if roleCfg.Thinking.Enabled {
		built = wrapThinkingModel(chatModel)
	}
	f.cache[role] = built
	return built, nil
}
func thinkingType(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func thinkingEffort(effort string) openai.ReasoningEffortLevel {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low":
		return openai.ReasoningEffortLevelLow
	case "high":
		return openai.ReasoningEffortLevelHigh
	default:
		return openai.ReasoningEffortLevelMedium
	}
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
