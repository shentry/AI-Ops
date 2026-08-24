package conversation

import (
	"context"
	"errors"
	"log"
	"testing"
	"time"

	"gorm.io/datatypes"

	"oncall-agent/internal/eventlog"
	"oncall-agent/internal/store"
)

type stubWorkerStore struct {
	binding store.IMBinding
	bindErr error
	events  []store.IncidentEvent
}

func (s *stubWorkerStore) GetIncident(context.Context, uint64) (store.Incident, error) {
	return store.Incident{}, nil
}
func (s *stubWorkerStore) ListIncidentMembers(context.Context, uint64) ([]store.IncidentMember, error) {
	return nil, nil
}
func (s *stubWorkerStore) ListAgentRuns(context.Context, uint64, uint64, int) ([]store.AgentRun, error) {
	return nil, nil
}
func (s *stubWorkerStore) ListRunSteps(context.Context, uint64) ([]store.AgentRunStep, error) {
	return nil, nil
}
func (s *stubWorkerStore) ListIncidentProblems(context.Context, uint64, string, int) ([]store.IncidentProblem, error) {
	return nil, nil
}
func (s *stubWorkerStore) ListIncidentApprovals(context.Context, uint64, string, int) ([]store.Approval, error) {
	return nil, nil
}
func (s *stubWorkerStore) ListConversationMessages(context.Context, uint64, uint64, int) ([]store.ConversationMessage, error) {
	return nil, nil
}
func (s *stubWorkerStore) CreateConversationMessage(_ context.Context, message store.ConversationMessage) (store.ConversationMessage, error) {
	return message, nil
}
func (s *stubWorkerStore) AppendIncidentEvent(_ context.Context, event store.IncidentEvent) (store.IncidentEvent, error) {
	s.events = append(s.events, event)
	return event, nil
}
func (s *stubWorkerStore) OpenIncidentProblem(_ context.Context, problem store.IncidentProblem) (store.IncidentProblem, error) {
	return problem, nil
}
func (s *stubWorkerStore) NextQueuedConversationMessage(context.Context) (store.ConversationMessage, bool, error) {
	return store.ConversationMessage{}, false, nil
}
func (s *stubWorkerStore) RequeueStaleConversationMessages(context.Context, time.Time) (int64, error) {
	return 0, nil
}
func (s *stubWorkerStore) ResolveIncidentProblem(context.Context, uint64, string, *uint64, time.Time) (bool, error) {
	return false, nil
}
func (s *stubWorkerStore) ClaimConversationMessage(context.Context, uint64, time.Time) (bool, error) {
	return true, nil
}
func (s *stubWorkerStore) CompleteConversationMessage(context.Context, uint64, string, time.Time) error {
	return nil
}
func (s *stubWorkerStore) GetIMBinding(context.Context, string, string) (store.IMBinding, error) {
	return s.binding, s.bindErr
}

type failReplier struct{ err error }

func (f failReplier) Reply(context.Context, string, string) error { return f.err }

func TestReplyThreadFailurePersistsEvent(t *testing.T) {
	storeStub := &stubWorkerStore{binding: store.IMBinding{IncidentID: 4, MessageID: "om_q"}}
	worker := &Worker{
		store:    storeStub,
		logger:   log.Default(),
		now:      func() time.Time { return time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC) },
		replier:  failReplier{err: errors.New("network")},
		provider: "feishu_app",
	}
	metadata := datatypes.JSON([]byte(`{"feishu_source_message_id":"om_q"}`))
	worker.replyThread(context.Background(), store.ConversationMessage{IncidentID: 4, Channel: "feishu", MetadataJSON: &metadata}, "答案")
	if len(storeStub.events) != 1 || storeStub.events[0].EventType != string(eventlog.EventNotificationFailed) {
		t.Fatalf("events = %+v", storeStub.events)
	}
}
