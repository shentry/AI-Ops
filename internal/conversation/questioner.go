package conversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Questioner answers an incident-bound question without creating a plan or
// invoking an executor. The asynchronous worker is the only caller in normal
// operation.
type Questioner interface {
	Answer(context.Context, QuestionInput) (QuestionAnswer, error)
}

// ErrQuestionParse classifies a model response that does not satisfy the
// question-answer JSON contract. Worker persistence maps it to the durable
// reasoner_parse_failed problem.
var ErrQuestionParse = errors.New("conversation: questioner response parse failure")

const (
	ActionCollectEvidence = "collect_evidence"
	ActionRediagnose      = "rediagnose"
	ActionManualReview    = "manual_review"
)

// Citation points to a persisted audit object. A citation can reference an
// event, a run step, or both; it never contains an arbitrary URL or tool args.
type Citation struct {
	EventID   uint64 `json:"event_id,omitempty"`
	StepID    uint64 `json:"step_id,omitempty"`
	ID        uint64 `json:"id,omitempty"`
	Type      string `json:"type,omitempty"`
	Reference string `json:"reference,omitempty"`
	Quote     string `json:"quote,omitempty"`
}

// SuggestedAction is a non-executable follow-up hint. The worker persists it
// for the UI but never dispatches it to Approval or Executor.
type SuggestedAction struct {
	Type        string `json:"type"`
	Reason      string `json:"reason,omitempty"`
	Description string `json:"description,omitempty"`
}

// ToolMessage is an audit record returned by an L1 read-only Questioner. It is
// deliberately separate from the assistant's content so Web and Feishu can
// render the exact call order.
type ToolMessage struct {
	Name       string
	CallID     string
	Input      string
	Output     string
	Err        string
	Truncated  bool
	StartedAt  time.Time
	FinishedAt time.Time
}

// QuestionAnswer is the stable answer contract shared by Web and Feishu.
type QuestionAnswer struct {
	Answer           string            `json:"answer"`
	Citations        []Citation        `json:"citations"`
	Uncertainties    []string          `json:"uncertainties"`
	SuggestedActions []SuggestedAction `json:"suggested_actions"`
	NeedsUserInput   bool              `json:"needs_user_input"`

	// ToolMessages is internal audit data, not part of the model's JSON output.
	ToolMessages []ToolMessage `json:"-"`
}

// NormalizeAnswer validates the non-executable contract and applies the
// persisted answer budget. It returns ErrQuestionParse-wrapped errors rather
// than silently accepting an unsupported action or malformed citation.
func NormalizeAnswer(answer QuestionAnswer) (QuestionAnswer, error) {
	answer.Answer = strings.TrimSpace(answer.Answer)
	if answer.Answer == "" {
		return QuestionAnswer{}, fmt.Errorf("%w: answer is empty", ErrQuestionParse)
	}
	if len(answer.Citations) == 0 && len(answer.Uncertainties) == 0 {
		return QuestionAnswer{}, fmt.Errorf("%w: answer requires a citation or uncertainty", ErrQuestionParse)
	}
	answer.Answer = truncateBytes(answer.Answer, MaxAnswerBytes)
	for index, action := range answer.SuggestedActions {
		switch action.Type {
		case ActionCollectEvidence, ActionRediagnose, ActionManualReview:
		default:
			return QuestionAnswer{}, fmt.Errorf("%w: unsupported suggested action %q", ErrQuestionParse, action.Type)
		}
		if index >= 8 {
			answer.SuggestedActions = answer.SuggestedActions[:8]
			break
		}
	}
	for index := range answer.Uncertainties {
		answer.Uncertainties[index] = truncateRunes(safeText(answer.Uncertainties[index]), 512)
	}
	for index := range answer.Citations {
		citation := &answer.Citations[index]
		if citation.EventID == 0 && citation.StepID == 0 && citation.ID == 0 {
			return QuestionAnswer{}, fmt.Errorf("%w: citation %d has no event or step id", ErrQuestionParse, index)
		}
		citation.Type = truncateRunes(safeText(citation.Type), 32)
		citation.Reference = truncateRunes(safeText(citation.Reference), 256)
		citation.Quote = truncateRunes(safeText(citation.Quote), 1024)
	}
	return answer, nil
}

func truncateBytes(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	for len(value) > maxBytes {
		value = value[:len(value)-1]
	}
	return strings.ToValidUTF8(value, "")
}
