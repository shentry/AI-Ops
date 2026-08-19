// Package tools 是 D06 的安全工具层：所有外部动作必须先注册成 ToolSpec，
// 由 Registry 统一套超时和输出截断后才允许调用。LLM 只能拿到 ForLLM()
// 导出的 L1 只读工具（GC-08/GC-12）。
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// SafetyLevel 是工具的安全分级（GC-10）：
// L1 只读自动执行；L2 低风险满足护栏自动执行；L3 必须审批；L4 永远禁止。
type SafetyLevel string

const (
	L1ReadOnly  SafetyLevel = "L1"
	L2LowRisk   SafetyLevel = "L2"
	L3Approval  SafetyLevel = "L3"
	L4Forbidden SafetyLevel = "L4"
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

// ToolSpec 声明一个工具的完整契约：没有注册、没有等级的动作不存在。
type ToolSpec struct {
	Name        string
	Description string
	Level       SafetyLevel
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

// Registry 是进程内唯一的工具目录。注册在启动时完成，运行时只读，
// 所以没有并发写，不需要锁。
type Registry struct {
	tools map[string]ToolSpec
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]ToolSpec)}
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
	switch spec.Level {
	case L1ReadOnly, L2LowRisk, L3Approval, L4Forbidden:
	default:
		return fmt.Errorf("tools: tool %s has invalid safety level %q", spec.Name, spec.Level)
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
	if _, exists := r.tools[spec.Name]; exists {
		return fmt.Errorf("tools: tool %s is already registered", spec.Name)
	}
	r.tools[spec.Name] = spec
	return nil
}

// Get 按名取工具契约，供执行面（D10/D11）做权限判断。
func (r *Registry) Get(name string) (ToolSpec, bool) {
	spec, ok := r.tools[name]
	return spec, ok
}

// ForLLM 只导出 L1 只读工具（GC-08）：LLM 看到的工具面就是它的全部权限面，
// L2/L3/L4 的动作只能由确定性执行路径触发，LLM 编造名字也调不到。
func (r *Registry) ForLLM() []ToolSpec {
	exposed := make([]ToolSpec, 0, len(r.tools))
	for _, spec := range r.tools {
		if spec.Level == L1ReadOnly {
			exposed = append(exposed, spec)
		}
	}
	// 按名排序：每次导出顺序一致，prompt 里的工具清单稳定可 diff。
	sort.Slice(exposed, func(i, j int) bool { return exposed[i].Name < exposed[j].Name })
	return exposed
}

// Execute 是工具的统一入口：套超时、截输出。任何调用方（LLM 工具循环、
// 审批执行器）都走这里，保证超时和截断纪律只有一份实现。
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	spec, ok := r.tools[name]
	if !ok {
		return "", ErrToolNotRegistered
	}
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	output, err := spec.Handler(ctx, args)
	if err != nil {
		// 超时单独标注，审计时能分清"工具坏了"和"工具太慢"。
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("tools: %s timed out after %s", name, spec.Timeout)
		}
		return "", fmt.Errorf("tools: %s failed: %w", name, err)
	}
	return Truncate(output, spec.MaxOutput), nil
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
