package incident

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

const (
	VerificationLease = 30 * time.Second
	// ExecutionContextVersion is the only accepted snapshot format. Snapshots of
	// any other version are never executed or verified: an old format must not
	// be reinterpreted under new rules.
	ExecutionContextVersion = 3
	// CompensationTTL bounds how long a frozen compensation may wait for the executor.
	CompensationTTL = 15 * time.Minute
)

// Rule modes. observe never produces an executable snapshot.
const (
	ModeObserve = "observe"
	ModeManual  = "manual"
	ModeAuto    = "auto"
)

// Snapshot kinds: a compensation undoes its parent's write under the parent's
// pre-granted compensation authority.
const (
	KindPrimary      = "primary"
	KindCompensation = "compensation"
)

// Verification check kinds. Actions choose checks when preparing; one verifier
// evaluates them, so no stage keeps its own per-action branches.
const (
	CheckHealth         = "health"
	CheckContainer      = "container"
	CheckProbe          = "probe"
	CheckErrorRatio     = "error_ratio"
	CheckAccount        = "account"
	CheckGroupAvailable = "group_available"
)

// ExecutionContext is immutable approval content, not runtime configuration.
// Everything that decided the action is frozen here and covered by PlanHash.
type ExecutionContext struct {
	Version       int     `json:"version"`
	Kind          string  `json:"kind"`
	Service       string  `json:"service"`
	Rule          RuleRef `json:"rule"`
	ActionVersion int     `json:"action_version"`
	Target        Object  `json:"target"`
	// Revision is the object's state when the snapshot was frozen; execution
	// refuses to write when the live object no longer matches it.
	Revision     string          `json:"revision"`
	PreState     json.RawMessage `json:"pre_state"`
	EvidenceRefs []string        `json:"evidence_refs"`
	Members      []string        `json:"member_fingerprints"`
	// FaultAlert names the alert that keys fault memory for this incident.
	FaultAlert   string           `json:"fault_alert"`
	Verification VerificationSpec `json:"verification"`
	Compensation *Compensation    `json:"compensation"`
	ExpiresAt    time.Time        `json:"expires_at"`
}

// RuleRef is the pre-authorization that produced a snapshot. Version is the
// rules release (operator label plus content digest): any rule edit invalidates
// snapshots made under the previous release.
type RuleRef struct {
	ID      string   `json:"id"`
	Version string   `json:"version"`
	Mode    string   `json:"mode"`
	Alerts  []string `json:"alerts"`
}

// Object identifies the action target. ID comes from a trusted read (container
// ID, account ID); Name is for display and configuration lookup.
type Object struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	ID   string `json:"id"`
}

type VerificationSpec struct {
	Checks          []Check `json:"checks"`
	IntervalSeconds int     `json:"interval_seconds"`
	WindowSeconds   int     `json:"window_seconds"`
	TimeoutSeconds  int     `json:"timeout_seconds"`
	// RequiredPasses consecutive healthy observations within the window pass;
	// the same number of consecutive unhealthy ones in the watch window is a recurrence.
	RequiredPasses int `json:"required_passes"`
	// WatchSeconds is the post-recovery window that tells a stable fix from a
	// brief improvement. Zero disables watching.
	WatchSeconds int `json:"watch_seconds"`
}

// Check is one frozen verification condition; Params are defined by Kind.
type Check struct {
	Kind   string          `json:"kind"`
	Params json.RawMessage `json:"params"`
}

// Compensation is the pre-frozen undo of a primary action.
type Compensation struct {
	Action        string          `json:"action"`
	ActionVersion int             `json:"action_version"`
	Args          json.RawMessage `json:"args"`
	Revision      string          `json:"revision"`
	Checks        []Check         `json:"checks"`
}

