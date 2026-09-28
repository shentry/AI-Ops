package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
)

// A requeue whose COMMIT fails is neither applied nor counted.
func TestRecoveryCountsOnlyCommittedChanges(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC().Truncate(time.Millisecond)
	at := now.Add(-time.Minute)
	approval, _, _ := verificationFixture(t, db, at)
	if _, claimed, err := db.ClaimVerificationTask(context.Background(), approval.ID, at); err != nil || !claimed {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The recovery transaction has completed every SQL write when this hook
	// cancels its outer context. database/sql then rejects COMMIT.
	callback := "test:recovery_cancel_before_commit"
	if err := db.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
		if event, ok := tx.Statement.Dest.(*IncidentEvent); ok && event.EventType == "verify.queued" {
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Create().Remove(callback) })
	count, err := db.RequeueStaleVerificationTasks(ctx, now.Add(-30*time.Second))
	if !errors.Is(err, context.Canceled) && !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("expected commit cancellation, got count=%d err=%v", count, err)
	}
	got, readErr := db.GetApproval(context.Background(), approval.ID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got.Status != "executed" || got.Verification.Status != "running" || !got.Verification.DeadlineAt.Equal(approval.Verification.DeadlineAt) {
		t.Fatalf("partial verification commit: %+v", got)
	}
	if count != 0 {
		t.Fatalf("counted %d uncommitted recovery changes", count)
	}
}
