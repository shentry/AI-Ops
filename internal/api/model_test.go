package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/llm"
)

type fakeModelSwitchService struct {
	selection llm.ModelSelection
	options   []llm.ModelOption
	selectErr error
	selected  string
}

func (s *fakeModelSwitchService) Current(context.Context) (llm.ModelSelection, error) {
	return s.selection, nil
}

func (s *fakeModelSwitchService) Options() []llm.ModelOption {
	return append([]llm.ModelOption(nil), s.options...)
}

func (s *fakeModelSwitchService) Select(_ context.Context, model string) (llm.ModelSelection, error) {
	if s.selectErr != nil {
		return llm.ModelSelection{}, s.selectErr
	}
	s.selected = model
	s.selection.Model = model
	s.selection.UpdatedAt = time.Date(2026, 8, 22, 22, 0, 0, 0, time.UTC)
	return s.selection, nil
}

func TestModelAPIViewerStatusAndAdminMutation(t *testing.T) {
	svc := &fakeModelSwitchService{
		selection: llm.ModelSelection{Model: "glm-5", UpdatedAt: time.Date(2026, 8, 22, 21, 0, 0, 0, time.UTC)},
		options:   []llm.ModelOption{{ID: "glm-5"}, {ID: "deepseek-v4-pro", ThinkingEnabled: true}},
	}
	api := NewModelAPI(svc, testAuth(t))

	public := httptest.NewRecorder()
	api.ServeHTTP(public, withBearer(httptest.NewRequest(http.MethodGet, "/api/v1/control-room/model", nil), testViewerToken))
	if public.Code != http.StatusOK {
		t.Fatalf("public status = %d body=%s", public.Code, public.Body.String())
	}
	var state ModelStateDTO
	if err := json.Unmarshal(public.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.CurrentModel != "glm-5" || len(state.Models) != 2 {
		t.Fatalf("public state = %+v", state)
	}

	unauthorized := httptest.NewRecorder()
	api.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPut, "/api/v1/admin/model", strings.NewReader(`{"model":"deepseek-v4-pro"}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized mutation = %d", unauthorized.Code)
	}
	// The shared automation token and non-admin operators cannot switch the model.
	for _, token := range []string{testMachineToken, testOperatorToken} {
		forbidden := httptest.NewRecorder()
		api.ServeHTTP(forbidden, withBearer(httptest.NewRequest(http.MethodPut, "/api/v1/admin/model", strings.NewReader(`{"model":"deepseek-v4-pro"}`)), token))
		if forbidden.Code != http.StatusForbidden || svc.selected != "" {
			t.Fatalf("token %.8s mutation = %d", token, forbidden.Code)
		}
	}

	change := httptest.NewRequest(http.MethodPut, "/api/v1/admin/model", strings.NewReader(`{"model":"deepseek-v4-pro"}`))
	change.Header.Set("Authorization", "Bearer "+testAdminToken)
	change.Header.Set("Content-Type", "application/json")
	changed := httptest.NewRecorder()
	api.ServeHTTP(changed, change)
	if changed.Code != http.StatusOK || svc.selected != "deepseek-v4-pro" {
		t.Fatalf("change = %d selected=%q body=%s", changed.Code, svc.selected, changed.Body.String())
	}
}

func TestModelAPIRejectsUnallowedModelBeforeSuccess(t *testing.T) {
	svc := &fakeModelSwitchService{selection: llm.ModelSelection{Model: "glm-5"}, selectErr: llm.ErrModelNotAllowed}
	api := NewModelAPI(svc, testAuth(t))
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/model", strings.NewReader(`{"model":"blocked"}`))
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("unallowed model = %d body=%s", resp.Code, resp.Body.String())
	}

	svc.selectErr = errors.New("mysql unavailable")
	req = httptest.NewRequest(http.MethodPut, "/api/v1/admin/model", strings.NewReader(`{"model":"blocked"}`))
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	req.Header.Set("Content-Type", "application/json")
	resp = httptest.NewRecorder()
	api.ServeHTTP(resp, req)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("store error = %d body=%s", resp.Code, resp.Body.String())
	}
}
