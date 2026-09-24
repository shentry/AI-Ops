package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"oncall-agent/internal/store"
)

type fakeIncidentStore struct {
	listStatus  string
	incidents   []store.Incident
	incident    store.Incident
	getErr      error
	members     []store.IncidentMember
	createdRun  store.AgentRun
	createCalls int
	request     store.RunRequest
	requestErr  error
}

func (f *fakeIncidentStore) ListIncidents(_ context.Context, status string) ([]store.Incident, error) {
	f.listStatus = status
	return f.incidents, nil
}

func (f *fakeIncidentStore) GetIncident(_ context.Context, id uint64) (store.Incident, error) {
	if f.getErr != nil {
		return store.Incident{}, f.getErr
	}
	if f.incident.ID != id {
		return store.Incident{}, store.ErrIncidentNotFound
	}
	return f.incident, nil
}

func (f *fakeIncidentStore) ListIncidentMembers(_ context.Context, incidentID uint64) ([]store.IncidentMember, error) {
	if f.incident.ID != incidentID {
		return nil, store.ErrIncidentNotFound
	}
	return f.members, nil
}

func (f *fakeIncidentStore) RequestRun(_ context.Context, request store.RunRequest) (store.AgentRun, bool, error) {
	f.createCalls++
	f.request = request
	if f.requestErr != nil {
		return store.AgentRun{}, false, f.requestErr
	}
	run := store.AgentRun{ID: 100 + uint64(f.createCalls), IncidentID: request.IncidentID, Mode: request.Mode, Status: "pending", StartedAt: request.RequestedAt}
	f.createdRun = run
	return run, true, nil
}

func incidentRequest(t *testing.T, api *IncidentAPI, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, req)
	return response
}

func TestIncidentAPIRequiresAuth(t *testing.T) {
	api := NewIncidentAPI(&fakeIncidentStore{}, "secret", nil)
	for _, path := range []string{"/api/v1/incidents", "/api/v1/incidents/1", "/api/v1/incidents/1/diagnose"} {
		response := incidentRequest(t, api, http.MethodGet, path, "Bearer wrong")
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", path, response.Code)
		}
	}
}

func TestIncidentAPIListFiltersByStatus(t *testing.T) {
	fake := &fakeIncidentStore{incidents: []store.Incident{{ID: 2, Status: "firing"}, {ID: 1, Status: "firing"}}}
	api := NewIncidentAPI(fake, "secret", nil)
	response := incidentRequest(t, api, http.MethodGet, "/api/v1/incidents?status=firing", "Bearer secret")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if fake.listStatus != "firing" {
		t.Fatalf("list status = %q, want firing", fake.listStatus)
	}
	var body struct {
		Incidents []store.Incident `json:"incidents"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Incidents) != 2 || body.Incidents[0].ID != 2 || body.Incidents[1].ID != 1 {
		t.Fatalf("incidents = %#v", body.Incidents)
	}
}

func TestIncidentAPIGetReturnsMembers(t *testing.T) {
	fake := &fakeIncidentStore{
		incident: store.Incident{ID: 7, Status: "firing", Severity: 5},
		members:  []store.IncidentMember{{Fingerprint: "fp", Name: "HighCPU", Status: "firing", Severity: 5}},
	}
	api := NewIncidentAPI(fake, "secret", nil)
	response := incidentRequest(t, api, http.MethodGet, "/api/v1/incidents/7", "Bearer secret")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	var body struct {
		Incident store.Incident         `json:"incident"`
		Members  []store.IncidentMember `json:"members"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Incident.ID != 7 || len(body.Members) != 1 || body.Members[0].Fingerprint != "fp" {
		t.Fatalf("body = %#v", body)
	}
}

func TestIncidentAPIGetMissingReturns404(t *testing.T) {
	api := NewIncidentAPI(&fakeIncidentStore{}, "secret", nil)
	response := incidentRequest(t, api, http.MethodGet, "/api/v1/incidents/404", "Bearer secret")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if response := incidentRequest(t, api, http.MethodGet, "/api/v1/incidents/abc", "Bearer secret"); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d, want 400", response.Code)
	}
}

func TestIncidentAPIDiagnoseCreatesPendingRun(t *testing.T) {
	route := map[string]string{"critical": "full", "info": "skip"}
	fake := &fakeIncidentStore{incident: store.Incident{ID: 7, Status: "firing", Severity: 5}}
	api := NewIncidentAPI(fake, "secret", route)
	response := incidentRequest(t, api, http.MethodPost, "/api/v1/incidents/7/diagnose", "Bearer secret")
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", response.Code)
	}
	if fake.request.Trigger != store.RunTriggerManual || fake.request.RequestedAt.IsZero() || fake.request.RetryOf != nil {
		t.Fatalf("manual admission request = %#v", fake.request)
	}
	// 手动重诊：pending、retry_of 为空、模式按 severity 路由。
	if fake.createdRun.IncidentID != 7 || fake.createdRun.Mode != "full" || fake.createdRun.Status != "pending" || fake.createdRun.RetryOf != nil {
		t.Fatalf("created run = %#v", fake.createdRun)
	}

	// 手动重诊覆盖 skip：人显式要求诊断时降级为 light，不落 skip。
	fakeSkip := &fakeIncidentStore{incident: store.Incident{ID: 8, Status: "firing", Severity: 2}}
	apiSkip := NewIncidentAPI(fakeSkip, "secret", route)
	response = incidentRequest(t, apiSkip, http.MethodPost, "/api/v1/incidents/8/diagnose", "Bearer secret")
	if response.Code != http.StatusCreated || fakeSkip.createdRun.Mode != "light" || fakeSkip.createdRun.Status != "pending" {
		t.Fatalf("skip override status = %d, run = %#v", response.Code, fakeSkip.createdRun)
	}

	missing := NewIncidentAPI(&fakeIncidentStore{}, "secret", route)
	if response := incidentRequest(t, missing, http.MethodPost, "/api/v1/incidents/9/diagnose", "Bearer secret"); response.Code != http.StatusNotFound {
		t.Fatalf("diagnose missing status = %d, want 404", response.Code)
	}
}

func TestIncidentAPIRejectsWrongMethod(t *testing.T) {
	api := NewIncidentAPI(&fakeIncidentStore{}, "secret", nil)
	if response := incidentRequest(t, api, http.MethodDelete, "/api/v1/incidents/1", "Bearer secret"); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.Code)
	}
	// 查询 API 只读：GET 列表不能落 run。
	fake := &fakeIncidentStore{}
	api = NewIncidentAPI(fake, "secret", nil)
	incidentRequest(t, api, http.MethodGet, "/api/v1/incidents", "Bearer secret")
	if fake.createCalls != 0 {
		t.Fatalf("read-only list created %d runs", fake.createCalls)
	}
}

// 防止 fake 与接口漂移的编译期断言。
var _ incidentStore = (*fakeIncidentStore)(nil)
