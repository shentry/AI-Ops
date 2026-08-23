package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"gorm.io/datatypes"

	"oncall-agent/internal/eventlog"
	"oncall-agent/internal/store"
)

const (
	conversationPollInterval = time.Second
	conversationStaleAfter   = 5 * time.Minute
)

// WorkerStore is the union of the narrow read/write surfaces used by the
// asynchronous conversation worker. *store.DB satisfies this interface.
type WorkerStore interface {
	MessageStore
	ContextStore
	EventStore
	OpenIncidentProblem(context.Context, store.IncidentProblem) (store.IncidentProblem, error)
	ListIncidents(context.Context, string) ([]store.Incident, error)
	ClaimConversationMessage(context.Context, uint64, time.Time) (bool, error)
	CompleteConversationMessage(context.Context, uint64, string, time.Time) error
}

// QueuedSource is optional. A store may implement it to avoid scanning every
// incident; the built-in store currently uses the ListIncidents fallback.
type QueuedSource interface {
	NextQueuedConversationMessage(context.Context) (store.ConversationMessage, bool, error)
}

type staleConversationRequeuer interface {
	RequeueStaleConversationMessages(context.Context, time.Time) (int64, error)
}

type conversationProblemResolver interface {
	ResolveIncidentProblem(context.Context, uint64, string, *uint64, time.Time) (bool, error)
}

// ThreadReplier posts an answer back to the originating Feishu thread.
type ThreadReplier interface {
	Reply(ctx context.Context, parentMessageID, content string) error
}

// BindingLookup finds the originating Feishu message recorded in a queued
// question's metadata. A reply must never select the newest Incident binding.
type BindingLookup interface {
	GetIMBinding(ctx context.Context, provider, messageID string) (store.IMBinding, error)
}

type Worker struct {
	store      WorkerStore
	questioner Questioner
	assembler  *ContextAssembler
	logger     *log.Logger
	interval   time.Duration
	now        func() time.Time
	replier    ThreadReplier
	provider   string

	mu      sync.Mutex
	started bool
	done    chan struct{}
}

// NewWorker constructs an asynchronous question worker. The optional logger
// keeps the constructor convenient for API wiring while preserving the same
// shape as the existing diagnose worker when a logger is supplied.
func NewWorker(reads WorkerStore, questioner Questioner, loggers ...*log.Logger) *Worker {
	logger := log.Default()
	if len(loggers) > 0 && loggers[0] != nil {
		logger = loggers[0]
	}
	return &Worker{
		store:      reads,
		questioner: questioner,
		assembler:  NewContextAssembler(reads),
		logger:     logger,
		interval:   conversationPollInterval,
		now:        time.Now,
	}
}

// NewWorkerWithInterval is useful for embedding the worker in a process with a
// custom poll cadence and for deterministic integration tests.
func NewWorkerWithInterval(reads WorkerStore, questioner Questioner, interval time.Duration, logger *log.Logger) *Worker {
	worker := NewWorker(reads, questioner, logger)
	if interval > 0 {
		worker.interval = interval
	}
	return worker
}

// SetThreadReply enables Feishu thread replies after a question is answered.
func (w *Worker) SetThreadReply(replier ThreadReplier, provider string) {
	if w == nil {
		return
	}
	w.replier = replier
	w.provider = strings.TrimSpace(provider)
}

// Start launches polling and returns immediately. No LLM work runs on an HTTP
// request goroutine.
func (w *Worker) Start(ctx context.Context) error {
	if w == nil || w.store == nil || w.questioner == nil {
		return ErrConversationDependency
	}
	if err := w.requeueStale(ctx); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return errors.New("conversation: worker already started")
	}
	w.started = true
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		w.consume(ctx)
	}()
	return nil
}

// Wait waits for a started worker to stop. It is safe to call before Start.
func (w *Worker) Wait() {
	if w == nil {
		return
	}
	w.mu.Lock()
	done := w.done
	w.mu.Unlock()
	if done != nil {
		<-done
	}
}

