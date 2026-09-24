package llm

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// diagnosisModel reserves the last available model turn for a conclusion.
// ReAct spends one graph step on the model and one on each batch of tools.
type diagnosisModel struct {
	model.ToolCallingChatModel
	maxSteps int
	context  contextBudget
}

func (m *diagnosisModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	toolRounds := 0
	for _, msg := range input {
		if msg != nil && msg.Role == schema.Assistant && len(msg.ToolCalls) > 0 {
			toolRounds++
		}
	}
	finalTurn := 2*toolRounds+3 > m.maxSteps
	if finalTurn {
		opts = append(opts, model.WithToolChoice(schema.ToolChoiceForbidden))
		messages := append([]*schema.Message(nil), input...)
		input = append(messages, schema.UserMessage("取证预算已结束。根据已有证据输出简短的最终 JSON；缺失信息写入 rca，不再调用工具。目标或动作不确定时 plan.action=none。"))
	}
	input, err := m.context.prepare(input)
	if err != nil {
		return nil, err
	}
	msg, err := m.ToolCallingChatModel.Generate(ctx, input, opts...)
	m.context.observe(msg)
	if err == nil && finalTurn && msg != nil && len(msg.ToolCalls) > 0 {
		return nil, errors.New("llm: model requested tools after evidence budget ended")
	}
	return msg, err
}

func (m *diagnosisModel) WithTools(infos []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	budget := m.context
	for _, info := range infos {
		parameters, err := info.ParamsOneOf.ToJSONSchema()
		if err != nil {
			return nil, err
		}
		definition, err := json.Marshal(map[string]any{"type": "function", "function": map[string]any{
			"name": info.Name, "description": info.Desc, "parameters": parameters,
		}})
		if err != nil {
			return nil, err
		}
		budget.toolTokens += estimatedTextTokens(string(definition))
	}
	bound, err := m.ToolCallingChatModel.WithTools(infos)
	if err != nil {
		return nil, err
	}
	return &diagnosisModel{ToolCallingChatModel: bound, maxSteps: m.maxSteps, context: budget}, nil
}
