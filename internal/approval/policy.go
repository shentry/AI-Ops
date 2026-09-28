// Package approval turns a diagnosis plan into a frozen, rule-authorized
// snapshot and executes approved snapshots. Model-provided confidence, risk or
// similarity is never execution permission; only a remediation rule is.
package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

const (
	DecisionNone     = "none"
	DecisionObserve  = "observe"
	DecisionAuto     = "auto"
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
	RuleID           string
	Service          string
	ExpiresAt        time.Time
	// Demoted is set when an auto rule's decision was turned into manual.
	Demoted bool
}

// Authority is the current trusted configuration: the service, the rules
// release and the enabled action definitions. Policy, claim and startup all
// read it, so there is one answer to "what is authorized now".
type Authority struct {
	service     string
	env         string
	remediation config.RemediationConfig
	release     string
	actions     map[string]int
}

// NewAuthority refuses a rule naming an action that is not enabled (for
// example deployment_rollback without service.release), so a rule can never
// silently authorize nothing.
func NewAuthority(service config.ServiceConfig, remediation config.RemediationConfig, registry *tools.Registry) (*Authority, error) {
	a := &Authority{service: service.Name, env: service.Env, remediation: remediation, release: remediation.Release(service), actions: map[string]int{}}
	for _, def := range registry.ActionDefinitions() {
		a.actions[def.Name] = def.Version
	}
	for _, rule := range remediation.Rules {
		action, ok := registry.Action(rule.Action)
		if !ok || action.Definition().Compensation {
			return nil, fmt.Errorf("approval: rule %s names action %q, which is not enabled in this deployment", rule.ID, rule.Action)
		}
	}
	return a, nil
}

func (a *Authority) Release() string { return a.release }

func (a *Authority) Rules() []config.RuleConfig { return a.remediation.Rules }

func (a *Authority) Service() string { return a.service }

func (a *Authority) Env() string { return a.env }

func (a *Authority) Binding() incident.ExecutionBinding {
	rules := make(map[string]incident.RuleRef, len(a.remediation.Rules))
	for _, rule := range a.remediation.Rules {
		rules[rule.ID] = incident.RuleRef{ID: rule.ID, Version: a.release, Mode: rule.Mode, Alerts: rule.Alerts}
	}
	return incident.ExecutionBinding{Service: a.service, RulesVersion: a.release, Rules: rules, Actions: a.actions}
}

// Policy is what a claim rechecks under locks at time now.
func (a *Authority) Policy(now time.Time) store.RemediationPolicy {
	budgets := make(map[string]store.RuleBudget, len(a.remediation.Rules))
	for _, rule := range a.remediation.Rules {
		budgets[rule.ID] = store.RuleBudget{Max: rule.MaxExecutions, Window: time.Duration(rule.WindowMinutes) * time.Minute}
	}
	maintenance, in := a.remediation.InMaintenance(now)
	if in && maintenance == "" {
		maintenance = "maintenance window"
	}
	return store.RemediationPolicy{Binding: a.Binding(), Budgets: budgets, Maintenance: maintenance}
}

// match returns the first rule for action whose alerts cover every firing
// member: a rule authorizes a fault condition, not just an action name.
func (a *Authority) match(action string, members []incident.ExecutionMember) (config.RuleConfig, []string, bool) {
	for _, rule := range a.remediation.Rules {
		if rule.Action != action {
			continue
		}
		if firing, err := incident.FiringFingerprints(members, a.service, rule.Alerts); err == nil && len(firing) > 0 {
			return rule, firing, true
		}
	}
	return config.RuleConfig{}, nil, false
}

type PolicyInput struct {
	IncidentID uint64
	Members    []incident.ExecutionMember
	// FaultAlert keys fault memory; it is the alert that opened the incident.
	FaultAlert string
	// Target is the plan target with the identity Guard proved from evidence.
	Target       incident.Object
	EvidenceRefs []string
	// ObservationOK is false when monitoring data was unavailable; automatic
	// writes then wait for a person.
	ObservationOK bool
}

type remediationReader interface {
	RemediationState(ctx context.Context, q store.RemediationQuery) (store.RemediationState, error)
}

type Policy struct {
	authority *Authority
	registry  *tools.Registry
	ttl       time.Duration
	state     remediationReader
	now       func() time.Time
}

// NewPolicy takes the approval TTL: how long a frozen snapshot stays valid.
func NewPolicy(authority *Authority, registry *tools.Registry, ttl time.Duration, state remediationReader) *Policy {
	return &Policy{authority: authority, registry: registry, ttl: ttl, state: state, now: func() time.Time { return time.Now().UTC() }}
}

