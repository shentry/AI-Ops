package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// raw_event 摄入与指纹去重。

func TestCreateRawEventRejectsInvalidJSON(t *testing.T) {
	db := &DB{}
	_, err := db.CreateRawEvent(context.Background(), "alertmanager", []byte(`{"version":`), time.Now())
	if !errors.Is(err, ErrInvalidRawEvent) {
		t.Fatalf("CreateRawEvent() error = %v, want ErrInvalidRawEvent", err)
	}
}

func TestApplyRawEventDedupAndAtomicStatus(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	seed := t.Name() + time.Now().UTC().Format(time.RFC3339Nano)
	fingerprint := sha256Hex(seed)
	rollbackFingerprint := sha256Hex(seed + "-rollback")
	createdAt := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	generatorURL := "http://127.0.0.1:9090/graph?g0.expr=vector(1)"
	var rawEventIDs []uint64
	t.Cleanup(func() {
		db.Where("fingerprint IN ?", []string{fingerprint, rollbackFingerprint}).Delete(&LastAlert{})
		db.Where("fingerprint IN ?", []string{fingerprint, rollbackFingerprint}).Delete(&Alert{})
		if len(rawEventIDs) > 0 {
			db.Where("id IN ?", rawEventIDs).Delete(&RawEvent{})
		}
		db.Close()
	})

	newInput := AlertInput{
		Fingerprint:  fingerprint,
		AlertHash:    md5Hex(seed + "-firing"),
		Source:       "alertmanager",
		Name:         "StoreIntegration",
		Severity:     5,
		Status:       "firing",
		Labels:       map[string]string{"alertname": "StoreIntegration", "instance": seed},
		Annotations:  map[string]string{"summary": "integration"},
		GeneratorURL: generatorURL,
		StartsAt:     createdAt.Add(-time.Minute),
		ReceivedAt:   createdAt,
	}

	first := createTestRawEvent(t, db, ctx, createdAt)
	rawEventIDs = append(rawEventIDs, first.ID)
	results, err := db.ApplyRawEvent(ctx, first.ID, []AlertInput{newInput}, createdAt.Add(time.Second), nil)
	if err != nil {
		t.Fatalf("ApplyRawEvent(new) error = %v", err)
	}
	if len(results) != 1 || results[0].Dedup != DedupNew {
		t.Fatalf("ApplyRawEvent(new) results = %#v", results)
	}
	assertRawStatus(t, db, first.ID, "processed")

	second := createTestRawEvent(t, db, ctx, createdAt.Add(time.Minute))
	rawEventIDs = append(rawEventIDs, second.ID)
	fullInput := newInput
	fullInput.ReceivedAt = second.CreatedAt
	results, err = db.ApplyRawEvent(ctx, second.ID, []AlertInput{fullInput}, second.CreatedAt.Add(time.Second), nil)
	if err != nil {
		t.Fatalf("ApplyRawEvent(full) error = %v", err)
	}
	if len(results) != 1 || results[0].Dedup != DedupFull {
		t.Fatalf("ApplyRawEvent(full) results = %#v", results)
	}

	third := createTestRawEvent(t, db, ctx, createdAt.Add(2*time.Minute))
	rawEventIDs = append(rawEventIDs, third.ID)
	partialInput := fullInput
	partialInput.AlertHash = md5Hex(seed + "-resolved")
	partialInput.Status = "resolved"
	partialInput.ReceivedAt = third.CreatedAt
	results, err = db.ApplyRawEvent(ctx, third.ID, []AlertInput{partialInput}, third.CreatedAt.Add(time.Second), nil)
	if err != nil {
		t.Fatalf("ApplyRawEvent(partial) error = %v", err)
	}
	if len(results) != 1 || results[0].Dedup != DedupPartial {
		t.Fatalf("ApplyRawEvent(partial) results = %#v", results)
	}

	var alertCount int64
	if err := db.Model(&Alert{}).Where("fingerprint = ?", fingerprint).Count(&alertCount).Error; err != nil {
		t.Fatal(err)
	}
	if alertCount != 2 {
		t.Fatalf("alert count = %d, want 2", alertCount)
	}
	var last LastAlert
	if err := db.First(&last, "fingerprint = ?", fingerprint).Error; err != nil {
		t.Fatal(err)
	}
	if last.Status != "resolved" || last.FiringCount != 2 || !last.LastSeen.Equal(third.CreatedAt) {
		t.Fatalf("last alert = %#v", last)
	}
	var latest Alert
	if err := db.Where("fingerprint = ?", fingerprint).Order("id DESC").First(&latest).Error; err != nil {
		t.Fatal(err)
	}
	if latest.GeneratorURL != generatorURL {
		t.Fatalf("generator URL = %q", latest.GeneratorURL)
	}

	rollbackRaw := createTestRawEvent(t, db, ctx, createdAt.Add(3*time.Minute))
	rawEventIDs = append(rawEventIDs, rollbackRaw.ID)
	valid := newInput
	valid.Fingerprint = rollbackFingerprint
	valid.AlertHash = md5Hex(seed + "-rollback-valid")
	invalid := valid
	invalid.Name = ""
	if _, err := db.ApplyRawEvent(ctx, rollbackRaw.ID, []AlertInput{valid, invalid}, time.Now().UTC(), nil); err == nil {
		t.Fatal("ApplyRawEvent() error = nil, want transaction failure")
	}
	assertRawStatus(t, db, rollbackRaw.ID, "pending")
	if err := db.Model(&Alert{}).Where("fingerprint = ?", rollbackFingerprint).Count(&alertCount).Error; err != nil {
		t.Fatal(err)
	}
	if alertCount != 0 {
		t.Fatalf("rolled-back alert count = %d, want 0", alertCount)
	}
}
