package approval

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"encoding/json"

	"gorm.io/datatypes"

	"oncall-agent/internal/store"
)

// approvalStore 是 Service 对存储层的收窄接口。
type approvalStore interface {
	CreateApproval(ctx context.Context, approval store.Approval) (store.Approval, error)
	GetApproval(ctx context.Context, id uint64) (store.Approval, error)
	DecideApproval(ctx context.Context, id uint64, status, decidedBy string, now time.Time) error
	ListApprovals(ctx context.Context, status string) ([]store.Approval, error)
}

// Service 管理审批单生命周期。reason 脱敏后才进数据库 ——
// 审批单与通知不能携带完整密钥、DSN 或敏感请求正文（GC-19）。
type Service struct {
	db         approvalStore
	ttlMinutes int
}

func NewService(db approvalStore, ttlMinutes int) *Service {
	return &Service{db: db, ttlMinutes: ttlMinutes}
}

// Create 为一次 L3 决策创建 pending 审批单。reason 必须已被调用方脱敏。
func (s *Service) Create(ctx context.Context, incidentID, runID uint64, decision Decision, reason string) (store.Approval, error) {
	now := time.Now().UTC()
	return s.db.CreateApproval(ctx, store.Approval{
		IncidentID: incidentID,
		RunID:      runID,
		ToolName:   decision.ToolName,
		ArgsJSON:   datatypes.JSON(decision.Args),
		Reason:     reason,
		PlanHash:   decision.PlanHash,
		ExpiresAt:  now.Add(time.Duration(s.ttlMinutes) * time.Minute),
		CreatedAt:  now,
	})
}

// Decide 审批或拒绝。幂等语义：已决/已过期 → ErrApprovalConflict；
// 不存在 → ErrApprovalNotFound。
func (s *Service) Decide(ctx context.Context, id uint64, approve bool, decidedBy string) error {
	status := "denied"
	if approve {
		status = "approved"
	}
	return s.db.DecideApproval(ctx, id, status, decidedBy, time.Now().UTC())
}

// ValidateExecution 是执行前的最终闸（GC-13）：
// 审批单必须 approved、未过期，且请求执行的 tool/args 与批准内容完全一致。
// 篡改 plan_hash、target 或 args 在这里失配被拒。
func (s *Service) ValidateExecution(ctx context.Context, id uint64, decision Decision) error {
	approval, err := s.db.GetApproval(ctx, id)
	if err != nil {
		return err
	}
	if approval.Status != "approved" {
		return fmt.Errorf("approval %d status is %q, not approved", id, approval.Status)
	}
	if time.Now().UTC().After(approval.ExpiresAt) {
		return fmt.Errorf("approval %d expired at %s", id, approval.ExpiresAt)
	}
	if approval.ToolName != decision.ToolName ||
		approval.PlanHash != decision.PlanHash ||
		!bytes.Equal(normalizeJSON(json.RawMessage(approval.ArgsJSON)), normalizeJSON(decision.Args)) {
		return fmt.Errorf("approval %d does not match the requested action", id)
	}
	return nil
}

// List 查询审批单。
func (s *Service) List(ctx context.Context, status string) ([]store.Approval, error) {
	return s.db.ListApprovals(ctx, status)
}

// CreateSystemApproved 为自动 L2 路径落"系统批准"的审批单：
// 执行面只认 approved 单，自动路径也走同一通道，审计链完整。
// 两步非原子：中间崩溃留下一张 pending 单，由人工兜底，语义安全。
func (s *Service) CreateSystemApproved(ctx context.Context, incidentID, runID uint64, decision Decision, reason string) (store.Approval, error) {
	created, err := s.Create(ctx, incidentID, runID, decision, reason)
	if err != nil {
		return store.Approval{}, err
	}
	if err := s.db.DecideApproval(ctx, created.ID, "approved", "system:auto_l2", time.Now().UTC()); err != nil {
		return store.Approval{}, fmt.Errorf("system approve: %w", err)
	}
	created.Status = "approved"
	return created, nil
}
