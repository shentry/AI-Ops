package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/datatypes"
)

func validBoundedAlert() AlertInput {
	return AlertInput{Fingerprint: strings.Repeat("f", 64), AlertHash: strings.Repeat("a", 32), Source: "alertmanager", Name: "ValidAlert", Status: "firing", Severity: 5, StartsAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ReceivedAt: time.Now().UTC()}
}

func TestApplyRawEventRejectsInvalidInputBeforeDatabaseAccess(t *testing.T) {
	for _, mutate := range []func(*AlertInput){
		func(a *AlertInput) { a.Name = strings.Repeat("n", 256) },
		func(a *AlertInput) { a.Name = strings.Repeat("警", 256) },
		func(a *AlertInput) { a.Fingerprint = strings.Repeat("f", 65) },
		func(a *AlertInput) { a.AlertHash = strings.Repeat("a", 33) },
		func(a *AlertInput) { a.Source = strings.Repeat("s", 65) },
		func(a *AlertInput) { a.GeneratorURL = strings.Repeat("警", 21846) },
		func(a *AlertInput) { a.StartsAt = time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC) },
		func(a *AlertInput) { a.ReceivedAt = time.Time{} },
		func(a *AlertInput) { a.StartsAt = time.Date(9999, 12, 31, 23, 59, 59, 999500000, time.UTC) },
	} {
		input := validBoundedAlert()
		mutate(&input)
		db := &DB{}
		if _, err := db.ApplyRawEvent(context.Background(), 1, []AlertInput{validBoundedAlert(), input}, time.Now().UTC(), nil); !errors.Is(err, ErrInvalidAlertInput) {
			t.Fatalf("error=%v want permanent input failure", err)
		}
	}
}

func TestValidateAlertInputUsesCharacterAndByteLimits(t *testing.T) {
	input := validBoundedAlert()
	input.Name = strings.Repeat("警", 255)
	input.GeneratorURL = strings.Repeat("警", 21845)
	input.StartsAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := ValidateAlertInput(input); err != nil {
		t.Fatal(err)
	}
	input.StartsAt = time.Date(9999, 12, 31, 23, 59, 59, 999499000, time.UTC)
	if err := ValidateAlertInput(input); err != nil {
		t.Fatal(err)
	}
}

func TestRawEventPendingCapacityIsAtomicAndReopens(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	source := "admission-" + time.Now().UTC().Format("150405.000000000")
	t.Cleanup(func() { db.Where("source = ?", source).Delete(&RawEvent{}) })
	before, err := db.CountRawEventsByStatus(ctx, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if before >= maxPendingRawEvents {
		t.Fatal("test database queue is already full")
	}
	rows := make([]RawEvent, maxPendingRawEvents-int(before)-1)
	for i := range rows {
		rows[i] = RawEvent{Source: source, Payload: datatypes.JSON(`{}`), Status: "pending", CreatedAt: time.Now().UTC()}
	}
	if len(rows) > 0 {
		if err := db.CreateInBatches(&rows, 100).Error; err != nil {
			t.Fatal(err)
		}
	}
	const contenders = 8
	start := make(chan struct{})
	type outcome struct {
		event RawEvent
		err   error
	}
	results := make(chan outcome, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			event, err := db.CreateRawEvent(ctx, source, []byte(`{}`), time.Now().UTC())
			results <- outcome{event, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var admitted []uint64
	for result := range results {
		if result.err == nil {
			admitted = append(admitted, result.event.ID)
		}
	}
	if len(admitted) != 1 {
		t.Fatalf("admitted=%v, want exactly one remaining slot", admitted)
	}
	count, err := db.CountRawEventsByStatus(ctx, "pending")
	if err != nil || count != maxPendingRawEvents {
		t.Fatalf("pending=%d err=%v", count, err)
	}
	if _, err := db.CreateRawEvent(ctx, source, []byte(`{}`), time.Now().UTC()); !errors.Is(err, ErrRawEventQueueFull) {
		t.Fatalf("full queue error=%v", err)
	}
	if err := db.MarkRawEventFailed(ctx, admitted[0], "integration frees capacity", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRawEvent(ctx, source, []byte(`{}`), time.Now().UTC()); err != nil {
		t.Fatalf("capacity did not reopen: %v", err)
	}
}

func TestRawEventSchemaBoundariesPersist(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	input := validBoundedAlert()
	input.Fingerprint = sha256Hex(t.Name() + time.Now().UTC().Format(time.RFC3339Nano))
	input.Name = strings.Repeat("警", 255)
	input.GeneratorURL = strings.Repeat("警", 21845)
	input.StartsAt = time.Date(9999, 12, 31, 23, 59, 59, 999000000, time.UTC)
	event := createTestRawEvent(t, db, ctx, time.Now().UTC())
	t.Cleanup(func() {
		db.Where("fingerprint = ?", input.Fingerprint).Delete(&LastAlert{})
		db.Where("fingerprint = ?", input.Fingerprint).Delete(&Alert{})
		db.Where("id = ?", event.ID).Delete(&RawEvent{})
	})
	if _, err := db.ApplyRawEvent(ctx, event.ID, []AlertInput{input}, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	var persisted Alert
	if err := db.Where("fingerprint = ?", input.Fingerprint).First(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.Name != input.Name || persisted.GeneratorURL != input.GeneratorURL || !persisted.StartsAt.Equal(input.StartsAt) {
		t.Fatal("boundary input changed in storage")
	}
}
