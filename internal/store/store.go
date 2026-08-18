package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrInvalidRawEvent = errors.New("store: raw event payload must be valid JSON")

type DedupResult string

const (
	DedupNew     DedupResult = "new"
	DedupFull    DedupResult = "full"
	DedupPartial DedupResult = "partial"
)

// AlertInput is the store-facing form of a normalized alert. It avoids a
// package cycle between store and ingest while keeping persistence in store.
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

// DB 持有进程级 GORM 连接。D01 复用这一条；后续包不要自己再开连接池。
type DB struct {
	*gorm.DB
}

// Open 只建连。表结构走 migrations/001_init.sql，绝不 AutoMigrate。
func Open(dsn string) (*DB, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("store: mysql DSN is required")
	}

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("store: open MySQL: %w", err)
	}
	return &DB{DB: db}, nil
}

// Close 关闭底层连接池。
func (db *DB) Close() error {
	if db == nil || db.DB == nil {
		return nil
	}

	sqlDB, err := db.DB.DB()
	if err != nil {
		return fmt.Errorf("store: access SQL database: %w", err)
	}
	return sqlDB.Close()
}
func (db *DB) CreateRawEvent(ctx context.Context, source string, payload []byte, createdAt time.Time) (RawEvent, error) {
	if len(bytes.TrimSpace(payload)) == 0 || !json.Valid(payload) {
		return RawEvent{}, ErrInvalidRawEvent
	}
	event := RawEvent{Source: source, Payload: datatypes.JSON(append([]byte(nil), payload...)), Status: "pending", CreatedAt: createdAt}
	if err := db.WithContext(ctx).Create(&event).Error; err != nil {
		return RawEvent{}, fmt.Errorf("store: create raw event: %w", err)
	}
	return event, nil
}

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

func (db *DB) MarkRawEventFailed(ctx context.Context, id uint64, message string, processedAt time.Time) error {
	updates := map[string]any{"status": "failed", "processed_at": processedAt, "error": message}
	if err := db.WithContext(ctx).Model(&RawEvent{}).Where("id = ? AND status = ?", id, "pending").Updates(updates).Error; err != nil {
		return fmt.Errorf("store: mark raw event failed: %w", err)
	}
	return nil
}

// ApplyRawEvent atomically applies every alert and marks its raw event processed.
// A failed transaction leaves the raw event pending for the worker to retry.
func (db *DB) ApplyRawEvent(ctx context.Context, rawEventID uint64, inputs []AlertInput, processedAt time.Time) ([]DedupResult, error) {
	results := make([]DedupResult, 0, len(inputs))
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var event RawEvent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND status = ?", rawEventID, "pending").First(&event).Error; err != nil {
			return fmt.Errorf("lock pending raw event: %w", err)
		}
		for _, input := range inputs {
			result, err := applyAlert(tx, input)
			if err != nil {
				return err
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

func applyAlert(tx *gorm.DB, input AlertInput) (DedupResult, error) {
	if input.Fingerprint == "" || input.AlertHash == "" || input.Name == "" {
		return "", errors.New("store: alert identity is required")
	}
	labels, err := json.Marshal(input.Labels)
	if err != nil {
		return "", fmt.Errorf("store: encode alert labels: %w", err)
	}
	annotations, err := json.Marshal(input.Annotations)
	if err != nil {
		return "", fmt.Errorf("store: encode alert annotations: %w", err)
	}
	alert := Alert{
		Fingerprint: input.Fingerprint, AlertHash: input.AlertHash, Source: input.Source,
		Name: input.Name, Severity: uint8(input.Severity), Status: input.Status,
		Labels: datatypes.JSON(labels), Annotations: datatypes.JSON(annotations),
		GeneratorURL: input.GeneratorURL, StartsAt: input.StartsAt, ReceivedAt: input.ReceivedAt,
	}

	var last LastAlert
	queryErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("fingerprint = ?", input.Fingerprint).First(&last).Error
	if errors.Is(queryErr, gorm.ErrRecordNotFound) {
		if err := tx.Create(&alert).Error; err != nil {
			return "", fmt.Errorf("insert alert: %w", err)
		}
		last = LastAlert{Fingerprint: input.Fingerprint, AlertID: alert.ID, AlertHash: input.AlertHash, Status: input.Status, Severity: uint8(input.Severity), FirstSeen: input.ReceivedAt, LastSeen: input.ReceivedAt, FiringCount: 1}
		if err := tx.Create(&last).Error; err != nil {
			return "", fmt.Errorf("insert last alert: %w", err)
		}
		return DedupNew, nil
	}
	if queryErr != nil {
		return "", fmt.Errorf("read last alert: %w", queryErr)
	}
	if last.AlertHash == input.AlertHash {
		if err := tx.Model(&LastAlert{}).Where("fingerprint = ?", input.Fingerprint).Update("last_seen", input.ReceivedAt).Error; err != nil {
			return "", fmt.Errorf("refresh full duplicate: %w", err)
		}
		return DedupFull, nil
	}
	if err := tx.Create(&alert).Error; err != nil {
		return "", fmt.Errorf("insert alert: %w", err)
	}
	updates := map[string]any{
		"alert_id": alert.ID, "alert_hash": input.AlertHash, "status": input.Status,
		"severity": input.Severity, "last_seen": input.ReceivedAt,
		"firing_count": gorm.Expr("firing_count + ?", 1),
	}
	if err := tx.Model(&LastAlert{}).Where("fingerprint = ?", input.Fingerprint).Updates(updates).Error; err != nil {
		return "", fmt.Errorf("update partial duplicate: %w", err)
	}
	return DedupPartial, nil
}
