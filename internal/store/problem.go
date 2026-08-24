package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// incident_problem 问题面板。open/resolve 都是幂等的 upsert 语义，
// 同一个 code 重复上报只会续 last_seen_at。

// ProblemMutationKind 是 AppendRunStepRecord 可接受的有限问题操作。
// 禁止通过批次接口传入任意 SQL 操作，问题只能 open 或 resolve。
type ProblemMutationKind string

const (
	ProblemOpen            ProblemMutationKind = "open"
	ProblemResolve         ProblemMutationKind = "resolve"
	ProblemMutationOpen    ProblemMutationKind = ProblemOpen
	ProblemMutationResolve ProblemMutationKind = ProblemResolve
)

// ProblemMutation 描述一次固定语义的问题读模型变更。
// open 使用 Problem；resolve 使用 IncidentID、Code、RunID、ResolvedAt。
type ProblemMutation struct {
	Kind       ProblemMutationKind
	Problem    IncidentProblem
	IncidentID uint64
	Code       string
	RunID      *uint64
	ResolvedAt time.Time
}

func openIncidentProblem(ctx context.Context, q *gorm.DB, problem IncidentProblem) (IncidentProblem, error) {
	if problem.IncidentID == 0 || strings.TrimSpace(problem.Code) == "" {
		return IncidentProblem{}, errors.New("store: incident problem incident and code are required")
	}
	problem.Summary = truncateStoreText(problem.Summary, 512)
	if strings.TrimSpace(problem.Summary) == "" {
		return IncidentProblem{}, errors.New("store: incident problem summary is required")
	}
	if strings.TrimSpace(problem.Severity) == "" {
		problem.Severity = "warning"
	}
	if problem.DetailJSON != nil && len(*problem.DetailJSON) > 0 && !json.Valid(*problem.DetailJSON) {
		return IncidentProblem{}, errors.New("store: incident problem detail must be valid JSON")
	}
	if problem.LastSeenAt.IsZero() {
		problem.LastSeenAt = problem.FirstSeenAt
	}
	if problem.LastSeenAt.IsZero() {
		problem.LastSeenAt = time.Now().UTC()
	}
	if problem.FirstSeenAt.IsZero() {
		problem.FirstSeenAt = problem.LastSeenAt
	}
	problem.FirstSeenAt = problem.FirstSeenAt.UTC()
	problem.LastSeenAt = problem.LastSeenAt.UTC()
	problem.Status = "open"
	problem.ResolvedAt = nil

	var current IncidentProblem
	query := q.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("incident_id = ? AND code = ?", problem.IncidentID, problem.Code).First(&current)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		if err := q.WithContext(ctx).Create(&problem).Error; err == nil {
			return problem, nil
		} else {
			query = q.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("incident_id = ? AND code = ?", problem.IncidentID, problem.Code).First(&current)
			if query.Error != nil {
				return IncidentProblem{}, fmt.Errorf("store: open incident problem: %w", err)
			}
		}
	}
	if query.Error != nil {
		return IncidentProblem{}, fmt.Errorf("store: find incident problem: %w", query.Error)
	}
	updates := map[string]any{"run_id": problem.RunID, "severity": problem.Severity, "status": "open", "summary": problem.Summary, "detail_json": problem.DetailJSON, "last_seen_at": problem.LastSeenAt, "resolved_at": nil}
	if err := q.WithContext(ctx).Model(&IncidentProblem{}).Where("id = ?", current.ID).Updates(updates).Error; err != nil {
		return IncidentProblem{}, fmt.Errorf("store: reopen incident problem: %w", err)
	}
	current.RunID = problem.RunID
	current.Severity = problem.Severity
	current.Status = "open"
	current.Summary = problem.Summary
	current.DetailJSON = problem.DetailJSON
	current.LastSeenAt = problem.LastSeenAt
	current.ResolvedAt = nil
	return current, nil
}

