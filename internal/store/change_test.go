package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func testRelease(service, id, digest, migration string, at time.Time) ChangeEvent {
	image := "ghcr.io/example/sub2api@sha256:" + strings.Repeat(digest, 64)
	return ChangeEvent{Env: "prod", Service: service, ChangeType: "release", ReleaseID: &id, ImageRef: &image, DBMigration: migration,
		OccurredAt: at, Source: "ci", Actor: "pipeline", IdempotencyKey: service + "-" + id, CreatedAt: at}
}

func TestRecordChangeIsValidatedAndIdempotent(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	service := "svc-change-" + sha256Hex(now.String())[:8]
	t.Cleanup(func() { db.Where("service = ?", service).Delete(&ChangeEvent{}) })
	tag := "ghcr.io/example/sub2api:latest"
	for name, mutate := range map[string]func(*ChangeEvent){
		"tag instead of digest": func(c *ChangeEvent) { c.ImageRef = &tag },
		"no release id":         func(c *ChangeEvent) { c.ReleaseID = nil },
		"unknown type":          func(c *ChangeEvent) { c.ChangeType = "hotfix" },
		"bad migration":         func(c *ChangeEvent) { c.DBMigration = "maybe" },
		"no actor":              func(c *ChangeEvent) { c.Actor = " " },
		"no idempotency key":    func(c *ChangeEvent) { c.IdempotencyKey = "" },
	} {
		change := testRelease(service, "v1", "a", "none", now)
		mutate(&change)
		if _, _, err := db.RecordChange(ctx, change); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	first, created, err := db.RecordChange(ctx, testRelease(service, "v1", "a", "none", now))
	if err != nil || !created || first.ID == 0 {
		t.Fatalf("record=%+v created=%v err=%v", first, created, err)
	}
	again, created, err := db.RecordChange(ctx, testRelease(service, "v1", "a", "none", now.Add(time.Minute)))
	if err != nil || created || again.ID != first.ID || !again.OccurredAt.Equal(first.OccurredAt) {
		t.Fatalf("repeat=%+v created=%v err=%v", again, created, err)
	}
	config := ChangeEvent{Env: "prod", Service: service, ChangeType: "config", OccurredAt: now.Add(time.Second), Source: "ops", Actor: "ops", IdempotencyKey: "cfg-1", CreatedAt: now}
	config, _, err = db.RecordChange(ctx, config)
	if err != nil || config.DBMigration != "unknown" {
		t.Fatalf("config change=%+v err=%v", config, err)
	}
	second, _, err := db.RecordChange(ctx, testRelease(service, "v2", "b", "incompatible", now.Add(2*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	releases, err := db.ListReleases(ctx, service, 10)
	if err != nil || len(releases) != 2 || releases[0].ID != second.ID || releases[1].ID != first.ID {
		t.Fatalf("releases=%+v err=%v", releases, err)
	}
	verified, err := db.MarkReleaseVerified(ctx, first.ID, now.Add(time.Hour))
	if err != nil || verified.VerifiedAt == nil || !verified.VerifiedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("verified=%+v err=%v", verified, err)
	}
	if again, err := db.MarkReleaseVerified(ctx, first.ID, now.Add(2*time.Hour)); err != nil || !again.VerifiedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("first verification must be kept: %+v %v", again, err)
	}
	if _, err := db.MarkReleaseVerified(ctx, config.ID, now); err == nil {
		t.Fatal("config change verified as a release")
	}
	if _, err := db.MarkReleaseVerified(ctx, 1<<62, now); !errors.Is(err, ErrChangeNotFound) {
		t.Fatalf("missing change=%v", err)
	}
}

// An agent rollback is recorded with its result and inherits the declared
// migration and verification of the release it returned to.
func TestRollbackChangeInheritsTargetRelease(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, policy := executionFixture(t, db, now, "approved")
	service := *approval.Service
	good, _, err := db.RecordChange(ctx, testRelease(service, "v1", "a", "none", now.Add(-2*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if good, err = db.MarkReleaseVerified(ctx, good.ID, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	bad, _, err := db.RecordChange(ctx, testRelease(service, "v2", "b", "compatible", now.Add(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := db.ClaimApprovalExecution(ctx, approval.ID, now, policy); err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	change := &ChangeEvent{Env: "prod", Service: service, ChangeType: "rollback", ReleaseID: good.ReleaseID, ImageRef: good.ImageRef, BeforeRef: bad.ImageRef, Actor: "agent"}
	if err := db.FinishExecution(ctx, ExecutionCompletion{ApprovalID: approval.ID, Status: "executed", ResultJSON: []byte(`{"written":true}`), Change: change, FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	releases, err := db.ListReleases(ctx, service, 1)
	if err != nil || len(releases) != 1 {
		t.Fatalf("releases=%v err=%v", releases, err)
	}
	rollback := releases[0]
	if rollback.ChangeType != "rollback" || rollback.Source != "agent" || rollback.ApprovalID == nil || *rollback.ApprovalID != approval.ID ||
		rollback.DBMigration != "none" || rollback.VerifiedAt == nil || !rollback.VerifiedAt.Equal(*good.VerifiedAt) || *rollback.BeforeRef != *bad.ImageRef {
		t.Fatalf("rollback=%+v", rollback)
	}
}

func TestReviewIsBoundToIncidentAndBlocksWrongActions(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, policy := executionFixture(t, db, now, "approved")
	executeFixture(t, db, approval, policy, now)
	passVerification(t, db, approval.ID, now.Add(time.Second))
	other, _ := executionFixture(t, db, now.Add(time.Millisecond), "pending")
	at := now.Add(time.Minute)
	for name, review := range map[string]Review{
		"no reviewer":           {IncidentID: approval.IncidentID, Subject: "action", Verdict: "wrong", ApprovalID: &approval.ID, CreatedAt: at},
		"bad verdict":           {IncidentID: approval.IncidentID, Subject: "action", Verdict: "fine", ApprovalID: &approval.ID, Reviewer: "ops", CreatedAt: at},
		"action without target": {IncidentID: approval.IncidentID, Subject: "action", Verdict: "wrong", Reviewer: "ops", CreatedAt: at},
		"diagnosis without run": {IncidentID: approval.IncidentID, Subject: "diagnosis", Verdict: "correct", Reviewer: "ops", CreatedAt: at},
		"other incident":        {IncidentID: approval.IncidentID, Subject: "action", Verdict: "wrong", ApprovalID: &other.ID, Reviewer: "ops", CreatedAt: at},
	} {
		if _, err := db.AddReview(ctx, review); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	diagnosis, err := db.AddReview(ctx, Review{IncidentID: approval.IncidentID, RunID: &approval.RunID, Subject: "diagnosis", Verdict: "partial", RootCause: "upstream 529", ActualFix: "restart", ManualMinutes: 5, Reviewer: "ops", CreatedAt: at})
	if err != nil || diagnosis.ID == 0 {
		t.Fatalf("diagnosis review=%+v err=%v", diagnosis, err)
	}
	state, err := db.RemediationState(ctx, RemediationQuery{RuleID: *approval.RuleID, Since: now.Add(-time.Hour)})
	if err != nil || state.Blocked != "" {
		t.Fatalf("a diagnosis review must not block the rule: %+v %v", state, err)
	}
	action, err := db.AddReview(ctx, Review{IncidentID: approval.IncidentID, ApprovalID: &approval.ID, Subject: "action", Verdict: "wrong", RootCause: "database was down", ActualFix: "restarted postgres", Reviewer: "ops", CreatedAt: at})
	if err != nil || action.RunID == nil || *action.RunID != approval.RunID {
		t.Fatalf("action review=%+v err=%v", action, err)
	}
	state, err = db.RemediationState(ctx, RemediationQuery{RuleID: *approval.RuleID, Since: now.Add(-time.Hour)})
	if err != nil || !strings.Contains(state.Blocked, "review.recorded") {
		t.Fatalf("an action reviewed wrong must block the rule: %+v %v", state, err)
	}
	reviews, err := db.ListReviews(ctx, approval.IncidentID)
	if err != nil || len(reviews) != 2 || reviews[0].ID != action.ID {
		t.Fatalf("reviews=%+v err=%v", reviews, err)
	}
}
