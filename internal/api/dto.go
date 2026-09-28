package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gorm.io/datatypes"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

// IncidentDTO is the browser-safe Incident projection. It deliberately omits
// raw alerts, annotations, labels and generator URLs.
type IncidentDTO struct {
	ID          uint64     `json:"id"`
	GroupKey    string     `json:"group_key"`
	Status      string     `json:"status"`
	Severity    uint8      `json:"severity"`
	AlertsCount int        `json:"alerts_count"`
	Title       string     `json:"title"`
	StartedAt   time.Time  `json:"started_at"`
	LastSeenAt  time.Time  `json:"last_seen_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
	DurationMS  int64      `json:"duration_ms"`
}

// IncidentMemberDTO contains only the current member identity and status.
type IncidentMemberDTO struct {
	Fingerprint string    `json:"fingerprint"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Severity    uint8     `json:"severity"`
	LinkedAt    time.Time `json:"linked_at"`
}

// RunDTO never exposes PlanJSON. RCA is a bounded, sanitised summary.
type RunDTO struct {
	ID         uint64     `json:"id"`
	IncidentID uint64     `json:"incident_id"`
	Mode       string     `json:"mode"`
	Status     string     `json:"status"`
	RetryOf    *uint64    `json:"retry_of,omitempty"`
	RCASummary string     `json:"rca_summary,omitempty"`
	TokensIn   int        `json:"tokens_in"`
	TokensOut  int        `json:"tokens_out"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms"`
}

// RunStepDTO exposes bounded summaries rather than complete evidence/tool
// input/output. Error text is sanitised before it reaches the browser.
type RunStepDTO struct {
	ID            uint64     `json:"id"`
	RunID         uint64     `json:"run_id"`
	Seq           int        `json:"seq"`
	Kind          string     `json:"kind"`
	Name          string     `json:"name"`
	InputSummary  string     `json:"input_summary,omitempty"`
	OutputSummary string     `json:"output_summary,omitempty"`
	Error         string     `json:"error,omitempty"`
	Status        string     `json:"status"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	DurationMS    int64      `json:"duration_ms"`
}

// FlowNodeDTO is the fixed flow-graph projection used by the Web control room.
type FlowNodeDTO struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	StepID     uint64     `json:"step_id,omitempty"`
	StartedAt  time.Time  `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms"`
	Error      string     `json:"error,omitempty"`
}

// EventDTO intentionally has no payload field. Payloads can contain raw
// evidence, tool output or credentials; the event summary and reference IDs
// are the only browser-visible event data.
type EventDTO struct {
	ID         uint64    `json:"id"`
	IncidentID uint64    `json:"incident_id"`
	RunID      *uint64   `json:"run_id,omitempty"`
	ApprovalID *uint64   `json:"approval_id,omitempty"`
	EventType  string    `json:"event_type"`
	Phase      string    `json:"phase"`
	Status     string    `json:"status"`
	Summary    string    `json:"summary"`
	CreatedAt  time.Time `json:"created_at"`
}

// ProblemDTO omits the raw detail JSON and returns a bounded detail summary.
type ProblemDTO struct {
	ID          uint64     `json:"id"`
	IncidentID  uint64     `json:"incident_id"`
	RunID       *uint64    `json:"run_id,omitempty"`
	Code        string     `json:"code"`
	Severity    string     `json:"severity"`
	Status      string     `json:"status"`
	Summary     string     `json:"summary"`
	Detail      string     `json:"detail,omitempty"`
	FirstSeenAt time.Time  `json:"first_seen_at"`
	LastSeenAt  time.Time  `json:"last_seen_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
}

// ApprovalDTO shows the frozen decision a person approves: the action, its
// target identity, the rule and mode that authorized it, what verification
// will check and the pre-frozen compensation. Snapshot fields are present
// only when the snapshot validates against its plan hash. Raw args, snapshot
// and result JSON are never returned.
type ApprovalDTO struct {
	ID               uint64          `json:"id"`
	IncidentID       uint64          `json:"incident_id"`
	RunID            uint64          `json:"run_id"`
	ParentApprovalID *uint64         `json:"parent_approval_id,omitempty"`
	ToolName         string          `json:"tool_name"`
	Kind             string          `json:"kind,omitempty"`
	Target           string          `json:"target,omitempty"`
	TargetID         string          `json:"target_id,omitempty"`
	RuleID           string          `json:"rule_id,omitempty"`
	RuleVersion      string          `json:"rule_version,omitempty"`
	Mode             string          `json:"mode,omitempty"`
	Revision         string          `json:"revision,omitempty"`
	Checks           []string        `json:"checks,omitempty"`
	Compensation     string          `json:"compensation,omitempty"`
	Result           *ReceiptDTO     `json:"result,omitempty"`
	Verification     VerificationDTO `json:"verification"`
	Reason           string          `json:"reason"`
	PlanHash         string          `json:"plan_hash"`
	Status           string          `json:"status"`
	ExpiresAt        time.Time       `json:"expires_at"`
	DecidedBy        *string         `json:"decided_by,omitempty"`
	DecidedAt        *time.Time      `json:"decided_at,omitempty"`
	DecisionReason   *string         `json:"decision_reason,omitempty"`
	DecisionSource   *string         `json:"decision_source,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
}

