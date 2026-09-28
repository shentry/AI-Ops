package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"oncall-agent/internal/eventlog"
)

// agent_run 队列的领取、超时恢复与读取。入队只走 RequestRun，
// 终态与审批发布只走 CompleteRun，避免绕过统一准入或审计事务。

// ErrAgentRunNotFound 是"诊断 run 不存在"的哨兵错误。Run/Step API 用它区分
// 不存在或不属于目标 Incident 的 run，避免把任意 run ID 当成授权凭据。
var ErrAgentRunNotFound = errors.New("store: agent run not found")

// ListAgentRuns 按严格 id > afterID、id ASC 读取目标 Incident 的诊断 run。
func (db *DB) ListAgentRuns(ctx context.Context, incidentID, afterID uint64, limit int) ([]AgentRun, error) {
	if incidentID == 0 {
		return nil, errors.New("store: agent run incident is required")
	}
	rows := make([]AgentRun, 0)
	err := db.WithContext(ctx).Where("incident_id = ? AND id > ?", incidentID, afterID).
		Order("id ASC").Limit(normalizePageLimit(limit)).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("store: list agent runs: %w", err)
	}
	return rows, nil
}

// NextPendingAgentRun 取诊断队列队首（id 最小的 pending run）。
// 队列空返回 found=false，与 NextPendingRawEvent 同语义。
func (db *DB) NextPendingAgentRun(ctx context.Context) (AgentRun, bool, error) {
	var run AgentRun
	err := db.WithContext(ctx).Where("status = ?", "pending").Order("id ASC").First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AgentRun{}, false, nil
	}
	if err != nil {
		return AgentRun{}, false, fmt.Errorf("store: get next pending agent run: %w", err)
	}
	return run, true, nil
}

// ClaimAgentRun 在短事务内锁定 pending run，读取其 Incident，完成
// pending → running 的 CAS，并把 run.started 与状态变更一起提交。
// 并发消费者或重启补账不会重复认领同一行；RowsAffected=0 表示已经被抢先。
func (db *DB) ClaimAgentRun(ctx context.Context, id uint64, startedAt time.Time) (claimed bool, err error) {
	if id == 0 {
		return false, errors.New("store: agent run id is required")
	}
	if startedAt.IsZero() {
		return false, errors.New("store: agent run start time is required")
	}
	startedAt = startedAt.UTC()

	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run AgentRun
		query := tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND status = ?", id, "pending").
			First(&run)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if query.Error != nil {
			return fmt.Errorf("lock pending agent run: %w", query.Error)
		}

		// The Incident lookup is intentionally inside the same transaction: event
		// rows must never be emitted for a run whose parent cannot be resolved.
		var incidentRow Incident
		query = tx.WithContext(ctx).Select("id").First(&incidentRow, run.IncidentID)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return fmt.Errorf("agent run %d incident %d not found", run.ID, run.IncidentID)
		}
		if query.Error != nil {
			return fmt.Errorf("read incident for agent run %d: %w", run.ID, query.Error)
		}

		result := tx.WithContext(ctx).Model(&AgentRun{}).
			Where("id = ? AND status = ?", id, "pending").
			Updates(map[string]any{"status": "running", "started_at": startedAt})
		if result.Error != nil {
			return fmt.Errorf("update claimed agent run: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return nil
		}

		runID := run.ID
		if _, err := appendIncidentEvent(ctx, tx, IncidentEvent{
			IncidentID: incidentRow.ID,
			RunID:      &runID,
			EventType:  string(eventlog.EventRunStarted),
			Phase:      "run",
			Status:     "running",
			Summary:    "diagnostic run started",
			CreatedAt:  startedAt,
		}); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("store: claim agent run: %w", err)
	}
	return claimed, nil
}

