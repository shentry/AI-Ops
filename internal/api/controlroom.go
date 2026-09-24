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
// projections. *store.DB satisfies it directly; tests provide a small fake.
// Every method is required: a partially-implemented store must fail to compile
// rather than degrade one panel of the room at runtime.
type ControlRoomStore interface {
	GetIncident(context.Context, uint64) (store.Incident, error)
	ListIncidentMembers(context.Context, uint64) ([]store.IncidentMember, error)
	ListIncidentEvents(context.Context, uint64, uint64, int) ([]store.IncidentEvent, error)
	ListLatestIncidentEvents(context.Context, uint64, int) ([]store.IncidentEvent, error)
	ListIncidentProblems(context.Context, uint64, string, int) ([]store.IncidentProblem, error)
	ListAgentRuns(context.Context, uint64, uint64, int) ([]store.AgentRun, error)
	ListIncidentRunSteps(context.Context, uint64, uint64, uint64, int) ([]store.AgentRunStep, error)
	ListIncidentApprovals(context.Context, uint64, string, int) ([]store.Approval, error)
	ListLatestIncidents(context.Context, string, int) ([]store.Incident, error)
	GetApproval(context.Context, uint64) (store.Approval, error)
}

// ControlRoomAPI serves the public, read-only Incident projection.
// Mutating operations live in ConversationAPI and are injected separately.
type ControlRoomAPI struct {
	db      ControlRoomStore
	console *Console
}

func NewControlRoomAPI(db ControlRoomStore, console *Console) *ControlRoomAPI {
	return &ControlRoomAPI{db: db, console: console}
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
		if _, ok := h.console.Actor(); !ok {
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
		if _, ok := h.console.Actor(); !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h.approval(w, r, id)
		return
	}
	if len(parts) == 2 && parts[0] == "control-room" && parts[1] == "incidents" {
		if _, ok := h.console.Actor(); !ok {
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
	latestActions, err := h.db.ListIncidentApprovals(r.Context(), id, "", 1)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "list latest incident action failed")
		return
	}
	events, err := h.db.ListLatestIncidentEvents(r.Context(), id, 20)
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
	if len(latestActions) > 0 {
		value := approvalDTO(latestActions[0])
		response.LatestAction = &value
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
		steps, err = h.db.ListIncidentRunSteps(r.Context(), id, currentRun.ID, 0, 100)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "list run steps failed")
			return
		}
	}
	flowAction := response.LatestAction
	if flowAction != nil && response.CurrentRun != nil && flowAction.RunID != response.CurrentRun.ID {
		// The separate latest_action panel retains earlier history, not this Run's flow.
		flowAction = nil
	}
	flowEvents := make([]store.IncidentEvent, 0, len(events))
	for _, event := range events {
		if response.CurrentRun == nil || (event.RunID != nil && *event.RunID == response.CurrentRun.ID) {
			flowEvents = append(flowEvents, event)
		}
	}
	response.FlowNodes = controlRoomFlowNodes(incident, steps, approvals, flowEvents, flowAction)
	writeJSON(w, http.StatusOK, response)
}

func controlRoomFlowNodes(incident store.Incident, steps []store.AgentRunStep, approvals []store.Approval, events []store.IncidentEvent, action *ApprovalDTO) []FlowNodeDTO {
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
		// A completed audit step is not a recovery verdict, and the event window
		// may no longer contain the terminal event. Use the persisted action/task.
		if stage.key == "execute" || stage.key == "verify" {
			node.Status = "not_started"
			if action != nil {
				if stage.key == "execute" {
					node.Status = action.Status
				} else {
					node.Status = action.Verification.Status
				}
			}
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
	row, err := h.db.GetApproval(r.Context(), id)
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
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := h.db.ListLatestIncidents(r.Context(), strings.TrimSpace(r.URL.Query().Get("status")), limit)
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
