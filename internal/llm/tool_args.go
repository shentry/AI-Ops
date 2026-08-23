package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"oncall-agent/internal/tools"
)

// executeLLMTool validates the model-provided argument stream before handing it
// to a registered tool. Some OpenAI-compatible gateways concatenate an empty
// object placeholder with the real arguments. Empty placeholders and identical
// repetitions are safe to collapse; conflicting non-empty values remain fail-closed.
func executeLLMTool(ctx context.Context, registry *tools.Registry, name, raw string) (string, tools.ExecutionMetadata, error) {
	normalized, err := normalizeToolArguments(raw)
	if err != nil {
		return "", tools.ExecutionMetadata{}, fmt.Errorf("tools: %s failed: invalid args: %w", name, err)
	}
	return registry.ExecuteWithMetadata(ctx, name, json.RawMessage(normalized))
}

func normalizeToolArguments(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw, errors.New("tool arguments are empty")
	}

	decoder := json.NewDecoder(strings.NewReader(trimmed))
	var first json.RawMessage
	if err := decoder.Decode(&first); err != nil {
		return raw, err
	}

	// A single JSON value keeps the original bytes. This preserves the model
	// output in the audit record and leaves type validation to the tool handler.
	selected := first
	selectedCanonical, err := canonicalJSONValue(first)
	if err != nil {
		return raw, err
	}
	selectedEmpty := isEmptyJSONObject(selectedCanonical)
	repeated := false
	objectsOnly := isJSONObject(first)
	for {
		var next json.RawMessage
		err := decoder.Decode(&next)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return raw, err
		}
		repeated = true
		objectsOnly = objectsOnly && isJSONObject(next)
		nextCanonical, err := canonicalJSONValue(next)
		if err != nil {
			return raw, err
		}
		if isEmptyJSONObject(nextCanonical) {
			continue
		}
		if selectedEmpty {
			selected = next
			selectedCanonical = nextCanonical
			selectedEmpty = false
			continue
		}
		if !bytes.Equal(selectedCanonical, nextCanonical) {
			return raw, errors.New("tool arguments contain multiple different JSON values")
		}
	}
	if !repeated {
		return raw, nil
	}
	if !objectsOnly {
		return raw, errors.New("tool arguments contain repeated non-object JSON values")
	}
	return string(bytes.TrimSpace(selected)), nil
}

func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func isEmptyJSONObject(canonical []byte) bool {
	return bytes.Equal(canonical, []byte("{}"))
}

func canonicalJSONValue(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