// resolveIncidentProblem 是问题 resolve 的幂等实现；不存在或已 resolved 返回 false。
func resolveIncidentProblem(ctx context.Context, q *gorm.DB, incidentID uint64, code string, runID *uint64, resolvedAt time.Time) (bool, error) {
	if incidentID == 0 || strings.TrimSpace(code) == "" {
		return false, errors.New("store: incident problem incident and code are required")
	}
	if resolvedAt.IsZero() {
		return false, errors.New("store: incident problem resolved_at is required")
	}
	resolvedAt = resolvedAt.UTC()
	var current IncidentProblem
	query := q.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("incident_id = ? AND code = ?", incidentID, code).First(&current)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if query.Error != nil {
		return false, fmt.Errorf("store: find incident problem for resolve: %w", query.Error)
	}
	if current.Status == "resolved" {
		return false, nil
	}
	updates := map[string]any{"status": "resolved", "resolved_at": resolvedAt, "last_seen_at": resolvedAt}
	if runID != nil {
		updates["run_id"] = runID
	}
	if err := q.WithContext(ctx).Model(&IncidentProblem{}).Where("id = ? AND status <> ?", current.ID, "resolved").Updates(updates).Error; err != nil {
		return false, fmt.Errorf("store: resolve incident problem: %w", err)
	}
	return true, nil
}

// OpenIncidentProblem 打开或重开一个同 incident/code 的问题。
func (db *DB) OpenIncidentProblem(ctx context.Context, problem IncidentProblem) (IncidentProblem, error) {
	return openIncidentProblem(ctx, db.DB, problem)
}

// ResolveIncidentProblem 幂等关闭一个 Incident 问题。
func (db *DB) ResolveIncidentProblem(ctx context.Context, incidentID uint64, code string, runID *uint64, resolvedAt time.Time) (bool, error) {
	return resolveIncidentProblem(ctx, db.DB, incidentID, code, runID, resolvedAt)
}

// OpenIncidentProblem 在 ApplyRawEvent 事务内打开或重开问题。
func (t *transactionIncidentTx) OpenIncidentProblem(ctx context.Context, problem IncidentProblem) (IncidentProblem, error) {
	return openIncidentProblem(ctx, t.db, problem)
}

// ResolveIncidentProblem 在 ApplyRawEvent 事务内幂等关闭问题。
func (t *transactionIncidentTx) ResolveIncidentProblem(ctx context.Context, incidentID uint64, code string, runID *uint64, resolvedAt time.Time) (bool, error) {
	return resolveIncidentProblem(ctx, t.db, incidentID, code, runID, resolvedAt)
}

// ListIncidentProblems 按 Incident 和可选状态读取问题，默认 id ASC。
func (db *DB) ListIncidentProblems(ctx context.Context, incidentID uint64, status string, limit int) ([]IncidentProblem, error) {
	if incidentID == 0 {
		return nil, errors.New("store: incident problem incident is required")
	}
	query := db.WithContext(ctx).Where("incident_id = ?", incidentID)
	if strings.TrimSpace(status) != "" {
		query = query.Where("status = ?", status)
	}
	rows := make([]IncidentProblem, 0)
	if err := query.Order("id ASC").Limit(normalizePageLimit(limit)).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: list incident problems: %w", err)
	}
	return rows, nil
}

func applyProblemMutation(ctx context.Context, tx *gorm.DB, mutation ProblemMutation) error {
	switch mutation.Kind {
	case ProblemOpen:
		problem := mutation.Problem
		if problem.IncidentID == 0 {
			problem.IncidentID = mutation.IncidentID
		}
		if _, err := openIncidentProblem(ctx, tx, problem); err != nil {
			return err
		}
		return nil
	case ProblemResolve:
		incidentID := mutation.IncidentID
		if incidentID == 0 {
			incidentID = mutation.Problem.IncidentID
		}
		code := mutation.Code
		if code == "" {
			code = mutation.Problem.Code
		}
		if _, err := resolveIncidentProblem(ctx, tx, incidentID, code, mutation.RunID, mutation.ResolvedAt); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("store: unsupported problem mutation kind %q", mutation.Kind)
	}
}
