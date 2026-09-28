package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/knowledge"
)

// SkillActivationCounter is the one store read the skills tab needs.
type SkillActivationCounter interface {
	CountSkillActivations(ctx context.Context, since time.Time) (map[string]int, error)
}

// KnowledgeAPI serves the knowledge page. Skills change only through the
// repository, so their tab is read-only.
type KnowledgeAPI struct {
	skills *knowledge.Skills
	counts SkillActivationCounter
	auth   *Auth
}

func NewKnowledgeAPI(skills *knowledge.Skills, counts SkillActivationCounter, auth *Auth) *KnowledgeAPI {
	return &KnowledgeAPI{skills: skills, counts: counts, auth: auth}
}

func (h *KnowledgeAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

type skillView struct {
	knowledge.Skill
	Activations30d int `json:"activations_30d"`
}

func (h *KnowledgeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.auth.Require(w, r, RoleViewer, false); !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	counts, err := h.counts.CountSkillActivations(r.Context(), time.Now().UTC().Add(-30*24*time.Hour))
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
