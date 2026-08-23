package llm

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func extractThinking(rawBody []byte) string {
	var body struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rawBody, &body); err != nil || len(body.Choices) == 0 {
		return ""
	}
	var msg map[string]any
	if err := json.Unmarshal(body.Choices[0].Message, &msg); err != nil {
		return ""
	}
	if s := stringField(msg, "reasoning_content"); s != "" {
		return s
	}
	if s := stringField(msg, "reasoning"); s != "" {
		return s
	}
	parts, ok := msg["content"].([]any)
	if !ok {
		return ""
	}
	for _, part := range parts {
		item, ok := part.(map[string]any)
		if !ok || stringField(item, "type") != "thinking" {
			continue
		}
		if s := stringField(item, "thinking"); s != "" {
			return s
		}
		if s := stringField(item, "text"); s != "" {
			return s
		}
	}
	return ""
}

func echoThinking(rawBody []byte, input []*schema.Message) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return rawBody, nil
	}
	rawMessages, ok := body["messages"].([]any)
	if !ok || len(rawMessages) != len(input) {
		return rawBody, nil
	}
	for i, in := range input {
		if in == nil || in.Role != schema.Assistant {
			continue
		}
		reasoning := strings.TrimSpace(in.ReasoningContent)
		if reasoning == "" {
			continue
		}
		msg, ok := rawMessages[i].(map[string]any)
		if !ok {
			continue
		}
		msg["reasoning_content"] = reasoning
		if shouldWriteThinkingContent(msg) {
			msg["content"] = []any{map[string]any{"type": "thinking", "thinking": reasoning}}
		}
		rawMessages[i] = msg
	}
	body["messages"] = rawMessages
	out, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func shouldWriteThinkingContent(msg map[string]any) bool {
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) == 0 {
		return false
	}
	content, exists := msg["content"]
	if !exists || content == nil {
		return true
	}
	if s, ok := content.(string); ok {
		return s == ""
	}
	return false
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

type thinkingModel struct {
	inner model.ToolCallingChatModel
}

func wrapThinkingModel(inner model.ToolCallingChatModel) model.ToolCallingChatModel {
	return &thinkingModel{inner: inner}
}

func (m *thinkingModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	opts = append(opts,
		openai.WithRequestPayloadModifier(func(_ context.Context, msgs []*schema.Message, rawBody []byte) ([]byte, error) {
			return echoThinking(rawBody, msgs)
		}),
		openai.WithResponseMessageModifier(func(_ context.Context, msg *schema.Message, rawBody []byte) (*schema.Message, error) {
			if msg == nil {
				return msg, nil
			}
			if strings.TrimSpace(msg.ReasoningContent) == "" {
				if thinking := extractThinking(rawBody); thinking != "" {
					msg.ReasoningContent = thinking
				}
			}
			return msg, nil
		}),
	)
	return m.inner.Generate(ctx, input, opts...)
}

func (m *thinkingModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return m.inner.Stream(ctx, input, opts...)
}

func (m *thinkingModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	wrapped, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &thinkingModel{inner: wrapped}, nil
}
