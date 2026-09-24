package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"oncall-agent/internal/eventlog"
	incidentrule "oncall-agent/internal/incident"
)

// ExecutionCompletion records an external result; retrying it never retries the action.
type ExecutionCompletion struct {
	ApprovalID uint64
	Status     string
	ResultJSON []byte
	FinishedAt time.Time
}

func (db *DB) NextApprovedApproval(ctx context.Context, now time.Time) (Approval, bool, error) {
	var row Approval
	err := db.WithContext(ctx).Where("status = ? AND expires_at > ?", "approved", now).Order("id ASC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, false, nil
	}
	return row, err == nil, err
}

// lockApproval uses the same parent-first ordering as Run admission and verification.
// Transactions that check member scope use READ COMMITTED: the identity lookup
// below must not freeze a REPEATABLE READ view before waiting for the parent.
// Ingest holds that parent through its member writes and commit.
func lockApproval(ctx context.Context, tx *gorm.DB, id uint64) (Approval, Incident, error) {
	var row Approval
	if err := tx.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return row, Incident{}, ErrApprovalNotFound
		}
		return row, Incident{}, err
	}
	parent, err := lockIncident(ctx, tx, row.IncidentID)
	if err != nil {
		return row, parent, err
	}
	err = tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, id).Error
	return row, parent, err
}

