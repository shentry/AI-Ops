package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/store"
)

// ControlRoomStore is the read-only database seam for Incident control-room
// projections. *store.DB satisfies it directly; tests can provide a small fake.
type ControlRoomStore interface {
	GetIncident(context.Context, uint64) (store.Incident, error)
	ListIncidentMembers(context.Context, uint64) ([]store.IncidentMember, error)
	ListIncidentEvents(context.Context, uint64, uint64, int) ([]store.IncidentEvent, error)
	ListIncidentProblems(context.Context, uint64, string, int) ([]store.IncidentProblem, error)
	ListAgentRuns(context.Context, uint64, uint64, int) ([]store.AgentRun, error)
	ListIncidentApprovals(context.Context, uint64, string, int) ([]store.Approval, error)
}

// ApprovalReader is optional because the control-room aggregate only needs the
// Incident-scoped approval list. Providing it enables GET /api/v1/approvals/:id
// with the same browser session boundary.
type ApprovalReader interface {
	GetApproval(context.Context, uint64) (store.Approval, error)
}

// IncidentLister is optional and powers the console's incident picker. The
// Bearer-authenticated IncidentAPI keeps returning store rows for automation;
// this seam exists so the browser reads the same sanitized DTO as every other
// control-room view.
type IncidentLister interface {
	ListLatestIncidents(context.Context, string, int) ([]store.Incident, error)
}

// IncidentRunStepReader is optional for first-paint flow nodes. The dedicated
// RunAPI always requires this method for ownership-safe step reads.
type IncidentRunStepReader interface {
	ListIncidentRunSteps(context.Context, uint64, uint64, uint64, int) ([]store.AgentRunStep, error)
}

// ControlRoomAPI serves the public, read-only Incident projection.
// Mutating operations live in ConversationAPI and are injected separately; the
// route assembly decides whether the authenticator is anonymous or session-backed.
type ControlRoomAPI struct {
	db   ControlRoomStore
	auth SessionAuthenticator
}

func NewControlRoomAPI(db ControlRoomStore, auth SessionAuthenticator) *ControlRoomAPI {
	return &ControlRoomAPI{db: db, auth: auth}
}

// Handle adapts the standard-library handler to GoFrame routing.
func (h *ControlRoomAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

func (h *ControlRoomAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.db == nil || r == nil {
		writeNotFound(w)
		return
	}
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 3 && parts[0] == "incidents" {
		id, err := parseIncidentID(parts[1])
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid incident id")
			return
		}
		if _, _, ok := authenticateSession(h.auth, r); !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		switch parts[2] {
		case "control-room":
			h.controlRoom(w, r, id)
		case "events":
			h.events(w, r, id)
		case "problems":
			h.problems(w, r, id)
		default:
			writeNotFound(w)
		}
		return
	}
	if len(parts) == 2 && parts[0] == "approvals" {
		id, err := parseIncidentID(parts[1])
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid approval id")
			return
		}
		if _, _, ok := authenticateSession(h.auth, r); !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h.approval(w, r, id)
		return
	}
	if len(parts) == 2 && parts[0] == "control-room" && parts[1] == "incidents" {
		if _, _, ok := authenticateSession(h.auth, r); !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h.incidents(w, r)
		return
	}
	writeNotFound(w)
}

