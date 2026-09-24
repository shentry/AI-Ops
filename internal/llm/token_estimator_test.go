package llm

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"oncall-agent/internal/config"
)

func TestTokenEstimateDistinguishesUTF8Bytes(t *testing.T) {
	for _, text := range []string{strings.Repeat("hello world ", 100), strings.Repeat("容器内存不足", 100), strings.Repeat(`{"value":123,"error":"timeout"}`, 100)} {
		estimate := (&tokenEstimator{}).estimate(estimatedTextTokens(text))
		if estimate <= 0 || estimate >= len(text) {
			t.Fatalf("estimate=%d bytes=%d", estimate, len(text))
		}
	}
}

func TestTokenEstimatorCalibratesPromptOnly(t *testing.T) {
	stats := &ContextStats{}
	b := contextBudget{inputLimit: 1000000, estimator: &tokenEstimator{}, stats: stats}
	messages := []*schema.Message{schema.UserMessage(strings.Repeat("diagnostic evidence ", 100))}
	if _, err := b.prepare(messages); err != nil {
		t.Fatal(err)
	}
	initial := stats.EstimatedAfter
	raw := b.lastRaw
	b.observe(&schema.Message{ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: raw * 2, CompletionTokens: 999999}}})
	if math.Abs(stats.EstimateFactor-2.2) > 0.001 {
		t.Fatalf("factor=%v", stats.EstimateFactor)
	}
	if _, err := b.prepare(messages); err != nil {
		t.Fatal(err)
	}
	if stats.EstimatedAfter <= initial || stats.EstimatedAfter > raw*3 {
		t.Fatalf("estimate=%d raw=%d", stats.EstimatedAfter, raw)
	}
	b.observe(&schema.Message{ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 1}}})
	b.observe(nil)
	if math.Abs(stats.EstimateFactor-2.2) > 0.001 {
		t.Fatal("small/missing usage erased calibration")
	}
}

func TestCalibrationUsesCompactedRequest(t *testing.T) {
	messages := contextFixture()
	raw, _ := estimatedMessageTokens(messages)
	stats := &ContextStats{}
	b := contextBudget{inputLimit: raw, estimator: &tokenEstimator{}, stats: stats}
	out, err := b.prepare(messages)
	if err != nil {
		t.Fatal(err)
	}
	compactedRaw, _ := estimatedMessageTokens(out)
	if compactedRaw >= raw || b.lastRaw != compactedRaw {
		t.Fatal("calibration denominator must use sent input")
	}
	b.observe(&schema.Message{ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: compactedRaw * 2}}})
	if math.Abs(stats.EstimateFactor-2.2) > 0.001 {
		t.Fatalf("factor=%v", stats.EstimateFactor)
	}
}

func TestMillionTokenWindowDoesNotCompact32KContext(t *testing.T) {
	messages := contextFixture()
	messages[1] = schema.UserMessage(strings.Repeat("target=payments observation=OOM ", 4000))
	stats := &ContextStats{}
	b := contextBudget{inputLimit: 1000000 - 8192 - 512, stats: stats, estimator: &tokenEstimator{}}
	out, err := b.prepare(messages)
	if err != nil {
		t.Fatal(err)
	}
	if stats.EstimatedAfter <= 32768 || stats.Compactions != 0 || out[3].Content != messages[3].Content {
		t.Fatalf("premature compaction: %+v", stats)
	}
}

func TestFactorySnapshotsModelAndWindow(t *testing.T) {
	fake := newFakeOpenAIServer(t, func(_ int, body map[string]any) map[string]any {
		return chatResponse(body["model"].(string), 1, 1)
	})
	profiles := []config.ModelProfile{{ID: "small", ContextWindowTokens: 16384}, {ID: "deepseek-v4-flash", ContextWindowTokens: 1000000}}
	f := NewFactory(config.LLMConfig{Roles: config.LLMRoles{Reasoner: config.RoleConfig{BaseURL: fake.server.URL, APIKey: "fake", Model: "small", MaxTokens: 1024}}, Models: profiles})
	first, firstLimit, err := f.buildForDiagnosis()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SelectModel(profiles[1]); err != nil {
		t.Fatal(err)
	}
	second, secondLimit, err := f.buildForDiagnosis()
	if err != nil {
		t.Fatal(err)
	}
	if firstLimit != 16384-1024-512 || secondLimit != 1000000-1024-512 {
		t.Fatalf("limits=%d/%d", firstLimit, secondLimit)
	}
	for i, client := range []model.ToolCallingChatModel{first, second} {
		msg, err := client.Generate(context.Background(), []*schema.Message{schema.UserMessage("test")})
		if err != nil || msg.Content != profiles[i].ID {
			t.Fatalf("model/window mismatch msg=%+v err=%v", msg, err)
		}
	}
}
