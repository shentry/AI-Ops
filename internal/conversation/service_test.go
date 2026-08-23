package conversation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/eventlog"
	"oncall-agent/internal/store"
)

type fakeConversationStore struct {
	incident    store.Incident
	incidentErr error
	listed      []store.ConversationMessage
	created     []store.ConversationMessage
	events      []store.IncidentEvent
	nextID      uint64
}

func (f *fakeConversationStore) GetIncident(context.Context, uint64) (store.Incident, error) {
	if f.incidentErr != nil {
		return store.Incident{}, f.incidentErr
	}
	if f.incident.ID == 0 {
		f.incident.ID = 1
	}
	return f.incident, nil
}

func (f *fakeConversationStore) ListConversationMessages(context.Context, uint64, uint64, int) ([]store.ConversationMessage, error) {
	return append([]store.ConversationMessage(nil), f.listed...), nil
}

func (f *fakeConversationStore) CreateConversationMessage(_ context.Context, message store.ConversationMessage) (store.ConversationMessage, error) {
	f.nextID++
	message.ID = f.nextID
	f.created = append(f.created, message)
	f.listed = append(f.listed, message)
	return message, nil
}

func (f *fakeConversationStore) AppendIncidentEvent(_ context.Context, event store.IncidentEvent) (store.IncidentEvent, error) {
	f.events = append(f.events, event)
	return event, nil
}

func TestAskQueuesWithoutLLMAndWritesEvent(t *testing.T) {
	messages := &fakeConversationStore{}
	svc := NewServiceWithClock(messages, func() time.Time { return time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC) })
	got, err := svc.Ask(context.Background(), 9, Actor{ID: "ou_1", Name: "ops", Source: "web"}, "根因是什么？", "web")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Status != "queued" || got.Role != "user" || got.Channel != "web" {
		t.Fatalf("message = %+v", got)
	}
	if len(messages.events) != 1 || messages.events[0].EventType != string(eventlog.EventConversationAsked) {
		t.Fatalf("events = %+v", messages.events)
	}
}

func TestAskRejectsEmptyAndInProgress(t *testing.T) {
	messages := &fakeConversationStore{}
	svc := NewService(messages)
	if _, err := svc.Ask(context.Background(), 1, Actor{ID: "ou_1"}, "   ", "web"); !errors.Is(err, ErrQuestionRequired) {
		t.Fatalf("empty question err = %v", err)
	}
	messages.listed = []store.ConversationMessage{{ID: 2, Status: "running"}}
	if _, err := svc.Ask(context.Background(), 1, Actor{ID: "ou_1"}, "还在吗", "web"); !errors.Is(err, ErrQuestionInProgress) {
		t.Fatalf("in-progress err = %v", err)
	}
}

func TestNormalizeAnswerRequiresCitationOrUncertainty(t *testing.T) {
	_, err := NormalizeAnswer(QuestionAnswer{Answer: "没有任何依据"})
	if !errors.Is(err, ErrQuestionParse) {
		t.Fatalf("err = %v", err)
	}
	if _, err := NormalizeAnswer(QuestionAnswer{Answer: "证据不足", Uncertainties: []string{"没有可用 collector"}}); err != nil {
		t.Fatalf("uncertainty answer: %v", err)
	}
}

func TestNormalizeAnswerRejectsExecutableActions(t *testing.T) {
	_, err := NormalizeAnswer(QuestionAnswer{
		Answer:           "x",
		SuggestedActions: []SuggestedAction{{Type: "execute"}},
	})
	if !errors.Is(err, ErrQuestionParse) {
		t.Fatalf("err = %v", err)
	}
	got, err := NormalizeAnswer(QuestionAnswer{
		Answer:           strings.Repeat("a", 16),
		SuggestedActions: []SuggestedAction{{Type: ActionCollectEvidence}},
		Citations:        []Citation{{EventID: 3, Type: "event"}},
	})
	if err != nil {
		t.Fatalf("NormalizeAnswer: %v", err)
	}
	if got.SuggestedActions[0].Type != ActionCollectEvidence {
		t.Fatalf("action = %+v", got.SuggestedActions)
	}
}
