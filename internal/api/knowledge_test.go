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
	"oncall-agent/internal/store"
)

type fakeKnowledgeStore struct {
	counts   map[string]int
	countErr error
	since    time.Time
	entries  []store.KnowledgeEntry
	query    string
	source   string
	reviews  []store.Review
	saved    []store.KnowledgeEntry
	deleted  []uint64
}

func (f *fakeKnowledgeStore) CountSkillActivations(_ context.Context, since time.Time) (map[string]int, error) {
	f.since = since
	return f.counts, f.countErr
}

func (f *fakeKnowledgeStore) SearchKnowledge(_ context.Context, query, source string, _ int) ([]store.KnowledgeHit, error) {
	f.query, f.source = query, source
	hits := []store.KnowledgeHit{}
	for _, entry := range f.entries {
		hits = append(hits, store.KnowledgeHit{KnowledgeEntry: entry, Score: 1})
	}
	return hits, nil
}

func (f *fakeKnowledgeStore) ListKnowledge(_ context.Context, source string) ([]store.KnowledgeEntry, error) {
	f.source = source
	return f.entries, nil
}

func (f *fakeKnowledgeStore) GetKnowledge(_ context.Context, id uint64) (store.KnowledgeEntry, error) {
	for _, entry := range f.entries {
		if entry.ID == id {
			return entry, nil
		}
	}
	return store.KnowledgeEntry{}, store.ErrKnowledgeNotFound
}

