package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestApprovalLifecycle(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	created, _ := executionFixture(t, db, now, false, "pending")
	if _, err := db.DecideApproval(ctx, created.ID, "approved", "wrong-hash", "ops", "test", "api", now); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("wrong hash=%v", err)
	}
	if _, err := db.DecideApproval(ctx, created.ID, "approved", created.PlanHash, "ops", "test", "api", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DecideApproval(ctx, created.ID, "denied", created.PlanHash, "ops", "test", "api", now.Add(2*time.Minute)); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("duplicate=%v", err)
	}
	if _, err := db.DecideApproval(ctx, 1<<62, "approved", created.PlanHash, "ops", "test", "api", now); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("missing=%v", err)
	}
	expired, _ := executionFixture(t, db, now.Add(-2*time.Hour), false, "pending")
	if _, err := db.DecideApproval(ctx, expired.ID, "approved", expired.PlanHash, "ops", "test", "api", now); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("expired=%v", err)
	}
	if swept, err := db.ExpireApprovals(ctx, now); err != nil || swept == 0 {
		t.Fatalf("sweep=%d %v", swept, err)
	}
	got, err := db.GetApproval(ctx, expired.ID)
	if err != nil || got.Status != "expired" {
		t.Fatalf("expired=%+v %v", got, err)
	}
	got, err = db.GetApproval(ctx, created.ID)
	if err != nil || got.Status != "approved" || got.DecidedBy == nil || *got.DecidedBy != "ops" {
		t.Fatalf("approved=%+v %v", got, err)
	}
	rows, err := db.ListApprovals(ctx, "expired")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.ID == expired.ID {
			found = true
		}
		if row.ID == created.ID {
			t.Fatal("status filter included approved")
		}
	}
	if !found {
		t.Fatal("expired approval missing")
	}
}

func TestApprovalDecisionChecksActualSnapshotInsideTransaction(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, _ := executionFixture(t, db, now, false, "pending")
	if err := db.Model(&Approval{}).Where("id = ?", approval.ID).Update("args_json", []byte(`{"target_kind":"container","target_name":"different"}`)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := db.DecideApproval(ctx, approval.ID, "approved", approval.PlanHash, "ops", "test", "web", now); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("modified content=%v", err)
	}
	got, err := db.GetApproval(ctx, approval.ID)
	if err != nil || got.Status != "pending" || got.DecidedBy != nil {
		t.Fatalf("rejected decision changed state: %+v %v", got, err)
	}
}
