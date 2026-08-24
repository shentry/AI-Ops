package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/datatypes"

	"oncall-agent/internal/eventlog"
	"oncall-agent/internal/store"
)

const feishuSourceMessageIDKey = "feishu_source_message_id"

const (
	// MaxQuestionRunes is the maximum size accepted for one user question.
	MaxQuestionRunes = 4000
	// MaxAnswerBytes is the persisted answer budget. The LLM adapter also uses
	// this value when constraining its response contract.
	MaxAnswerBytes = 8 * 1024
)

var (
	ErrQuestionRequired       = errors.New("conversation: question is required")
	ErrQuestionTooLong        = errors.New("conversation: question exceeds 4000 characters")
	ErrQuestionInProgress     = errors.New("conversation: another question is already running")
	ErrConversationDependency = errors.New("conversation: dependency is required")
)

type Actor struct {
	ID              string
	Name            string
	Source          string
	SourceMessageID string
}

// MessageStore is the small persistence surface needed by the synchronous
// conversation service. *store.DB satisfies it; tests and API adapters can
// provide a focused fake without depending on GORM. The event writer is part
// of the same contract: queueing a question is an auditable operation, so a
// store that cannot record the fact must fail to compile.
type MessageStore interface {
	GetIncident(context.Context, uint64) (store.Incident, error)
	ListConversationMessages(context.Context, uint64, uint64, int) ([]store.ConversationMessage, error)
	CreateConversationMessage(context.Context, store.ConversationMessage) (store.ConversationMessage, error)
	EventStore
}

// EventStore persists the incident fact emitted by a conversation operation.
type EventStore interface {
	AppendIncidentEvent(context.Context, store.IncidentEvent) (store.IncidentEvent, error)
}

// Service is the incident-bound conversation API shared by Web and Feishu.
type Service interface {
	Ask(context.Context, uint64, Actor, string, string) (store.ConversationMessage, error)
	List(context.Context, uint64, uint64, int) ([]store.ConversationMessage, error)
}

type service struct {
	messages MessageStore
	now      func() time.Time
}

// NewService builds the incident-bound conversation service.
func NewService(messages MessageStore) Service {
	return NewServiceWithClock(messages, time.Now)
}

// NewServiceWithClock is NewService with an injectable clock for deterministic
// persistence tests.
func NewServiceWithClock(messages MessageStore, now func() time.Time) Service {
	if now == nil {
		now = time.Now
	}
	return &service{messages: messages, now: now}
}

func (s *service) Ask(ctx context.Context, incidentID uint64, actor Actor, question, channel string) (store.ConversationMessage, error) {
	if s == nil || s.messages == nil {
		return store.ConversationMessage{}, ErrConversationDependency
	}
	if incidentID == 0 {
		return store.ConversationMessage{}, store.ErrIncidentNotFound
	}
	clean := safeText(question)
	if strings.TrimSpace(clean) == "" {
		return store.ConversationMessage{}, ErrQuestionRequired
	}
	if utf8.RuneCountInString(clean) > MaxQuestionRunes {
		return store.ConversationMessage{}, ErrQuestionTooLong
	}
	if strings.TrimSpace(channel) == "" {
		return store.ConversationMessage{}, errors.New("conversation: channel is required")
	}
	if _, err := s.messages.GetIncident(ctx, incidentID); err != nil {
		return store.ConversationMessage{}, fmt.Errorf("conversation: check incident: %w", err)
	}

	if running, err := s.hasRunning(ctx, incidentID); err != nil {
		return store.ConversationMessage{}, err
	} else if running {
		return store.ConversationMessage{}, ErrQuestionInProgress
	}

	now := s.now().UTC()
	message := store.ConversationMessage{
		IncidentID: incidentID,
		Channel:    safeText(channel),
		Role:       "user",
		Content:    clean,
		Status:     "queued",
		CreatedAt:  now,
	}
	metadata := make(map[string]any)
	if source := safeText(actor.Source); strings.TrimSpace(source) != "" {
		metadata["actor_source"] = truncateRunes(source, 32)
	}
	if strings.TrimSpace(channel) == "feishu" {
		if sourceMessageID := safeText(actor.SourceMessageID); strings.TrimSpace(sourceMessageID) != "" {
			metadata[feishuSourceMessageIDKey] = truncateRunes(sourceMessageID, 128)
		}
	}
	if len(metadata) > 0 {
		message.MetadataJSON = jsonPayload(metadata)
	}
	if value := safeOptional(actor.ID); value != nil {
		message.ActorID = value
	}
	if value := safeOptional(actor.Name); value != nil {
		message.ActorName = value
	}
	created, err := s.messages.CreateConversationMessage(ctx, message)
	if err != nil {
		return store.ConversationMessage{}, fmt.Errorf("conversation: create question: %w", err)
	}

	event := store.IncidentEvent{
		IncidentID: created.IncidentID,
		EventType:  string(eventlog.EventConversationAsked),
		Phase:      "conversation",
		Status:     "queued",
		Summary:    boundedSummary("question queued"),
		PayloadJSON: jsonPayload(map[string]any{
			"message_id": created.ID,
			"channel":    created.Channel,
		}),
		CreatedAt: now,
	}
	if _, err := s.messages.AppendIncidentEvent(ctx, event); err != nil {
		// The message remains queued so the asynchronous worker can still answer
		// it. Returning the persisted row plus the event error makes the missing
		// audit fact visible to the caller without rolling back user input.
		return created, fmt.Errorf("conversation: append asked event: %w", err)
	}
	return created, nil
}

