package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"oncall-agent/internal/store"
)

type admissionActions struct{ err error }

func (a admissionActions) Rediagnose(context.Context, uint64, Actor) (store.AgentRun, error) {
	return store.AgentRun{}, a.err
}
func (a admissionActions) RequestEvidence(context.Context, uint64, Actor, string) error { return nil }

func TestManualRunAdmissionErrorsSharedAcrossRoutes(t *testing.T) {
	for _, test := range []struct {
		name        string
		err         error
		status      int
		code, retry string
	}{
		{"active", fmt.Errorf("wrapped: %w", &store.RunAdmissionError{Code: "active_processing", RunID: 12, ApprovalID: 7}), 409, "active_processing", ""},
		{"not firing", &store.RunAdmissionError{Code: "incident_not_firing"}, 409, "incident_not_firing", ""},
		{"cooldown", &store.RunAdmissionError{Code: "cooldown", RetryAfterSeconds: 38}, 429, "cooldown", "38"},
		{"budget", &store.RunAdmissionError{Code: "retry_budget"}, 409, "retry_budget", ""},
		{"chain", &store.RunAdmissionError{Code: "retry_chain"}, 409, "retry_chain", ""},
		{"missing", store.ErrIncidentNotFound, 404, "", ""},
		{"database", errors.New("password=secret database unavailable"), 503, "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			legacy := NewIncidentAPI(&fakeIncidentStore{incident: store.Incident{ID: 11, Status: "firing"}, requestErr: test.err}, testAuth(t), nil)
			console := NewConversationAPI(nil, testAuth(t), admissionActions{err: test.err})
			for _, route := range []struct {
				path    string
				handler http.Handler
			}{{"/api/v1/incidents/11/diagnose", legacy}, {"/api/v1/incidents/11/rediagnose", console}} {
				request := httptest.NewRequest(http.MethodPost, route.path, nil)
				request.Header.Set("Authorization", "Bearer "+testOperatorToken)
				response := httptest.NewRecorder()
				route.handler.ServeHTTP(response, request)
				if response.Code != test.status || response.Header().Get("Retry-After") != test.retry {
					t.Fatalf("%s: status=%d retry=%q body=%s", route.path, response.Code, response.Header().Get("Retry-After"), response.Body.String())
				}
				var body map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if test.code != "" && body["code"] != test.code {
					t.Fatalf("missing admission code: %s", response.Body.String())
				}
				if test.code == "active_processing" && (body["run_id"] != float64(12) || body["approval_id"] != float64(7)) {
					t.Fatalf("missing conflict references: %s", response.Body.String())
				}
				if strings.Contains(response.Body.String(), "secret") {
					t.Fatalf("raw DB error leaked: %s", response.Body.String())
				}
			}
		})
	}
}
