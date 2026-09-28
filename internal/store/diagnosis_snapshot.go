package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm/clause"
)

// DiagnosisSnapshot is the replay record of one diagnosis run: the structured
// evidence, and for model runs the exact sanitized input, model, prompt and tool
// definitions, plus every tool call with the output the model actually saw.
// A nil Model means no model was called (memory hit or failure before the call).
type DiagnosisSnapshot struct {
	RunID         uint64          `gorm:"column:run_id;primaryKey;autoIncrement:false"`
	CodeVersion   string          `gorm:"column:code_version;size:64;not null"`
	EvidenceJSON  datatypes.JSON  `gorm:"column:evidence_json;type:json;not null"`
	Model         *string         `gorm:"column:model;size:128"`
	PromptSHA256  *string         `gorm:"column:prompt_sha256;size:64"`
	ToolsJSON     *datatypes.JSON `gorm:"column:tools_json;type:json"`
	InputText     *string         `gorm:"column:input_text;type:mediumtext"`
	ToolCallsJSON *datatypes.JSON `gorm:"column:tool_calls_json;type:json"`
	ContextJSON   *datatypes.JSON `gorm:"column:context_json;type:json"`
	CreatedAt     time.Time       `gorm:"column:created_at;not null"`
	FinishedAt    *time.Time      `gorm:"column:finished_at"`
}

func (DiagnosisSnapshot) TableName() string { return "diagnosis_snapshot" }

// SaveDiagnosisSnapshot writes the whole row. The pipeline fills the snapshot
// stage by stage and rewrites it each time, so a crash leaves the last complete stage.
func (db *DB) SaveDiagnosisSnapshot(ctx context.Context, snapshot DiagnosisSnapshot) error {
	if snapshot.RunID == 0 || snapshot.CodeVersion == "" || len(snapshot.EvidenceJSON) == 0 || snapshot.CreatedAt.IsZero() {
		return errors.New("store: diagnosis snapshot run, code version, evidence and creation time are required")
	}
	err := db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&snapshot).Error
	if err != nil {
		return fmt.Errorf("store: save diagnosis snapshot: %w", err)
	}
	return nil
}

// GetDiagnosisSnapshot reads one run's replay record.
func (db *DB) GetDiagnosisSnapshot(ctx context.Context, runID uint64) (DiagnosisSnapshot, error) {
	var snapshot DiagnosisSnapshot
	if err := db.WithContext(ctx).First(&snapshot, "run_id = ?", runID).Error; err != nil {
		return DiagnosisSnapshot{}, fmt.Errorf("store: get diagnosis snapshot: %w", err)
	}
	return snapshot, nil
}
