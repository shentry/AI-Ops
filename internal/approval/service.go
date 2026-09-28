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
	db approvalStore
}

func NewService(db approvalStore) *Service {
	return &Service{db: db}
}

// Prepare validates an immutable draft without writing it. CompleteRun publishes
// this draft atomically with the diagnosis, terminal run state and audit events.
// An auto decision is approved by its rule; a manual one waits for a person.
// The caller must sanitize reason before handing it to this service.
func (s *Service) Prepare(incidentID, runID uint64, decision Decision, reason string) (store.Approval, error) {
	if incidentID == 0 || runID == 0 || strings.TrimSpace(reason) == "" {
		return store.Approval{}, fmt.Errorf("approval: incident, run and reason are required")
	}
	if decision.Kind != DecisionApproval && decision.Kind != DecisionAuto {
		return store.Approval{}, fmt.Errorf("approval: decision %q cannot create an approval", decision.Kind)
	}
	snapshot, err := incident.ParseExecutionContext(decision.ExecutionContext)
	if err != nil {
		return store.Approval{}, err
	}
	hash, err := incident.PlanHash(decision.ToolName, decision.Args, decision.ExecutionContext)
	if err != nil || hash != decision.PlanHash {
		return store.Approval{}, fmt.Errorf("approval: plan hash does not match execution content")
	}
	if (decision.Kind == DecisionAuto) != (snapshot.Rule.Mode == incident.ModeAuto) {
		return store.Approval{}, fmt.Errorf("approval: decision %q contradicts rule mode %q", decision.Kind, snapshot.Rule.Mode)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	service, rule := snapshot.Service, snapshot.Rule.ID
	draft := store.Approval{
		IncidentID: incidentID, RunID: runID, Service: &service, RuleID: &rule, ToolName: decision.ToolName,
		ArgsJSON:         datatypes.JSON(append([]byte(nil), decision.Args...)),
		ExecutionContext: datatypes.JSON(append([]byte(nil), decision.ExecutionContext...)),
		PlanHash:         hash, Reason: reason, Status: "pending", ExpiresAt: snapshot.ExpiresAt, CreatedAt: now,
	}
	if decision.Kind == DecisionAuto {
		actor, source := "system:rule:"+rule, "rule"
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