// VerificationDTO exposes only task facts and a bounded, sanitised reason.
// It never exposes the verification URL, snapshot or raw observation JSON.
type VerificationDTO struct {
	Status        string     `json:"status"`
	Phase         string     `json:"phase,omitempty"`
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`
	DeadlineAt    *time.Time `json:"deadline_at,omitempty"`
	Detail        string     `json:"detail,omitempty"`
}

// ReceiptDTO is the structured execution result: whether the write happened
// and the observed revision before and after.
type ReceiptDTO struct {
	Written     bool   `json:"written"`
	Outcome     string `json:"outcome,omitempty"`
	Before      string `json:"before,omitempty"`
	After       string `json:"after,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Error       string `json:"error,omitempty"`
	ManualCheck bool   `json:"manual_check,omitempty"`
}

type ConversationMessageDTO struct {
	ID               uint64               `json:"id"`
	IncidentID       uint64               `json:"incident_id"`
	RunID            *uint64              `json:"run_id,omitempty"`
	ReplyToID        *uint64              `json:"reply_to_id,omitempty"`
	Channel          string               `json:"channel"`
	Role             string               `json:"role"`
	ActorID          *string              `json:"actor_id,omitempty"`
	ActorName        *string              `json:"actor_name,omitempty"`
	Content          string               `json:"content"`
	ToolName         *string              `json:"tool_name,omitempty"`
	ToolCallID       *string              `json:"tool_call_id,omitempty"`
	Status           string               `json:"status"`
	Citations        []CitationDTO        `json:"citations,omitempty"`
	Uncertainties    []string             `json:"uncertainties,omitempty"`
	SuggestedActions []SuggestedActionDTO `json:"suggested_actions,omitempty"`
	NeedsUserInput   bool                 `json:"needs_user_input,omitempty"`
	CreatedAt        time.Time            `json:"created_at"`
	FinishedAt       *time.Time           `json:"finished_at,omitempty"`
}

// PageInfo is shared by all cursor-like list endpoints. The cursor is an
// opaque monotonic database ID to clients; after is always strict (id > after).
type PageInfo struct {
	After     uint64 `json:"after"`
	NextAfter uint64 `json:"next_after"`
	Limit     int    `json:"limit"`
	HasMore   bool   `json:"has_more"`
}

// CitationDTO contains only durable event/step references and bounded text.
// It is extracted from assistant metadata rather than returning metadata JSON.
type CitationDTO struct {
	EventID   uint64 `json:"event_id,omitempty"`
	StepID    uint64 `json:"step_id,omitempty"`
	ID        uint64 `json:"id,omitempty"`
	Type      string `json:"type,omitempty"`
	Reference string `json:"reference,omitempty"`
	Quote     string `json:"quote,omitempty"`
}

type SuggestedActionDTO struct {
	Type        string `json:"type"`
	Reason      string `json:"reason,omitempty"`
	Description string `json:"description,omitempty"`
}