func (db *DB) ClaimApprovalExecution(ctx context.Context, id uint64, now time.Time, binding incidentrule.ExecutionBinding) (row Approval, claimed bool, err error) {
	if id == 0 || now.IsZero() {
		return row, false, errors.New("store: execution id and claim time required")
	}
	now = now.UTC().Truncate(time.Millisecond)
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var parent Incident
		var err error
		row, parent, err = lockApproval(ctx, tx, id)
		if errors.Is(err, ErrApprovalNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.Status != "approved" {
			return nil
		}
		reason := ""
		if !row.ExpiresAt.After(now) {
			reason = "approval expired"
		}
		snapshot, parseErr := incidentrule.ParseExecutionContext(row.ExecutionContext)
		hash, hashErr := incidentrule.PlanHash(row.ToolName, row.ArgsJSON, row.ExecutionContext)
		if parseErr != nil || hashErr != nil || hash != row.PlanHash {
			reason = "approval execution snapshot is invalid"
		}
		if reason == "" {
			if err := snapshot.ValidateBinding(binding, true); err != nil {
				reason = err.Error()
			}
		}
		if reason == "" && parent.Status != incidentrule.StatusFiring {
			reason = "incident is no longer firing"
		}
		if reason == "" {
			members, err := listIncidentExecutionMembers(ctx, tx, parent.ID)
			if err != nil {
				return err
			}
			if err := snapshot.ValidateMembers(members, true); err != nil {
				reason = err.Error()
			}
		}
		if reason != "" {
			if err := tx.WithContext(ctx).Model(&Approval{}).Where("id = ? AND status = ?", id, "approved").Update("status", "expired").Error; err != nil {
				return err
			}
			row.Status = "expired"
			return appendApprovalEvent(ctx, tx, row, eventlog.EventApprovalExpired, "expired", truncateStoreText(reason, 512), now)
		}
		result := tx.WithContext(ctx).Model(&Approval{}).Where("id = ? AND status = ?", id, "approved").Update("status", "executing")
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		if err := appendApprovalEvent(ctx, tx, row, eventlog.EventExecutionStarted, "executing", "approval execution started", now); err != nil {
			return err
		}
		row.Status = "executing"
		claimed = true
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	return row, claimed && err == nil, err
}

func (db *DB) FinishExecution(ctx context.Context, completion ExecutionCompletion) error {
	if completion.ApprovalID == 0 || completion.FinishedAt.IsZero() {
		return errors.New("store: execution id and completion time required")
	}
	if completion.Status != "executed" && completion.Status != "simulated" && completion.Status != "failed" {
		return errors.New("store: invalid execution final status")
	}
	if !json.Valid(completion.ResultJSON) {
		return errors.New("store: execution result must be JSON")
	}
	canonical, err := incidentrule.CanonicalJSON(completion.ResultJSON)
	if err != nil {
		return err
	}
	bounded := boundedExecutionResult(canonical)
	now := completion.FinishedAt.UTC().Truncate(time.Millisecond)
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, parent, err := lockApproval(ctx, tx, completion.ApprovalID)
		if err != nil {
			return err
		}
		if row.Status != "executing" {
			if row.Status != completion.Status || row.ResultJSON == nil {
				return ErrApprovalConflict
			}
			previous, err := incidentrule.CanonicalJSON(*row.ResultJSON)
			if err != nil || !bytes.Equal(previous, bounded) {
				return ErrApprovalConflict
			}
			if row.Status == "executed" {
				var task VerifyTask
				if err := tx.WithContext(ctx).First(&task, "approval_id = ?", row.ID).Error; err != nil {
					return err
				}
			}
			return nil
		}
		snapshot, err := incidentrule.ParseExecutionContext(row.ExecutionContext)
		if err != nil {
			return err
		}
		if (completion.Status == "executed" && snapshot.DryRun) || (completion.Status == "simulated" && !snapshot.DryRun) {
			return errors.New("store: execution mode differs from approved snapshot")
		}
		if err := tx.WithContext(ctx).Model(&Approval{}).Where("id = ? AND status = ?", row.ID, "executing").Updates(map[string]any{"status": completion.Status, "result_json": bounded}).Error; err != nil {
			return err
		}
		eventType := eventlog.EventExecutionCompleted
		if completion.Status == "simulated" {
			eventType = eventlog.EventExecutionSimulated
		}
		if completion.Status == "failed" {
			eventType = eventlog.EventExecutionFailed
		}
		if err := appendApprovalEvent(ctx, tx, row, eventType, completion.Status, "approval execution "+completion.Status, now); err != nil {
			return err
		}
		if completion.Status == "executed" {
			task := VerifyTask{ApprovalID: row.ID, Status: "pending", NextCheckAt: now, DeadlineAt: now.Add(time.Duration(snapshot.Verification.WindowSeconds) * time.Second), CreatedAt: now}
			if err := tx.WithContext(ctx).Create(&task).Error; err != nil {
				return err
			}
			if err := appendApprovalEvent(ctx, tx, row, eventlog.EventVerifyQueued, "pending", "recovery verification queued", now); err != nil {
				return err
			}
		}
		if !snapshot.DryRun {
			var result struct {
				Output string `json:"output"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal(bounded, &result); err != nil {
				return err
			}
			brief := completion.Status + ": " + result.Output
			if result.Error != "" {
				brief += " " + result.Error
			}
			history := FaultCmdHistory{Fingerprint: incidentrule.FaultFingerprint(parent.GroupKey, incidentrule.SupportedAlert), ToolName: row.ToolName, ArgsJSON: row.ArgsJSON, ResultBrief: truncateStoreText(brief, 1024), ApprovalID: &row.ID, CreatedAt: now}
			if err := tx.WithContext(ctx).Create(&history).Error; err != nil {
				return err
			}
		}
		if completion.Status == "failed" {
			for _, code := range []string{"execution_failed", "manual_check"} {
				if _, err := openIncidentProblem(ctx, tx, IncidentProblem{IncidentID: row.IncidentID, RunID: &row.RunID, Code: code, Severity: "error", Summary: "execution failed or result uncertain; manual verification required", FirstSeenAt: now, LastSeenAt: now}); err != nil {
					return err
				}
			}
		} else if _, err := resolveIncidentProblem(ctx, tx, row.IncidentID, "execution_failed", &row.RunID, now); err != nil {
			return err
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
}

// RecoverExecutingApprovals is intentionally conservative and single-instance only.
// A committed executed/simulated row is never reclassified or re-executed.
func (db *DB) RecoverExecutingApprovals(ctx context.Context, now time.Time) (recovered int64, err error) {
	if now.IsZero() {
		return 0, errors.New("store: recovery time required")
	}
	var rows []Approval
	if err := db.WithContext(ctx).Where("status = ?", "executing").Order("id ASC").Find(&rows).Error; err != nil {
		return 0, err
	}
	for _, candidate := range rows {
		changed := false
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			row, _, err := lockApproval(ctx, tx, candidate.ID)
			if err != nil {
				return err
			}
			if row.Status != "executing" {
				return nil
			}
			result := datatypes.JSON(`{"error":"executor interrupted; manual verification required","manual_check":true}`)
			if err := tx.WithContext(ctx).Model(&Approval{}).Where("id = ? AND status = ?", row.ID, "executing").Updates(map[string]any{"status": "failed", "result_json": result}).Error; err != nil {
				return err
			}
			if err := appendApprovalEvent(ctx, tx, row, eventlog.EventExecutionFailed, "failed", "execution interrupted; manual verification required", now); err != nil {
				return err
			}
			for _, code := range []string{"manual_check", "execution_failed"} {
				if _, err := openIncidentProblem(ctx, tx, IncidentProblem{IncidentID: row.IncidentID, RunID: &row.RunID, Code: code, Severity: "critical", Summary: "manual verification required after interrupted execution", FirstSeenAt: now, LastSeenAt: now}); err != nil {
					return err
				}
			}
			changed = true
			return nil
		})
		if err != nil {
			return recovered, err
		}
		if changed {
			recovered++
		}
	}
	return recovered, nil
}

const maxExecutionResultBytes = 8192

// result is canonical JSON. Retain content identity even when the body is omitted.
func boundedExecutionResult(result []byte) datatypes.JSON {
	if len(result) <= maxExecutionResultBytes {
		return datatypes.JSON(result)
	}
	digest := sha256.Sum256(result)
	bounded, _ := json.Marshal(map[string]any{"truncated": true, "reason": "execution result omitted", "result_sha256": fmt.Sprintf("%x", digest)})
	return datatypes.JSON(bounded)
}

// CountRecentExecutions deliberately does not use PlanHash: snapshot membership
// and timing change the content hash, but must not reset the action/target budget.
func (db *DB) CountRecentExecutions(ctx context.Context, toolName, targetName string, since time.Time) (int, error) {
	if strings.TrimSpace(toolName) == "" || strings.TrimSpace(targetName) == "" {
		return 0, errors.New("store: rate limit tool and target required")
	}
	var count int64
	err := db.WithContext(ctx).Table("approval").Distinct("approval.id").
		Joins("JOIN incident_event ON incident_event.approval_id = approval.id AND incident_event.event_type = ?", eventlog.EventExecutionStarted).
		Where("approval.tool_name = ? AND JSON_UNQUOTE(JSON_EXTRACT(approval.args_json, '$.target_name')) = ? AND incident_event.created_at >= ?", toolName, targetName, since).
		Where("approval.execution_context IS NULL OR JSON_UNQUOTE(JSON_EXTRACT(approval.execution_context, '$.dry_run')) = 'false'").Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("store: count executions: %w", err)
	}
	return int(count), nil
}