// RequeueStaleAgentRuns 在一个短事务内锁定超时 running 行，逐行执行
// running → pending，并为每行写 run.stalled 与幂等的 run_stalled 问题。
// 事务失败时状态、事件和问题全部回滚，下一轮仍可完整补账。
func (db *DB) RequeueStaleAgentRuns(ctx context.Context, staleBefore time.Time) (requeued int64, err error) {
	if staleBefore.IsZero() {
		return 0, errors.New("store: stale-before time is required")
	}
	staleBefore = staleBefore.UTC()

	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var runs []AgentRun
		if err := tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("status = ? AND started_at < ?", "running", staleBefore).
			Order("id ASC").
			Find(&runs).Error; err != nil {
			return fmt.Errorf("lock stale agent runs: %w", err)
		}
		if len(runs) == 0 {
			return nil
		}
		requeuedAt := time.Now().UTC()
		for _, run := range runs {
			var incidentRow Incident
			query := tx.WithContext(ctx).Select("id").First(&incidentRow, run.IncidentID)
			if errors.Is(query.Error, gorm.ErrRecordNotFound) {
				return fmt.Errorf("agent run %d incident %d not found", run.ID, run.IncidentID)
			}
			if query.Error != nil {
				return fmt.Errorf("read incident for stale agent run %d: %w", run.ID, query.Error)
			}

			result := tx.WithContext(ctx).Model(&AgentRun{}).
				Where("id = ? AND status = ?", run.ID, "running").
				Update("status", "pending")
			if result.Error != nil {
				return fmt.Errorf("requeue stale agent run %d: %w", run.ID, result.Error)
			}
			if result.RowsAffected == 0 {
				continue
			}

			runID := run.ID
			if _, err := appendIncidentEvent(ctx, tx, IncidentEvent{
				IncidentID: incidentRow.ID,
				RunID:      &runID,
				EventType:  string(eventlog.EventRunStalled),
				Phase:      "run",
				Status:     "pending",
				Summary:    "diagnostic run stalled and was requeued",
				CreatedAt:  requeuedAt,
			}); err != nil {
				return err
			}
			if _, err := openIncidentProblem(ctx, tx, IncidentProblem{
				IncidentID:  incidentRow.ID,
				RunID:       &runID,
				Code:        "run_stalled",
				Severity:    "warning",
				Status:      "open",
				Summary:     "diagnostic run stalled and was requeued",
				FirstSeenAt: requeuedAt,
				LastSeenAt:  requeuedAt,
			}); err != nil {
				return err
			}
			requeued++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: requeue stale agent runs: %w", err)
	}
	return requeued, nil
}

// GetAgentRun 按 id 取 run。重诊注入上一轮失败上下文用。
func (db *DB) GetAgentRun(ctx context.Context, id uint64) (AgentRun, error) {
	var run AgentRun
	err := db.WithContext(ctx).First(&run, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AgentRun{}, ErrAgentRunNotFound
	}
	if err != nil {
		return AgentRun{}, fmt.Errorf("store: get agent run: %w", err)
	}
	return run, nil
}

// UpdateAgentRunMode 在运行中改 mode（记忆命中后 full/light → memory_hit）。
// 只允许 running 状态改：终态行的 mode 是审计记录，不可改写。
func (db *DB) UpdateAgentRunMode(ctx context.Context, id uint64, mode string) error {
	result := db.WithContext(ctx).Model(&AgentRun{}).
		Where("id = ? AND status IN ?", id, []string{"pending", "running"}).
		Update("mode", mode)
	if result.Error != nil {
		return fmt.Errorf("store: update agent run mode: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("store: agent run %d is not pending/running", id)
	}
	return nil
}

// CountAgentRunsByStatus 统计 agent_run 状态行数。
func (db *DB) CountAgentRunsByStatus(ctx context.Context, status string) (int64, error) {
	var count int64
	if err := db.WithContext(ctx).Model(&AgentRun{}).Where("status = ?", status).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("store: count agent runs: %w", err)
	}
	return count, nil
}
