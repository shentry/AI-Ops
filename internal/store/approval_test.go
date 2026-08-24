package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 审批单生命周期。

func TestApprovalLifecycle(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 14, 0, 0, 0, time.UTC)
	var ids []uint64
	t.Cleanup(func() {
		if len(ids) > 0 {
			db.Where("id IN ?", ids).Delete(&Approval{})
		}
		db.Close()
	})

	created, err := db.CreateApproval(ctx, Approval{
		IncidentID: 1, RunID: 2, ToolName: "resize_pool", ArgsJSON: []byte(`{"target_name":"sub2api"}`),
		Reason: "L3", PlanHash: "hash-1", ExpiresAt: now.Add(30 * time.Minute), CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, created.ID)
	if created.Status != "pending" {
		t.Fatalf("status = %q", created.Status)
	}

	// 批准成功；重复批准 409；过期窗口外不能决策。
	if _, err := db.DecideApproval(ctx, created.ID, "approved", "ops", "test", "api", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DecideApproval(ctx, created.ID, "denied", "ops", "test", "api", now.Add(2*time.Minute)); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("re-decide error = %v, want conflict", err)
	}
	if _, err := db.DecideApproval(ctx, 1<<62, "approved", "ops", "test", "api", now); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("missing error = %v, want not found", err)
	}

	// 过期单不能被批准。
	expired, err := db.CreateApproval(ctx, Approval{
		IncidentID: 1, RunID: 2, ToolName: "x", ArgsJSON: []byte(`{}`),
		Reason: "r", PlanHash: "h", ExpiresAt: now.Add(time.Minute), CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, expired.ID)
	if _, err := db.DecideApproval(ctx, expired.ID, "approved", "ops", "test", "api", now.Add(2*time.Minute)); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("expired approve error = %v, want conflict", err)
	}

	// 主动过期 sweep。
	swept, err := db.ExpireApprovals(ctx, now.Add(2*time.Minute))
	if err != nil || swept == 0 {
		t.Fatalf("ExpireApprovals() = %d, %v", swept, err)
	}
	got, err := db.GetApproval(ctx, expired.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "expired" {
		t.Fatalf("status = %q, want expired", got.Status)
	}
	// 已批准的不会被 sweep。
	got, _ = db.GetApproval(ctx, created.ID)
	if got.Status != "approved" {
		t.Fatalf("approved status = %q", got.Status)
	}

	// 列表过滤。
	approvals, err := db.ListApprovals(ctx, "expired")
	if err != nil {
		t.Fatal(err)
	}
	foundExpired := false
	for _, a := range approvals {
		if a.ID == expired.ID {
			foundExpired = true
		}
		if a.ID == created.ID {
			t.Fatal("approved approval leaked into expired filter")
		}
	}
	if !foundExpired {
		t.Fatal("expired approval missing from list")
	}
}