func ParseExecutionContext(raw []byte) (ExecutionContext, error) {
	var c ExecutionContext
	if err := decodeExactJSON(raw, &c); err != nil {
		return ExecutionContext{}, errors.New("incident: incomplete execution context")
	}
	if c.Version != ExecutionContextVersion {
		return ExecutionContext{}, fmt.Errorf("incident: unsupported execution context version %d", c.Version)
	}
	if c.Kind != KindPrimary && c.Kind != KindCompensation {
		return ExecutionContext{}, errors.New("incident: snapshot kind must be primary or compensation")
	}
	r := c.Rule
	if blank(c.Service) || blank(r.ID) || blank(r.Version) || (r.Mode != ModeManual && r.Mode != ModeAuto) || len(r.Alerts) == 0 {
		return ExecutionContext{}, errors.New("incident: snapshot requires service and a manual or auto rule with alerts")
	}
	if c.ActionVersion < 1 || blank(c.Target.Kind) || blank(c.Target.Name) || blank(c.Target.ID) || blank(c.Revision) || !json.Valid(c.PreState) {
		return ExecutionContext{}, errors.New("incident: snapshot requires action version, trusted target identity, revision and pre-state")
	}
	if c.ExpiresAt.IsZero() {
		return ExecutionContext{}, errors.New("incident: snapshot requires an expiry")
	}
	if c.Kind == KindPrimary && (len(c.Members) == 0 || !unique(c.Members) || !contains(r.Alerts, c.FaultAlert)) {
		return ExecutionContext{}, errors.New("incident: verification members must be nonempty and unique, and the fault alert must be covered by the rule")
	}
	if err := c.Verification.validate(); err != nil {
		return ExecutionContext{}, err
	}
	if p := c.Compensation; p != nil {
		if c.Kind != KindPrimary || blank(p.Action) || p.ActionVersion < 1 || !json.Valid(p.Args) || blank(p.Revision) || validateChecks(p.Checks) != nil {
			return ExecutionContext{}, errors.New("incident: invalid compensation plan")
		}
	}
	return c, nil
}

func (v VerificationSpec) validate() error {
	if err := validateChecks(v.Checks); err != nil {
		return err
	}
	if v.TimeoutSeconds <= 0 || v.TimeoutSeconds >= 30 || v.IntervalSeconds <= v.TimeoutSeconds || v.WindowSeconds <= v.IntervalSeconds || int64(v.WindowSeconds) > (1<<63-1)/int64(time.Second) {
		return errors.New("incident: invalid verification timing")
	}
	if v.WatchSeconds < 0 || (v.WatchSeconds > 0 && v.WatchSeconds <= v.IntervalSeconds) || int64(v.WatchSeconds) > (1<<63-1)/int64(time.Second) {
		return errors.New("incident: invalid watch window")
	}
	return ValidatePassWindow(v.RequiredPasses, v.IntervalSeconds, v.WindowSeconds)
}

// ObservationFresh allows at most one missed sampling interval. Longer gaps
// break a recovery streak and cannot prove a continuously observed watch window.
func (v VerificationSpec) ObservationFresh(last *time.Time, now time.Time) bool {
	return last != nil && !last.After(now) && now.Sub(*last) <= 2*time.Duration(v.IntervalSeconds)*time.Second
}

func validateChecks(checks []Check) error {
	if len(checks) == 0 {
		return errors.New("incident: at least one verification check is required")
	}
	for _, check := range checks {
		switch check.Kind {
		case CheckHealth, CheckContainer, CheckProbe, CheckErrorRatio, CheckAccount, CheckGroupAvailable:
		default:
			return fmt.Errorf("incident: unsupported verification check %q", check.Kind)
		}
		if !json.Valid(check.Params) {
			return fmt.Errorf("incident: verification check %s has invalid params", check.Kind)
		}
	}
	return nil
}

// ValidatePassWindow requires that the consecutive passes fit in the window,
// otherwise verification could never pass and every recovery would read as unknown.
func ValidatePassWindow(requiredPasses, intervalSeconds, windowSeconds int) error {
	if requiredPasses < 1 || requiredPasses > 100 || int64(requiredPasses-1)*int64(intervalSeconds) >= int64(windowSeconds) {
		return errors.New("incident: required consecutive passes must fit within the verification window")
	}
	return nil
}