func (h *ControlRoomAPI) controlRoom(w http.ResponseWriter, r *http.Request, id uint64) {
	incident, err := h.db.GetIncident(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, store.ErrIncidentNotFound, "incident not found", "get incident failed")
		return
	}
	members, err := h.db.ListIncidentMembers(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "list incident members failed")
		return
	}
	runs, err := h.db.ListAgentRuns(r.Context(), id, 0, 100)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "list incident runs failed")
		return
	}
	problems, err := h.db.ListIncidentProblems(r.Context(), id, "open", 100)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "list incident problems failed")
		return
	}
	approvals, err := h.db.ListIncidentApprovals(r.Context(), id, "pending", 1)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "list incident approvals failed")
		return
	}
	events, err := h.latestEvents(r.Context(), id, 20)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "list incident events failed")
		return
	}

	response := ControlRoomDTO{
		Incident:     incidentDTO(incident),
		Members:      make([]IncidentMemberDTO, 0, len(members)),
		FlowNodes:    make([]FlowNodeDTO, 0, 8),
		OpenProblems: make([]ProblemDTO, 0, len(problems)),
		RecentEvents: make([]EventDTO, 0, len(events)),
	}
	for _, member := range members {
		response.Members = append(response.Members, memberDTO(member))
	}
	for _, problem := range problems {
		response.OpenProblems = append(response.OpenProblems, problemDTO(problem))
	}
	for _, event := range events {
		response.RecentEvents = append(response.RecentEvents, eventDTO(event))
	}
	if len(approvals) > 0 {
		value := approvalDTO(approvals[0])
		response.PendingApproval = &value
	}
	var steps []store.AgentRunStep
	if len(runs) > 0 {
		currentRun := runs[0]
		for _, candidate := range runs[1:] {
			if candidate.ID > currentRun.ID {
				currentRun = candidate
			}
		}
		current := runDTO(currentRun)
		response.CurrentRun = &current
		if stepReader, ok := h.db.(IncidentRunStepReader); ok {
			steps, err = stepReader.ListIncidentRunSteps(r.Context(), id, currentRun.ID, 0, 100)
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "list run steps failed")
				return
			}
		}
	}
	response.FlowNodes = controlRoomFlowNodes(incident, steps, approvals, events)
	writeJSON(w, http.StatusOK, response)
}

func controlRoomFlowNodes(incident store.Incident, steps []store.AgentRunStep, approvals []store.Approval, events []store.IncidentEvent) []FlowNodeDTO {
	stages := []struct{ key, label string }{{"alert", "alert"}, {"evidence", "evidence"}, {"reasoner", "llm"}, {"guard", "guard"}, {"policy", "policy"}, {"approval", "approval"}, {"execute", "execution"}, {"verify", "verify"}}
	nodes := make([]FlowNodeDTO, 0, len(stages))
	for _, stage := range stages {
		node := FlowNodeDTO{ID: stage.key, Kind: stage.key, Name: stage.label, Status: "queued"}
		if stage.key == "alert" {
			node.Status = incident.Status
			if node.Status == "firing" || node.Status == "acknowledged" || node.Status == "resolved" {
				node.Status = "succeeded"
			}
		}
		for _, step := range steps {
			if stepMatchesStage(step, stage.key) {
				candidate := flowNodeDTO(step)
				candidate.ID, candidate.Kind, candidate.Name = stage.key, stage.key, stage.label
				node = candidate
			}
		}
		for _, event := range events {
			if eventMatchesStage(event, stage.key) {
				node.Status = flowEventStatus(event.Status)
				node.StartedAt = event.CreatedAt
			}
		}
		if stage.key == "approval" && len(approvals) > 0 && approvals[0].Status == "pending" {
			node.Status = "blocked"
		}
		nodes = append(nodes, node)
	}
	return nodes
}

func stepMatchesStage(step store.AgentRunStep, stage string) bool {
	switch stage {
	case "reasoner":
		return step.Kind == "llm"
	case "policy":
		return step.Kind == "approval" && step.Name == "policy"
	case "approval":
		return step.Kind == "approval" && step.Name != "policy"
	default:
		return step.Kind == stage
	}
}

func eventMatchesStage(event store.IncidentEvent, stage string) bool {
	return (stage == "execute" && strings.HasPrefix(event.EventType, "execution.")) || (stage == "approval" && strings.HasPrefix(event.EventType, "approval.")) || (stage == "verify" && strings.HasPrefix(event.EventType, "verify."))
}

