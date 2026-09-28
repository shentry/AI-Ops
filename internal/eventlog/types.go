package eventlog

import "encoding/json"

// EventType identifies a persisted Incident event.
type EventType string

const (
	EventIncidentCreated      EventType = "incident.created"
	EventIncidentPromoted     EventType = "incident.promoted"
	EventIncidentResolved     EventType = "incident.resolved"
	EventRunQueued            EventType = "run.queued"
	EventRunStarted           EventType = "run.started"
	EventRunSucceeded         EventType = "run.succeeded"
	EventRunFailed            EventType = "run.failed"
	EventRunStalled           EventType = "run.stalled"
	EventCollectorStarted     EventType = "collector.started"
	EventCollectorCompleted   EventType = "collector.completed"
	EventCollectorFailed      EventType = "collector.failed"
	EventLLMStarted           EventType = "llm.started"
	EventLLMToolCalled        EventType = "llm.tool_called"
	EventLLMCompleted         EventType = "llm.completed"
	EventLLMFailed            EventType = "llm.failed"
	EventGuardEvaluated       EventType = "guard.evaluated"
	EventGuardOverridden      EventType = "guard.overridden"
	EventPolicyEvaluated      EventType = "policy.evaluated"
	EventPolicyDegraded       EventType = "policy.degraded"
	EventApprovalCreated      EventType = "approval.created"
	EventApprovalApproved     EventType = "approval.approved"
	EventApprovalDenied       EventType = "approval.denied"
	EventApprovalExpired      EventType = "approval.expired"
	EventExecutionStarted     EventType = "execution.started"
	EventExecutionCompleted   EventType = "execution.completed"
	EventExecutionFailed      EventType = "execution.failed"
	EventExecutionAborted     EventType = "execution.aborted"
	EventCompensationQueued   EventType = "compensation.queued"
	EventVerifyQueued         EventType = "verify.queued"
	EventVerifyStarted        EventType = "verify.started"
	EventVerifyChecked        EventType = "verify.checked"
	EventVerifyPassed         EventType = "verify.passed"
	EventVerifyFailed         EventType = "verify.failed"
	EventVerifyInconclusive   EventType = "verify.inconclusive"
	EventVerifyStable         EventType = "verify.stable"
	EventVerifyRecurred       EventType = "verify.recurred"
	EventReviewRecorded       EventType = "review.recorded"
	EventRetryScheduled       EventType = "retry.scheduled"
	EventEscalationRequired   EventType = "escalation.required"
	EventNotificationSent     EventType = "notification.sent"
	EventNotificationFailed   EventType = "notification.failed"
	EventConversationAsked    EventType = "conversation.asked"
	EventConversationAnswered EventType = "conversation.answered"
	EventConversationFailed   EventType = "conversation.failed"
)

// EventRecord is the stable, dependency-free input shape for a persisted event.
// RunID and ApprovalID are nil when the event is not associated with either
// resource. Payload must contain only sanitized, bounded JSON data.
type EventRecord struct {
	IncidentID uint64          `json:"incident_id"`
	RunID      *uint64         `json:"run_id,omitempty"`
	ApprovalID *uint64         `json:"approval_id,omitempty"`
	EventType  EventType       `json:"event_type"`
	Phase      string          `json:"phase"`
	Status     string          `json:"status"`
	Summary    string          `json:"summary"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}
