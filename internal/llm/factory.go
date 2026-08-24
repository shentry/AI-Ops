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

// Factory 创建并缓存 reasoner 模型客户端。进程内复用（GC：不重复建连接池），
// 密钥只进 client 配置，不进日志、不进错误文本。
type Factory struct {
	cfg    config.LLMConfig
	mu     sync.Mutex
	cached model.ToolCallingChatModel
}

func NewFactory(cfg config.LLMConfig) *Factory {
	return &Factory{cfg: cfg}
}

// SelectModel applies a configured model profile while preserving the
// configured endpoint, credential and token limits. Existing in-flight callers
// retain their model; later Build calls receive a new client.
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
	f.cached = nil
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
		return fmt.Errorf("llm: reasoner missing config: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Build 取 reasoner 模型，进程内缓存。
func (f *Factory) Build() (model.ToolCallingChatModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cached != nil {
		return f.cached, nil
	}
	roleCfg := f.cfg.Roles.Reasoner
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
		return nil, fmt.Errorf("llm: build reasoner model failed")
	}
	var built model.ToolCallingChatModel = chatModel
	if roleCfg.Thinking.Enabled {
		built = wrapThinkingModel(chatModel)
	}
	f.cached = built
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
