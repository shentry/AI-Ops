package store

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"
)

// 集成测试的共享脚手架。
//
// 全部 store 测试都打真 MySQL —— 队列 CAS、事务回滚、恢复路径这些最需要测的
// 东西，用 sqlmock 测等于测 mock。代价是默认 go test ./... 会整包 skip，
// 要真跑必须显式给库：
//
//	TEST_MYSQL_DSN="$MYSQL_DSN" go test ./internal/store

func insertTestIncident(t *testing.T, db *DB, now time.Time, suffix string) Incident {
	t.Helper()
	incident := Incident{
		GroupKey:    t.Name() + "-" + suffix + "-" + now.UTC().Format(time.RFC3339Nano),
		Status:      "firing",
		Severity:    5,
		AlertsCount: 1,
		Title:       "queue test",
		StartedAt:   now,
		LastSeenAt:  now,
	}
	if err := db.Create(&incident).Error; err != nil {
		t.Fatalf("insert incident: %v", err)
	}
	t.Cleanup(func() {
		db.Where("incident_id = ?", incident.ID).Delete(&IncidentEvent{})
		db.Where("incident_id = ?", incident.ID).Delete(&IncidentProblem{})
		db.Where("id = ?", incident.ID).Delete(&Incident{})
	})
	return incident
}

func openIntegrationDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is not set")
	}
	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return db
}

func createTestRawEvent(t *testing.T, db *DB, ctx context.Context, createdAt time.Time) RawEvent {
	t.Helper()
	event, err := db.CreateRawEvent(ctx, "alertmanager", []byte(`{"version":"4","alerts":[]}`), createdAt)
	if err != nil {
		t.Fatalf("CreateRawEvent() error = %v", err)
	}
	return event
}

func assertRawStatus(t *testing.T, db *DB, id uint64, want string) {
	t.Helper()
	var event RawEvent
	if err := db.First(&event, id).Error; err != nil {
		t.Fatal(err)
	}
	if event.Status != want {
		t.Fatalf("raw event %d status = %q, want %q", id, event.Status, want)
	}
}

func sha256Hex(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func md5Hex(value string) string {
	digest := md5.Sum([]byte(value))
	return hex.EncodeToString(digest[:])
}
