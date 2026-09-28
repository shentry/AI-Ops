package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// raw_event 摄入侧：落原文、取 pending、按指纹去重应用。
// 事务边界在 ApplyRawEvent —— 一条报文的全部副作用要么全落要么全回滚。

// ErrInvalidRawEvent 是"报文不是合法 JSON"的哨兵错误。HTTP 层用 errors.Is
// 区分输入问题和存储故障：前者回 400 怪调用方，后者回 503 怪自己。
var ErrInvalidRawEvent = errors.New("store: raw event payload must be valid JSON")

// Pending admission is part of the durable write, including across server processes.
const maxPendingRawEvents = 1000

var ErrRawEventQueueFull = errors.New("store: raw event queue is full")

// ErrInvalidAlertInput identifies permanent input failures; database errors remain retryable.
var ErrInvalidAlertInput = errors.New("store: invalid alert input")

// ValidateAlertInput checks the actual alert/last_alert column limits before any writes.
func ValidateAlertInput(input AlertInput) error {
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{"fingerprint", input.Fingerprint, 64},
		{"alert_hash", input.AlertHash, 32},
		{"source", input.Source, 64},
		{"name", input.Name, 255},
	} {
		if strings.TrimSpace(field.value) == "" || utf8.RuneCountInString(field.value) > field.limit {
			return fmt.Errorf("%w: %s must contain 1..%d characters", ErrInvalidAlertInput, field.name, field.limit)
		}
	}
	if len(input.GeneratorURL) > 65535 {
		return fmt.Errorf("%w: generatorURL exceeds TEXT capacity", ErrInvalidAlertInput)
	}
	if input.Status != "firing" && input.Status != "resolved" {
		return fmt.Errorf("%w: unsupported status", ErrInvalidAlertInput)
	}
	if input.Severity < 0 || input.Severity > 127 {
		return fmt.Errorf("%w: severity exceeds TINYINT range", ErrInvalidAlertInput)
	}
	for _, timestamp := range []time.Time{input.StartsAt, input.ReceivedAt} {
		// DATETIME(3) rounds fractions, so the upper half millisecond would overflow.
		utc := timestamp.UTC()
		if utc.Year() < 1000 || utc.Year() > 9999 || !utc.Before(time.Date(9999, 12, 31, 23, 59, 59, 999500000, time.UTC)) {
			return fmt.Errorf("%w: timestamp outside DATETIME(3) range", ErrInvalidAlertInput)
		}
	}
	return nil
}

// DedupResult 是新到告警与同指纹 last_alert 比对后的三种结论，
// 下游（worker 的 hook）按它决定建 incident 还是只续心跳。
type DedupResult string

const (
	// DedupNew：指纹第一次出现，alert 和 last_alert 都是新插的。
	DedupNew DedupResult = "new"
	// DedupFull：AlertHash 完全一致 —— 内容一模一样的重复推送
	//（多半来自 Alertmanager 的 repeat_interval 重发）。不插 alert 行。
	DedupFull DedupResult = "full"
	// DedupPartial：指纹相同但内容变了（severity 升级、annotation 更新……）。
	// 追加一条新版本 alert，last_alert 改指到它。
	DedupPartial DedupResult = "partial"
)

// AlertInput 是 ingest.NormalizedAlert 的 store 侧镜像。依赖方向只能是
// ingest → store，store 反过来 import ingest 会成环，所以持久化层
// 自己声明一份入参结构，字段一一对应。
type AlertInput struct {
	Fingerprint  string
	AlertHash    string
	Source       string
	Name         string
	Severity     int
	Status       string
	Labels       map[string]string
	Annotations  map[string]string
	GeneratorURL string
	StartsAt     time.Time
	ReceivedAt   time.Time
}

// AlertApplyResult 是单条告警 apply 之后回给 hook 的结果：
// 输入本身 + 去重结论 + apply 之后（不是之前）的 last_alert 快照，
// hook 拿 Last.IncidentID 就知道这条告警当前挂在哪个 incident 上。
type AlertApplyResult struct {
	Input AlertInput
	Dedup DedupResult
	Last  LastAlert
}

// RawEventApplyHook 在每条告警 apply 完、事务提交前被调用。
// 返回 error 会连累整条 raw_event 回滚，所以这里只该放
// "必须和 alert 落库同生共死"的逻辑 —— D04 的 incident 关联正是。
type RawEventApplyHook func(context.Context, IncidentTx, AlertApplyResult) error

