package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/incident"
)

func TestCountRecentExecutions(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	cases := []struct {
		status string
		age    time.Duration
		dry    bool
		other  bool
	}{
		{"executed", 10 * time.Minute, false, false},
		{"failed", 20 * time.Minute, false, false},
		{"executing", time.Minute, false, false},
		{"approved", 5 * time.Minute, false, false},
		{"simulated", 6 * time.Minute, true, false},
		{"executed", 90 * time.Minute, false, false},
		{"executed", 2 * time.Minute, false, true},
	}
	for _, tc := range cases {
		at := now.Add(-tc.age)
		approval, binding := executionFixture(t, db, at, tc.dry, "approved")
		if tc.status == "approved" {
			continue
		}
		if _, claimed, err := db.ClaimApprovalExecution(ctx, approval.ID, at, binding); err != nil || !claimed {
			t.Fatalf("claim=%v %v", claimed, err)
		}
		if tc.status != "executing" {
			if err := db.FinishExecution(ctx, ExecutionCompletion{ApprovalID: approval.ID, Status: tc.status, ResultJSON: []byte(`{"output":"test"}`), FinishedAt: at}); err != nil {
				t.Fatal(err)
			}
		}
		// Historical tools outside this deployment still must not consume its budget.
		if tc.other {
			if err := db.Model(&Approval{}).Where("id = ?", approval.ID).Update("tool_name", "another_tool").Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	count, err := db.CountRecentExecutions(ctx, incident.RestartAction, "sub2api", now.Add(-time.Hour))
	if err != nil || count != 3 {
		t.Fatalf("count=%d err=%v; hashes differ but target budget must remain shared", count, err)
	}
	if _, err := db.CountRecentExecutions(ctx, "", "sub2api", now); err == nil {
		t.Fatal("empty tool accepted")
	}
}

func TestRecoverExecutingApprovals(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	stuck, binding := executionFixture(t, db, now, false, "approved")
	if _, claimed, err := db.ClaimApprovalExecution(ctx, stuck.ID, now, binding); err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	if _, err := db.ExpireApprovals(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetApproval(ctx, stuck.ID); got.Status != "executing" {
		t.Fatalf("TTL changed executing to %s", got.Status)
	}
	recovered, err := db.RecoverExecutingApprovals(ctx, now)
	if err != nil || recovered < 1 {
		t.Fatalf("recover=%d err=%v", recovered, err)
	}
	got, err := db.GetApproval(ctx, stuck.ID)
	if err != nil || got.Status != "failed" || got.Verification != nil || got.ResultJSON == nil || !strings.Contains(string(*got.ResultJSON), "manual verification") {
		t.Fatalf("approval=%+v err=%v", got, err)
	}
	if recovered, err := db.RecoverExecutingApprovals(ctx, now); err != nil || recovered != 0 {
		t.Fatalf("duplicate recovery=%d err=%v", recovered, err)
	}
}
