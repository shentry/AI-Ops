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

	"oncall-agent/internal/config"
	"oncall-agent/internal/store"
)

type fakeAuthority struct{ maintenance string }

func (fakeAuthority) Service() string { return "sub2api" }
func (fakeAuthority) Env() string     { return "prod" }
func (fakeAuthority) Release() string { return "r1@abcdefabcdef" }
func (fakeAuthority) Rules() []config.RuleConfig {
	return []config.RuleConfig{
		{ID: "restart", Action: "docker_restart", Mode: "auto", Alerts: []string{"Sub2APIDown"}, MaxExecutions: 2, WindowMinutes: 60},
		{ID: "rollback", Action: "deployment_rollback", Mode: "observe", Alerts: []string{"Sub2APIBusinessErrors"}, MaxExecutions: 1, WindowMinutes: 120},
	}
}
func (a fakeAuthority) Policy(time.Time) store.RemediationPolicy {
	return store.RemediationPolicy{Maintenance: a.maintenance}
}

type fakeRemediationStore struct {
	states     map[string]store.RemediationState
	controls   []store.ControlEvent
	reviews    []store.Review
	changes    []store.ChangeEvent
	reportArgs []string
	reportDays time.Duration
	err        error
}

func (f *fakeRemediationStore) RemediationState(_ context.Context, q store.RemediationQuery) (store.RemediationState, error) {
	return f.states[q.RuleID+q.Service], f.err
}
func (f *fakeRemediationStore) AppendControlEvent(_ context.Context, e store.ControlEvent) (store.ControlEvent, error) {
	e.ID = uint64(len(f.controls) + 1)
	f.controls = append(f.controls, e)
	return e, f.err
}
func (f *fakeRemediationStore) ListControlEvents(context.Context, int) ([]store.ControlEvent, error) {
	return f.controls, f.err
}
func (f *fakeRemediationStore) RemediationReport(_ context.Context, since, until time.Time, autoAlerts []string) (store.Report, error) {
	f.reportArgs, f.reportDays = autoAlerts, until.Sub(since)
	return store.Report{Incidents: 3, ReviewQueue: []store.ReviewItem{}}, f.err
}
func (f *fakeRemediationStore) AddReview(_ context.Context, r store.Review) (store.Review, error) {
	if r.Subject != "action" && r.Subject != "diagnosis" {
		return store.Review{}, errors.New("store: review needs incident, subject, verdict, reviewer and time")
	}
	r.ID = 9
	f.reviews = append(f.reviews, r)
	return r, nil
}
func (f *fakeRemediationStore) ListReviews(context.Context, uint64) ([]store.Review, error) {
	return f.reviews, f.err
}
func (f *fakeRemediationStore) RecordChange(_ context.Context, c store.ChangeEvent) (store.ChangeEvent, bool, error) {
	c.ID = 5
	f.changes = append(f.changes, c)
	return c, true, nil
}
func (f *fakeRemediationStore) ListChanges(context.Context, string, time.Time, int) ([]store.ChangeEvent, error) {
	return f.changes, f.err
}
func (f *fakeRemediationStore) MarkReleaseVerified(_ context.Context, id uint64, at time.Time) (store.ChangeEvent, error) {
	if id != 5 {
		return store.ChangeEvent{}, store.ErrChangeNotFound
	}
	return store.ChangeEvent{ID: 5, ChangeType: "release", VerifiedAt: &at}, nil
}

func remediationRequest(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req = withBearer(req, token)
	}
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	return resp
}

func TestRemediationStateShowsRulesStopAndBlocks(t *testing.T) {
	db := &fakeRemediationStore{states: map[string]store.RemediationState{
		"sub2api": {Stopped: true, StopReason: "drill", BusyWith: 7},
		"restart": {Executions: 1, Blocked: "verify.failed on approval 3"},
	}}
	h := NewRemediationAPI(db, fakeAuthority{maintenance: "db upgrade"}, testAuth(t))
	resp := remediationRequest(t, h, http.MethodGet, "/api/v1/remediation", testMachineToken, "")
	var body RemediationDTO
	if resp.Code != http.StatusOK || json.Unmarshal(resp.Body.Bytes(), &body) != nil {
		t.Fatalf("response = %d %s", resp.Code, resp.Body.String())
	}
	if !body.EmergencyStop || body.StopReason != "drill" || body.BusyWith != 7 || body.Maintenance != "db upgrade" || body.RulesVersion != "r1@abcdefabcdef" || len(body.Rules) != 2 {
		t.Fatalf("state = %+v", body)
	}
	if rule := body.Rules[0]; rule.ID != "restart" || rule.Executions != 1 || !strings.Contains(rule.Blocked, "verify.failed") {
		t.Fatalf("rule = %+v", rule)
	}
}

// Emergency stop, resume and rule reset are admin decisions with a reason.
func TestRemediationControlRequiresAdminAndReason(t *testing.T) {
	db := &fakeRemediationStore{}
	h := NewRemediationAPI(db, fakeAuthority{}, testAuth(t))
	for _, tc := range []struct {
		path, token, body string
		want              int
	}{
		{"/api/v1/remediation/stop", testOperatorToken, `{"reason":"x"}`, http.StatusForbidden},
		{"/api/v1/remediation/stop", testMachineToken, `{"reason":"x"}`, http.StatusForbidden},
		{"/api/v1/remediation/stop", testAdminToken, `{"reason":" "}`, http.StatusBadRequest},
		{"/api/v1/remediation/rules/unknown/reset", testAdminToken, `{"reason":"x"}`, http.StatusNotFound},
		{"/api/v1/remediation/stop", testAdminToken, `{"reason":"bad deploy in progress"}`, http.StatusOK},
		{"/api/v1/remediation/resume", testAdminToken, `{"reason":"deploy rolled back"}`, http.StatusOK},
		{"/api/v1/remediation/rules/restart/reset", testAdminToken, `{"reason":"upstream replaced"}`, http.StatusOK},
	} {
		if resp := remediationRequest(t, h, http.MethodPost, tc.path, tc.token, tc.body); resp.Code != tc.want {
			t.Fatalf("%s as %s = %d %s, want %d", tc.path, tc.token[:1], resp.Code, resp.Body.String(), tc.want)
		}
	}
	if len(db.controls) != 3 || db.controls[0].Kind != store.ControlEmergencyStop || db.controls[0].Actor != "admin" ||
		db.controls[1].Kind != store.ControlEmergencyResume || db.controls[2].Kind != store.ControlRuleReset || *db.controls[2].RuleID != "restart" {
		t.Fatalf("controls = %+v", db.controls)
	}
}

