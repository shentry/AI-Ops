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

	"oncall-agent/internal/eventlog"
	"oncall-agent/internal/incident"
)

// approval 审批单生命周期：创建、人工/系统裁决、过期清理、查询。
// 执行阶段在 execution.go。

// ListIncidentApprovals 读取目标 Incident 的审批，最新记录优先。
func (db *DB) ListIncidentApprovals(ctx context.Context, incidentID uint64, status string, limit int) ([]Approval, error) {
	if incidentID == 0 {
		return nil, errors.New("store: approval incident is required")
	}
	query := db.WithContext(ctx).Preload("Verification").Where("incident_id = ?", incidentID)
	if strings.TrimSpace(status) != "" {
		query = query.Where("status = ?", status)
	}
	rows := make([]Approval, 0)
	if err := query.Order("id DESC").Limit(normalizePageLimit(limit)).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: list incident approvals: %w", err)
	}
	return rows, nil
}

// ErrApprovalNotFound / ErrApprovalConflict 是审批决策的哨兵错误：
// 404 与 409 的 HTTP 映射靠它们。
var ErrApprovalNotFound = errors.New("store: approval not found")

var ErrApprovalConflict = errors.New("store: approval already decided")

const approvalNearExpiryWindow = 5 * time.Minute

func validateApprovalCreate(approval Approval) error {
	if approval.IncidentID == 0 || approval.RunID == 0 {
		return errors.New("store: approval incident and run are required")
	}
	if approval.ToolName == "" || len(approval.ArgsJSON) == 0 || approval.PlanHash == "" {
		return errors.New("store: approval tool, args and plan_hash are required")
	}
	if approval.ExpiresAt.IsZero() || approval.CreatedAt.IsZero() {
		return errors.New("store: approval times are required")
	}
	if !json.Valid(approval.ArgsJSON) {
		return errors.New("store: approval args must be valid JSON")
	}
	if strings.TrimSpace(approval.Reason) == "" || !approval.ExpiresAt.After(approval.CreatedAt) {
		return errors.New("store: approval reason and valid expiry are required")
	}
	hash, err := incident.PlanHash(approval.ToolName, approval.ArgsJSON, approval.ExecutionContext)
	if err != nil || hash != approval.PlanHash {
		return errors.New("store: approval execution snapshot is invalid")
	}
	return nil
}

func validateApprovalDecision(status, decidedBy, decisionSource string, now time.Time) error {
	if status != "approved" && status != "denied" {
		return errors.New("store: approval decision must be approved or denied")
	}
	if strings.TrimSpace(decidedBy) == "" {
		return errors.New("store: approval decider is required")
	}
	if len([]rune(decidedBy)) > 64 {
		return errors.New("store: approval decider is too long")
	}
	if strings.TrimSpace(decisionSource) == "" {
		return errors.New("store: approval decision source is required")
	}
	if len([]rune(decisionSource)) > 16 {
		return errors.New("store: approval decision source is too long")
	}
	if now.IsZero() {
		return errors.New("store: approval decision time is required")
	}
	return nil
}

func appendApprovalEvent(ctx context.Context, tx *gorm.DB, approval Approval, eventType eventlog.EventType, status, summary string, createdAt time.Time) error {
	approvalID := approval.ID
	runID := approval.RunID
	_, err := appendIncidentEvent(ctx, tx, IncidentEvent{
		IncidentID: approval.IncidentID,
		RunID:      &runID,
		ApprovalID: &approvalID,
		EventType:  string(eventType),
		Phase:      "approval",
		Status:     status,
		Summary:    summary,
		CreatedAt:  createdAt,
	})
	return err
}

func appendApprovalCreatedEvent(ctx context.Context, tx *gorm.DB, approval Approval) error {
	return appendApprovalEvent(ctx, tx, approval, eventlog.EventApprovalCreated, "pending", "approval created", approval.CreatedAt)
}