func (f *fakeKnowledgeStore) DeleteIncidentKnowledge(_ context.Context, id uint64, _ string, _ time.Time) error {
	entry, err := f.GetKnowledge(context.Background(), id)
	if err != nil {
		return err
	}
	if entry.Source != store.KnowledgeIncident {
		return store.ErrKnowledgeReadOnly
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeKnowledgeStore) GetIncident(_ context.Context, id uint64) (store.Incident, error) {
	if id != 3 {
		return store.Incident{}, store.ErrIncidentNotFound
	}
	return store.Incident{ID: 3, Title: "t", StartedAt: time.Unix(0, 0)}, nil
}

func (f *fakeKnowledgeStore) ListIncidentAlerts(context.Context, uint64) ([]store.Alert, error) {
	return nil, nil
}

func (f *fakeKnowledgeStore) ListReviews(context.Context, uint64) ([]store.Review, error) {
	return f.reviews, nil
}

func (f *fakeKnowledgeStore) GetAgentRun(context.Context, uint64) (store.AgentRun, error) {
	return store.AgentRun{}, errors.New("unused")
}

func (f *fakeKnowledgeStore) GetApproval(context.Context, uint64) (store.Approval, error) {
	return store.Approval{}, errors.New("unused")
}

func (f *fakeKnowledgeStore) SaveIncidentKnowledge(_ context.Context, _ uint64, entry store.KnowledgeEntry) (store.KnowledgeEntry, error) {
	entry.ID, entry.Source = 99, store.KnowledgeIncident
	f.saved = append(f.saved, entry)
	return entry, nil
}

func knowledgeRequest(t *testing.T, db *fakeKnowledgeStore, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	skills, err := knowledge.LoadSkills()
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	NewKnowledgeAPI(skills, db, testAuth(t)).ServeHTTP(resp, withBearer(httptest.NewRequest(method, target, nil), token))
	return resp
}

func TestSkillsListsCatalogWithActivations(t *testing.T) {
	db := &fakeKnowledgeStore{counts: map[string]int{"container_exit_oom": 3}}
	resp := knowledgeRequest(t, db, http.MethodGet, "/api/v1/skills", testViewerToken)
	if resp.Code != http.StatusOK || time.Since(db.since) < 30*24*time.Hour-time.Minute {
		t.Fatalf("status = %d since=%s body=%s", resp.Code, db.since, resp.Body)
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

func TestKnowledgeSearchListsOrMatches(t *testing.T) {
	db := &fakeKnowledgeStore{entries: []store.KnowledgeEntry{{ID: 1, Source: "repo", Ref: "a.md#b", Title: "A / B", Body: "应用账号密码错误 28P01"}}}
	var body struct {
		Entries []KnowledgeEntryDTO `json:"entries"`
	}
	resp := knowledgeRequest(t, db, http.MethodGet, "/api/v1/knowledge?source=repo", testViewerToken)
	if json.Unmarshal(resp.Body.Bytes(), &body); resp.Code != http.StatusOK || len(body.Entries) != 1 || body.Entries[0].Body != "" || body.Entries[0].Snippet != "" || db.source != "repo" {
		t.Fatalf("list = %d %s", resp.Code, resp.Body)
	}
	resp = knowledgeRequest(t, db, http.MethodGet, "/api/v1/knowledge?q=28P01", testViewerToken)
	if json.Unmarshal(resp.Body.Bytes(), &body); resp.Code != http.StatusOK || db.query != "28P01" || body.Entries[0].Snippet == "" || body.Entries[0].Body != "" {
		t.Fatalf("search = %d %s", resp.Code, resp.Body)
	}
	resp = knowledgeRequest(t, db, http.MethodGet, "/api/v1/knowledge/1", testViewerToken)
	var entry KnowledgeEntryDTO
	if json.Unmarshal(resp.Body.Bytes(), &entry); resp.Code != http.StatusOK || entry.Body == "" {
		t.Fatalf("read = %d %s", resp.Code, resp.Body)
	}
}

func TestKnowledgeWritesNeedTheirRoleAndAReview(t *testing.T) {
	db := &fakeKnowledgeStore{entries: []store.KnowledgeEntry{{ID: 1, Source: "repo"}, {ID: 2, Source: "incident", Ref: "incident/3"}}}
	if resp := knowledgeRequest(t, db, http.MethodPost, "/api/v1/incidents/3/knowledge", testOperatorToken); resp.Code != http.StatusConflict {
		t.Fatalf("unreviewed incident = %d %s", resp.Code, resp.Body)
	}
	db.reviews = []store.Review{{Verdict: "partial", RootCause: "凭据轮换未同步", Reviewer: "ops"}}
	if resp := knowledgeRequest(t, db, http.MethodPost, "/api/v1/incidents/3/knowledge", testViewerToken); resp.Code != http.StatusForbidden {
		t.Fatalf("viewer add = %d", resp.Code)
	}
	resp := knowledgeRequest(t, db, http.MethodPost, "/api/v1/incidents/3/knowledge", testOperatorToken)
	if resp.Code != http.StatusCreated || len(db.saved) != 1 || db.saved[0].CreatedBy != "ops" {
		t.Fatalf("add = %d %s saved=%+v", resp.Code, resp.Body, db.saved)
	}
	if resp := knowledgeRequest(t, db, http.MethodDelete, "/api/v1/knowledge/2", testOperatorToken); resp.Code != http.StatusForbidden {
		t.Fatalf("operator delete = %d", resp.Code)
	}
	if resp := knowledgeRequest(t, db, http.MethodDelete, "/api/v1/knowledge/1", testAdminToken); resp.Code != http.StatusConflict {
		t.Fatalf("repository delete = %d", resp.Code)
	}
	if resp := knowledgeRequest(t, db, http.MethodDelete, "/api/v1/knowledge/2", testAdminToken); resp.Code != http.StatusNoContent || len(db.deleted) != 1 {
		t.Fatalf("admin delete = %d deleted=%v", resp.Code, db.deleted)
	}
}

func TestKnowledgeRejectsBadRequests(t *testing.T) {
	for name, test := range map[string]struct {
		db     *fakeKnowledgeStore
		method string
		target string
		token  string
		want   int
	}{
		"machine token":        {&fakeKnowledgeStore{}, http.MethodGet, "/api/v1/skills", testMachineToken, http.StatusForbidden},
		"anonymous":            {&fakeKnowledgeStore{}, http.MethodGet, "/api/v1/knowledge", "", http.StatusUnauthorized},
		"skills write":         {&fakeKnowledgeStore{}, http.MethodPost, "/api/v1/skills", testViewerToken, http.StatusMethodNotAllowed},
		"store unavailable":    {&fakeKnowledgeStore{countErr: errors.New("db down")}, http.MethodGet, "/api/v1/skills", testViewerToken, http.StatusServiceUnavailable},
		"bad source":           {&fakeKnowledgeStore{}, http.MethodGet, "/api/v1/knowledge?source=web", testViewerToken, http.StatusBadRequest},
		"bad id":               {&fakeKnowledgeStore{}, http.MethodGet, "/api/v1/knowledge/abc", testViewerToken, http.StatusBadRequest},
		"missing entry":        {&fakeKnowledgeStore{}, http.MethodGet, "/api/v1/knowledge/5", testViewerToken, http.StatusNotFound},
		"missing incident":     {&fakeKnowledgeStore{}, http.MethodPost, "/api/v1/incidents/8/knowledge", testOperatorToken, http.StatusNotFound},
		"machine incident add": {&fakeKnowledgeStore{}, http.MethodPost, "/api/v1/incidents/3/knowledge", testMachineToken, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			if resp := knowledgeRequest(t, test.db, test.method, test.target, test.token); resp.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", resp.Code, test.want, resp.Body)
			}
		})
	}
}
