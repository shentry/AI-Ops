package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/knowledge"
	"oncall-agent/internal/store"
)

// KnowledgeStore is what the knowledge page reads and writes.
type KnowledgeStore interface {
	knowledge.IncidentStore
	CountSkillActivations(ctx context.Context, since time.Time) (map[string]int, error)
	SearchKnowledge(ctx context.Context, query, source string, limit int) ([]store.KnowledgeHit, error)
	ListKnowledge(ctx context.Context, source string) ([]store.KnowledgeEntry, error)
	GetKnowledge(ctx context.Context, id uint64) (store.KnowledgeEntry, error)
	DeleteIncidentKnowledge(ctx context.Context, id uint64, actor string, now time.Time) error
}

// KnowledgeAPI serves the knowledge page: read-only skills, search and
// reading of entries, adding a reviewed incident (operator) and removing an
// incident entry (admin). Repository entries change only through the repository.
type KnowledgeAPI struct {
	skills *knowledge.Skills
	db     KnowledgeStore
	auth   *Auth
	now    func() time.Time
}

func NewKnowledgeAPI(skills *knowledge.Skills, db KnowledgeStore, auth *Auth) *KnowledgeAPI {
	return &KnowledgeAPI{skills: skills, db: db, auth: auth, now: func() time.Time { return time.Now().UTC() }}
}

func (h *KnowledgeAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

const knowledgePageLimit = 20

func (h *KnowledgeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 3 && parts[2] == "skills" && r.Method == http.MethodGet:
		h.listSkills(w, r)
	case len(parts) == 3 && parts[2] == "knowledge" && r.Method == http.MethodGet:
		h.search(w, r)
	case len(parts) == 4 && parts[2] == "knowledge" && r.Method == http.MethodGet:
		h.read(w, r, parts[3])
	case len(parts) == 4 && parts[2] == "knowledge" && r.Method == http.MethodDelete:
		h.remove(w, r, parts[3])
	case len(parts) == 5 && parts[2] == "incidents" && parts[4] == "knowledge" && r.Method == http.MethodPost:
		h.addIncident(w, r, parts[3])
	case len(parts) >= 3 && (parts[2] == "skills" || parts[2] == "knowledge" || parts[2] == "incidents"):
		writeMethodNotAllowed(w)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

type skillView struct {
	knowledge.Skill
	Activations30d int `json:"activations_30d"`
}

func (h *KnowledgeAPI) listSkills(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.auth.Require(w, r, RoleViewer, false); !ok {
		return
	}
	counts, err := h.db.CountSkillActivations(r.Context(), h.now().Add(-30*24*time.Hour))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "skill activations are unavailable")
		return
	}
	views := []skillView{}
	for _, skill := range h.skills.List() {
		views = append(views, skillView{Skill: skill, Activations30d: counts[skill.Name]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": views})
}

// KnowledgeEntryDTO is an entry in a list; Body is set only when one entry is read.
type KnowledgeEntryDTO struct {
	ID        uint64    `json:"id"`
	Source    string    `json:"source"`
	Ref       string    `json:"ref"`
	Title     string    `json:"title"`
	CreatedBy string    `json:"created_by"`
	UpdatedAt time.Time `json:"updated_at"`
	Snippet   string    `json:"snippet,omitempty"`
	Body      string    `json:"body,omitempty"`
}

func knowledgeDTO(entry store.KnowledgeEntry) KnowledgeEntryDTO {
	return KnowledgeEntryDTO{ID: entry.ID, Source: entry.Source, Ref: entry.Ref, Title: entry.Title, CreatedBy: entry.CreatedBy, UpdatedAt: entry.UpdatedAt.UTC()}
}

// search lists every entry without q, else the best matches with a snippet.
func (h *KnowledgeAPI) search(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.auth.Require(w, r, RoleViewer, false); !ok {
		return
	}
	query, source := strings.TrimSpace(r.URL.Query().Get("q")), r.URL.Query().Get("source")
	if source != "" && source != store.KnowledgeRepo && source != store.KnowledgeIncident {
		writeError(w, http.StatusBadRequest, "source must be repo or incident")
		return
	}
	if len(query) > 256 {
		writeError(w, http.StatusBadRequest, "query is too long")
		return
	}
	entries := []KnowledgeEntryDTO{}
	if query == "" {
		rows, err := h.db.ListKnowledge(r.Context(), source)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "knowledge is unavailable")
			return
		}
		for _, row := range rows {
			entries = append(entries, knowledgeDTO(row))
		}
	} else {
		hits, err := h.db.SearchKnowledge(r.Context(), query, source, knowledgePageLimit)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "knowledge is unavailable")
			return
		}
		for _, hit := range hits {
			dto := knowledgeDTO(hit.KnowledgeEntry)
			dto.Snippet = knowledge.Snippet(hit.Body, query)
			entries = append(entries, dto)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (h *KnowledgeAPI) read(w http.ResponseWriter, r *http.Request, rawID string) {
	if _, ok := h.auth.Require(w, r, RoleViewer, false); !ok {
		return
	}
	id, err := parseIncidentID(rawID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return
	}
	entry, err := h.db.GetKnowledge(r.Context(), id)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	dto := knowledgeDTO(entry)
	dto.Body = entry.Body
	writeJSON(w, http.StatusOK, dto)
}

func (h *KnowledgeAPI) remove(w http.ResponseWriter, r *http.Request, rawID string) {
	actor, ok := h.auth.Require(w, r, RoleAdmin, false)
	if !ok {
		return
	}
	id, err := parseIncidentID(rawID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return
	}
	if err := h.db.DeleteIncidentKnowledge(r.Context(), id, actor.ID, h.now()); err != nil {
		h.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *KnowledgeAPI) addIncident(w http.ResponseWriter, r *http.Request, rawID string) {
	actor, ok := h.auth.Require(w, r, RoleOperator, false)
	if !ok {
		return
	}
	id, err := parseIncidentID(rawID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return
	}
	entry, err := knowledge.AddIncident(r.Context(), h.db, id, actor.ID, h.now())
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, knowledgeDTO(entry))
}

func (h *KnowledgeAPI) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrKnowledgeNotFound), errors.Is(err, store.ErrIncidentNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrKnowledgeReadOnly):
		writeError(w, http.StatusConflict, "repository entries change only through the repository")
	case errors.Is(err, knowledge.ErrNotReviewed):
		writeError(w, http.StatusConflict, "the incident needs a review with a confirmed root cause first")
	default:
		writeError(w, http.StatusServiceUnavailable, "knowledge is unavailable")
	}
}
