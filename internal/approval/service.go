package approval

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/store"
)

type approvalStore interface {
	GetApproval(ctx context.Context, id uint64) (store.Approval, error)
	DecideApproval(ctx context.Context, id uint64, status, expectedPlanHash, decidedBy, decisionReason, decisionSource string, now time.Time) (store.Approval, error)
	ListApprovals(ctx context.Context, status string) ([]store.Approval, error)
}

type Service struct {
	db         approvalStore
	ttlMinutes int
}

func NewService(db approvalStore, ttlMinutes int) *Service {
	return &Service{db: db, ttlMinutes: ttlMinutes}
}

// Prepare validates an immutable draft without writing it. CompleteRun publishes
// this draft atomically with the diagnosis, terminal run state and audit events.
// The caller must sanitize reason before handing it to this service.
func (s *Service) Prepare(incidentID, runID uint64, decision Decision, reason string) (store.Approval, error) {
	if incidentID == 0 || runID == 0 || s.ttlMinutes <= 0 || strings.TrimSpace(reason) == "" {
		return store.Approval{}, fmt.Errorf("approval: incident, run, positive TTL and reason are required")
	}
	if decision.Kind != DecisionApproval && decision.Kind != DecisionAutoL2 {
		return store.Approval{}, fmt.Errorf("approval: decision %q cannot create an approval", decision.Kind)
	}
	snapshot, err := incident.ParseExecutionContext(decision.ExecutionContext)
	if err != nil {
		return store.Approval{}, err
	}
	hash, err := incident.PlanHash(decision.ToolName, decision.Args, decision.ExecutionContext)
	if err != nil {
		return store.Approval{}, err
	}
	if hash != decision.PlanHash {
		return store.Approval{}, fmt.Errorf("approval: plan hash does not match execution content")
	}
	if decision.Kind == DecisionAutoL2 && (snapshot.SafetyLevel != "L2" || snapshot.DryRun) {
		return store.Approval{}, fmt.Errorf("approval: automatic approval requires a real L2 action")
	}
	now := time.Now().UTC()
	draft := store.Approval{
		IncidentID: incidentID, RunID: runID, ToolName: decision.ToolName,
		ArgsJSON:         datatypes.JSON(append([]byte(nil), decision.Args...)),
		ExecutionContext: datatypes.JSON(append([]byte(nil), decision.ExecutionContext...)),
		PlanHash:         hash, Reason: reason, Status: "pending",
		ExpiresAt: now.Add(time.Duration(s.ttlMinutes) * time.Minute), CreatedAt: now,
	}
	if decision.Kind == DecisionAutoL2 {
		actor, source := "system:auto_l2", "system"
		draft.Status, draft.DecidedBy, draft.DecisionSource = "approved", &actor, &source
		draft.DecidedAt, draft.DecisionReason = &now, &reason
	}
	return draft, nil
}

// Decide delegates hash/content/state/TTL validation to the store's row-locked transaction.
func (s *Service) Decide(ctx context.Context, id uint64, approve bool, expectedPlanHash, decidedBy, decisionReason, decisionSource string) (store.Approval, error) {
	status := "denied"
	if approve {
		status = "approved"
	}
	return s.db.DecideApproval(ctx, id, status, expectedPlanHash, decidedBy, decisionReason, decisionSource, time.Now().UTC())
}

func (s *Service) List(ctx context.Context, status string) ([]store.Approval, error) {
	return s.db.ListApprovals(ctx, status)
}

func (s *Service) Get(ctx context.Context, id uint64) (store.Approval, error) {
	return s.db.GetApproval(ctx, id)
}
