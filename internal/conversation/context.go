package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/datatypes"

	"oncall-agent/internal/store"
)

// call. It intentionally exposes summaries, never raw event payloads or
// external credentials.
type ContextStore interface {
	GetIncident(context.Context, uint64) (store.Incident, error)
	ListIncidentMembers(context.Context, uint64) ([]store.IncidentMember, error)
	ListAgentRuns(context.Context, uint64, uint64, int) ([]store.AgentRun, error)
	ListRunSteps(context.Context, uint64) ([]store.AgentRunStep, error)
	ListIncidentProblems(context.Context, uint64, string, int) ([]store.IncidentProblem, error)
	ListIncidentApprovals(context.Context, uint64, string, int) ([]store.Approval, error)
	ListConversationMessages(context.Context, uint64, uint64, int) ([]store.ConversationMessage, error)
}

// QuestionInput is the fully assembled, incident-bound request handed to a
// Questioner. Context is already sanitized and bounded; Questioner
// implementations must still treat it as untrusted evidence.
type QuestionInput struct {
	IncidentID  uint64
	MessageID   uint64
	Actor       Actor
	Channel     string
	Question    string
	Context     string
	LatestRunID *uint64
}

// ContextAssembler loads all read-only state required for a question. Keeping
// this as a separate dependency makes the worker easy to test and prevents an
// HTTP handler from accidentally running LLM work synchronously.
type ContextAssembler struct {
	store ContextStore
}

func NewContextAssembler(reads ContextStore) *ContextAssembler {
	return &ContextAssembler{store: reads}
}

