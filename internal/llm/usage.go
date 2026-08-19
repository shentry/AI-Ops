package llm

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// usageCounter 累计一次 Diagnose 里所有模型调用的 token：
// ReAct 的多轮调用、以及契约解析失败后的那一次重试，都计入。
// 只读最终消息的 usage 会丢掉中间轮 —— D13 的 tokens 断言以这个累计为据。
type usageCounter struct {
	in  int
	out int
}

func (c *usageCounter) add(msg *schema.Message) {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		return
	}
	c.in += msg.ResponseMeta.Usage.PromptTokens
	c.out += msg.ResponseMeta.Usage.CompletionTokens
}

// usageModel 包装 ToolCallingChatModel，把每次 Generate 的 usage 累计进
// 计数器。Reasoner 走非流式 Generate；Stream 直接委托（当前无人调用，
// 引入时再补逐帧累计）。
type usageModel struct {
	inner   model.ToolCallingChatModel
	counter *usageCounter
}

func wrapUsageModel(inner model.ToolCallingChatModel, counter *usageCounter) model.ToolCallingChatModel {
	return &usageModel{inner: inner, counter: counter}
}

func (m *usageModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	msg, err := m.inner.Generate(ctx, input, opts...)
	m.counter.add(msg)
	return msg, err
}

func (m *usageModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return m.inner.Stream(ctx, input, opts...)
}

func (m *usageModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	wrapped, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	// WithTools 返回的是绑了工具的新模型，包装关系要跟过去，
	// 否则计数器统计不到绑定后的调用。
	return &usageModel{inner: wrapped, counter: m.counter}, nil
}
