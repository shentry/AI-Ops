package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"oncall-agent/internal/eventlog"
)

// 执行闸门：approved → executing → executed/failed 的 CAS 推进，
// 结果有界落库，崩溃恢复只留痕不重放。

// NextApprovedApproval 取执行队列队首（id 最小且未过期的 approved 单）。
// 过期 approved 会被 ExpireApprovals 扫走；扫走前也不能堵死队列头。
func (db *DB) NextApprovedApproval(ctx context.Context, now time.Time) (Approval, bool, error) {
	var approval Approval
	err := db.WithContext(ctx).Where("status = ? AND expires_at > ?", "approved", now).Order("id ASC").First(&approval).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, fmt.Errorf("store: get next approved approval: %w", err)
	}
	return approval, true, nil
}

// ClaimApprovalExecution 把 approved 原子置为 executing，并与 execution.started
// 事实事件同一短事务提交。重复领取或过期审批返回 claimed=false。
func (db *DB) ClaimApprovalExecution(ctx context.Context, id uint64, now time.Time) (claimed bool, err error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var approval Approval
		query := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", id).First(&approval)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if query.Error != nil {
			return fmt.Errorf("store: lock approval for execution: %w", query.Error)
		}
		if approval.Status != "approved" || !approval.ExpiresAt.After(now) {
			return nil
		}
		result := tx.WithContext(ctx).Model(&Approval{}).
			Where("id = ? AND status = ? AND expires_at > ?", id, "approved", now).
			Update("status", "executing")
		if result.Error != nil {
			return fmt.Errorf("store: claim approval execution: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return nil
		}
		runID, approvalID := approval.RunID, approval.ID
		if _, eventErr := appendIncidentEvent(ctx, tx, IncidentEvent{
			IncidentID:  approval.IncidentID,
			RunID:       &runID,
			ApprovalID:  &approvalID,
			EventType:   string(eventlog.EventExecutionStarted),
			Phase:       "execution",
			Status:      "executing",
			Summary:     "approval execution started",
			PayloadJSON: executionStatusPayload("executing"),
			CreatedAt:   now,
		}); eventErr != nil {
			return eventErr
		}
		claimed = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("store: claim approval execution transaction: %w", err)
	}
	return claimed, nil
}

// FinishApprovalExecution 在一笔短事务中完成 executing→executed/failed 的 CAS，
// 保存有界结果，并追加 execution 事件及 execution_failed 问题变更。
func (db *DB) FinishApprovalExecution(ctx context.Context, id uint64, status string, resultJSON []byte) error {
	if status != "executed" && status != "failed" {
		return errors.New("store: approval execution status must be executed or failed")
	}
	now := time.Now().UTC()
	boundedResult := boundedExecutionResult(resultJSON)
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var approval Approval
		query := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", id).First(&approval)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return fmt.Errorf("store: approval %d not found", id)
		}
		if query.Error != nil {
			return fmt.Errorf("store: lock approval for finish: %w", query.Error)
		}
		if approval.Status != "executing" {
			return fmt.Errorf("store: approval %d is not executing", id)
		}
		updates := map[string]any{"status": status}
		if boundedResult != nil {
			updates["result_json"] = boundedResult
		}
		result := tx.WithContext(ctx).Model(&Approval{}).
			Where("id = ? AND status = ?", id, "executing").Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("store: finish approval execution: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("store: approval %d is not executing", id)
		}
		runID, approvalID := approval.RunID, approval.ID
		eventType := eventlog.EventExecutionCompleted
		summary := "approval execution completed"
		if status == "failed" {
			eventType = eventlog.EventExecutionFailed
			summary = "approval execution failed"
		}
		if _, eventErr := appendIncidentEvent(ctx, tx, IncidentEvent{
			IncidentID:  approval.IncidentID,
			RunID:       &runID,
			ApprovalID:  &approvalID,
			EventType:   string(eventType),
			Phase:       "execution",
			Status:      status,
			Summary:     summary,
			PayloadJSON: executionStatusPayload(status),
			CreatedAt:   now,
		}); eventErr != nil {
			return eventErr
		}
		if status == "failed" {
			if _, problemErr := openIncidentProblem(ctx, tx, IncidentProblem{
				IncidentID:  approval.IncidentID,
				RunID:       &runID,
				Code:        "execution_failed",
				Severity:    "error",
				Status:      "open",
				Summary:     "approval execution failed",
				FirstSeenAt: now,
				LastSeenAt:  now,
			}); problemErr != nil {
				return problemErr
			}
		} else if _, problemErr := resolveIncidentProblem(ctx, tx, approval.IncidentID, "execution_failed", &runID, now); problemErr != nil {
			return problemErr
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: finish approval execution transaction: %w", err)
	}
	return nil
}

