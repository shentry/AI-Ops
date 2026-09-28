package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"oncall-agent/internal/eventlog"
)

// CheckExecutionReady is read-only. Upgrading the binary must never implicitly
// decide old approvals, nor accept traffic before the queue schema is present.
func (db *DB) CheckExecutionReady(ctx context.Context) error {
	// Check the actual schema, not just whether an empty verify_task can be counted.
	// The migration files are applied manually; there is no schema-version table.
	var statusType string
	if err := db.WithContext(ctx).Raw(`SELECT column_type FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = 'approval' AND column_name = 'status'`).Scan(&statusType).Error; err != nil {
		return fmt.Errorf("inspect execution schema: %w", err)
	}
	if !strings.Contains(statusType, "'simulated'") {
		return errors.New("apply migration 009: approval status must support simulated")
	}
	if !strings.Contains(statusType, "'aborted'") {
		return errors.New("apply migration 014: approval status must support aborted")
	}
	var task VerifyTask
	if err := db.WithContext(ctx).Select("approval_id, status, next_check_at, deadline_at, claimed_at, last_checked_at, last_result_json, created_at, finished_at").Limit(1).Find(&task).Error; err != nil {
		return fmt.Errorf("apply migration 010: %w", err)
	}
	if err := db.WithContext(ctx).Select("consecutive_passes").Limit(1).Find(&task).Error; err != nil {
		return fmt.Errorf("apply migration 012: %w", err)
	}
	var snapshot DiagnosisSnapshot
	if err := db.WithContext(ctx).Limit(1).Find(&snapshot).Error; err != nil {
		return fmt.Errorf("apply migration 013: %w", err)
	}
	var approval Approval
	if err := db.WithContext(ctx).Select("service, rule_id, parent_approval_id, operation_id, operation_started_at").Limit(1).Find(&approval).Error; err != nil {
		return fmt.Errorf("apply migration 014: %w", err)
	}
	if err := db.WithContext(ctx).Select("phase, consecutive_failures").Limit(1).Find(&task).Error; err != nil {
		return fmt.Errorf("apply migration 014: %w", err)
	}
	for _, table := range []any{&ServiceLock{}, &ControlEvent{}, &ChangeEvent{}, &Review{}} {
		if err := db.WithContext(ctx).Model(table).Limit(1).Find(table).Error; err != nil {
			return fmt.Errorf("apply migration 014: %w", err)
		}
	}
	if err := db.WithContext(ctx).Limit(1).Find(&NotificationTask{}).Error; err != nil {
		return fmt.Errorf("apply migration 015: %w", err)
	}
	var count int64
	if err := db.WithContext(ctx).Raw(`SELECT COUNT(DISTINCT table_name, index_name) FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND (
		(table_name = 'verify_task' AND index_name IN ('idx_verify_due', 'idx_verify_claim')) OR
		(table_name = 'approval' AND index_name IN ('idx_approval_ready', 'idx_approval_incident_status')) OR
		(table_name = 'agent_run' AND index_name = 'idx_run_incident_status') OR
		(table_name = 'incident_event' AND index_name = 'idx_event_incident_type'))`).Scan(&count).Error; err != nil {
		return fmt.Errorf("inspect execution indexes: %w", err)
	}
	if count != 6 {
		return errors.New("apply migrations 010–011: execution queue/admission indexes are missing")
	}
	if err := db.WithContext(ctx).Model(&Approval{}).Where("execution_context IS NULL AND status IN ?", []string{"pending", "approved", "executing"}).Count(&count).Error; err != nil {
		return fmt.Errorf("apply approval execution-context migration: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("%d legacy active approvals require explicit retirement with cmd/retire-approvals before startup", count)
	}
	return nil
}

// RetireLegacyApprovals must only be called by the explicit offline maintenance
// command on a non-transactional DB handle. It works before or after schema 009;
// each approval and its audit/problem commit together. The count includes only
// acknowledged commits; after any error the command can safely be rerun.
func (db *DB) RetireLegacyApprovals(ctx context.Context, now time.Time) (int, error) {
	if now.IsZero() {
		return 0, errors.New("store: retirement time required")
	}
	now = now.UTC().Truncate(time.Millisecond)
	// HasColumn returns only bool and discards inspection errors. Never interpret
	// an unavailable schema as permission to retire approvals with modern snapshots.
	var contextColumns int64
	if err := db.WithContext(ctx).Raw(`SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = 'approval' AND column_name = 'execution_context'`).Scan(&contextColumns).Error; err != nil {
		return 0, fmt.Errorf("inspect legacy approval schema: %w", err)
	}
	query := db.WithContext(ctx).Where("status IN ?", []string{"pending", "approved", "executing"})
	if contextColumns != 0 {
		query = query.Where("execution_context IS NULL")
	}
	var candidates []Approval
	if err := query.Order("id ASC").Find(&candidates).Error; err != nil {
		return 0, err
	}
	count := 0
	for _, candidate := range candidates {
		retired := false
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			row, _, err := lockApproval(ctx, tx, candidate.ID)
			if err != nil {
				return err
			}
			if row.Status != "pending" && row.Status != "approved" && row.Status != "executing" {
				return nil
			}
			status, eventType, summary := "expired", eventlog.EventApprovalExpired, "legacy approval retired for execution-context upgrade"
			updates := map[string]any{"status": status}
			if row.Status == "executing" {
				status, eventType, summary = "failed", eventlog.EventExecutionFailed, "legacy execution outcome unknown during execution-context upgrade; manual verification required"
				updates["status"] = status
				updates["result_json"] = datatypes.JSON(`{"manual_check":true,"error":"legacy execution outcome unknown"}`)
			}
			update := tx.WithContext(ctx).Model(&Approval{}).Where("id = ? AND status = ?", row.ID, row.Status)
			if contextColumns != 0 {
				update = update.Where("execution_context IS NULL")
			}
			result := update.Updates(updates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return nil
			}
			if _, err := appendIncidentEvent(ctx, tx, IncidentEvent{IncidentID: row.IncidentID, RunID: &row.RunID, ApprovalID: &row.ID,
				EventType: string(eventType), Phase: "upgrade", Status: status, Summary: summary, CreatedAt: now}); err != nil {
				return err
			}
			if status == "failed" {
				if _, err := openIncidentProblem(ctx, tx, IncidentProblem{IncidentID: row.IncidentID, RunID: &row.RunID, Code: "manual_check", Severity: "critical", Summary: summary, FirstSeenAt: now, LastSeenAt: now}); err != nil {
					return err
				}
			}
			retired = true
			return nil
		})
		if err != nil {
			return count, fmt.Errorf("retire approval %d: %w", candidate.ID, err)
		}
		if retired {
			count++
		}
	}
	return count, nil
}
