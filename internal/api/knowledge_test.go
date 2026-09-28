package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"oncall-agent/internal/knowledge"
)

type fakeSkillCounts struct {
	counts map[string]int
	err    error
	since  time.Time
}

func (f *fakeSkillCounts) CountSkillActivations(_ context.Context, since time.Time) (map[string]int, error) {
	f.since = since
	return f.counts, f.err
}

func skillsRequest(t *testing.T, counts *fakeSkillCounts, method, token string) *httptest.ResponseRecorder {
	t.Helper()
	skills, err := knowledge.LoadSkills()
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	NewKnowledgeAPI(skills, counts, testAuth(t)).ServeHTTP(resp, withBearer(httptest.NewRequest(method, "/api/v1/skills", nil), token))
	return resp
}

func TestSkillsListsCatalogWithActivations(t *testing.T) {
	counts := &fakeSkillCounts{counts: map[string]int{"container_exit_oom": 3}}
	resp := skillsRequest(t, counts, http.MethodGet, testViewerToken)
	if resp.Code != http.StatusOK || time.Since(counts.since) < 30*24*time.Hour-time.Minute {
		t.Fatalf("status = %d since=%s body=%s", resp.Code, counts.since, resp.Body)
	}
	var body struct {
		Skills []struct {
			Name        string   `json:"name"`
			Alerts      []string `json:"alerts"`
			SHA256      string   `json:"sha256"`
			Body        string   `json:"body"`
			Activations int      `json:"activations_30d"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil || len(body.Skills) != 6 {
		t.Fatalf("body = %s (%v)", resp.Body, err)
	}
	for _, skill := range body.Skills {
		want := map[string]int{"container_exit_oom": 3}[skill.Name]
		if skill.Activations != want || len(skill.Alerts) == 0 || len(skill.SHA256) != 64 || skill.Body == "" {
			t.Fatalf("skill = %+v", skill)
		}
	}
}

func TestSkillsRejectsBadRequests(t *testing.T) {
	for name, test := range map[string]struct {
		counts *fakeSkillCounts
		method string
		token  string
		want   int
	}{
		"machine token":     {&fakeSkillCounts{}, http.MethodGet, testMachineToken, http.StatusForbidden},
		"anonymous":         {&fakeSkillCounts{}, http.MethodGet, "", http.StatusUnauthorized},
		"write":             {&fakeSkillCounts{}, http.MethodPost, testViewerToken, http.StatusMethodNotAllowed},
		"store unavailable": {&fakeSkillCounts{err: errors.New("db down")}, http.MethodGet, testViewerToken, http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			if resp := skillsRequest(t, test.counts, test.method, test.token); resp.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", resp.Code, test.want, resp.Body)
			}
		})
	}
}