// RunOnce drains all currently visible queued messages. It is exported for
// smoke tests and for hosts that prefer to schedule polling themselves.
func (w *Worker) RunOnce(ctx context.Context) error {
	if w == nil || w.store == nil || w.questioner == nil {
		return ErrConversationDependency
	}
	if err := w.requeueStale(ctx); err != nil {
		return err
	}
	return w.drain(ctx)
}

func (w *Worker) consume(ctx context.Context) {
	interval := w.interval
	if interval <= 0 {
		interval = conversationPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := w.drain(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Printf("conversation worker drain failed: %v", err)
		}
		if err := w.requeueStale(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Printf("conversation worker stale recovery failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) requeueStale(ctx context.Context) error {
	requeuer, ok := w.store.(staleConversationRequeuer)
	if !ok {
		return nil
	}
	count, err := requeuer.RequeueStaleConversationMessages(ctx, w.now().UTC().Add(-conversationStaleAfter))
	if err == nil && count > 0 {
		w.logger.Printf("conversation: requeued %d stale questions", count)
	}
	return err
}

func (w *Worker) resolveConversationProblems(ctx context.Context, incidentID uint64, runID *uint64, resolvedAt time.Time) {
	resolver, ok := w.store.(conversationProblemResolver)
	if !ok {
		return
	}
	for _, code := range []string{"conversation_failed", "reasoner_parse_failed"} {
		if _, err := resolver.ResolveIncidentProblem(ctx, incidentID, code, runID, resolvedAt); err != nil {
			w.logger.Printf("conversation: resolve problem %s: %v", code, err)
		}
	}
}

func (w *Worker) drain(ctx context.Context) error {
	if source, ok := w.store.(QueuedSource); ok {
		for {
			message, found, err := source.NextQueuedConversationMessage(ctx)
			if err != nil {
				return fmt.Errorf("conversation: find queued message: %w", err)
			}
			if !found {
				return nil
			}
			if err := w.process(ctx, message); err != nil {
				w.logger.Printf("conversation message %d failed: %v", message.ID, err)
			}
		}
	}
	incidents, err := w.store.ListIncidents(ctx, "")
	if err != nil {
		return fmt.Errorf("conversation: list incidents: %w", err)
	}
	var firstErr error
	for _, incident := range incidents {
		messages, err := listAllMessages(ctx, w.store, incident.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, message := range messages {
			if message.Role != "user" || message.Status != "queued" {
				continue
			}
			if err := w.process(ctx, message); err != nil {
				w.logger.Printf("conversation message %d failed: %v", message.ID, err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}

func (w *Worker) process(ctx context.Context, queued store.ConversationMessage) error {
	claimed, err := w.store.ClaimConversationMessage(ctx, queued.ID, w.now().UTC())
	if err != nil {
		return fmt.Errorf("conversation: claim message: %w", err)
	}
	if !claimed {
		return nil
	}
	input, err := w.assembler.Build(ctx, queued)
	if err != nil {
		return w.persistFailure(ctx, queued, input, err)
	}
	answer, answerErr := w.questioner.Answer(ctx, input)
	if answerErr != nil {
		// A model/parse failure may still have executed read-only tools. Keep
		// those audit rows even though the parent question becomes failed.
		w.persistTools(ctx, input, queued, answer.ToolMessages)
		return w.persistFailure(ctx, queued, input, answerErr)
	}
	rawAnswer := answer
	answer, err = NormalizeAnswer(rawAnswer)
	if err != nil {
		w.persistTools(ctx, input, queued, rawAnswer.ToolMessages)
		return w.persistFailure(ctx, queued, input, err)
	}
	w.persistTools(ctx, input, queued, answer.ToolMessages)

	metadata := jsonPayload(map[string]any{
		"citations":         answer.Citations,
		"uncertainties":     answer.Uncertainties,
		"suggested_actions": answer.SuggestedActions,
		"needs_user_input":  answer.NeedsUserInput,
	})
	assistant := store.ConversationMessage{
		IncidentID:   queued.IncidentID,
		RunID:        input.LatestRunID,
		ReplyToID:    uint64Pointer(queued.ID),
		Channel:      bounded(queued.Channel, 32),
		Role:         "assistant",
		Content:      truncateBytes(safeText(answer.Answer), MaxAnswerBytes),
		Status:       "completed",
		MetadataJSON: metadata,
		CreatedAt:    w.now().UTC(),
	}
	createdAssistant, err := w.store.CreateConversationMessage(ctx, assistant)
	if err != nil {
		return w.persistFailure(ctx, queued, input, fmt.Errorf("conversation: save answer: %w", err))
	}
	finished := w.now().UTC()
	if err := w.store.CompleteConversationMessage(ctx, queued.ID, "completed", finished); err != nil {
		return fmt.Errorf("conversation: complete question: %w", err)
	}
	w.resolveConversationProblems(ctx, queued.IncidentID, input.LatestRunID, finished)
	w.appendEvent(ctx, store.IncidentEvent{
		IncidentID: queued.IncidentID,
		RunID:      input.LatestRunID,
		EventType:  string(eventlog.EventConversationAnswered),
		Phase:      "conversation",
		Status:     "completed",
		Summary:    boundedSummary("question answered"),
		PayloadJSON: jsonPayload(map[string]any{
			"message_id": queued.ID,
			"answer_id":  createdAssistant.ID,
		}),
		CreatedAt: finished,
	})
	w.replyThread(ctx, queued, assistant.Content)
	return nil
}

func (w *Worker) persistTools(ctx context.Context, input QuestionInput, parent store.ConversationMessage, tools []ToolMessage) {
	for _, call := range tools {
		created := call.StartedAt
		if created.IsZero() {
			created = w.now().UTC()
		}
		finished := call.FinishedAt
		if finished.IsZero() {
			finished = created
		}
		content := call.Output
		status := "completed"
		if strings.TrimSpace(call.Err) != "" {
			content = call.Err
			status = "failed"
		}
		if strings.TrimSpace(content) == "" {
			content = "tool returned no output"
		}
		message := store.ConversationMessage{
			IncidentID: input.IncidentID,
			RunID:      input.LatestRunID,
			ReplyToID:  uint64Pointer(parent.ID),
			Channel:    bounded(parent.Channel, 32),
			Role:       "tool",
			Content:    truncateBytes(safeText(content), MaxAnswerBytes),
			ToolName:   stringPointer(bounded(call.Name, 128)),
			ToolCallID: stringPointer(bounded(call.CallID, 128)),
			Status:     status,
			MetadataJSON: jsonPayload(map[string]any{
				"input":     bounded(call.Input, 2048),
				"truncated": call.Truncated,
			}),
			CreatedAt:  created.UTC(),
			FinishedAt: timePointer(finished.UTC()),
		}
		createdMessage, err := w.store.CreateConversationMessage(ctx, message)
		if err != nil {
			w.logger.Printf("conversation tool message save failed: %v", err)
			continue
		}
		w.appendEvent(ctx, store.IncidentEvent{
			IncidentID: input.IncidentID,
			RunID:      input.LatestRunID,
			EventType:  string(eventlog.EventLLMToolCalled),
			Phase:      "conversation",
			Status:     status,
			Summary:    boundedSummary("question tool called: " + call.Name),
			PayloadJSON: jsonPayload(map[string]any{
				"message_id": createdMessage.ID,
				"parent_id":  parent.ID,
				"tool":       bounded(call.Name, 128),
				"truncated":  call.Truncated,
			}),
			CreatedAt: finished.UTC(),
		})
	}
}

func (w *Worker) replyThread(_ context.Context, queued store.ConversationMessage, content string) {
	if w == nil || w.replier == nil || strings.TrimSpace(queued.Channel) != "feishu" {
		return
	}
	lookup, ok := w.store.(BindingLookup)
	if !ok {
		return
	}
	messageID := feishuSourceMessageID(queued.MetadataJSON)
	if messageID == "" {
		w.recordReplyFailure(queued, "feishu reply binding missing")
		return
	}
	binding, err := lookup.GetIMBinding(context.Background(), w.provider, messageID)
	if err != nil || binding.IncidentID != queued.IncidentID || strings.TrimSpace(binding.MessageID) == "" {
		w.recordReplyFailure(queued, "feishu reply binding missing")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.replier.Reply(ctx, binding.MessageID, content); err != nil {
		w.logger.Printf("conversation: feishu reply failed: %v", err)
		w.recordReplyFailure(queued, "feishu reply failed")
	}
}

func (w *Worker) recordReplyFailure(queued store.ConversationMessage, summary string) {
	w.appendEvent(context.Background(), store.IncidentEvent{
		IncidentID: queued.IncidentID,
		EventType:  string(eventlog.EventNotificationFailed),
		Phase:      "conversation",
		Status:     "failed",
		Summary:    boundedSummary(summary),
		CreatedAt:  w.now().UTC(),
	})
}

func feishuSourceMessageID(metadata *datatypes.JSON) string {
	if metadata == nil || !json.Valid(*metadata) {
		return ""
	}
	var value struct {
		FeishuSourceMessageID string `json:"feishu_source_message_id"`
	}
	if json.Unmarshal(*metadata, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value.FeishuSourceMessageID)
}

func (w *Worker) persistFailure(ctx context.Context, queued store.ConversationMessage, input QuestionInput, cause error) error {
	reason := "reasoner_failed"
	problemCode := "conversation_failed"
	if errors.Is(cause, ErrQuestionParse) {
		reason = "reasoner_parse_failed"
		problemCode = "reasoner_parse_failed"
	}
	reason = bounded(reason, 64)
	finished := w.now().UTC()
	assistant := store.ConversationMessage{
		IncidentID: queued.IncidentID,
		RunID:      input.LatestRunID,
		ReplyToID:  uint64Pointer(queued.ID),
		Channel:    bounded(queued.Channel, 32),
		Role:       "assistant",
		Content:    "暂时无法回答该问题。",
		Status:     "failed",
		MetadataJSON: jsonPayload(map[string]any{
			"reason": reason,
		}),
		CreatedAt:  finished,
		FinishedAt: timePointer(finished),
	}
	if _, err := w.store.CreateConversationMessage(ctx, assistant); err != nil {
		w.logger.Printf("conversation failed answer save failed: %v", err)
	}
	if err := w.store.CompleteConversationMessage(ctx, queued.ID, "failed", finished); err != nil {
		w.logger.Printf("conversation failed question completion failed: %v", err)
	}
	w.appendEvent(ctx, store.IncidentEvent{
		IncidentID: queued.IncidentID,
		RunID:      input.LatestRunID,
		EventType:  string(eventlog.EventConversationFailed),
		Phase:      "conversation",
		Status:     "failed",
		Summary:    boundedSummary("question failed: " + reason),
		PayloadJSON: jsonPayload(map[string]any{
			"message_id": queued.ID,
			"reason":     reason,
		}),
		CreatedAt: finished,
	})
	problem := store.IncidentProblem{
		IncidentID:  queued.IncidentID,
		RunID:       input.LatestRunID,
		Code:        problemCode,
		Severity:    "warning",
		Status:      "open",
		Summary:     boundedSummary("conversation answer failed: " + reason),
		DetailJSON:  jsonPayload(map[string]any{"reason": reason}),
		FirstSeenAt: finished,
		LastSeenAt:  finished,
	}
	if _, err := w.store.OpenIncidentProblem(ctx, problem); err != nil {
		w.logger.Printf("conversation problem save failed: %v", err)
	}
	return cause
}

func (w *Worker) appendEvent(ctx context.Context, event store.IncidentEvent) {
	if _, err := w.store.AppendIncidentEvent(ctx, event); err != nil {
		w.logger.Printf("conversation event save failed: %v", err)
	}
}

func uint64Pointer(value uint64) *uint64 {
	if value == 0 {
		return nil
	}
	return &value
}

func stringPointer(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func timePointer(value time.Time) *time.Time {
	return &value
}
