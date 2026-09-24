package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestRecoveryCountsOnlyCommittedChanges(t *testing.T) {
	for _, kind := range []string{"execution", "verification"} {
		t.Run(kind, func(t *testing.T) {
			db := openIntegrationDB(t)
			t.Cleanup(func() { db.Close() })
			now := time.Now().UTC().Truncate(time.Millisecond)
			at := now.Add(-time.Minute)
			var approval Approval
			if kind == "execution" {
				row, binding := executionFixture(t, db, at, false, "approved")
				approval = row
				if _, claimed, err := db.ClaimApprovalExecution(context.Background(), row.ID, at, binding); err != nil || !claimed {
					t.Fatalf("claim=%v %v", claimed, err)
				}
			} else {
				row, _, _ := verificationFixture(t, db, at)
				approval = row
				if _, claimed, err := db.ClaimVerificationTask(context.Background(), row.ID, at); err != nil || !claimed {
					t.Fatalf("claim=%v %v", claimed, err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Both recovery transactions have completed every SQL write when this
			// hook cancels their outer context. database/sql then rejects COMMIT.
			callback := "test:recovery_cancel_before_commit"
			if err := db.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
				if problem, ok := tx.Statement.Dest.(*IncidentProblem); ok && kind == "execution" && problem.Code == "execution_failed" {
					cancel()
				}
				if event, ok := tx.Statement.Dest.(*IncidentEvent); ok && kind == "verification" && event.EventType == "verify.queued" {
					cancel()
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Callback().Create().Remove(callback) })
			var count int64
			var err error
			if kind == "execution" {
				count, err = db.RecoverExecutingApprovals(ctx, now)
			} else {
				count, err = db.RequeueStaleVerificationTasks(ctx, now.Add(-30*time.Second))
			}
			if !errors.Is(err, context.Canceled) && !errors.Is(err, sql.ErrTxDone) {
				t.Fatalf("expected commit cancellation, got count=%d err=%v", count, err)
			}
			got, readErr := db.GetApproval(context.Background(), approval.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if kind == "execution" && (got.Status != "executing" || got.ResultJSON != nil) {
				t.Fatalf("partial execution commit: %+v", got)
			}
			if kind == "verification" && (got.Status != "executed" || got.Verification.Status != "running" || !got.Verification.DeadlineAt.Equal(approval.Verification.DeadlineAt)) {
				t.Fatalf("partial verification commit: %+v", got)
			}
			if count != 0 {
				t.Fatalf("counted %d uncommitted recovery changes", count)
			}
		})
	}
}