// CreateRawEvent validates JSON and admits a pending event only if capacity remains.
// Locking the indexed pending range at REPEATABLE READ prevents concurrent admissions
// from exceeding the cap, even when the queue is empty. Lock/deadlock errors are
// returned to the webhook as retryable failures; 202 requires a committed insert.
func (db *DB) CreateRawEvent(ctx context.Context, source string, payload []byte, createdAt time.Time) (RawEvent, error) {
	if len(bytes.TrimSpace(payload)) == 0 || !json.Valid(payload) {
		return RawEvent{}, ErrInvalidRawEvent
	}
	event := RawEvent{Source: source, Payload: datatypes.JSON(append([]byte(nil), payload...)), Status: "pending", CreatedAt: createdAt}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var pending []uint64
		if err := tx.Model(&RawEvent{}).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("status = ?", "pending").Limit(maxPendingRawEvents).Pluck("id", &pending).Error; err != nil {
			return err
		}
		if len(pending) >= maxPendingRawEvents {
			return ErrRawEventQueueFull
		}
		return tx.Create(&event).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return RawEvent{}, fmt.Errorf("store: create raw event: %w", err)
	}
	return event, nil
}

// NextPendingRawEvent 取队首（id 最小的 pending 行）。队列空是正常状态，
// 返回 found=false 而不是 error —— worker 的 drain 靠它判断"排空了，收手"。
func (db *DB) NextPendingRawEvent(ctx context.Context) (RawEvent, bool, error) {
	var event RawEvent
	err := db.WithContext(ctx).Where("status = ?", "pending").Order("id ASC").First(&event).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RawEvent{}, false, nil
	}
	if err != nil {
		return RawEvent{}, false, fmt.Errorf("store: get next pending raw event: %w", err)
	}
	return event, true, nil
}

// MarkRawEventFailed 给报文本身没救的 raw_event 判死刑并落原因。
// WHERE 带 status='pending'：只允许 pending → failed 这一条路，
// 终态（processed/failed）不会被并发的另一轮处理覆盖。
func (db *DB) MarkRawEventFailed(ctx context.Context, id uint64, message string, processedAt time.Time) error {
	updates := map[string]any{"status": "failed", "processed_at": processedAt, "error": message}
	if err := db.WithContext(ctx).Model(&RawEvent{}).Where("id = ? AND status = ?", id, "pending").Updates(updates).Error; err != nil {
		return fmt.Errorf("store: mark raw event failed: %w", err)
	}
	return nil
}

// ApplyRawEvent 在单个事务里完成：锁住 pending 的 raw_event → 逐条
// applyAlert（每条之后跑一次关联 hook）→ 最后标 processed。
// 任何一步失败整条回滚，raw_event 保持 pending 等 worker 重试 ——
// 它和 MarkRawEventFailed 正是 worker 那套二分法的落库侧：
// 报文没救走 failed，库出问题走这里的回滚重试。
func (db *DB) ApplyRawEvent(ctx context.Context, rawEventID uint64, inputs []AlertInput, processedAt time.Time, hook RawEventApplyHook) ([]AlertApplyResult, error) {
	for _, input := range inputs {
		if err := ValidateAlertInput(input); err != nil {
			return nil, err
		}
	}
	results := make([]AlertApplyResult, 0, len(inputs))
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var event RawEvent
		// FOR UPDATE + status=pending 双保险：锁住行并重验状态，
		// 同一条 raw_event 不可能被应用两次 —— 等到锁时状态已不是
		// pending 的话，这里查不到行，直接报错回滚。
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND status = ?", rawEventID, "pending").First(&event).Error; err != nil {
			return fmt.Errorf("lock pending raw event: %w", err)
		}
		incidentTx := &transactionIncidentTx{db: tx}
		for _, input := range inputs {
			result, err := applyAlert(tx, input)
			if err != nil {
				return err
			}
			if hook != nil {
				if err := hook(ctx, incidentTx, result); err != nil {
					return fmt.Errorf("apply raw event hook: %w", err)
				}
			}
			results = append(results, result)
		}
		updates := map[string]any{"status": "processed", "processed_at": processedAt, "error": nil}
		if err := tx.Model(&RawEvent{}).Where("id = ? AND status = ?", rawEventID, "pending").Updates(updates).Error; err != nil {
			return fmt.Errorf("mark raw event processed: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: apply raw event: %w", err)
	}
	return results, nil
}