func conversationMetadata(value *datatypes.JSON) (citations []CitationDTO, uncertainties []string, actions []SuggestedActionDTO, needsInput bool) {
	citations = make([]CitationDTO, 0)
	uncertainties = make([]string, 0)
	actions = make([]SuggestedActionDTO, 0)
	if value == nil || len(*value) == 0 || !json.Valid(*value) {
		return citations, uncertainties, actions, false
	}
	var envelope struct {
		Citations []struct {
			EventID   uint64 `json:"event_id"`
			StepID    uint64 `json:"step_id"`
			ID        uint64 `json:"id"`
			Type      string `json:"type"`
			Reference string `json:"reference"`
			Quote     string `json:"quote"`
		} `json:"citations"`
		Uncertainties    []string `json:"uncertainties"`
		SuggestedActions []struct {
			Type        string `json:"type"`
			Reason      string `json:"reason"`
			Description string `json:"description"`
		} `json:"suggested_actions"`
		NeedsUserInput bool `json:"needs_user_input"`
	}
	if json.Unmarshal(*value, &envelope) != nil {
		return citations, uncertainties, actions, false
	}
	for i, citation := range envelope.Citations {
		if i >= 20 {
			break
		}
		citations = append(citations, CitationDTO{EventID: citation.EventID, StepID: citation.StepID, ID: citation.ID, Type: safeText(citation.Type, 32), Reference: safeText(citation.Reference, 256), Quote: safeText(citation.Quote, 1024)})
	}
	for i, uncertainty := range envelope.Uncertainties {
		if i >= 20 {
			break
		}
		uncertainties = append(uncertainties, safeText(uncertainty, 512))
	}
	for i, action := range envelope.SuggestedActions {
		if i >= 20 {
			break
		}
		actions = append(actions, SuggestedActionDTO{Type: safeText(action.Type, 32), Reason: safeText(action.Reason, 512), Description: safeText(action.Description, 512)})
	}
	return citations, uncertainties, actions, envelope.NeedsUserInput

}

// ControlRoomDTO is the single read model needed for the Incident room first
// paint. All nested values are the safe DTO projections above.
type ControlRoomDTO struct {
	Incident        IncidentDTO         `json:"incident"`
	Members         []IncidentMemberDTO `json:"members"`
	CurrentRun      *RunDTO             `json:"current_run,omitempty"`
	FlowNodes       []FlowNodeDTO       `json:"flow_nodes"`
	OpenProblems    []ProblemDTO        `json:"open_problems"`
	PendingApproval *ApprovalDTO        `json:"pending_approval,omitempty"`
	LatestAction    *ApprovalDTO        `json:"latest_action"`
	RecentEvents    []EventDTO          `json:"recent_events"`
}

func safeText(value string, max int) string {
	value = tools.ToSafeText(value)
	value = tools.Sanitize(value)
	if max < 1 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max]) + "…"
}

func safePtr(value *string, max int) *string {
	if value == nil {
		return nil
	}
	clean := safeText(*value, max)
	return &clean
}

func durationMillis(start time.Time, end *time.Time) int64 {
	if start.IsZero() {
		return 0
	}
	stop := time.Now().UTC()
	if end != nil && !end.IsZero() {
		stop = end.UTC()
	}
	if stop.Before(start) {
		return 0
	}
	return stop.Sub(start).Milliseconds()
}

func incidentDTO(row store.Incident) IncidentDTO {
	return IncidentDTO{
		ID: row.ID, GroupKey: safeText(row.GroupKey, 255), Status: safeText(row.Status, 32),
		Severity: row.Severity, AlertsCount: row.AlertsCount, Title: safeText(row.Title, 512),
		StartedAt: row.StartedAt.UTC(), LastSeenAt: row.LastSeenAt.UTC(), ResolvedAt: utcTimePtr(row.ResolvedAt),
		DurationMS: durationMillis(row.StartedAt, row.ResolvedAt),
	}
}

func memberDTO(row store.IncidentMember) IncidentMemberDTO {
	return IncidentMemberDTO{Fingerprint: safeText(row.Fingerprint, 128), Name: safeText(row.Name, 255), Status: safeText(row.Status, 32), Severity: row.Severity, LinkedAt: row.LinkedAt.UTC()}
}

