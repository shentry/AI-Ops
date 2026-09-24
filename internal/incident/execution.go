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
	RestartAction      = "docker_restart"
	HealthVerification = "sub2api_http_health"
	SupportedAlert     = "Sub2APIDown"
	VerificationLease  = 30 * time.Second
)

// ExecutionContext is immutable approval content, not runtime configuration.
// It stays in the pure domain package so both store and approval can validate it.
type ExecutionContext struct {
	SafetyLevel  string           `json:"safety_level"`
	DryRun       bool             `json:"dry_run"`
	Verification VerificationSpec `json:"verification"`
}

type VerificationSpec struct {
	Kind               string   `json:"kind"`
	TargetName         string   `json:"target_name"`
	BaseURL            string   `json:"base_url"`
	MemberFingerprints []string `json:"member_fingerprints"`
	IntervalSeconds    int      `json:"interval_seconds"`
	WindowSeconds      int      `json:"window_seconds"`
	TimeoutSeconds     int      `json:"timeout_seconds"`
}

type ActionTarget struct {
	Kind string `json:"target_kind"`
	Name string `json:"target_name"`
}

func ParseExecutionContext(raw []byte) (ExecutionContext, error) {
	// A missing boolean must not silently become permission for a real action.
	var wire struct {
		SafetyLevel  string           `json:"safety_level"`
		DryRun       *bool            `json:"dry_run"`
		Verification VerificationSpec `json:"verification"`
	}
	if err := decodeExactJSON(raw, &wire); err != nil || wire.DryRun == nil {
		return ExecutionContext{}, errors.New("incident: incomplete execution context")
	}
	result := ExecutionContext{SafetyLevel: wire.SafetyLevel, DryRun: *wire.DryRun, Verification: wire.Verification}
	if result.SafetyLevel != "L2" && result.SafetyLevel != "L3" {
		return ExecutionContext{}, errors.New("incident: execution safety level must be L2 or L3")
	}
	v := result.Verification
	if v.Kind != HealthVerification || strings.TrimSpace(v.TargetName) == "" || len(v.MemberFingerprints) == 0 {
		return ExecutionContext{}, errors.New("incident: supported verification target and members are required")
	}
	if v.TimeoutSeconds <= 0 || v.TimeoutSeconds >= 30 || v.IntervalSeconds <= v.TimeoutSeconds || v.WindowSeconds <= v.IntervalSeconds || int64(v.WindowSeconds) > (1<<63-1)/int64(time.Second) {
		return ExecutionContext{}, errors.New("incident: invalid verification timing")
	}
	base, err := NormalizeHealthBaseURL(v.BaseURL)
	if err != nil || base != v.BaseURL {
		return ExecutionContext{}, errors.New("incident: verification URL must be a normalized trusted HTTP base URL")
	}
	seen := make(map[string]bool, len(v.MemberFingerprints))
	for _, fp := range v.MemberFingerprints {
		if strings.TrimSpace(fp) == "" || seen[fp] {
			return ExecutionContext{}, errors.New("incident: verification members must be nonempty and unique")
		}
		seen[fp] = true
	}
	return result, nil
}

func (c ExecutionContext) Target(toolName string, args []byte) (ActionTarget, error) {
	var target ActionTarget
	if err := decodeExactJSON(args, &target); err != nil || toolName != RestartAction || target.Kind != "container" || target.Name == "" || target.Name != c.Verification.TargetName {
		return ActionTarget{}, errors.New("incident: action is not bound to a supported container verification")
	}
	return target, nil
}

// PlanHash has one format. Old approvals without execution context are never executable.
func PlanHash(toolName string, args, executionContext []byte) (string, error) {
	context, err := ParseExecutionContext(executionContext)
	if err != nil {
		return "", err
	}
	if _, err := context.Target(toolName, args); err != nil {
		return "", err
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

// ExecutionBinding is the current trusted configuration, never supplied by a client.
type ExecutionBinding struct {
	Container         string
	BaseURL           string
	AllowedContainers []string
	SafetyLevel       string
	DryRun            bool
}

func (c ExecutionContext) ValidateBinding(binding ExecutionBinding, executing bool) error {
	base, err := NormalizeHealthBaseURL(binding.BaseURL)
	if err != nil || base != c.Verification.BaseURL || binding.Container != c.Verification.TargetName || binding.SafetyLevel != c.SafetyLevel {
		return errors.New("incident: approved target configuration has changed")
	}
	allowed := false
	for _, target := range binding.AllowedContainers {
		allowed = allowed || target == c.Verification.TargetName
	}
	if !allowed {
		return errors.New("incident: approved target is no longer allowlisted")
	}
	if executing && binding.DryRun && !c.DryRun {
		return errors.New("incident: real execution disabled by dry_run")
	}
	return nil
}

type ExecutionMember struct {
	Fingerprint string
	Name        string
	Status      string
	Container   string
	Service     string
}

// FiringFingerprints defines the deliberately narrow supported fault class.
func FiringFingerprints(members []ExecutionMember, container string) ([]string, error) {
	var fingerprints []string
	for _, member := range members {
		if member.Status == "resolved" {
			continue
		}
		if member.Status != "firing" || member.Name != SupportedAlert || member.Container != container || member.Service != "sub2api" || member.Fingerprint == "" {
			return nil, errors.New("incident: unsupported or mixed fault scope; manual review required")
		}
		fingerprints = append(fingerprints, member.Fingerprint)
	}
	return fingerprints, nil
}

func (c ExecutionContext) ValidateMembers(members []ExecutionMember, executing bool) error {
	if len(members) == 0 {
		return errors.New("incident: verification members are missing")
	}
	firing, err := FiringFingerprints(members, c.Verification.TargetName)
	if err != nil {
		return err
	}
	if executing && len(firing) == 0 {
		return errors.New("incident: approved fault is no longer firing")
	}
	approved := make(map[string]bool, len(c.Verification.MemberFingerprints))
	for _, fp := range c.Verification.MemberFingerprints {
		approved[fp] = true
	}
	for _, fp := range firing {
		if !approved[fp] {
			return errors.New("incident: fault scope changed after approval")
		}
	}
	return nil
}

// FaultFingerprint preserves the existing exact memory key across the migration.
func FaultFingerprint(groupKey, alertName string) string {
	sum := md5.Sum([]byte(groupKey + alertName))
	return hex.EncodeToString(sum[:])[:12]
}
