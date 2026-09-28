package knowledge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"

	"oncall-agent/internal/store"
)

type fakeIncidentStore struct {
	reviews []store.Review
	saved   *store.KnowledgeEntry
}

func (f *fakeIncidentStore) GetIncident(context.Context, uint64) (store.Incident, error) {
	return store.Incident{ID: 3, GroupKey: "sub2api", Title: "Sub2API business errors", StartedAt: time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC)}, nil
}
func (f *fakeIncidentStore) ListIncidentAlerts(context.Context, uint64) ([]store.Alert, error) {
	return []store.Alert{{Name: "Sub2APIBusinessErrors"}}, nil
}
func (f *fakeIncidentStore) ListReviews(context.Context, uint64) ([]store.Review, error) {
	return f.reviews, nil
}
func (f *fakeIncidentStore) GetAgentRun(context.Context, uint64) (store.AgentRun, error) {
	rca, plan := "模型判断：数据库宕机", datatypes.JSON(`{"action":"docker_restart","target":{"kind":"container","name":"sub2api"}}`)
	return store.AgentRun{ID: 9, RCAText: &rca, PlanJSON: &plan}, nil
}
func (f *fakeIncidentStore) GetApproval(context.Context, uint64) (store.Approval, error) {
	return store.Approval{ID: 4, ToolName: "docker_restart", Status: "executed", Verification: &store.VerifyTask{Status: "failed"}}, nil
}
func (f *fakeIncidentStore) SaveIncidentKnowledge(_ context.Context, _ uint64, entry store.KnowledgeEntry) (store.KnowledgeEntry, error) {
	f.saved = &entry
	return entry, nil
}

func TestAddIncidentUsesTheLatestConfirmedReview(t *testing.T) {
	run, approval := uint64(9), uint64(4)
	db := &fakeIncidentStore{reviews: []store.Review{
		{Verdict: "unknown", RootCause: "还不清楚", Reviewer: "a"},
		{Verdict: "wrong", RootCause: "", Reviewer: "b"}, // a correction without a cause is not knowledge
		{Verdict: "wrong", RootCause: "应用账号密码轮换未同步 password=hunter2", ActualFix: "人工更新凭据", RunID: &run, ApprovalID: &approval, Reviewer: "ops"},
	}}
	entry, err := AddIncident(context.Background(), db, 3, "ops", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Sub2APIBusinessErrors", "诊断错误，已人工修正", "## 根因\n应用账号密码轮换未同步", "## 实际处置\n人工更新凭据",
		"## 当时的诊断\n模型判断：数据库宕机", "docker_restart container/sub2api", "恢复验证 failed", "/incidents/3"} {
		if !strings.Contains(entry.Body, want) {
			t.Fatalf("body lacks %q:\n%s", want, entry.Body)
		}
	}
	if strings.Contains(entry.Body, "hunter2") || entry.Title != "Incident #3 复盘：Sub2API business errors" || entry.CreatedBy != "ops" || len(entry.SHA256) != 64 {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestAddIncidentCorrectVerdictFallsBackToTheDiagnosis(t *testing.T) {
	run := uint64(9)
	db := &fakeIncidentStore{reviews: []store.Review{{Verdict: "correct", RunID: &run, Reviewer: "ops"}}}
	entry, err := AddIncident(context.Background(), db, 3, "ops", time.Now())
	if err != nil || !strings.Contains(entry.Body, "## 根因\n模型判断：数据库宕机") || strings.Contains(entry.Body, "## 当时的诊断") {
		t.Fatalf("entry = %+v %v", entry, err)
	}
}

func TestAddIncidentRejectsUnconfirmedReviews(t *testing.T) {
	for name, reviews := range map[string][]store.Review{
		"no review":             nil,
		"unknown":               {{Verdict: "unknown", RootCause: "猜测"}},
		"partial without cause": {{Verdict: "partial"}},
		"correct without a run": {{Verdict: "correct"}},
	} {
		db := &fakeIncidentStore{reviews: reviews}
		if _, err := AddIncident(context.Background(), db, 3, "ops", time.Now()); !errors.Is(err, ErrNotReviewed) || db.saved != nil {
			t.Errorf("%s: err = %v saved = %v", name, err, db.saved)
		}
	}
}