// Build assembles the incident snapshot around one queued user message.
func (a *ContextAssembler) Build(ctx context.Context, message store.ConversationMessage) (QuestionInput, error) {
	if a == nil || a.store == nil {
		return QuestionInput{}, ErrConversationDependency
	}
	if message.IncidentID == 0 || message.ID == 0 {
		return QuestionInput{}, fmt.Errorf("conversation: message incident and id are required")
	}
	incident, err := a.store.GetIncident(ctx, message.IncidentID)
	if err != nil {
		return QuestionInput{}, fmt.Errorf("conversation: load incident: %w", err)
	}
	members, err := a.store.ListIncidentMembers(ctx, message.IncidentID)
	if err != nil {
		return QuestionInput{}, fmt.Errorf("conversation: load incident members: %w", err)
	}
	runs, err := a.store.ListAgentRuns(ctx, message.IncidentID, 0, 100)
	if err != nil {
		return QuestionInput{}, fmt.Errorf("conversation: load incident runs: %w", err)
	}
	var latest *store.AgentRun
	if len(runs) > 0 {
		candidate := runs[len(runs)-1]
		latest = &candidate
	}

	steps := make([]store.AgentRunStep, 0)
	if latest != nil {
		steps, err = a.store.ListRunSteps(ctx, latest.ID)
		if err != nil {
			return QuestionInput{}, fmt.Errorf("conversation: load latest run steps: %w", err)
		}
		if len(steps) > 32 {
			steps = steps[len(steps)-32:]
		}
	}
	problems, err := a.store.ListIncidentProblems(ctx, message.IncidentID, "open", 100)
	if err != nil {
		return QuestionInput{}, fmt.Errorf("conversation: load open problems: %w", err)
	}
	approvals, err := a.store.ListIncidentApprovals(ctx, message.IncidentID, "", 100)
	if err != nil {
		return QuestionInput{}, fmt.Errorf("conversation: load approvals: %w", err)
	}
	recent, err := listAllMessages(ctx, a.store, message.IncidentID)
	if err != nil {
		return QuestionInput{}, err
	}
	if len(recent) > 20 {
		recent = recent[len(recent)-20:]
	}

	snapshot := questionSnapshot{
		Incident: incidentSummary{
			ID:          incident.ID,
			GroupKey:    bounded(incident.GroupKey, 512),
			Status:      bounded(incident.Status, 32),
			Severity:    incident.Severity,
			AlertsCount: incident.AlertsCount,
			Title:       bounded(incident.Title, 512),
			StartedAt:   formatTime(incident.StartedAt),
			LastSeenAt:  formatTime(incident.LastSeenAt),
			ResolvedAt:  formatOptionalTime(incident.ResolvedAt),
		},
		Members:   make([]memberSummary, 0, len(members)),
		Problems:  make([]problemSummary, 0, len(problems)),
		Approvals: make([]approvalSummary, 0, len(approvals)),
		Messages:  make([]messageSummary, 0, len(recent)),
	}
	for _, member := range members {
		snapshot.Members = append(snapshot.Members, memberSummary{
			Fingerprint: bounded(member.Fingerprint, 128),
			Name:        bounded(member.Name, 255),
			Status:      bounded(member.Status, 32),
			Severity:    member.Severity,
			LinkedAt:    formatTime(member.LinkedAt),
		})
	}
	for _, problem := range problems {
		snapshot.Problems = append(snapshot.Problems, problemSummary{
			ID:        problem.ID,
			Code:      bounded(problem.Code, 64),
			Severity:  bounded(problem.Severity, 16),
			Status:    bounded(problem.Status, 16),
			Summary:   bounded(problem.Summary, 512),
			FirstSeen: formatTime(problem.FirstSeenAt),
			LastSeen:  formatTime(problem.LastSeenAt),
		})
	}
	for _, approval := range approvals {
		snapshot.Approvals = append(snapshot.Approvals, approvalSummary{
			ID:        approval.ID,
			RunID:     approval.RunID,
			ToolName:  bounded(approval.ToolName, 128),
			PlanHash:  bounded(approval.PlanHash, 128),
			Status:    bounded(approval.Status, 32),
			Reason:    bounded(approval.Reason, 1024),
			ExpiresAt: formatTime(approval.ExpiresAt),
			DecidedBy: bounded(pointerString(approval.DecidedBy), 128),
			DecidedAt: formatOptionalTime(approval.DecidedAt),
		})
	}
	for _, item := range recent {
		snapshot.Messages = append(snapshot.Messages, messageSummary{
			ID:        item.ID,
			Role:      bounded(item.Role, 32),
			Channel:   bounded(item.Channel, 32),
			Actor:     bounded(pointerString(item.ActorName), 128),
			Content:   bounded(item.Content, 4096),
			Status:    bounded(item.Status, 32),
			CreatedAt: formatTime(item.CreatedAt),
		})
	}
	if latest != nil {
		snapshot.LatestRun = &runSummary{
			ID:         latest.ID,
			Mode:       bounded(latest.Mode, 32),
			Status:     bounded(latest.Status, 32),
			RCA:        bounded(pointerString(latest.RCAText), 4096),
			TokensIn:   latest.TokensIn,
			TokensOut:  latest.TokensOut,
			StartedAt:  formatTime(latest.StartedAt),
			FinishedAt: formatOptionalTime(latest.FinishedAt),
			Steps:      make([]stepSummary, 0, len(steps)),
		}
		for _, step := range steps {
			snapshot.LatestRun.Steps = append(snapshot.LatestRun.Steps, stepSummary{
				ID:         step.ID,
				Seq:        step.Seq,
				Kind:       bounded(step.Kind, 32),
				Name:       bounded(step.Name, 128),
				Input:      bounded(jsonPointer(step.InputJSON), 2048),
				Output:     bounded(jsonPointer(step.OutputJSON), 4096),
				Error:      bounded(pointerString(step.Error), 1024),
				StartedAt:  formatTime(step.StartedAt),
				FinishedAt: formatOptionalTime(step.FinishedAt),
			})
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return QuestionInput{}, fmt.Errorf("conversation: encode question context: %w", err)
	}
	actor := Actor{Source: bounded(message.Channel, 32)}
	if message.ActorID != nil {
		actor.ID = bounded(*message.ActorID, 128)
	}
	if message.ActorName != nil {
		actor.Name = bounded(*message.ActorName, 128)
	}
	var latestID *uint64
	if latest != nil {
		id := latest.ID
		latestID = &id
	}
	return QuestionInput{
		IncidentID:  message.IncidentID,
		MessageID:   message.ID,
		Actor:       actor,
		Channel:     bounded(message.Channel, 32),
		Question:    bounded(message.Content, MaxQuestionRunes),
		Context:     string(encoded),
		LatestRunID: latestID,
	}, nil
}

func listAllMessages(ctx context.Context, reads interface {
	ListConversationMessages(context.Context, uint64, uint64, int) ([]store.ConversationMessage, error)
}, incidentID uint64) ([]store.ConversationMessage, error) {
	messages := make([]store.ConversationMessage, 0)
	var after uint64
	for {
		page, err := reads.ListConversationMessages(ctx, incidentID, after, 100)
		if err != nil {
			return nil, fmt.Errorf("conversation: load recent messages: %w", err)
		}
		if len(page) == 0 {
			return messages, nil
		}
		messages = append(messages, page...)
		last := after
		for _, item := range page {
			if item.ID > last {
				last = item.ID
			}
		}
		if last == after || len(page) < 100 {
			return messages, nil
		}
		after = last
	}
}

type questionSnapshot struct {
	Incident  incidentSummary   `json:"incident"`
	Members   []memberSummary   `json:"members"`
	LatestRun *runSummary       `json:"latest_run,omitempty"`
	Problems  []problemSummary  `json:"open_problems"`
	Approvals []approvalSummary `json:"approvals"`
	Messages  []messageSummary  `json:"recent_messages"`
}

type incidentSummary struct {
	ID          uint64 `json:"id"`
	GroupKey    string `json:"group_key"`
	Status      string `json:"status"`
	Severity    uint8  `json:"severity"`
	AlertsCount int    `json:"alerts_count"`
	Title       string `json:"title"`
	StartedAt   string `json:"started_at"`
	LastSeenAt  string `json:"last_seen_at"`
	ResolvedAt  string `json:"resolved_at,omitempty"`
}

type memberSummary struct {
	Fingerprint string `json:"fingerprint"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Severity    uint8  `json:"severity"`
	LinkedAt    string `json:"linked_at"`
}

type runSummary struct {
	ID         uint64        `json:"id"`
	Mode       string        `json:"mode"`
	Status     string        `json:"status"`
	RCA        string        `json:"rca,omitempty"`
	TokensIn   int           `json:"tokens_in"`
	TokensOut  int           `json:"tokens_out"`
	StartedAt  string        `json:"started_at"`
	FinishedAt string        `json:"finished_at,omitempty"`
	Steps      []stepSummary `json:"steps"`
}

type stepSummary struct {
	ID         uint64 `json:"id"`
	Seq        int    `json:"seq"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Input      string `json:"input,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

type problemSummary struct {
	ID        uint64 `json:"id"`
	Code      string `json:"code"`
	Severity  string `json:"severity"`
	Status    string `json:"status"`
	Summary   string `json:"summary"`
	FirstSeen string `json:"first_seen_at"`
	LastSeen  string `json:"last_seen_at"`
}

type approvalSummary struct {
	ID        uint64 `json:"id"`
	RunID     uint64 `json:"run_id"`
	ToolName  string `json:"tool_name"`
	PlanHash  string `json:"plan_hash"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	ExpiresAt string `json:"expires_at"`
	DecidedBy string `json:"decided_by,omitempty"`
	DecidedAt string `json:"decided_at,omitempty"`
}

type messageSummary struct {
	ID        uint64 `json:"id"`
	Role      string `json:"role"`
	Channel   string `json:"channel"`
	Actor     string `json:"actor,omitempty"`
	Content   string `json:"content"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

func bounded(value string, maxRunes int) string {
	return truncateRunes(safeText(value), maxRunes)
}

func truncateRunes(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "…[truncated]"
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func jsonPointer(value *datatypes.JSON) string {
	if value == nil {
		return ""
	}
	return string(*value)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return formatTime(*value)
}
