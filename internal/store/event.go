package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// incident_event 审计流。写入方分两路：*DB 直写（单条独立事务）
// 与 appendIncidentEvent（复用调用方事务，跨域收尾时用）。

// EventProblemWriter 是事务内写入 Incident 事实事件和当前问题读模型的窄接口。
// IncidentTx 嵌入该接口，保证事件/问题和告警、Incident、run 同一事务提交。
type EventProblemWriter interface {
	AppendIncidentEvent(context.Context, IncidentEvent) (IncidentEvent, error)
	OpenIncidentProblem(context.Context, IncidentProblem) (IncidentProblem, error)
	ResolveIncidentProblem(context.Context, uint64, string, *uint64, time.Time) (bool, error)
}

// appendIncidentEvent 是 DB 与 transactionIncidentTx 共用的事件写入实现。
// 调用方负责在进入这里前完成 Sanitization；store 只拒绝明显无效的 JSON。
func appendIncidentEvent(ctx context.Context, q *gorm.DB, event IncidentEvent) (IncidentEvent, error) {
	if event.IncidentID == 0 {
		return IncidentEvent{}, errors.New("store: incident event incident is required")
	}
	if strings.TrimSpace(event.EventType) == "" || strings.TrimSpace(event.Phase) == "" || strings.TrimSpace(event.Status) == "" {
		return IncidentEvent{}, errors.New("store: incident event type, phase and status are required")
	}
	event.Summary = truncateStoreText(event.Summary, 512)
	if strings.TrimSpace(event.Summary) == "" {
		return IncidentEvent{}, errors.New("store: incident event summary is required")
	}
	if event.CreatedAt.IsZero() {
		return IncidentEvent{}, errors.New("store: incident event created_at is required")
	}
	if event.PayloadJSON != nil && len(*event.PayloadJSON) > 0 && !json.Valid(*event.PayloadJSON) {
		return IncidentEvent{}, errors.New("store: incident event payload must be valid JSON")
	}
	event.CreatedAt = event.CreatedAt.UTC()
	if err := q.WithContext(ctx).Create(&event).Error; err != nil {
		return IncidentEvent{}, fmt.Errorf("store: append incident event: %w", err)
	}
	return event, nil
}

// AppendIncidentEvent 写入非事务路径的 Incident 事件。
func (db *DB) AppendIncidentEvent(ctx context.Context, event IncidentEvent) (IncidentEvent, error) {
	return appendIncidentEvent(ctx, db.DB, event)
}

// AppendIncidentEvent 在 ApplyRawEvent 事务内追加事实事件。
func (t *transactionIncidentTx) AppendIncidentEvent(ctx context.Context, event IncidentEvent) (IncidentEvent, error) {
	return appendIncidentEvent(ctx, t.db, event)
}

// ListIncidentEvents 按严格 id > afterID、id ASC 读取 Incident 事实事件。
func (db *DB) ListIncidentEvents(ctx context.Context, incidentID, afterID uint64, limit int) ([]IncidentEvent, error) {
	if incidentID == 0 {
		return nil, errors.New("store: incident event incident is required")
	}
	rows := make([]IncidentEvent, 0)
	err := db.WithContext(ctx).Where("incident_id = ? AND id > ?", incidentID, afterID).
		Order("id ASC").Limit(normalizePageLimit(limit)).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("store: list incident events: %w", err)
	}
	return rows, nil
}

// ListLatestIncidentEvents 取 Incident 最新 N 条事件，仍按 id ASC 返回，
// 供控制室首屏使用。SSE 回放继续走 ListIncidentEvents 的 afterID 游标。
func (db *DB) ListLatestIncidentEvents(ctx context.Context, incidentID uint64, limit int) ([]IncidentEvent, error) {
	if incidentID == 0 {
		return nil, errors.New("store: incident event incident is required")
	}
	limit = normalizePageLimit(limit)
	desc := make([]IncidentEvent, 0, limit)
	err := db.WithContext(ctx).Where("incident_id = ?", incidentID).
		Order("id DESC").Limit(limit).Find(&desc).Error
	if err != nil {
		return nil, fmt.Errorf("store: list latest incident events: %w", err)
	}
	for i, j := 0, len(desc)-1; i < j; i, j = i+1, j-1 {
		desc[i], desc[j] = desc[j], desc[i]
	}
	return desc, nil
}
