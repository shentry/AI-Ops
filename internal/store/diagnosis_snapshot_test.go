package store

import (
	"context"
	"testing"
	"time"

	"gorm.io/datatypes"
)

// The pipeline rewrites the row stage by stage; each save replaces the whole record.
func TestDiagnosisSnapshotSaveIsStagedUpsert(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	runID := uint64(now.UnixNano())
	t.Cleanup(func() { db.Where("run_id = ?", runID).Delete(&DiagnosisSnapshot{}) })

	if err := db.SaveDiagnosisSnapshot(ctx, DiagnosisSnapshot{RunID: runID, CodeVersion: "abc"}); err == nil {
		t.Fatal("snapshot without evidence accepted")
	}
	snapshot := DiagnosisSnapshot{RunID: runID, CodeVersion: "abc", EvidenceJSON: datatypes.JSON(`{"Items":[]}`), CreatedAt: now}
	if err := db.SaveDiagnosisSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	model, input, calls := "m", "evidence text", datatypes.JSON(`[{"Name":"prom_instant_query"}]`)
	snapshot.Model, snapshot.InputText, snapshot.ToolCallsJSON, snapshot.FinishedAt = &model, &input, &calls, &now
	if err := db.SaveDiagnosisSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetDiagnosisSnapshot(ctx, runID)
	if err != nil || got.Model == nil || *got.Model != "m" || got.InputText == nil || *got.InputText != input || got.ToolCallsJSON == nil || got.FinishedAt == nil || !got.CreatedAt.Equal(now) {
		t.Fatalf("snapshot = %+v %v", got, err)
	}
}