// PlanHash has one format: the action name, its executable args and the whole
// snapshot. Content that fails validation has no hash and is never executable.
func PlanHash(toolName string, args, executionContext []byte) (string, error) {
	if _, err := ParseExecutionContext(executionContext); err != nil {
		return "", err
	}
	var object map[string]json.RawMessage
	if blank(toolName) || json.Unmarshal(args, &object) != nil || object == nil {
		return "", errors.New("incident: action name and JSON object args are required")
	}
	content, err := json.Marshal(map[string]json.RawMessage{
		"tool_name": mustJSONString(toolName), "args": args, "execution_context": executionContext,
	})
	if err != nil {
		return "", fmt.Errorf("incident: encode plan: %w", err)
	}
	canonical, err := CanonicalJSON(content)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func mustJSONString(value string) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

// CanonicalJSON removes object-key/whitespace differences introduced by MySQL JSON.
func CanonicalJSON(raw []byte) ([]byte, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("incident: expected one JSON value")
	}
	return json.Marshal(value)
}

func decodeExactJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("incident: expected one JSON value")
	}
	return nil
}

func NormalizeHealthBaseURL(value string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("incident: health base URL must be HTTP(S) without credentials, query or path")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = ""
	return u.String(), nil
}

// ExecutionBinding is the current trusted configuration, never supplied by a
// client. A snapshot stays executable only while it still matches it.
type ExecutionBinding struct {
	Service      string
	RulesVersion string
	Rules        map[string]RuleRef
	// Actions maps each enabled action to its definition version.
	Actions map[string]int
}

// ValidateBinding revokes a snapshot when the service, rules release, rule or
// action definition it was frozen under has changed, or the rule was demoted
// to observe. An auto snapshot also needs the rule to still be auto.
func (c ExecutionContext) ValidateBinding(toolName string, binding ExecutionBinding) error {
	if binding.Service != c.Service {
		return errors.New("incident: approved service configuration has changed")
	}
	rule, ok := binding.Rules[c.Rule.ID]
	if !ok || binding.RulesVersion != c.Rule.Version {
		return errors.New("incident: remediation rules changed after approval; a new decision is required")
	}
	if rule.Mode == ModeObserve || (c.Kind == KindPrimary && c.Rule.Mode == ModeAuto && rule.Mode != ModeAuto) {
		return errors.New("incident: rule no longer authorizes this action")
	}
	if version, ok := binding.Actions[toolName]; !ok || version != c.ActionVersion {
		return errors.New("incident: action is no longer enabled with the approved definition")
	}
	return nil
}

type ExecutionMember struct {
	Fingerprint string
	Name        string
	Status      string
	Service     string
}

// FiringFingerprints returns the firing members, refusing a fault scope that
// includes alerts outside the rule or another service.
func FiringFingerprints(members []ExecutionMember, service string, alerts []string) ([]string, error) {
	allowed := make(map[string]bool, len(alerts))
	for _, name := range alerts {
		allowed[name] = true
	}
	var fingerprints []string
	for _, member := range members {
		if member.Status == "resolved" {
			continue
		}
		if member.Status != "firing" || !allowed[member.Name] || member.Service != service || member.Fingerprint == "" {
			return nil, errors.New("incident: fault scope is outside the rule; manual review required")
		}
		fingerprints = append(fingerprints, member.Fingerprint)
	}
	return fingerprints, nil
}

// ValidateMembers rejects a changed fault scope. A primary action also needs
// its fault to be still firing when executing; a compensation undoes our own
// write and does not depend on the alert.
func (c ExecutionContext) ValidateMembers(members []ExecutionMember, executing bool) error {
	if c.Kind == KindCompensation {
		return nil
	}
	if len(members) == 0 {
		return errors.New("incident: verification members are missing")
	}
	firing, err := FiringFingerprints(members, c.Service, c.Rule.Alerts)
	if err != nil {
		return err
	}
	if executing && len(firing) == 0 {
		return errors.New("incident: approved fault is no longer firing")
	}
	approved := make(map[string]bool, len(c.Members))
	for _, fp := range c.Members {
		approved[fp] = true
	}
	for _, fp := range firing {
		if !approved[fp] {
			return errors.New("incident: fault scope changed after approval")
		}
	}
	return nil
}

// FaultFingerprint is the memory key of a fault: the incident group and the
// alert that opened it.
func FaultFingerprint(groupKey, alertName string) string {
	sum := md5.Sum([]byte(groupKey + alertName))
	return hex.EncodeToString(sum[:])[:12]
}

func blank(value string) bool { return strings.TrimSpace(value) == "" }

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func unique(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if blank(value) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}
