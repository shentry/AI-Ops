package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"oncall-agent/internal/eventlog"
)

var (
	changeTypes    = map[string]bool{"release": true, "rollback": true, "config": true}
	migrationKinds = map[string]bool{"none": true, "compatible": true, "incompatible": true, "unknown": true}
	// imageDigestRef is repo@sha256:<64 hex>: tags drift, digests do not.
	imageDigestRef = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]{0,400}@sha256:[0-9a-f]{64}$`)
)

func validateChange(change ChangeEvent) error {
	if strings.TrimSpace(change.Env) == "" || strings.TrimSpace(change.Service) == "" || !changeTypes[change.ChangeType] {
		return errors.New("store: change needs env, service and type release, rollback or config")
	}
	if !migrationKinds[change.DBMigration] {
		return errors.New("store: db_migration must be none, compatible, incompatible or unknown")
	}
	if change.ChangeType != "config" && (change.ReleaseID == nil || strings.TrimSpace(*change.ReleaseID) == "" || change.ImageRef == nil || !imageDigestRef.MatchString(*change.ImageRef)) {
		return errors.New("store: a release or rollback needs release_id and an image_ref pinned by digest (repo@sha256:...)")
	}
	if change.OccurredAt.IsZero() || strings.TrimSpace(change.Source) == "" || strings.TrimSpace(change.Actor) == "" || strings.TrimSpace(change.IdempotencyKey) == "" {
		return errors.New("store: change needs occurred_at, source, actor and idempotency key")
	}
	return nil
}

func insertChange(ctx context.Context, tx *gorm.DB, change ChangeEvent) error {
	if change.DBMigration == "" {
		change.DBMigration = "unknown"
	}
	if err := validateChange(change); err != nil {
		return err
	}
	return tx.WithContext(ctx).Create(&change).Error
}

// RecordChange stores a reported change once per (source, idempotency key);
// a repeated report returns the stored row unchanged.
func (db *DB) RecordChange(ctx context.Context, change ChangeEvent) (ChangeEvent, bool, error) {
	if change.DBMigration == "" {
		change.DBMigration = "unknown"
	}
	if err := validateChange(change); err != nil {
		return ChangeEvent{}, false, err
	}
	change.ID = 0
	change.OccurredAt = change.OccurredAt.UTC().Truncate(time.Millisecond)
	change.CreatedAt = change.CreatedAt.UTC().Truncate(time.Millisecond)
	result := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&change)
	if result.Error != nil {
		return ChangeEvent{}, false, fmt.Errorf("store: record change: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return change, true, nil
	}
	var existing ChangeEvent
	if err := db.WithContext(ctx).Where("source = ? AND idempotency_key = ?", change.Source, change.IdempotencyKey).First(&existing).Error; err != nil {
		return ChangeEvent{}, false, fmt.Errorf("store: read recorded change: %w", err)
	}
	return existing, false, nil
}

// ListChanges returns a service's changes since a time, newest first.
func (db *DB) ListChanges(ctx context.Context, service string, since time.Time, limit int) ([]ChangeEvent, error) {
	rows := make([]ChangeEvent, 0)
	query := db.WithContext(ctx).Where("service = ?", service)
	if !since.IsZero() {
		query = query.Where("occurred_at >= ?", since)
	}
	if err := query.Order("occurred_at DESC, id DESC").Limit(normalizePageLimit(limit)).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: list changes: %w", err)
	}
	return rows, nil
}

// ListReleases returns what ran over time, newest first: releases and the
// rollbacks that returned to an earlier release.
func (db *DB) ListReleases(ctx context.Context, service string, limit int) ([]ChangeEvent, error) {
	rows := make([]ChangeEvent, 0)
	if err := db.WithContext(ctx).Where("service = ? AND change_type IN ?", service, []string{"release", "rollback"}).
		Order("occurred_at DESC, id DESC").Limit(normalizePageLimit(limit)).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: list releases: %w", err)
	}
	return rows, nil
}

// MarkReleaseVerified records that a release ran healthy; only verified
// releases are rollback targets. The first verification time is kept.
func (db *DB) MarkReleaseVerified(ctx context.Context, id uint64, at time.Time) (ChangeEvent, error) {
	var row ChangeEvent
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, id).Error; err != nil {
			return err
		}
		if row.ChangeType != "release" {
			return errors.New("store: only a release can be verified")
		}
		if row.VerifiedAt != nil {
			return nil
		}
		at = at.UTC().Truncate(time.Millisecond)
		row.VerifiedAt = &at
		return tx.WithContext(ctx).Model(&ChangeEvent{}).Where("id = ?", id).Update("verified_at", at).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ChangeEvent{}, ErrChangeNotFound
	}
	return row, err
}

var ErrChangeNotFound = errors.New("store: change not found")

var (
	reviewSubjects = map[string]bool{"diagnosis": true, "action": true}
	reviewVerdicts = map[string]bool{"correct": true, "partial": true, "wrong": true, "unknown": true}
)

// AddReview records a verdict and its audit event together. An action reviewed
// wrong blocks the approval's rule through that event.
func (db *DB) AddReview(ctx context.Context, review Review) (Review, error) {
	if review.IncidentID == 0 || !reviewSubjects[review.Subject] || !reviewVerdicts[review.Verdict] || strings.TrimSpace(review.Reviewer) == "" || review.CreatedAt.IsZero() || review.ManualMinutes < 0 {
		return Review{}, errors.New("store: review needs incident, subject, verdict, reviewer and time")
	}
	if (review.Subject == "action") != (review.ApprovalID != nil) || (review.Subject == "diagnosis" && review.RunID == nil) {
		return Review{}, errors.New("store: an action review names an approval; a diagnosis review names a run")
	}
	review.ID = 0
	review.RootCause, review.ActualFix = truncateStoreText(review.RootCause, 1024), truncateStoreText(review.ActualFix, 1024)
	review.CreatedAt = review.CreatedAt.UTC().Truncate(time.Millisecond)
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if review.ApprovalID != nil {
			var approval Approval
			if err := tx.WithContext(ctx).First(&approval, *review.ApprovalID).Error; err != nil || approval.IncidentID != review.IncidentID {
				return errors.New("store: reviewed approval does not belong to the incident")
			}
			review.RunID = &approval.RunID
		}
		if review.RunID != nil {
			var run AgentRun
			if err := tx.WithContext(ctx).First(&run, *review.RunID).Error; err != nil || run.IncidentID != review.IncidentID {
				return errors.New("store: reviewed run does not belong to the incident")
			}
		}
		if err := tx.WithContext(ctx).Create(&review).Error; err != nil {
			return err
		}
		_, err := appendIncidentEvent(ctx, tx, IncidentEvent{IncidentID: review.IncidentID, RunID: review.RunID, ApprovalID: review.ApprovalID,
			EventType: string(eventlog.EventReviewRecorded), Phase: review.Subject, Status: review.Verdict,
			Summary: truncateStoreText(review.Subject+" reviewed "+review.Verdict+" by "+review.Reviewer, 512), CreatedAt: review.CreatedAt})
		return err
	})
	if err != nil {
		return Review{}, fmt.Errorf("store: add review: %w", err)
	}
	return review, nil
}

func (db *DB) ListReviews(ctx context.Context, incidentID uint64) ([]Review, error) {
	rows := make([]Review, 0)
	if err := db.WithContext(ctx).Where("incident_id = ?", incidentID).Order("id DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: list reviews: %w", err)
	}
	return rows, nil
}
