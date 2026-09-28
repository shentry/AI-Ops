package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"oncall-agent/internal/eventlog"
	incidentrule "oncall-agent/internal/incident"
)

const (
	RunTriggerAlert  = "alert"
	RunTriggerManual = "manual"
	RunTriggerRetry  = "retry"
)

// RunRequest is shared by ingress, human rediagnosis and verification retries.
type RunRequest struct {
	IncidentID  uint64
	Mode        string
	Trigger     string
	RetryOf     *uint64
	Reason      string
	RequestedAt time.Time
}

// RunAdmissionError is a caller-visible refusal, not a database failure.
type RunAdmissionError struct {
	Code              string
	RunID             uint64
	ApprovalID        uint64
	RetryAfterSeconds int
}

func (e *RunAdmissionError) Error() string { return e.Code }

func (db *DB) RequestRun(ctx context.Context, request RunRequest) (run AgentRun, created bool, err error) {
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var admissionErr error
		run, created, admissionErr = requestRun(ctx, tx, request)
		return admissionErr
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	return run, created && err == nil, err
}

func lockIncident(ctx context.Context, tx *gorm.DB, id uint64) (Incident, error) {
	var row Incident
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, ErrIncidentNotFound
	}
	return row, err
}

// requestRun is also used inside ingest and verification transactions. Every
// admission locks the same parent first, including the no-active-row case.
func requestRun(ctx context.Context, tx *gorm.DB, request RunRequest) (AgentRun, bool, error) {
	if request.IncidentID == 0 || request.RequestedAt.IsZero() {
		return AgentRun{}, false, errors.New("store: run incident and requested time are required")
	}
	if request.Mode != incidentrule.ModeFull && request.Mode != incidentrule.ModeLight && request.Mode != incidentrule.ModeSkip {
		return AgentRun{}, false, errors.New("store: invalid run mode")
	}
	if request.Trigger != RunTriggerAlert && request.Trigger != RunTriggerManual && request.Trigger != RunTriggerRetry {
		return AgentRun{}, false, errors.New("store: invalid run trigger")
	}
	if (request.Trigger == RunTriggerRetry) != (request.RetryOf != nil) {
		return AgentRun{}, false, errors.New("store: retry parent required only for automatic retries")
	}
	if request.Trigger != RunTriggerAlert && request.Mode == incidentrule.ModeSkip {
		return AgentRun{}, false, errors.New("store: only alert routing can skip diagnosis")
	}
	parent, err := lockIncident(ctx, tx, request.IncidentID)
	if err != nil {
		return AgentRun{}, false, err
	}
	if parent.Status != incidentrule.StatusFiring {
		return AgentRun{}, false, &RunAdmissionError{Code: "incident_not_firing"}
	}
	if err := checkActiveProcessing(ctx, tx, request.IncidentID); err != nil {
		return AgentRun{}, false, err
	}
	now := request.RequestedAt.UTC().Truncate(time.Millisecond)
	if request.Trigger == RunTriggerManual {
		var last IncidentEvent
		err := tx.WithContext(ctx).Where("incident_id = ? AND event_type = ?", parent.ID, eventlog.EventRunQueued).Order("id DESC").First(&last).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return AgentRun{}, false, err
		}
		if err == nil && now.Before(last.CreatedAt.Add(time.Minute)) {
			return AgentRun{}, false, &RunAdmissionError{Code: "cooldown", RetryAfterSeconds: int(math.Ceil(last.CreatedAt.Add(time.Minute).Sub(now).Seconds()))}
		}
	}
	if request.Trigger == RunTriggerRetry {
		if err := checkRetryBudget(ctx, tx, parent.ID, *request.RetryOf); err != nil {
			return AgentRun{}, false, err
		}
	}
	queued := incidentrule.NewQueueRun(parent.ID, request.Mode, now)
	run := AgentRun{IncidentID: parent.ID, Mode: queued.Mode, Status: queued.Status, StartedAt: now, RetryOf: request.RetryOf}
	if queued.Finished {
		run.FinishedAt = &now
	}
	if err := tx.WithContext(ctx).Create(&run).Error; err != nil {
		return AgentRun{}, false, err
	}
	eventType, status, summary := eventlog.EventRunQueued, "pending", "diagnostic run queued"
	if queued.Finished {
		eventType, status, summary = eventlog.EventRunSucceeded, "succeeded", "diagnosis skipped by severity route"
	}
	var payload *datatypes.JSON
	if request.Trigger == RunTriggerRetry {
		reason := incidentrule.NormalizeRetryReason(request.Reason)
		summary = incidentrule.RetryQueuedSummary(reason)
		payloadBytes, _ := json.Marshal(map[string]any{"reason": reason, "retry_of": *request.RetryOf})
		value := datatypes.JSON(payloadBytes)
		payload = &value
	}
	event := IncidentEvent{IncidentID: parent.ID, RunID: &run.ID, EventType: string(eventType), Phase: "run", Status: status, Summary: summary, PayloadJSON: payload, CreatedAt: now}
	if _, err := appendIncidentEvent(ctx, tx, event); err != nil {
		return AgentRun{}, false, err
	}
	if request.Trigger == RunTriggerRetry {
		event.EventType, event.Phase, event.Status = string(eventlog.EventRetryScheduled), "retry", "scheduled"
		event.Summary = incidentrule.RetryScheduledSummary(incidentrule.NormalizeRetryReason(request.Reason))
		if _, err := appendIncidentEvent(ctx, tx, event); err != nil {
			return AgentRun{}, false, err
		}
	}
	return run, true, nil
}

