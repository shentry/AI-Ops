package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestUpdatingLinkedAlertSerializesWithExistingIncident(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC().Truncate(time.Millisecond)
	approval, _ := executionFixture(t, db, now, "approved")
	alerts, err := db.ListIncidentAlerts(context.Background(), approval.IncidentID)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("alerts=%v err=%v", alerts, err)
	}
	t.Cleanup(func() { db.Where("fingerprint = ? AND id <> ?", alerts[0].Fingerprint, alerts[0].ID).Delete(&Alert{}) })
	holder := db.Begin()
	if holder.Error != nil {
		t.Fatal(holder.Error)
	}
	defer holder.Rollback()
	var parent Incident
	if err := holder.Clauses(clause.Locking{Strength: "UPDATE"}).First(&parent, approval.IncidentID).Error; err != nil {
		t.Fatal(err)
	}
	// A fingerprint may be retained while configured grouping labels change.
	// The old Incident still references it, even if the hook will choose a new group.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	requestedParent := false
	const callback = "test:linked_alert_parent_lock"
	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Context == ctx && tx.Statement.Table == "incident" {
			_, requestedParent = tx.Statement.Clauses["FOR"]
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Query().Remove(callback)
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, err := applyAlert(tx, AlertInput{Fingerprint: alerts[0].Fingerprint, AlertHash: md5Hex("changed group"), Source: "alertmanager", Name: "Sub2APISlow", Severity: 5, Status: "firing", Labels: map[string]string{"service": "changed-group", "container": "sub2api"}, StartsAt: now, ReceivedAt: now})
		return err
	})
	if !requestedParent || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("linked alert bypassed existing parent lock: requested=%v err=%v", requestedParent, err)
	}
	current, err := db.ListIncidentAlerts(context.Background(), approval.IncidentID)
	if err != nil || len(current) != 1 || current[0].Name != alerts[0].Name {
		t.Fatalf("scope changed while parent held: %v %v", current, err)
	}
}