func (s *service) List(ctx context.Context, incidentID, afterID uint64, limit int) ([]store.ConversationMessage, error) {
	if s == nil || s.messages == nil {
		return nil, ErrConversationDependency
	}
	if incidentID == 0 {
		return nil, store.ErrIncidentNotFound
	}
	if _, err := s.messages.GetIncident(ctx, incidentID); err != nil {
		return nil, fmt.Errorf("conversation: check incident: %w", err)
	}
	messages, err := s.messages.ListConversationMessages(ctx, incidentID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("conversation: list messages: %w", err)
	}
	return messages, nil
}

func (s *service) hasRunning(ctx context.Context, incidentID uint64) (bool, error) {
	var after uint64
	for {
		messages, err := s.messages.ListConversationMessages(ctx, incidentID, after, 100)
		if err != nil {
			return false, fmt.Errorf("conversation: inspect active questions: %w", err)
		}
		if len(messages) == 0 {
			return false, nil
		}
		for _, message := range messages {
			if message.Status == "running" {
				return true, nil
			}
			if message.ID > after {
				after = message.ID
			}
		}
		if len(messages) < 100 {
			return false, nil
		}
	}
}

type textSanitizeRule struct {
	pattern *regexp.Regexp
	replace string
}

var conversationSanitizeRules = []textSanitizeRule{
	{regexp.MustCompile(`(?i)(authorization["'\s:=]+\s*(?:bearer|basic)\s+)[^\s"',}]+`), "${1}***"},
	{regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|auth[_-]?token|token|password|passwd|secret|cookie|dsn)(["'\s]*[:=]["'\s]*)([^\s"',;}]+)`), "${1}${2}***"},
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^:/\s"']+:)[^@\s"']+(@)`), "${1}***${2}"},
	{regexp.MustCompile(`([\w.-]+):[^@\s]+@(tcp|unix|http|https)\(`), "${1}:***@${2}("},
}

func safeText(value string) string {
	out := strings.ToValidUTF8(value, "")
	for _, rule := range conversationSanitizeRules {
		out = rule.pattern.ReplaceAllString(out, rule.replace)
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || (r >= 0x20 && r != 0x7f) {
			return r
		}
		return -1
	}, out)
}

func safeOptional(value string) *string {
	value = safeText(value)
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func boundedSummary(value string) string {
	return truncateRunes(safeText(value), 512)
}

func jsonPayload(value any) *datatypes.JSON {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	payload := datatypes.JSON(encoded)
	return &payload
}