func flowEventStatus(status string) string {
	switch status {
	case "pending":
		return "blocked"
	case "completed", "passed", "sent":
		return "succeeded"
	default:
		return status
	}
}

func (h *ControlRoomAPI) latestEvents(ctx context.Context, incidentID uint64, limit int) ([]store.IncidentEvent, error) {
	type latestReader interface {
		ListLatestIncidentEvents(context.Context, uint64, int) ([]store.IncidentEvent, error)
	}
	if reader, ok := h.db.(latestReader); ok {
		return reader.ListLatestIncidentEvents(ctx, incidentID, limit)
	}
	events, err := h.db.ListIncidentEvents(ctx, incidentID, 0, 100)
	if err != nil {
		return nil, err
	}
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events, nil
}

func (h *ControlRoomAPI) events(w http.ResponseWriter, r *http.Request, id uint64) {
	after, limit, ok := parsePageQuery(w, r)
	if !ok {
		return
	}
	rows, err := h.db.ListIncidentEvents(r.Context(), id, after, limit)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "list incident events failed")
		return
	}
	values := make([]EventDTO, 0, len(rows))
	var next uint64 = after
	for _, row := range rows {
		values = append(values, eventDTO(row))
		if row.ID > next {
			next = row.ID
		}
	}
	writePage(w, "events", values, after, limit, len(rows), next)
}

func (h *ControlRoomAPI) problems(w http.ResponseWriter, r *http.Request, id uint64) {
	after, limit, ok := parsePageQuery(w, r)
	if !ok {
		return
	}
	// Problem queries have no cursor in the store contract. We still expose a
	// stable PageInfo and reject non-zero after rather than silently returning a
	// misleading page.
	if after != 0 {
		writeError(w, http.StatusBadRequest, "problem cursor is not supported")
		return
	}
	rows, err := h.db.ListIncidentProblems(r.Context(), id, strings.TrimSpace(r.URL.Query().Get("status")), limit)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "list incident problems failed")
		return
	}
	values := make([]ProblemDTO, 0, len(rows))
	var next uint64
	for _, row := range rows {
		values = append(values, problemDTO(row))
		if row.ID > next {
			next = row.ID
		}
	}
	writePage(w, "problems", values, after, limit, len(rows), next)
}

func (h *ControlRoomAPI) approval(w http.ResponseWriter, r *http.Request, id uint64) {
	reader, ok := h.db.(ApprovalReader)
	if !ok {
		writeError(w, http.StatusNotImplemented, "approval reader unavailable")
		return
	}
	row, err := reader.GetApproval(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, store.ErrApprovalNotFound, "approval not found", "get approval failed")
		return
	}
	writeJSON(w, http.StatusOK, approvalDTO(row))
}

// incidents lists the newest incidents for the console picker. It is limit-only
// by design: the picker needs the current page, and every deeper view is reached
// by incident id.
func (h *ControlRoomAPI) incidents(w http.ResponseWriter, r *http.Request) {
	lister, ok := h.db.(IncidentLister)
	if !ok {
		writeError(w, http.StatusNotImplemented, "incident lister unavailable")
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := lister.ListLatestIncidents(r.Context(), strings.TrimSpace(r.URL.Query().Get("status")), limit)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "list incidents failed")
		return
	}
	values := make([]IncidentDTO, 0, len(rows))
	for _, row := range rows {
		values = append(values, incidentDTO(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": values, "limit": limit})
}

func parsePageQuery(w http.ResponseWriter, r *http.Request) (uint64, int, bool) {
	after, err := parseCursor(r.URL.Query().Get("after"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return 0, 0, false
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return 0, 0, false
	}
	return after, limit, true
}

func writeStoreError(w http.ResponseWriter, err error, notFound error, notFoundMessage, fallback string) {
	if errors.Is(err, notFound) {
		writeError(w, http.StatusNotFound, notFoundMessage)
		return
	}
	writeError(w, http.StatusServiceUnavailable, fallback)
}