func checkActiveProcessing(ctx context.Context, tx *gorm.DB, incidentID uint64) error {
	var run AgentRun
	err := tx.WithContext(ctx).Select("id").Where("incident_id = ? AND status IN ?", incidentID, []string{"pending", "running"}).Order("id ASC").First(&run).Error
	if err == nil {
		return &RunAdmissionError{Code: "active_processing", RunID: run.ID}
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	var approval Approval
	err = tx.WithContext(ctx).Select("id", "run_id").Where("incident_id = ? AND status IN ?", incidentID, []string{"pending", "approved", "executing"}).Order("id ASC").First(&approval).Error
	if err == nil {
		return &RunAdmissionError{Code: "active_processing", RunID: approval.RunID, ApprovalID: approval.ID}
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	// Only deciding recovery blocks a new diagnosis; the post-recovery watch is
	// passive, and a second action in the incident already needs a person.
	var task VerifyTask
	err = tx.WithContext(ctx).Table("verify_task").Select("verify_task.*").Joins("JOIN approval ON approval.id = verify_task.approval_id").Where("approval.incident_id = ? AND verify_task.phase = ? AND verify_task.status IN ?", incidentID, "verify", []string{"pending", "running"}).Order("verify_task.approval_id ASC").Take(&task).Error
	if err == nil {
		return &RunAdmissionError{Code: "active_processing", ApprovalID: task.ApprovalID}
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return nil
}

func checkRetryBudget(ctx context.Context, tx *gorm.DB, incidentID, failedRunID uint64) error {
	if failedRunID == 0 {
		return &RunAdmissionError{Code: "retry_chain"}
	}
	var existing AgentRun
	err := tx.WithContext(ctx).Select("id").Where("incident_id = ? AND retry_of = ?", incidentID, failedRunID).First(&existing).Error
	if err == nil {
		return &RunAdmissionError{Code: "active_processing", RunID: existing.ID}
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	seen := make(map[uint64]bool)
	cursor := failedRunID
	for depth := 1; ; depth++ {
		if cursor == 0 || seen[cursor] {
			return &RunAdmissionError{Code: "retry_chain"}
		}
		seen[cursor] = true
		var row AgentRun
		err := tx.WithContext(ctx).First(&row, cursor).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &RunAdmissionError{Code: "retry_chain"}
		}
		if err != nil {
			return fmt.Errorf("store: read retry chain: %w", err)
		}
		if row.IncidentID != incidentID || (row.Status != "succeeded" && row.Status != "failed") {
			return &RunAdmissionError{Code: "retry_chain"}
		}
		if depth > 2 {
			return &RunAdmissionError{Code: "retry_budget"}
		}
		if row.RetryOf == nil {
			return nil
		}
		cursor = *row.RetryOf
	}
}