// applyAlert 是去重核心：按指纹取 last_alert，和本次 AlertHash 比对，
// 三种结果分别对应 DedupNew / DedupFull / DedupPartial。必须在事务里跑：
// last_alert 的"读-比-写"不是原子的，靠 FOR UPDATE 串行化。
func applyAlert(tx *gorm.DB, input AlertInput) (AlertApplyResult, error) {
	labels, err := json.Marshal(input.Labels)
	if err != nil {
		return AlertApplyResult{}, fmt.Errorf("store: encode alert labels: %w", err)
	}
	annotations, err := json.Marshal(input.Annotations)
	if err != nil {
		return AlertApplyResult{}, fmt.Errorf("store: encode alert annotations: %w", err)
	}
	alert := Alert{
		Fingerprint: input.Fingerprint, AlertHash: input.AlertHash, Source: input.Source,
		Name: input.Name, Severity: uint8(input.Severity), Status: input.Status,
		Labels: datatypes.JSON(labels), Annotations: datatypes.JSON(annotations),
		GeneratorURL: input.GeneratorURL, StartsAt: input.StartsAt, ReceivedAt: input.ReceivedAt,
	}

	// FOR UPDATE 锁 last_alert 行：同一指纹并发到达时后到的等先到的提交，
	// 不会都读到"不存在"然后各插一份。
	var last LastAlert
	queryErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("fingerprint = ?", input.Fingerprint).First(&last).Error
	if errors.Is(queryErr, gorm.ErrRecordNotFound) {
		// 第一次见到这个指纹：alert 与 last_alert 各插一条，firing_count 从 1 起步。
		if err := tx.Create(&alert).Error; err != nil {
			return AlertApplyResult{}, fmt.Errorf("insert alert: %w", err)
		}
		last = LastAlert{Fingerprint: input.Fingerprint, AlertID: alert.ID, AlertHash: input.AlertHash, Status: input.Status, Severity: uint8(input.Severity), FirstSeen: input.ReceivedAt, LastSeen: input.ReceivedAt, FiringCount: 1}
		if err := tx.Create(&last).Error; err != nil {
			return AlertApplyResult{}, fmt.Errorf("insert last alert: %w", err)
		}
		return AlertApplyResult{Input: input, Dedup: DedupNew, Last: last}, nil
	}
	if queryErr != nil {
		return AlertApplyResult{}, fmt.Errorf("read last alert: %w", queryErr)
	}
	if last.AlertHash == input.AlertHash {
		// 内容一模一样（AlertHash 不含时间字段，repeat_interval 重发的就是这种）：
		// 不插 alert 行 —— 全量去重的意义就在这，告警持续期间 alert 表不膨胀。
		// 只续 last_seen，worker 拿最新时间去 TouchIncident 续 incident 的时间窗。
		if err := tx.Model(&LastAlert{}).Where("fingerprint = ?", input.Fingerprint).Update("last_seen", input.ReceivedAt).Error; err != nil {
			return AlertApplyResult{}, fmt.Errorf("refresh full duplicate: %w", err)
		}
		last.LastSeen = input.ReceivedAt
		return AlertApplyResult{Input: input, Dedup: DedupFull, Last: last}, nil
	}
	// A changed current alert also changes the scope of its existing Incident.
	// Lock it even when the correlator will choose a different group afterward.
	if last.IncidentID != nil {
		if _, err := lockIncident(tx.Statement.Context, tx, *last.IncidentID); err != nil {
			return AlertApplyResult{}, fmt.Errorf("lock incident before alert change: %w", err)
		}
	}
	// 同指纹不同内容：追加一条新版本 alert，last_alert 改指到它。
	// firing_count 用 SQL 原子自增（gorm.Expr）而不是读-改-写，并发下不丢计数。
	if err := tx.Create(&alert).Error; err != nil {
		return AlertApplyResult{}, fmt.Errorf("insert alert: %w", err)
	}
	updates := map[string]any{
		"alert_id": alert.ID, "alert_hash": input.AlertHash, "status": input.Status,
		"severity": input.Severity, "last_seen": input.ReceivedAt,
		"firing_count": gorm.Expr("firing_count + ?", 1),
	}
	if err := tx.Model(&LastAlert{}).Where("fingerprint = ?", input.Fingerprint).Updates(updates).Error; err != nil {
		return AlertApplyResult{}, fmt.Errorf("update partial duplicate: %w", err)
	}
	last.AlertID = alert.ID
	last.AlertHash = input.AlertHash
	last.Status = input.Status
	last.Severity = uint8(input.Severity)
	last.LastSeen = input.ReceivedAt
	last.FiringCount++
	return AlertApplyResult{Input: input, Dedup: DedupPartial, Last: last}, nil
}

// CountRawEventsByStatus 统计 raw_event 状态行数（/metrics 队列深度用）。
func (db *DB) CountRawEventsByStatus(ctx context.Context, status string) (int64, error) {
	var count int64
	if err := db.WithContext(ctx).Model(&RawEvent{}).Where("status = ?", status).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("store: count raw events: %w", err)
	}
	return count, nil
}