func runDTO(row store.AgentRun) RunDTO {
	var rca string
	if row.RCAText != nil {
		rca = safeText(*row.RCAText, 2048)
	}
	return RunDTO{ID: row.ID, IncidentID: row.IncidentID, Mode: safeText(row.Mode, 32), Status: safeText(row.Status, 32), RetryOf: row.RetryOf, RCASummary: rca, TokensIn: row.TokensIn, TokensOut: row.TokensOut, StartedAt: row.StartedAt.UTC(), FinishedAt: utcTimePtr(row.FinishedAt), DurationMS: durationMillis(row.StartedAt, row.FinishedAt)}
}

func stepDTO(row store.AgentRunStep) RunStepDTO {
	status := "running"
	if row.Error != nil && strings.TrimSpace(*row.Error) != "" {
		status = "failed"
	} else if row.FinishedAt != nil {
		status = "succeeded"
	}
	var input, output string
	if row.InputJSON != nil {
		input = safeText(string(*row.InputJSON), 2048)
	}
	if row.OutputJSON != nil {
		output = safeText(string(*row.OutputJSON), 4096)
	}
	errText := ""
	if row.Error != nil {
		errText = safeText(*row.Error, 2048)
	}
	return RunStepDTO{ID: row.ID, RunID: row.RunID, Seq: row.Seq, Kind: safeText(row.Kind, 32), Name: safeText(row.Name, 128), InputSummary: input, OutputSummary: output, Error: errText, Status: status, StartedAt: row.StartedAt.UTC(), FinishedAt: utcTimePtr(row.FinishedAt), DurationMS: durationMillis(row.StartedAt, row.FinishedAt)}
}

func flowNodeDTO(row store.AgentRunStep) FlowNodeDTO {
	step := stepDTO(row)
	return FlowNodeDTO{ID: step.Kind + ":" + strconv.FormatUint(step.ID, 10), Kind: step.Kind, Name: step.Name, Status: step.Status, StepID: step.ID, StartedAt: step.StartedAt, FinishedAt: step.FinishedAt, DurationMS: step.DurationMS, Error: step.Error}
}

func eventDTO(row store.IncidentEvent) EventDTO {
	return EventDTO{ID: row.ID, IncidentID: row.IncidentID, RunID: row.RunID, ApprovalID: row.ApprovalID, EventType: safeText(row.EventType, 64), Phase: safeText(row.Phase, 32), Status: safeText(row.Status, 32), Summary: safeText(row.Summary, 512), CreatedAt: row.CreatedAt.UTC()}
}

func problemDTO(row store.IncidentProblem) ProblemDTO {
	detail := ""
	if row.DetailJSON != nil {
		detail = safeText(string(*row.DetailJSON), 2048)
	}
	return ProblemDTO{ID: row.ID, IncidentID: row.IncidentID, RunID: row.RunID, Code: safeText(row.Code, 64), Severity: safeText(row.Severity, 16), Status: safeText(row.Status, 16), Summary: safeText(row.Summary, 512), Detail: detail, FirstSeenAt: row.FirstSeenAt.UTC(), LastSeenAt: row.LastSeenAt.UTC(), ResolvedAt: utcTimePtr(row.ResolvedAt)}
}

