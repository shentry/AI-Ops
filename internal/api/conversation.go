package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/auth"
	"oncall-agent/internal/conversation"
	"oncall-agent/internal/store"
)

// ConversationService is the asynchronous Incident question seam. Ask must
// enqueue a queued user message and return; it must not execute an LLM in the
// HTTP request.
type ConversationService interface {
	Ask(context.Context, uint64, conversation.Actor, string, string) (store.ConversationMessage, error)
	List(context.Context, uint64, uint64, int) ([]store.ConversationMessage, error)
}

// IncidentActionService contains only queueing operations used by the Web
// action endpoints. Implementations own authorization/business validation and
// must not run LLM, Docker or Verify work synchronously in this handler.
type IncidentActionService interface {
	Rediagnose(context.Context, uint64, auth.Actor) (store.AgentRun, error)
	RequestEvidence(context.Context, uint64, auth.Actor, string) error
}

// ConversationAPI serves public conversation history and asynchronous Incident
// actions. The route assembly may provide an anonymous or session authenticator.
type ConversationAPI struct {
	service ConversationService
	auth    SessionAuthenticator
	actions IncidentActionService
}

func NewConversationAPI(service ConversationService, authn SessionAuthenticator, actions ...IncidentActionService) *ConversationAPI {
	var action IncidentActionService
	if len(actions) > 0 {
		action = actions[0]
	}
	return &ConversationAPI{service: service, auth: authn, actions: action}
}

func (h *ConversationAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

func (h *ConversationAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || r == nil {
		writeNotFound(w)
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 3 || parts[0] != "incidents" {
		writeNotFound(w)
		return
	}
	id, err := parseIncidentID(parts[1])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid incident id")
		return
	}
	switch parts[2] {
	case "conversation":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		h.list(w, r, id)
	case "questions":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		h.ask(w, r, id)
	case "rediagnose":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		h.rediagnose(w, r, id)
	case "request-evidence":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		h.requestEvidence(w, r, id)
	default:
		writeNotFound(w)
	}
}

func (h *ConversationAPI) list(w http.ResponseWriter, r *http.Request, incidentID uint64) {
	if _, _, ok := authenticateSession(h.auth, r); !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.service == nil {
		writeError(w, http.StatusNotImplemented, "conversation service unavailable")
		return
	}
	after, limit, ok := parsePageQuery(w, r)
	if !ok {
		return
	}
	rows, err := h.service.List(r.Context(), incidentID, after, limit)
	if err != nil {
		if errors.Is(err, store.ErrIncidentNotFound) {
			writeError(w, http.StatusNotFound, "incident not found")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "list conversation failed")
		return
	}
	values := make([]ConversationMessageDTO, 0, len(rows))
	var next uint64 = after
	for _, row := range rows {
		if row.IncidentID != incidentID {
			continue
		}
		values = append(values, conversationMessageDTO(row))
		if row.ID > next {
			next = row.ID
		}
	}
	writePage(w, "messages", values, after, limit, len(rows), next)
}

func (h *ConversationAPI) ask(w http.ResponseWriter, r *http.Request, incidentID uint64) {
	actor, _, status, message := requireWriteSession(h.auth, r)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	if h.service == nil {
		writeError(w, http.StatusNotImplemented, "conversation service unavailable")
		return
	}
	var request struct {
		Question string `json:"question"`
	}
	if err := decodeBoundedJSON(r, &request, 16<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid question")
		return
	}
	question := strings.TrimSpace(request.Question)
	if question == "" || len([]rune(question)) > 4000 {
		writeError(w, http.StatusBadRequest, "question must contain 1-4000 characters")
		return
	}
	messageRow, err := h.service.Ask(r.Context(), incidentID, conversation.Actor{ID: actor.ID, Name: actor.Name, Source: "web"}, question, "web")
	if err != nil {
		switch {
		case errors.Is(err, store.ErrIncidentNotFound):
			writeError(w, http.StatusNotFound, "incident not found")
		case errors.Is(err, conversation.ErrQuestionRequired), errors.Is(err, conversation.ErrQuestionTooLong):
			writeError(w, http.StatusBadRequest, "invalid question")
		case errors.Is(err, conversation.ErrQuestionInProgress):
			writeError(w, http.StatusConflict, "another question is already running")
		default:
			writeError(w, http.StatusServiceUnavailable, "queue question failed")
		}
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"message": conversationMessageDTO(messageRow), "message_id": messageRow.ID})
}

func (h *ConversationAPI) rediagnose(w http.ResponseWriter, r *http.Request, incidentID uint64) {
	actor, _, status, message := requireWriteSession(h.auth, r)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	if h.actions == nil {
		writeError(w, http.StatusNotImplemented, "rediagnose service unavailable")
		return
	}
	run, err := h.actions.Rediagnose(r.Context(), incidentID, actor)
	if err != nil {
		if errors.Is(err, store.ErrIncidentNotFound) {
			writeError(w, http.StatusNotFound, "incident not found")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "queue rediagnosis failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": runDTO(run)})
}

func (h *ConversationAPI) requestEvidence(w http.ResponseWriter, r *http.Request, incidentID uint64) {
	actor, _, status, message := requireWriteSession(h.auth, r)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	if h.actions == nil {
		writeError(w, http.StatusNotImplemented, "evidence request service unavailable")
		return
	}
	var request struct {
		Request string `json:"request"`
	}
	if err := decodeBoundedJSON(r, &request, 16<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid evidence request")
		return
	}
	request.Request = strings.TrimSpace(request.Request)
	if request.Request == "" || len([]rune(request.Request)) > 4000 {
		writeError(w, http.StatusBadRequest, "request must contain 1-4000 characters")
		return
	}
	if err := h.actions.RequestEvidence(r.Context(), incidentID, actor, request.Request); err != nil {
		if errors.Is(err, store.ErrIncidentNotFound) {
			writeError(w, http.StatusNotFound, "incident not found")
			return
		}
		if errors.Is(err, conversation.ErrQuestionInProgress) {
			writeError(w, http.StatusConflict, "another question is already running")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "queue evidence request failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "incident_id": incidentID})
}

func decodeBoundedJSON(r *http.Request, target any, max int64) error {
	if r == nil || r.Body == nil {
		return errors.New("request body is required")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > max {
		return errors.New("request body too large")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return errors.New("request body is required")
	}
	return json.Unmarshal(body, target)
}
