package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/incident"
)

// Action is a registered write. The model may only name an enabled action in
// its plan; the executor runs it from an approved, frozen snapshot. Each action
// owns its parameter checks, snapshot preparation, execution, reconciliation
// and verification checks, so no other stage keeps per-action branches.
type Action interface {
	Definition() ActionDefinition
	// Prepare validates the model's suggestion against trusted configuration
	// and the live object, and freezes everything execution needs.
	Prepare(ctx context.Context, req PrepareRequest) (Prepared, error)
	// Execute re-reads the object and writes at most once. It returns a
	// receipt with Written=false when it refused before writing (the object
	// changed, a deploy lock is held); an error means the write failed or its
	// effect is unknown and must be reconciled.
	Execute(ctx context.Context, op Operation) (Receipt, error)
	// Reconcile reads the actual state to decide whether op's write happened.
	Reconcile(ctx context.Context, op Operation) (Reconciliation, error)
}

type ActionDefinition struct {
	Name        string
	Version     int
	TargetKind  string
	Description string
	// Params are the model-suggested parameters; everything else comes from
	// trusted configuration during Prepare.
	Params []ParamSpec
	// Compensation actions are planned only by the system as a frozen undo.
	Compensation bool
	Timeout      time.Duration
}

// PrepareRequest is the model's suggestion plus trusted context. Target.ID,
// when set, is the identity proven by this run's evidence; Prepare must refuse
// if the live object is a different one.
type PrepareRequest struct {
	Target incident.Object
	Params json.RawMessage
	Rule   config.RuleConfig
}

type Prepared struct {
	Target       incident.Object
	Args         json.RawMessage
	Revision     string
	PreState     json.RawMessage
	Checks       []incident.Check
	Compensation *incident.Compensation
}

// Operation is the frozen content the executor hands to an action. ID is
// persisted before the call and doubles as the idempotency key where the
// external system supports one.
type Operation struct {
	ID       string
	Target   incident.Object
	Args     json.RawMessage
	Revision string
	PreState json.RawMessage
}

// Receipt is the structured result of one execution attempt, persisted as is.
type Receipt struct {
	Written bool    `json:"written"`
	Before  string  `json:"before"`
	After   string  `json:"after"`
	Detail  string  `json:"detail"`
	Change  *Change `json:"change,omitempty"`
}

// Change is a deployment change an action made; it is recorded as a change event.
type Change struct {
	Type      string `json:"type"`
	ReleaseID string `json:"release_id"`
	Before    string `json:"before"`
	After     string `json:"after"`
}

// Reconciliation carries both the observed outcome and any reconstructable audit change.
type Reconciliation struct {
	Outcome Outcome
	Change  *Change
}

type Outcome string

const (
	OutcomeWritten    Outcome = "written"
	OutcomeNotWritten Outcome = "not_written"
	OutcomeUnknown    Outcome = "unknown"
)

// ErrActionRefused marks a Prepare refusal grounded in trusted facts; the
// decision becomes "denied" with the refusal as its reason.
var ErrActionRefused = errors.New("tools: action refused")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrActionRefused, fmt.Sprintf(format, args...))
}

// RegisterAction adds a write action to the catalog.
func (r *Registry) RegisterAction(action Action) error {
	def := action.Definition()
	if def.Name == "" || def.Version < 1 || def.TargetKind == "" || def.Description == "" || def.Timeout <= 0 {
		return fmt.Errorf("tools: action %q needs name, version, target kind, description and timeout", def.Name)
	}
	if _, exists := r.tools[def.Name]; exists || r.actions[def.Name] != nil {
		return fmt.Errorf("tools: action %s is already registered", def.Name)
	}
	r.actions[def.Name] = action
	return nil
}

func (r *Registry) Action(name string) (Action, bool) {
	action, ok := r.actions[name]
	return action, ok
}

// ActionDefinitions lists enabled actions in name order. The plan contract and
// its parser use the same list, so there is no second hard-coded whitelist.
func (r *Registry) ActionDefinitions() []ActionDefinition {
	out := make([]ActionDefinition, 0, len(r.actions))
	for _, action := range r.actions {
		out = append(out, action.Definition())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// PlannableActions are the actions the model may suggest.
func (r *Registry) PlannableActions() []ActionDefinition {
	all := r.ActionDefinitions()
	out := all[:0]
	for _, def := range all {
		if !def.Compensation {
			out = append(out, def)
		}
	}
	return out
}

func decodeParams(raw json.RawMessage, into any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return refuse("invalid params: %v", err)
	}
	return nil
}

func mustJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func check(kind string, params any) incident.Check {
	return incident.Check{Kind: kind, Params: mustJSON(params)}
}

// Check parameter shapes, decoded by the verifier.
type (
	// HealthCheck reads BaseURL + "/health".
	HealthCheck struct {
		BaseURL string `json:"base_url"`
	}
	ContainerCheck struct {
		Name string `json:"name"`
		ID   string `json:"id"`
		// StartedAfter requires a restart to have happened since the snapshot.
		StartedAfter time.Time `json:"started_after,omitzero"`
		// RepoDigest requires the running image to carry this digest.
		RepoDigest string `json:"repo_digest,omitempty"`
	}
	ErrorRatioCheck struct {
		MaxRatio    float64 `json:"max_ratio"`
		MinRequests int     `json:"min_requests"`
	}
	AccountCheck struct {
		AccountID   int64 `json:"account_id"`
		Schedulable bool  `json:"schedulable"`
	}
	GroupAvailableCheck struct {
		GroupIDs []int64 `json:"group_ids"`
		Min      int     `json:"min"`
	}
)
