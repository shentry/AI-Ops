package store

import (
	"context"
	"testing"
	"time"
)

func TestBuildReportDefinitions(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	rule, web := "rule", "web"
	auto, person := "system:rule:restart", "ops"
	id := func(v uint64) *uint64 { return &v }
	approval := func(approvalID, incidentID uint64, status, source, by string, task *VerifyTask) Approval {
		return Approval{ID: approvalID, IncidentID: incidentID, Status: status, DecisionSource: &source, DecidedBy: &by, Verification: task}
	}
	var f reportFacts
	for i := uint64(1); i <= 5; i++ {
		status := "firing"
		if i <= 2 {
			status = "resolved"
		}
		f.incidents = append(f.incidents, Incident{ID: i, Status: status, StartedAt: start})
		name := "Sub2APIDown"
		if i == 4 {
			name = "HostDiskAlmostFull"
		}
		f.alerts = append(f.alerts, struct {
			IncidentID uint64
			Name       string
		}{i, name})
	}
	f.approvals = []Approval{
		approval(11, 1, "executed", rule, auto, &VerifyTask{Status: "stable", Phase: "watch"}),
		approval(21, 2, "executed", web, person, &VerifyTask{Status: "passed", Phase: "verify"}),
		approval(31, 3, "executed", rule, auto, &VerifyTask{Status: "recurred", Phase: "watch"}),
		approval(51, 5, "failed", rule, auto, nil),
	}
	f.reviews = []Review{
		{IncidentID: 1, ApprovalID: id(11), Subject: "action", Verdict: "correct"},
		{IncidentID: 5, ApprovalID: id(51), Subject: "action", Verdict: "wrong"},
		{IncidentID: 4, RunID: id(40), Subject: "diagnosis", Verdict: "wrong", ManualMinutes: 30},
	}
	f.passed = append(f.passed, struct {
		ApprovalID uint64
		CreatedAt  time.Time
	}{11, start.Add(10 * time.Minute)})
	f.tokens = append(f.tokens, struct {
		IncidentID uint64
		TokensIn   int
		TokensOut  int
	}{1, 100, 50})

	r := buildReport(f, start, start.Add(24*time.Hour), []string{"Sub2APIDown"})
	e := r.Executions
	if r.Incidents != 5 || e.Total != 4 || e.Reviewed != 2 || e.Wrong != 1 || *e.Coverage != 0.5 || *e.ErrorRate != 0.5 {
		t.Fatalf("executions = %+v", e)
	}
	u := r.Unattended
	if u.Recovered != 1 || u.Confirmed != 1 || *u.Rate != 0.2 || u.InScope != 4 || *u.InScopeRate != 0.25 {
		t.Fatalf("unattended = %+v", u)
	}
	if *r.RecoveryMinutes.Median != 10 || r.Recurrence.Watched != 2 || r.Recurrence.Recurred != 1 || *r.Recurrence.Rate != 0.5 {
		t.Fatalf("recovery = %+v recurrence = %+v", r.RecoveryMinutes, r.Recurrence)
	}
	if r.Manual.Incidents != 2 || *r.Manual.Share != 0.4 || r.Manual.Minutes != 30 {
		t.Fatalf("manual = %+v", r.Manual)
	}
	if rc := r.RootCause; rc.Reviewed != 1 || rc.Wrong != 1 || *rc.Accuracy != 0 {
		t.Fatalf("root cause = %+v", rc)
	}
	if *r.Cost.TokensPerIncident != 30 {
		t.Fatalf("cost = %+v", r.Cost)
	}
	if len(r.ReviewQueue) != 1 || r.ReviewQueue[0] != (ReviewItem{IncidentID: 3, ApprovalID: 31, Reason: "recurred"}) {
		t.Fatalf("review queue = %+v", r.ReviewQueue)
	}
}

// With no reviewed executions there is no error rate, not a zero one.
func TestBuildReportEmptyDenominatorsAreUnknown(t *testing.T) {
	r := buildReport(reportFacts{incidents: []Incident{{ID: 1}}}, time.Now().Add(-time.Hour), time.Now(), nil)
	if r.Executions.ErrorRate != nil || r.Executions.Coverage != nil || r.Unattended.InScopeRate != nil || r.RootCause.Accuracy != nil || r.RecoveryMinutes.Median != nil {
		t.Fatalf("empty denominators reported as numbers: %+v", r)
	}
	if r.ReviewQueue == nil {
		t.Fatal("review queue must be an empty list, not null")
	}
}

func TestRemediationReportReadsPersistedFacts(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, policy := executionFixture(t, db, now, "approved")
	executeFixture(t, db, approval, policy, now)
	passVerification(t, db, approval.ID, now.Add(90*time.Second))
	if err := db.Model(&Incident{}).Where("id = ?", approval.IncidentID).Updates(map[string]any{"status": "resolved", "resolved_at": now.Add(5 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	r, err := db.RemediationReport(ctx, now.Add(-time.Millisecond), now.Add(time.Millisecond), []string{testAlert})
	if err != nil {
		t.Fatal(err)
	}
	if r.Incidents != 1 || r.Executions.Total != 1 || r.Unattended.Recovered != 1 || r.Unattended.InScope != 1 || r.RecoveryMinutes.Median == nil || *r.RecoveryMinutes.Median != 1.5 {
		t.Fatalf("report = %+v", r)
	}
	if _, err := db.RemediationReport(ctx, now, now, nil); err == nil {
		t.Fatal("empty window accepted")
	}
}
