package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// fault_memory 故障记忆与 fault_cmd_history 执行历史。

// InsertFaultCmdHistory 记录一次已审批动作的执行结果（D13 的诊断注入源）。
func (db *DB) InsertFaultCmdHistory(ctx context.Context, row FaultCmdHistory) error {
	if row.Fingerprint == "" || row.ToolName == "" {
		return errors.New("store: cmd history fingerprint and tool are required")
	}
	if row.CreatedAt.IsZero() {
		return errors.New("store: cmd history time is required")
	}
	if err := db.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("store: insert cmd history: %w", err)
	}
	return nil
}

// ErrMemoryNotFound 是故障记忆未命中的哨兵错误。
var ErrMemoryNotFound = errors.New("store: fault memory not found")

// GetFaultMemory 按键取记忆条目。
func (db *DB) GetFaultMemory(ctx context.Context, fingerprint string) (FaultMemory, error) {
	var entry FaultMemory
	err := db.WithContext(ctx).First(&entry, "fingerprint = ?", fingerprint).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return FaultMemory{}, ErrMemoryNotFound
	}
	if err != nil {
		return FaultMemory{}, fmt.Errorf("store: get fault memory: %w", err)
	}
	return entry, nil
}

// UpsertFaultMemory 写入或覆盖记忆条目（同指纹复诊成功刷新内容）。
func (db *DB) UpsertFaultMemory(ctx context.Context, entry FaultMemory) error {
	if entry.Fingerprint == "" {
		return errors.New("store: fault memory fingerprint is required")
	}
	err := db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "fingerprint"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"group_key", "alert_name", "rca_text", "plan_json", "confidence", "last_success", "ttl_sec",
		}),
	}).Create(&entry).Error
	if err != nil {
		return fmt.Errorf("store: upsert fault memory: %w", err)
	}
	return nil
}

// TouchFaultMemory 命中计数：hits+1、刷新 last_used。安全等级不变。
func (db *DB) TouchFaultMemory(ctx context.Context, fingerprint string, now time.Time) error {
	result := db.WithContext(ctx).Model(&FaultMemory{}).Where("fingerprint = ?", fingerprint).
		Updates(map[string]any{"hits": gorm.Expr("hits + 1"), "last_used": now})
	if result.Error != nil {
		return fmt.Errorf("store: touch fault memory: %w", result.Error)
	}
	return nil
}

// DemoteFaultMemory 把置信度降为 low（验证失败拉黑）。last_used 同时刷新，
// 让"这条记忆坑过人"在审计里可见。
func (db *DB) DemoteFaultMemory(ctx context.Context, fingerprint string, now time.Time) error {
	result := db.WithContext(ctx).Model(&FaultMemory{}).Where("fingerprint = ?", fingerprint).
		Updates(map[string]any{"confidence": "low", "last_used": now})
	if result.Error != nil {
		return fmt.Errorf("store: demote fault memory: %w", result.Error)
	}
	return nil
}

// ListCmdHistory 按时间倒序取同指纹的最近命令历史。
func (db *DB) ListCmdHistory(ctx context.Context, fingerprint string, limit int) ([]FaultCmdHistory, error) {
	if limit <= 0 {
		limit = 5
	}
	rows := make([]FaultCmdHistory, 0)
	err := db.WithContext(ctx).Where("fingerprint = ?", fingerprint).
		Order("id DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("store: list cmd history: %w", err)
	}
	return rows, nil
}
