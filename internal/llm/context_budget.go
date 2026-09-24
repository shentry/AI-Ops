package llm

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// ErrContextBudget means protected evidence/recent results cannot fit. We never
// delete them silently or send a request known to exceed the local budget.
var ErrContextBudget = errors.New("llm: context budget exceeded")

// ContextStats describes input preparation, not billed/cumulative token usage.
type ContextStats struct {
	Compactions        int
	EstimatedBefore    int
	EstimatedAfter     int
	SavedEstimate      int
	ActualPromptTokens int
	EstimateFactor     float64
}

type contextBudget struct {
	inputLimit int
	toolTokens int
	stats      *ContextStats
	estimator  *tokenEstimator
	lastRaw    int
}

// estimatedMessageTokens estimates serialized messages without calibration.
// It is a heuristic, NOT a provider tokenizer or a hard guarantee.
// Keep protocol extras and account for thinking echoed in two wire fields.
func estimatedMessageTokens(messages []*schema.Message) (int, error) {
	total := 0
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		copy := *msg
		copy.ResponseMeta = nil
		encoded, err := json.Marshal(copy)
		if err != nil {
			return 0, fmt.Errorf("llm: estimate message: %w", err)
		}
		total += estimatedTextTokens(string(encoded)) + estimatedTextTokens(msg.ReasoningContent)
	}
	return total, nil
}

// prepare alters only old tool-result content in a copy for this request.
// Eino's original history and step audit remain unchanged. The latest entire
// tool batch, all assistant reasoning/metadata, and original Evidence are kept.
func (b *contextBudget) prepare(messages []*schema.Message) ([]*schema.Message, error) {
	size, err := estimatedMessageTokens(messages)
	if err != nil {
		return nil, err
	}
	size += b.toolTokens
	rawSize := size
	size = b.estimator.estimate(rawSize)
	before := size
	out := messages
	if b.inputLimit <= 0 {
		return nil, fmt.Errorf("%w: no input space after output reserve", ErrContextBudget)
	}
	if size > b.inputLimit-b.inputLimit/5 {
		lastBatch := -1
		for i, msg := range messages {
			if msg != nil && msg.Role == schema.Assistant && len(msg.ToolCalls) > 0 {
				lastBatch = i
			}
		}
		out = append([]*schema.Message(nil), messages...)
		for i := 0; i < lastBatch && size > b.inputLimit-b.inputLimit/5; i++ {
			msg := messages[i]
			if msg == nil || msg.Role != schema.Tool {
				continue
			}
			content := compactToolResult(msg.ToolName, msg.Content)
			if content == msg.Content {
				continue
			}
			copy := *msg // Preserve call ID, name, extras and multimodal fields.
			copy.Content = content
			oldSize, err := estimatedMessageTokens([]*schema.Message{msg})
			if err != nil {
				return nil, err
			}
			newSize, err := estimatedMessageTokens([]*schema.Message{&copy})
			if err != nil {
				return nil, err
			}
			if newSize < oldSize {
				out[i] = &copy
				rawSize -= oldSize - newSize
				size = b.estimator.estimate(rawSize)
			}
		}
	}
	b.lastRaw = rawSize
	if b.stats != nil {
		b.stats.EstimateFactor = b.estimator.scale()
		b.stats.EstimatedBefore = before
		b.stats.EstimatedAfter = size
		if size < before {
			b.stats.Compactions++
			b.stats.SavedEstimate += before - size
		}
	}
	if size > b.inputLimit {
		return nil, fmt.Errorf("%w: estimated input %d > %d; evidence, recent tools and protocol fields retained", ErrContextBudget, size, b.inputLimit)
	}
	return out, nil
}

// observe uses the usage from the exact request prepared above, including any
// compaction. It is deliberately independent of usageCounter's cumulative sum.
func (b *contextBudget) observe(msg *schema.Message) {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		return
	}
	actual := msg.ResponseMeta.Usage.PromptTokens
	b.estimator.observe(b.lastRaw, actual)
	if b.stats != nil {
		b.stats.ActualPromptTokens = actual
		b.stats.EstimateFactor = b.estimator.scale()
	}
}
