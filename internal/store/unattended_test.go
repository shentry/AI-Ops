package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"oncall-agent/internal/eventlog"
	"oncall-agent/internal/incident"
)

func TestHumanNotificationIsAtomicAndRetryable(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx, now := context.Background(), time.Now().UTC().Truncate(time.Millisecond)
	parent := insertTestIncident(t, db, now, "notify-atomic")
	defer db.Where("incident_id = ?", parent.ID).Delete(&IncidentEvent{})
	event := IncidentEvent{IncidentID: parent.ID, EventType: string(eventlog.EventExecutionFailed), Phase: "approval", Status: "failed", Summary: "operation unknown", CreatedAt: now}
	var saved IncidentEvent
	err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		saved, err = appendIncidentEvent(ctx, tx, event)
		if err != nil {
			return err
		}
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	var count int64
	db.Model(&NotificationTask{}).Where("event_id = ?", saved.ID).Count(&count)
	if count != 0 {
		t.Fatal("uncommitted failure queued a notification")
	}
	saved, err = db.AppendIncidentEvent(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	var task NotificationTask
	if err := db.First(&task, "event_id = ?", saved.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.FinishNotification(ctx, task, now, "network timeout"); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&task, "event_id = ?", saved.ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.DeliveredAt != nil || task.Attempts != 1 || !task.NextAttemptAt.Equal(now.Add(5*time.Second)) {
		t.Fatalf("retry=%+v", task)
	}
	if err := db.FinishNotification(ctx, task, now.Add(5*time.Second), ""); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&task, "event_id = ?", saved.ID).Error; err != nil || task.DeliveredAt == nil {
		t.Fatalf("delivery=%+v %v", task, err)
	}
}

func TestLostExecutorConnectionRevokesAuthority(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	release, err := db.AcquireExecutorLock(ctx, "lost-lease-"+fmt.Sprint(time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := db.CheckExecutionLease(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.executionLease.conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckExecutionLease(ctx); err == nil {
		t.Fatal("closed lock connection retained execution permission")
	}
}

func TestWatchCoverageLossCannotBecomeStable(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx, now := context.Background(), time.Now().UTC().Truncate(time.Millisecond)
	approval, policy := executionFixture(t, db, now, "approved")
	resnapshot(t, db, &approval, func(s *incident.ExecutionContext) { s.Verification.WatchSeconds = 60 })
	executeFixture(t, db, approval, policy, now)
	task, _, err := db.ClaimVerificationTask(ctx, approval.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.FinalizeVerification(ctx, VerificationCompletion{ApprovalID: approval.ID, ClaimedAt: *task.ClaimedAt, CheckedAt: now, Status: "passed", Observation: "healthy"})
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Minute)
	task, _, err = db.ClaimVerificationTask(ctx, approval.ID, later)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.FinalizeVerification(ctx, VerificationCompletion{ApprovalID: approval.ID, ClaimedAt: *task.ClaimedAt, CheckedAt: later, Status: "stable", Observation: "healthy"})
	if err != nil || result.Status != "inconclusive" {
		t.Fatalf("stale verdict=%+v %v", result, err)
	}
}

func TestQueueMonitoringReportsWaitingTime(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	event, err := db.CreateRawEvent(context.Background(), "test", []byte(`{"alerts":[]}`), time.Now().UTC().Add(-90*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Delete(&event) })
	ages, err := db.QueueAges(context.Background())
	if err != nil || ages["raw_event"] < 89 || len(ages) != 4 {
		t.Fatalf("queue ages=%v err=%v", ages, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.QueueAges(ctx); err == nil {
		t.Fatal("failed observation became a healthy queue")
	}
}
