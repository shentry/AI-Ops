package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/llm"
)

// ModelSwitchService is the global LLM selection seam. It exposes only the
// configured model IDs and never provider credentials.
type ModelSwitchService interface {
	Current(context.Context) (llm.ModelSelection, error)
	Options() []llm.ModelOption
	Select(context.Context, string) (llm.ModelSelection, error)
}

type ModelOptionDTO struct {
	ID              string `json:"id"`
	ThinkingEnabled bool   `json:"thinking_enabled"`
}

type ModelStateDTO struct {
	CurrentModel string           `json:"current_model"`
	UpdatedAt    string           `json:"updated_at"`
	Models       []ModelOptionDTO `json:"models"`
}

// ModelAPI separates public, read-only model status from the Bearer-only
// management endpoint. It intentionally does not accept anonymous-console
// identity for mutation.
type ModelAPI struct {
	service    ModelSwitchService
	authToken  string
	publicAuth SessionAuthenticator
}

func NewModelAPI(service ModelSwitchService, authToken string, publicAuth SessionAuthenticator) *ModelAPI {
	return &ModelAPI{service: service, authToken: authToken, publicAuth: publicAuth}
}

func (h *ModelAPI) Handle(r *ghttp.Request) {
	if r == nil || r.Response == nil || r.Request == nil {
		return
	}
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

func (h *ModelAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || r == nil {
		writeNotFound(w)
		return
	}
	switch r.URL.Path {
	case "/api/v1/control-room/model":
		h.public(w, r)
	case "/api/v1/admin/model":
		h.admin(w, r)
	default:
		writeNotFound(w)
	}
}

func (h *ModelAPI) public(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	if _, _, ok := authenticateSession(h.publicAuth, r); !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	h.writeState(w, r)
}

func (h *ModelAPI) admin(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.writeState(w, r)
	case http.MethodPut:
		h.selectModel(w, r)
	default:
		writeMethodNotAllowed(w)
	}
}

func (h *ModelAPI) authorized(r *http.Request) bool {
	if h == nil || r == nil || strings.TrimSpace(h.authToken) == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.authToken)) == 1
}

func (h *ModelAPI) writeState(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		writeError(w, http.StatusServiceUnavailable, "model switching unavailable")
		return
	}
	selection, err := h.service.Current(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "get current model failed")
		return
	}
	writeJSON(w, http.StatusOK, modelStateDTO(selection, h.service.Options()))
}

func (h *ModelAPI) selectModel(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		writeError(w, http.StatusServiceUnavailable, "model switching unavailable")
		return
	}
	var input struct {
		Model string `json:"model"`
	}
	if err := decodeBoundedJSON(r, &input, 16<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid model selection")
		return
	}
	selection, err := h.service.Select(r.Context(), input.Model)
	if err != nil {
		if errors.Is(err, llm.ErrModelNotAllowed) {
			writeError(w, http.StatusBadRequest, "model is not allowed")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "set current model failed")
		return
	}
	writeJSON(w, http.StatusOK, modelStateDTO(selection, h.service.Options()))
}

func modelStateDTO(selection llm.ModelSelection, options []llm.ModelOption) ModelStateDTO {
	models := make([]ModelOptionDTO, 0, len(options))
	for _, option := range options {
		models = append(models, ModelOptionDTO{ID: option.ID, ThinkingEnabled: option.ThinkingEnabled})
	}
	return ModelStateDTO{CurrentModel: selection.Model, UpdatedAt: selection.UpdatedAt.UTC().Format(time.RFC3339Nano), Models: models}
}