func TestRemediationReportUsesAutoRulesForScope(t *testing.T) {
	db := &fakeRemediationStore{}
	h := NewRemediationAPI(db, fakeAuthority{}, testAuth(t))
	if resp := remediationRequest(t, h, http.MethodGet, "/api/v1/remediation/report?days=91", testViewerToken, ""); resp.Code != http.StatusBadRequest {
		t.Fatalf("days=91 = %d", resp.Code)
	}
	resp := remediationRequest(t, h, http.MethodGet, "/api/v1/remediation/report?days=7", testViewerToken, "")
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"incidents":3`) {
		t.Fatalf("report = %d %s", resp.Code, resp.Body.String())
	}
	if len(db.reportArgs) != 1 || db.reportArgs[0] != "Sub2APIDown" || db.reportDays != 7*24*time.Hour {
		t.Fatalf("report scope=%v window=%s; observe rules are not in scope", db.reportArgs, db.reportDays)
	}
}

// The reviewer is the authenticated operator, never a field of the request.
func TestReviewsRecordAuthenticatedReviewer(t *testing.T) {
	db := &fakeRemediationStore{}
	h := NewRemediationAPI(db, fakeAuthority{}, testAuth(t))
	body := `{"subject":"action","verdict":"wrong","approval_id":3,"root_cause":"db down","actual_fix":"restart postgres","manual_minutes":20,"reviewer":"someone-else"}`
	if resp := remediationRequest(t, h, http.MethodPost, "/api/v1/incidents/11/reviews", testViewerToken, body); resp.Code != http.StatusForbidden {
		t.Fatalf("viewer review = %d", resp.Code)
	}
	resp := remediationRequest(t, h, http.MethodPost, "/api/v1/incidents/11/reviews", testOperatorToken, body)
	if resp.Code != http.StatusCreated || len(db.reviews) != 1 || db.reviews[0].Reviewer != "ops" || db.reviews[0].IncidentID != 11 || *db.reviews[0].ApprovalID != 3 {
		t.Fatalf("review = %d %s %+v", resp.Code, resp.Body.String(), db.reviews)
	}
	if resp := remediationRequest(t, h, http.MethodPost, "/api/v1/incidents/11/reviews", testOperatorToken, `{"subject":"other"}`); resp.Code != http.StatusBadRequest {
		t.Fatalf("invalid review = %d", resp.Code)
	}
	if resp := remediationRequest(t, h, http.MethodGet, "/api/v1/incidents/11/reviews", testViewerToken, ""); resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"reviewer":"ops"`) {
		t.Fatalf("list = %d %s", resp.Code, resp.Body.String())
	}
}

// CI may report releases; only a person may verify one, since verified
// releases are what a rollback may return to.
func TestChangesRecordReleasesAndPersonVerifies(t *testing.T) {
	db := &fakeRemediationStore{}
	h := NewRemediationAPI(db, fakeAuthority{}, testAuth(t))
	release := `{"change_type":"release","release_id":"v2","image_ref":"repo@sha256:` + strings.Repeat("a", 64) + `","db_migration":"none","occurred_at":"2026-09-24T00:00:00Z","idempotency_key":"pipeline-42"}`
	resp := remediationRequest(t, h, http.MethodPost, "/api/v1/changes", testMachineToken, release)
	if resp.Code != http.StatusCreated || db.changes[0].Source != "ci" || db.changes[0].Actor != "machine" || db.changes[0].Service != "sub2api" || db.changes[0].Env != "prod" {
		t.Fatalf("release = %d %s %+v", resp.Code, resp.Body.String(), db.changes)
	}
	if resp := remediationRequest(t, h, http.MethodPost, "/api/v1/changes", testOperatorToken, strings.Replace(release, `"release"`, `"rollback"`, 1)); resp.Code != http.StatusBadRequest {
		t.Fatalf("reported rollback = %d", resp.Code)
	}
	if resp := remediationRequest(t, h, http.MethodPost, "/api/v1/changes/5/verify", testMachineToken, ""); resp.Code != http.StatusForbidden {
		t.Fatalf("machine verification = %d", resp.Code)
	}
	if resp := remediationRequest(t, h, http.MethodPost, "/api/v1/changes/6/verify", testOperatorToken, ""); resp.Code != http.StatusNotFound {
		t.Fatalf("missing change = %d", resp.Code)
	}
	if resp := remediationRequest(t, h, http.MethodPost, "/api/v1/changes/5/verify", testOperatorToken, ""); resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "verified_at") {
		t.Fatalf("verification = %d %s", resp.Code, resp.Body.String())
	}
	if resp := remediationRequest(t, h, http.MethodGet, "/api/v1/changes", testViewerToken, ""); resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"release_id":"v2"`) {
		t.Fatalf("list = %d %s", resp.Code, resp.Body.String())
	}
}
