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

// Activation counts come from the skills recorded in each replay catalog.
func TestCountSkillActivations(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	base := uint64(now.UnixNano())
	t.Cleanup(func() { db.Where("run_id BETWEEN ? AND ?", base, base+3).Delete(&DiagnosisSnapshot{}) })
	for i, catalog := range []string{
		`{"tools":[],"actions":[],"skills":[{"name":"probe_skill_a","sha256":"x"},{"name":"probe_skill_b","sha256":"y"}]}`,
		`{"tools":[],"actions":[],"skills":[{"name":"probe_skill_a","sha256":"x"}]}`,
		`{"tools":[],"actions":[]}`,
	} {
		tools := datatypes.JSON(catalog)
		if err := db.SaveDiagnosisSnapshot(ctx, DiagnosisSnapshot{RunID: base + uint64(i), CodeVersion: "t", EvidenceJSON: datatypes.JSON(`{}`), ToolsJSON: &tools, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	old := datatypes.JSON(`{"skills":[{"name":"probe_skill_a"}]}`)
	if err := db.SaveDiagnosisSnapshot(ctx, DiagnosisSnapshot{RunID: base + 3, CodeVersion: "t", EvidenceJSON: datatypes.JSON(`{}`), ToolsJSON: &old, CreatedAt: now.Add(-31 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	counts, err := db.CountSkillActivations(ctx, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if counts["probe_skill_a"] != 2 || counts["probe_skill_b"] != 1 {
		t.Fatalf("counts = %v", counts)
	}
}
