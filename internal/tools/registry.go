// Package tools 是唯一的工具目录：只读工具（ToolSpec）由 Registry 统一套超时、
// 脱敏和输出截断，模型和采集器都经它调用；写操作是 Action，只由执行器按
// 已批准的冻结快照调用，从不作为可调用工具交给模型。
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Handler 是工具的实际执行体。args 是 LLM 或执行器给出的 JSON 参数，
// 返回值必须是可截断的文本（工具自己负责 JSON 序列化结果）。
type Handler func(ctx context.Context, args json.RawMessage) (string, error)

// ParamSpec 声明一个工具参数，供 LLM function-calling schema 使用。
// 全部按 string 传递 —— 工具 handler 自己做解析和校验。
type ParamSpec struct {
	Name        string
	Description string
	Required    bool
}

// ToolSpec 声明一个只读工具的完整契约：没有注册的工具不存在。
type ToolSpec struct {
	Name        string
	Description string
	Timeout     time.Duration
	MaxOutput   int // 输出按 rune 截断的上限；<=0 表示用默认
	Params      []ParamSpec
	Handler     Handler
}

// defaultMaxOutput 防止工具漏配 MaxOutput 时把超长响应原样塞给 LLM。
const defaultMaxOutput = 4096

// ErrToolNotRegistered 是"不在名单即拒绝"的哨兵：调用未注册工具一律走这里，
// 不存在"默认放行"的路径。
var ErrToolNotRegistered = errors.New("tools: tool is not registered")

// Registry 是进程内唯一的工具目录：只读工具与写动作。注册在启动时完成，
// 运行时只读，所以没有并发写，不需要锁。
type Registry struct {
	tools   map[string]ToolSpec
	actions map[string]Action
}

// ExecutionMetadata records invocation facts that cannot be safely inferred
// from returned text.
type ExecutionMetadata struct {
	Truncated bool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]ToolSpec), actions: make(map[string]Action)}
}

// Register 校验并登记一个工具。重复名字或契约不完整都在启动期暴露，
// 不拖到 LLM 真正调用时才失败。
func (r *Registry) Register(spec ToolSpec) error {
	if spec.Name == "" {
		return errors.New("tools: tool name is required")
	}
	if spec.Description == "" {
		return fmt.Errorf("tools: tool %s description is required", spec.Name)
	}
	if spec.Handler == nil {
		return fmt.Errorf("tools: tool %s handler is required", spec.Name)
	}
	if spec.Timeout <= 0 {
		return fmt.Errorf("tools: tool %s timeout must be positive", spec.Name)
	}
	if spec.MaxOutput <= 0 {
		spec.MaxOutput = defaultMaxOutput
	}
	if _, exists := r.tools[spec.Name]; exists || r.actions[spec.Name] != nil {
		return fmt.Errorf("tools: tool %s is already registered", spec.Name)
	}
	r.tools[spec.Name] = spec
	return nil
}

// ForLLM 导出全部只读工具：模型看到的工具面就是它的全部权限面。写动作
// 不在这里，模型编造动作名也调不到；它只能在计划里建议已启用的动作。
func (r *Registry) ForLLM() []ToolSpec {
	exposed := make([]ToolSpec, 0, len(r.tools))
	for _, spec := range r.tools {
		exposed = append(exposed, spec)
	}
	// 按名排序：每次导出顺序一致，prompt 里的工具清单稳定可 diff。
	sort.Slice(exposed, func(i, j int) bool { return exposed[i].Name < exposed[j].Name })
	return exposed
}

// Execute 是只读工具的统一入口：套超时、脱敏、截输出。任何调用方（LLM 工具
// 循环、证据采集、恢复验证）都走这里，保证这三条纪律只有一份实现：
// 模型看到的工具输出和落库回放的内容是同一份脱敏后的文本。
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	output, _, err := r.ExecuteWithMetadata(ctx, name, args)
	return output, err
}

// ExecuteWithMetadata is Execute's metadata-bearing form.
func (r *Registry) ExecuteWithMetadata(ctx context.Context, name string, args json.RawMessage) (string, ExecutionMetadata, error) {
	spec, ok := r.tools[name]
	if !ok {
		return "", ExecutionMetadata{}, ErrToolNotRegistered
	}
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	output, err := spec.Handler(ctx, args)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", ExecutionMetadata{}, fmt.Errorf("tools: %s timed out after %s", name, spec.Timeout)
		}
		return "", ExecutionMetadata{}, fmt.Errorf("tools: %s failed: %w", name, err)
	}
	clean := Sanitize(ToSafeText(output))
	truncated := Truncate(clean, spec.MaxOutput)
	return truncated, ExecutionMetadata{Truncated: truncated != clean}, nil
}

// Truncate 按 rune 截断，避免把 UTF-8 字符切成两半。
// 截断后追加标记，让下游（LLM、审计）知道看到的不是完整输出。
func Truncate(output string, maxRunes int) string {
	if maxRunes <= 0 {
		return output
	}
	runes := []rune(output)
	if len(runes) <= maxRunes {
		return output
	}
	return string(runes[:maxRunes]) + "…[truncated]"
}
