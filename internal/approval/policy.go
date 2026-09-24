// Package approval translates diagnosis plans into deterministic permissions.
// Model-provided risk is never an execution permission.
package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/tools"
)

const (
	DecisionNone     = "none"
	DecisionAutoL1   = "auto_l1"
	DecisionAutoL2   = "auto_l2"
	DecisionApproval = "approval"
	DecisionDenied   = "denied"
)

type Decision struct {
	Kind             string
	ToolName         string
	Args             json.RawMessage
	ExecutionContext json.RawMessage
	PlanHash         string
	Reason           string
}

type PolicyConfig struct {
	AutoExecuteL2  bool
	DryRun         bool
	AllowedTargets []string
	Container      string
	HealthBaseURL  string
	Verification   config.VerificationConfig
	// Rate limits bind tool + target, not the snapshot hash (which changes with faults/config).
	RateWindow   time.Duration
	MaxPerWindow int
}

type PolicyInput struct {
	Members []incident.ExecutionMember
}

type executionCounter interface {
	CountRecentExecutions(ctx context.Context, toolName, targetName string, since time.Time) (int, error)
}

type Policy struct {
	registry *tools.Registry
	cfg      PolicyConfig
	counter  executionCounter
	now      func() time.Time
}

func NewPolicy(registry *tools.Registry, cfg PolicyConfig, counter executionCounter) *Policy {
	cfg.AllowedTargets = append([]string(nil), cfg.AllowedTargets...)
	return &Policy{registry: registry, cfg: cfg, counter: counter, now: func() time.Time { return time.Now().UTC() }}
}

// Decide validates mutation scope before considering either manual or automatic approval.
// Only registered L1 tools bypass the narrowly supported restart/verification contract.
func (p *Policy) Decide(ctx context.Context, plan llm.Plan, input PolicyInput) Decision {
	action := strings.TrimSpace(plan.Action)
	if action == "" || action == "none" {
		return Decision{Kind: DecisionNone, Reason: "no action"}
	}
	spec, ok := p.registry.Get(action)
	if !ok {
		return Decision{Kind: DecisionDenied, Reason: fmt.Sprintf("action %q is not a registered tool", action)}
	}
	if spec.Level == tools.L4Forbidden {
		return Decision{Kind: DecisionDenied, Reason: "L4 is forbidden and cannot be approved"}
	}
	args, err := json.Marshal(incident.ActionTarget{Kind: plan.Target.Kind, Name: plan.Target.Name})
	if err != nil {
		return Decision{Kind: DecisionDenied, Reason: fmt.Sprintf("encode plan args: %v", err)}
	}
	if spec.Level == tools.L1ReadOnly {
		return Decision{Kind: DecisionAutoL1, ToolName: action, Args: args, Reason: "L1 read-only"}
	}

	fingerprints, err := incident.FiringFingerprints(input.Members, p.cfg.Container)
	if err != nil {
		return Decision{Kind: DecisionDenied, Reason: err.Error()}
	}
	baseURL, err := incident.NormalizeHealthBaseURL(p.cfg.HealthBaseURL)
	if err != nil {
		return Decision{Kind: DecisionDenied, Reason: err.Error()}
	}
	snapshot := incident.ExecutionContext{
		SafetyLevel: string(spec.Level), DryRun: p.cfg.DryRun,
		Verification: incident.VerificationSpec{
			Kind: incident.HealthVerification, TargetName: p.cfg.Container, BaseURL: baseURL,
			MemberFingerprints: fingerprints,
			IntervalSeconds:    p.cfg.Verification.IntervalSeconds,
			WindowSeconds:      p.cfg.Verification.WindowSeconds,
			TimeoutSeconds:     p.cfg.Verification.TimeoutSeconds,
		},
	}
	if err := snapshot.ValidateBinding(incident.ExecutionBinding{Container: p.cfg.Container, BaseURL: p.cfg.HealthBaseURL,
		AllowedContainers: p.cfg.AllowedTargets, SafetyLevel: string(spec.Level), DryRun: p.cfg.DryRun}, false); err != nil {
		return Decision{Kind: DecisionDenied, Reason: err.Error()}
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return Decision{Kind: DecisionDenied, Reason: fmt.Sprintf("encode execution context: %v", err)}
	}
	// The sole hash function also validates the full snapshot and supported action/target.
	hash, err := incident.PlanHash(action, args, raw)
	if err != nil {
		return Decision{Kind: DecisionDenied, Reason: err.Error()}
	}
	decision := Decision{Kind: DecisionApproval, ToolName: action, Args: args, ExecutionContext: raw, PlanHash: hash, Reason: "L3 requires approval"}
	if spec.Level == tools.L2LowRisk {
		if blocked := p.l2Guardrails(ctx, action, snapshot.Verification.TargetName); blocked != "" {
			decision.Reason = "L2 guardrail not satisfied (" + blocked + "), degraded to approval"
		} else {
			decision.Kind, decision.Reason = DecisionAutoL2, "L2 guardrails satisfied"
		}
	}
	return decision
}

func (p *Policy) l2Guardrails(ctx context.Context, toolName, targetName string) string {
	if !p.cfg.AutoExecuteL2 {
		return "auto_execute_l2 disabled"
	}
	if p.cfg.DryRun {
		return "dry_run enabled"
	}
	if p.counter == nil || p.cfg.RateWindow <= 0 || p.cfg.MaxPerWindow < 1 {
		return "rate limit is not configured"
	}
	count, err := p.counter.CountRecentExecutions(ctx, toolName, targetName, p.now().Add(-p.cfg.RateWindow))
	if err != nil {
		return "rate limit check failed: " + err.Error()
	}
	if count >= p.cfg.MaxPerWindow {
		return fmt.Sprintf("rate limit reached (%d in %s)", count, p.cfg.RateWindow)
	}
	return ""
}