// decideApprovalInTx 执行带行锁的 pending + expires_at CAS，并追加决定事件。
// 调用方必须已经校验参数；所有修改和事件写入都在 tx 中完成。
func decideApprovalInTx(ctx context.Context, tx *gorm.DB, id uint64, status, expectedPlanHash, decidedBy, decisionReason, decisionSource string, now time.Time) (Approval, error) {
	var approval Approval
	query := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&approval, id)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return Approval{}, ErrApprovalNotFound
	}
	if query.Error != nil {
		return Approval{}, fmt.Errorf("store: lock approval: %w", query.Error)
	}
	if approval.Status != "pending" || !approval.ExpiresAt.After(now) {
		return Approval{}, ErrApprovalConflict
	}
	hash, err := incident.PlanHash(approval.ToolName, approval.ArgsJSON, approval.ExecutionContext)
	if err != nil || expectedPlanHash == "" || hash != approval.PlanHash || hash != expectedPlanHash {
		return Approval{}, ErrApprovalConflict
	}

	decidedAt := now.UTC()
	var reason any
	if decisionReason != "" {
		reason = decisionReason
	}
	result := tx.WithContext(ctx).Model(&Approval{}).
		Where("id = ? AND status = ? AND expires_at > ?", id, "pending", now).
		Updates(map[string]any{
			"status":          status,
			"decided_by":      decidedBy,
			"decided_at":      decidedAt,
			"decision_reason": reason,
			"decision_source": decisionSource,
		})
	if result.Error != nil {
		return Approval{}, fmt.Errorf("store: decide approval: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return Approval{}, ErrApprovalConflict
	}

	decidedByCopy := decidedBy
	sourceCopy := decisionSource
	approval.Status = status
	approval.DecidedBy = &decidedByCopy
	approval.DecidedAt = &decidedAt
	approval.DecisionSource = &sourceCopy
	if decisionReason != "" {
		reasonCopy := decisionReason
		approval.DecisionReason = &reasonCopy
	} else {
		approval.DecisionReason = nil
	}
	eventType := eventlog.EventApprovalDenied
	if status == "approved" {
		eventType = eventlog.EventApprovalApproved
	}
	if err := appendApprovalEvent(ctx, tx, approval, eventType, status, "approval "+status, decidedAt); err != nil {
		return Approval{}, err
	}
	return approval, nil
}

// GetApproval 按 id 取审批单。
func (db *DB) GetApproval(ctx context.Context, id uint64) (Approval, error) {
	var approval Approval
	err := db.WithContext(ctx).Preload("Verification").First(&approval, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Approval{}, ErrApprovalNotFound
	}
	if err != nil {
		return Approval{}, fmt.Errorf("store: get approval: %w", err)
	}
	return approval, nil
}

// DecideApproval 以 pending + expires_at 为 CAS 条件决定审批，并在同一事务
// 写入 decided_at/reason/source 与 approval.approved/denied 事件。冲突不会覆盖
// 原操作者、时间或原因。
func (db *DB) DecideApproval(ctx context.Context, id uint64, status, expectedPlanHash, decidedBy, decisionReason, decisionSource string, now time.Time) (Approval, error) {
	if err := validateApprovalDecision(status, decidedBy, decisionSource, now); err != nil {
		return Approval{}, err
	}
	now = now.UTC()
	var decided Approval
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		decided, err = decideApprovalInTx(ctx, tx, id, status, expectedPlanHash, decidedBy, decisionReason, decisionSource, now)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrApprovalNotFound) || errors.Is(err, ErrApprovalConflict) {
			return Approval{}, err
		}
		return Approval{}, fmt.Errorf("store: decide approval: %w", err)
	}
	return decided, nil
}

