package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/store"
)

// RunStore is the ownership-safe run/step query seam. The store method checks
// that runID belongs to incidentID before returning any step rows.
type RunStore interface {
	ListAgentRuns(context.Context, uint64, uint64, int) ([]store.AgentRun, error)
	ListIncidentRunSteps(context.Context, uint64, uint64, uint64, int) ([]store.AgentRunStep, error)
}

// RunAPI serves public Incident-scoped run and step history.
type RunAPI struct {
	db   RunStore
	auth SessionAuthenticator
}

func NewRunAPI(db RunStore, auth SessionAuthenticator) *RunAPI {
	return &RunAPI{db: db, auth: auth}
}

func (h *RunAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

func (h *RunAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	if _, _, ok := authenticateSession(h.auth, r); !ok {
		// Do not distinguish an invalid resource from a missing session on known
		// data paths; the caller must authenticate before any store read.
		if isRunPath(parts) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}
	switch {
	case len(parts) == 3 && parts[0] == "incidents" && parts[2] == "runs":
		id, err := parseIncidentID(parts[1])
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid incident id")
			return
		}
		h.listRuns(w, r, id)
	case len(parts) == 3 && parts[0] == "runs" && parts[2] == "steps":
		runID, err := parseIncidentID(parts[1])
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid run id")
			return
		}
		incidentID, err := parseIncidentID(r.URL.Query().Get("incident_id"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "incident_id is required")
			return
		}
		h.listSteps(w, r, incidentID, runID)
	case len(parts) == 5 && parts[0] == "incidents" && parts[2] == "runs" && parts[4] == "steps":
		incidentID, err := parseIncidentID(parts[1])
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid incident id")
			return
		}
		runID, err := parseIncidentID(parts[3])
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid run id")
			return
		}
		h.listSteps(w, r, incidentID, runID)
	default:
		writeNotFound(w)
	}
}

func isRunPath(parts []string) bool {
	return len(parts) > 0 && (parts[0] == "runs" || parts[0] == "incidents")
}

func (h *RunAPI) listRuns(w http.ResponseWriter, r *http.Request, incidentID uint64) {
	after, limit, ok := parsePageQuery(w, r)
	if !ok {
		return
	}
	rows, err := h.db.ListAgentRuns(r.Context(), incidentID, after, limit)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "list incident runs failed")
		return
	}
	values := make([]RunDTO, 0, len(rows))
	var next uint64 = after
	for _, row := range rows {
		// Defensive ownership check protects a fake or future adapter that does
		// not include incident_id in its SQL predicate.
		if row.IncidentID != incidentID {
			continue
		}
		values = append(values, runDTO(row))
		if row.ID > next {
			next = row.ID
		}
	}
	writePage(w, "runs", values, after, limit, len(rows), next)
}

func (h *RunAPI) listSteps(w http.ResponseWriter, r *http.Request, incidentID, runID uint64) {
	after, limit, ok := parsePageQuery(w, r)
	if !ok {
		return
	}
	rows, err := h.db.ListIncidentRunSteps(r.Context(), incidentID, runID, after, limit)
	if err != nil {
		if errors.Is(err, store.ErrAgentRunNotFound) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "list run steps failed")
		return
	}
	values := make([]RunStepDTO, 0, len(rows))
	var next uint64 = after
	for _, row := range rows {
		values = append(values, stepDTO(row))
		if row.ID > next {
			next = row.ID
		}
	}
	writePage(w, "steps", values, after, limit, len(rows), next)
}
