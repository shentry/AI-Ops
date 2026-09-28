package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"oncall-agent/internal/eventlog"
)

// Knowledge sources: repository handbook sections and reviewed incidents.
const (
	KnowledgeRepo     = "repo"
	KnowledgeIncident = "incident"
)

var (
	ErrKnowledgeNotFound = errors.New("store: knowledge entry not found")
	// ErrKnowledgeReadOnly: repository entries change only through the repository.
	ErrKnowledgeReadOnly = errors.New("store: repository knowledge is read-only")
)

// KnowledgeEntry is one searchable section. Ref is "<file>#<heading>" for
// the repository and "incident/<id>" for a reviewed incident.
type KnowledgeEntry struct {
	ID        uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	Source    string    `gorm:"column:source;size:8;not null"`
	Ref       string    `gorm:"column:ref;size:255;not null"`
	Title     string    `gorm:"column:title;size:255;not null"`
	Body      string    `gorm:"column:body;type:text;not null"`
	SHA256    string    `gorm:"column:sha256;size:64;not null"`
	CreatedBy string    `gorm:"column:created_by;size:64;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
}

func (KnowledgeEntry) TableName() string { return "knowledge_entry" }

// KnowledgeHit is a search result with its full-text relevance.
type KnowledgeHit struct {
	KnowledgeEntry
	Score float64 `gorm:"column:score"`
}

// SyncRepoKnowledge makes the repository entries equal to entries: new refs
// are inserted, changed ones rewritten, refs no longer present deleted.
func (db *DB) SyncRepoKnowledge(ctx context.Context, entries []KnowledgeEntry, now time.Time) (added, updated, deleted int, err error) {
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing []KnowledgeEntry
		if err := tx.Select("id, ref, sha256").Where("source = ?", KnowledgeRepo).Find(&existing).Error; err != nil {
			return err
		}
		current := make(map[string]KnowledgeEntry, len(existing))
		for _, row := range existing {
			current[row.Ref] = row
		}
		for _, entry := range entries {
			entry.Source, entry.CreatedBy, entry.UpdatedAt = KnowledgeRepo, KnowledgeRepo, now.UTC()
			old, ok := current[entry.Ref]
			delete(current, entry.Ref)
			switch {
			case !ok:
				entry.ID = 0
				if err := tx.Create(&entry).Error; err != nil {
					return err
				}
				added++
			case old.SHA256 != entry.SHA256:
				if err := tx.Model(&KnowledgeEntry{}).Where("id = ?", old.ID).
					Updates(map[string]any{"title": entry.Title, "body": entry.Body, "sha256": entry.SHA256, "updated_at": entry.UpdatedAt}).Error; err != nil {
					return err
				}
				updated++
			}
		}
		for _, stale := range current {
			if err := tx.Delete(&KnowledgeEntry{}, stale.ID).Error; err != nil {
				return err
			}
			deleted++
		}
		return nil
	})
	if err != nil {
		return 0, 0, 0, fmt.Errorf("store: sync repository knowledge: %w", err)
	}
	return added, updated, deleted, nil
}

// SearchKnowledge ranks entries by MySQL natural-language full-text relevance
// over the ngram index; source may be empty for both sources.
func (db *DB) SearchKnowledge(ctx context.Context, query, source string, limit int) ([]KnowledgeHit, error) {
	q := db.WithContext(ctx).Model(&KnowledgeEntry{}).
		Select("*, MATCH(title, body) AGAINST (? IN NATURAL LANGUAGE MODE) AS score", query).
		Where("MATCH(title, body) AGAINST (? IN NATURAL LANGUAGE MODE)", query)
	if source != "" {
		q = q.Where("source = ?", source)
	}
	hits := make([]KnowledgeHit, 0, limit)
	if err := q.Order("score DESC").Order("id").Limit(limit).Scan(&hits).Error; err != nil {
		return nil, fmt.Errorf("store: search knowledge: %w", err)
	}
	return hits, nil
}

// ListKnowledge lists entries without their bodies, repository first.
func (db *DB) ListKnowledge(ctx context.Context, source string) ([]KnowledgeEntry, error) {
	q := db.WithContext(ctx).Select("id, source, ref, title, sha256, created_by, updated_at")
	if source != "" {
		q = q.Where("source = ?", source)
	}
	rows := make([]KnowledgeEntry, 0)
	if err := q.Order("source DESC").Order("ref").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: list knowledge: %w", err)
	}
	return rows, nil
}

func (db *DB) GetKnowledge(ctx context.Context, id uint64) (KnowledgeEntry, error) {
	var entry KnowledgeEntry
	if err := db.WithContext(ctx).First(&entry, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return KnowledgeEntry{}, ErrKnowledgeNotFound
		}
		return KnowledgeEntry{}, fmt.Errorf("store: get knowledge: %w", err)
	}
	return entry, nil
}

func incidentKnowledgeRef(incidentID uint64) string { return fmt.Sprintf("incident/%d", incidentID) }

// SaveIncidentKnowledge adds or rewrites the entry of one reviewed incident,
// with its audit event in the same transaction.
func (db *DB) SaveIncidentKnowledge(ctx context.Context, incidentID uint64, entry KnowledgeEntry) (KnowledgeEntry, error) {
	if incidentID == 0 || strings.TrimSpace(entry.Title) == "" || strings.TrimSpace(entry.Body) == "" || len(entry.SHA256) != 64 || strings.TrimSpace(entry.CreatedBy) == "" || entry.UpdatedAt.IsZero() {
		return KnowledgeEntry{}, errors.New("store: incident knowledge needs incident, title, body, digest, author and time")
	}
	entry.ID, entry.Source, entry.Ref = 0, KnowledgeIncident, incidentKnowledgeRef(incidentID)
	entry.UpdatedAt = entry.UpdatedAt.UTC().Truncate(time.Millisecond)
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing KnowledgeEntry
		err := tx.Where("ref = ?", entry.Ref).Take(&existing).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			err = tx.Create(&entry).Error
		case err == nil:
			entry.ID = existing.ID
			err = tx.Save(&entry).Error
		}
		if err != nil {
			return err
		}
		_, err = appendIncidentEvent(ctx, tx, IncidentEvent{IncidentID: incidentID, EventType: string(eventlog.EventKnowledgeAdded), Phase: "knowledge", Status: "added",
			Summary: truncateStoreText("added to knowledge by "+entry.CreatedBy, 512), CreatedAt: entry.UpdatedAt})
		return err
	})
	if err != nil {
		return KnowledgeEntry{}, fmt.Errorf("store: save incident knowledge: %w", err)
	}
	return entry, nil
}

// DeleteIncidentKnowledge removes an incident entry; repository entries are
// read-only.
func (db *DB) DeleteIncidentKnowledge(ctx context.Context, id uint64, actor string, now time.Time) error {
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var entry KnowledgeEntry
		if err := tx.Select("id, source, ref").Take(&entry, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrKnowledgeNotFound
			}
			return err
		}
		var incidentID uint64
		if _, err := fmt.Sscanf(entry.Ref, "incident/%d", &incidentID); entry.Source != KnowledgeIncident || err != nil {
			return ErrKnowledgeReadOnly
		}
		if err := tx.Delete(&KnowledgeEntry{}, entry.ID).Error; err != nil {
			return err
		}
		_, err := appendIncidentEvent(ctx, tx, IncidentEvent{IncidentID: incidentID, EventType: string(eventlog.EventKnowledgeDeleted), Phase: "knowledge", Status: "deleted",
			Summary: truncateStoreText("removed from knowledge by "+actor, 512), CreatedAt: now.UTC()})
		return err
	})
	if errors.Is(err, ErrKnowledgeNotFound) || errors.Is(err, ErrKnowledgeReadOnly) {
		return err
	}
	if err != nil {
		return fmt.Errorf("store: delete incident knowledge: %w", err)
	}
	return nil
}