// ExpireApprovals 在短事务内逐行锁定过期 pending/approved 审批，转为
// expired，并为每行写 approval.expired；near-expiry 问题同步 resolve/open。
// executing 与其它终态不会被触碰。
func (db *DB) ExpireApprovals(ctx context.Context, now time.Time) (int64, error) {
	if now.IsZero() {
		return 0, errors.New("store: approval expiry time is required")
	}
	now = now.UTC()
	var expired int64
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var approvals []Approval
		if err := tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("status IN ? AND expires_at <= ?", []string{"pending", "approved"}, now).
			Order("id ASC").Find(&approvals).Error; err != nil {
			return fmt.Errorf("store: lock expired approvals: %w", err)
		}
		for _, approval := range approvals {
			result := tx.WithContext(ctx).Model(&Approval{}).
				Where("id = ? AND status IN ? AND expires_at <= ?", approval.ID, []string{"pending", "approved"}, now).
				Update("status", "expired")
			if result.Error != nil {
				return fmt.Errorf("store: expire approval %d: %w", approval.ID, result.Error)
			}
			if result.RowsAffected != 1 {
				continue
			}
			if err := appendApprovalEvent(ctx, tx, approval, eventlog.EventApprovalExpired, "expired", "approval expired", now); err != nil {
				return err
			}
			runID := approval.RunID
			if _, err := resolveIncidentProblem(ctx, tx, approval.IncidentID, "approval_near_expiry", &runID, now); err != nil {
				return err
			}
			expired++
		}

		var nearExpiry []Approval
		if err := tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("status IN ? AND expires_at > ? AND expires_at <= ?", []string{"pending", "approved"}, now, now.Add(approvalNearExpiryWindow)).
			Order("id ASC").Find(&nearExpiry).Error; err != nil {
			return fmt.Errorf("store: lock near-expiry approvals: %w", err)
		}
		for _, approval := range nearExpiry {
			runID := approval.RunID
			if _, err := openIncidentProblem(ctx, tx, IncidentProblem{
				IncidentID:  approval.IncidentID,
				RunID:       &runID,
				Code:        "approval_near_expiry",
				Severity:    "warning",
				Summary:     "approval is nearing expiration",
				FirstSeenAt: now,
				LastSeenAt:  now,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: expire approvals: %w", err)
	}
	return expired, nil
}

// ListApprovals 按状态过滤列出审批单，id 倒序稳定排序。
func (db *DB) ListApprovals(ctx context.Context, status string) ([]Approval, error) {
	query := db.WithContext(ctx).Preload("Verification").Model(&Approval{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	approvals := make([]Approval, 0)
	if err := query.Order("id DESC").Find(&approvals).Error; err != nil {
		return nil, fmt.Errorf("store: list approvals: %w", err)
	}
	return approvals, nil
}

// ListApprovalsPage 是审批列表的分页入口，缺省 20，最大 100。
func (db *DB) ListApprovalsPage(ctx context.Context, status string, limit int) ([]Approval, error) {
	query := db.WithContext(ctx).Preload("Verification").Model(&Approval{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	approvals := make([]Approval, 0)
	if err := query.Order("id DESC").Limit(normalizePageLimit(limit)).Find(&approvals).Error; err != nil {
		return nil, fmt.Errorf("store: list approvals page: %w", err)
	}
	return approvals, nil
}

// insertApproval publishes a prepared draft only from the diagnosis transaction.
func insertApproval(ctx context.Context, tx *gorm.DB, row Approval) (Approval, error) {
	if err := validateApprovalCreate(row); err != nil {
		return Approval{}, err
	}
	if row.Status != "pending" && row.Status != "approved" {
		return Approval{}, errors.New("store: invalid prepared approval status")
	}
	if row.Status == "approved" && (row.DecidedBy == nil || *row.DecidedBy != "system:auto_l2" || row.DecisionSource == nil || *row.DecisionSource != "system" || row.DecidedAt == nil) {
		return Approval{}, errors.New("store: automatic approval requires system decision metadata")
	}
	if row.Status == "pending" && (row.DecidedBy != nil || row.DecidedAt != nil || row.DecisionSource != nil) {
		return Approval{}, errors.New("store: pending approval cannot have a decision")
	}
	row.Verification = nil
	if err := tx.WithContext(ctx).Omit("Verification").Create(&row).Error; err != nil {
		return Approval{}, err
	}
	if err := appendApprovalCreatedEvent(ctx, tx, row); err != nil {
		return Approval{}, err
	}
	if row.Status == "approved" {
		if err := appendApprovalEvent(ctx, tx, row, eventlog.EventApprovalApproved, "approved", "approval approved by system", *row.DecidedAt); err != nil {
			return Approval{}, err
		}
	}
	return row, nil
}