// RecoverExecutingApprovals 逐行锁定 executing 审批并标记 failed。
// executing 可能已经触达外部系统，恢复阶段绝不自动重放，只写人工核查问题。
func (db *DB) RecoverExecutingApprovals(ctx context.Context, now time.Time) (recovered int64, err error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	interrupted := []byte(`{"error":"executor interrupted; manual verification required","manual_check":true}`)
	b := boundedExecutionResult(interrupted)
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var approvals []Approval
		query := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("status = ?", "executing").Order("id ASC").Find(&approvals)
		if query.Error != nil {
			return fmt.Errorf("store: lock executing approvals: %w", query.Error)
		}
		for _, approval := range approvals {
			result := tx.WithContext(ctx).Model(&Approval{}).
				Where("id = ? AND status = ?", approval.ID, "executing").
				Updates(map[string]any{"status": "failed", "result_json": b})
			if result.Error != nil {
				return fmt.Errorf("store: recover approval %d: %w", approval.ID, result.Error)
			}
			if result.RowsAffected != 1 {
				continue
			}
			runID, approvalID := approval.RunID, approval.ID
			if _, eventErr := appendIncidentEvent(ctx, tx, IncidentEvent{
				IncidentID:  approval.IncidentID,
				RunID:       &runID,
				ApprovalID:  &approvalID,
				EventType:   string(eventlog.EventExecutionFailed),
				Phase:       "execution",
				Status:      "failed",
				Summary:     "approval execution interrupted; manual verification required",
				PayloadJSON: executionManualPayload(),
				CreatedAt:   now,
			}); eventErr != nil {
				return eventErr
			}
			for _, problem := range []IncidentProblem{
				{IncidentID: approval.IncidentID, RunID: &runID, Code: "manual_check", Severity: "critical", Status: "open", Summary: "manual verification required after interrupted execution", FirstSeenAt: now, LastSeenAt: now},
				{IncidentID: approval.IncidentID, RunID: &runID, Code: "execution_failed", Severity: "error", Status: "open", Summary: "approval execution interrupted", FirstSeenAt: now, LastSeenAt: now},
			} {
				if _, problemErr := openIncidentProblem(ctx, tx, problem); problemErr != nil {
					return problemErr
				}
			}
			recovered++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: recover executing approvals transaction: %w", err)
	}
	return recovered, nil
}

const maxExecutionResultBytes = 8192

// boundedExecutionResult keeps result_json valid JSON and below the audit budget.
// Oversized or malformed external output is represented by a fixed safe marker.
func boundedExecutionResult(result []byte) datatypes.JSON {
	if len(result) == 0 {
		return nil
	}
	if len(result) <= maxExecutionResultBytes && json.Valid(result) {
		return datatypes.JSON(append([]byte(nil), result...))
	}
	return datatypes.JSON([]byte(`{"truncated":true,"reason":"execution result omitted"}`))
}

func executionStatusPayload(status string) *datatypes.JSON {
	payload := datatypes.JSON([]byte(fmt.Sprintf(`{"status":%q}`, status)))
	return &payload
}

func executionManualPayload() *datatypes.JSON {
	payload := datatypes.JSON([]byte(`{"manual_check":true}`))
	return &payload
}

// CountRecentExecutions 数同一 plan_hash（同 tool + 同 target）在 since 之后
// 进入过执行的审批单。L2 限频护栏用它判断"这个动作最近做过几次"。
// 统计包含 failed：反复失败的重复动作正是限频要拦的（配置错/凭据错重启无救）。
func (db *DB) CountRecentExecutions(ctx context.Context, planHash string, since time.Time) (int, error) {
	if strings.TrimSpace(planHash) == "" {
		return 0, errors.New("store: plan hash is required")
	}
	var count int64
	err := db.WithContext(ctx).Model(&Approval{}).
		Where("plan_hash = ? AND status IN ? AND created_at >= ?",
			planHash, []string{"executing", "executed", "failed"}, since).
		Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("store: count recent executions: %w", err)
	}
	return int(count), nil
}