// Decide freezes a rule-authorized snapshot or explains why there is none.
// Checks that need a person (maintenance, a blocked rule, a second action in
// the incident, missing monitoring) turn auto into manual; checks that make
// any write wrong (stop, busy service, exhausted budget, refused facts) deny.
func (p *Policy) Decide(ctx context.Context, plan llm.Plan, input PolicyInput) Decision {
	action := strings.TrimSpace(plan.Action)
	if action == "" || action == "none" {
		return Decision{Kind: DecisionNone, Reason: "no action"}
	}
	denied := func(format string, args ...any) Decision {
		return Decision{Kind: DecisionDenied, ToolName: action, Reason: fmt.Sprintf(format, args...)}
	}
	act, ok := p.registry.Action(action)
	if !ok || act.Definition().Compensation {
		return denied("action %q is not an enabled action", action)
	}
	rule, firing, ok := p.authority.match(action, input.Members)
	if !ok {
		return denied("no remediation rule authorizes %s for the firing alerts", action)
	}
	now := p.now()
	prepared, err := act.Prepare(ctx, tools.PrepareRequest{Target: input.Target, Params: plan.Params, Rule: rule})
	if rule.Mode == incident.ModeObserve {
		if err != nil {
			return Decision{Kind: DecisionObserve, ToolName: action, RuleID: rule.ID, Reason: fmt.Sprintf("observe: rule %s would refuse %s: %v", rule.ID, action, err)}
		}
		return Decision{Kind: DecisionObserve, ToolName: action, RuleID: rule.ID, Args: prepared.Args,
			Reason: fmt.Sprintf("observe: rule %s would %s %s/%s at revision %s", rule.ID, action, prepared.Target.Kind, prepared.Target.Name, prepared.Revision)}
	}
	if err != nil {
		if errors.Is(err, tools.ErrActionRefused) {
			return denied("%v", err)
		}
		return denied("cannot prepare %s: %v", action, err)
	}
	state, err := p.state.RemediationState(ctx, store.RemediationQuery{Service: p.authority.service, RuleID: rule.ID, IncidentID: input.IncidentID,
		Since: now.Add(-time.Duration(rule.WindowMinutes) * time.Minute)})
	if err != nil {
		return denied("remediation state unavailable: %v", err)
	}
	switch {
	case state.Stopped:
		return denied("emergency stop is active: %s", state.StopReason)
	case state.BusyWith != 0:
		return denied("service %s is busy with approval %d", p.authority.service, state.BusyWith)
	case state.Executions >= rule.MaxExecutions:
		return denied("rule %s budget exhausted: %d executions in %d minutes", rule.ID, state.Executions, rule.WindowMinutes)
	}
	mode, why := rule.Mode, "rule "+rule.ID+" authorizes automatic execution"
	if mode == incident.ModeManual {
		why = "rule " + rule.ID + " requires a person to approve"
	}
	var demoted []string
	if reason, in := p.authority.remediation.InMaintenance(now); in {
		demoted = append(demoted, "maintenance window "+reason)
	}
	if state.Blocked != "" {
		demoted = append(demoted, "rule blocked by "+state.Blocked)
	}
	if state.IncidentActions > 0 {
		demoted = append(demoted, "a primary action already ran in this incident")
	}
	if !input.ObservationOK {
		demoted = append(demoted, "monitoring data is unavailable")
	}
	if mode == incident.ModeAuto && len(demoted) > 0 {
		mode, why = incident.ModeManual, "automatic execution withheld: "+strings.Join(demoted, "; ")
	}
	expires := now.Add(p.ttl).Truncate(time.Millisecond)
	v := p.authority.remediation.VerificationFor(rule)
	snapshot := incident.ExecutionContext{
		Version: incident.ExecutionContextVersion, Kind: incident.KindPrimary, Service: p.authority.service,
		Rule:          incident.RuleRef{ID: rule.ID, Version: p.authority.release, Mode: mode, Alerts: rule.Alerts},
		ActionVersion: act.Definition().Version, Target: prepared.Target, Revision: prepared.Revision, PreState: prepared.PreState,
		EvidenceRefs: input.EvidenceRefs, Members: firing, FaultAlert: input.FaultAlert,
		Verification: incident.VerificationSpec{Checks: prepared.Checks, IntervalSeconds: v.IntervalSeconds, WindowSeconds: v.WindowSeconds,
			TimeoutSeconds: v.TimeoutSeconds, RequiredPasses: v.RequiredPasses, WatchSeconds: v.WatchSeconds},
		Compensation: prepared.Compensation, ExpiresAt: expires,
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return denied("encode execution context: %v", err)
	}
	// The sole hash function also validates the complete snapshot.
	hash, err := incident.PlanHash(action, prepared.Args, raw)
	if err != nil {
		return denied("%v", err)
	}
	kind := DecisionApproval
	if mode == incident.ModeAuto {
		kind = DecisionAuto
	}
	return Decision{Kind: kind, ToolName: action, Args: prepared.Args, ExecutionContext: raw, PlanHash: hash, Reason: why,
		RuleID: rule.ID, Service: p.authority.service, ExpiresAt: expires, Demoted: rule.Mode == incident.ModeAuto && mode == incident.ModeManual}
}