func approvalDTO(row store.Approval) ApprovalDTO {
	value := ApprovalDTO{ID: row.ID, IncidentID: row.IncidentID, RunID: row.RunID, ParentApprovalID: row.ParentApprovalID, ToolName: safeText(row.ToolName, 128), Reason: safeText(row.Reason, 2048), PlanHash: safeText(row.PlanHash, 128), Status: safeText(row.Status, 32), ExpiresAt: row.ExpiresAt.UTC(), DecidedBy: safePtr(row.DecidedBy, 128), DecidedAt: utcTimePtr(row.DecidedAt), DecisionReason: safePtr(row.DecisionReason, 2048), DecisionSource: safePtr(row.DecisionSource, 32), CreatedAt: row.CreatedAt.UTC(), Verification: VerificationDTO{Status: "unknown"}}
	snapshot, contextErr := incident.ParseExecutionContext(row.ExecutionContext)
	hash, hashErr := incident.PlanHash(row.ToolName, row.ArgsJSON, row.ExecutionContext)
	if contextErr == nil && hashErr == nil && hash == row.PlanHash {
		value.Kind = snapshot.Kind
		value.Target = safeText(snapshot.Target.Kind+"/"+snapshot.Target.Name, 256)
		value.TargetID = safeText(snapshot.Target.ID, 128)
		value.RuleID, value.RuleVersion, value.Mode = safeText(snapshot.Rule.ID, 64), safeText(snapshot.Rule.Version, 128), snapshot.Rule.Mode
		value.Revision = safeText(snapshot.Revision, 256)
		for _, check := range snapshot.Verification.Checks {
			value.Checks = append(value.Checks, check.Kind)
		}
		if snapshot.Compensation != nil {
			value.Compensation = safeText(snapshot.Compensation.Action, 128)
		}
		switch row.Status {
		case "pending", "approved", "executing":
			value.Verification.Status = "not_started"
		case "aborted", "denied", "expired", "failed":
			// These outcomes never queue recovery verification. Failed or
			// unknown executions still require a manual check of the target.
			value.Verification.Status = "not_applicable"
		case "executed":
			// A modern successful execution must have a task; absent data is unknown.
			value.Verification.Status = "unknown"
		}
	}
	if row.ResultJSON != nil {
		var receipt ReceiptDTO
		if json.Unmarshal(*row.ResultJSON, &receipt) == nil {
			receipt.Outcome, receipt.Before, receipt.After = safeText(receipt.Outcome, 32), safeText(receipt.Before, 256), safeText(receipt.After, 256)
			receipt.Detail, receipt.Error = safeText(receipt.Detail, 1024), safeText(receipt.Error, 512)
			value.Result = &receipt
		}
	}
	if task := row.Verification; task != nil {
		value.Verification = VerificationDTO{Status: safeText(task.Status, 32), Phase: safeText(task.Phase, 8), LastCheckedAt: utcTimePtr(task.LastCheckedAt)}
		if !task.DeadlineAt.IsZero() {
			value.Verification.DeadlineAt = utcTimePtr(&task.DeadlineAt)
		}
		var result struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(task.LastResultJSON, &result) == nil {
			value.Verification.Detail = safeText(result.Detail, 2048)
		}
	}
	return value
}

func conversationMessageDTO(row store.ConversationMessage) ConversationMessageDTO {
	citations, uncertainties, actions, needsInput := conversationMetadata(row.MetadataJSON)
	return ConversationMessageDTO{
		ID: row.ID, IncidentID: row.IncidentID, RunID: row.RunID, ReplyToID: row.ReplyToID,
		Channel: safeText(row.Channel, 32), Role: safeText(row.Role, 32), ActorID: safePtr(row.ActorID, 128), ActorName: safePtr(row.ActorName, 128),
		Content: safeText(row.Content, 8192), ToolName: safePtr(row.ToolName, 128), ToolCallID: safePtr(row.ToolCallID, 128), Status: safeText(row.Status, 32),
		Citations: citations, Uncertainties: uncertainties, SuggestedActions: actions, NeedsUserInput: needsInput,
		CreatedAt: row.CreatedAt.UTC(), FinishedAt: utcTimePtr(row.FinishedAt),
	}
}

func utcTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	clean := value.UTC()
	return &clean
}

func parseCursor(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, errors.New("invalid after cursor")
	}
	return value, nil
}

func parseLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 20, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("invalid limit")
	}
	if value < 1 {
		return 20, nil
	}
	if value > 100 {
		value = 100
	}
	return value, nil
}

func pageInfo(after uint64, limit uint64, count int, next uint64) PageInfo {
	return PageInfo{After: after, NextAfter: next, Limit: int(limit), HasMore: count >= int(limit) && next != after}
}

func writePage(w http.ResponseWriter, key string, values any, after uint64, limit int, count int, next uint64) {
	writeJSON(w, http.StatusOK, map[string]any{key: values, "page": pageInfo(after, uint64(limit), count, next)})
}

func writeMethodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}
func writeNotFound(w http.ResponseWriter) { writeError(w, http.StatusNotFound, "not found") }
