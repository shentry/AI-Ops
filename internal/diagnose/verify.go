package diagnose

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/sub2api"
	"oncall-agent/internal/tools"
)

// AccountReader is the read-only sub2api surface verification may use.
type AccountReader interface {
	Account(ctx context.Context, id int64) (sub2api.Account, error)
	Availability(ctx context.Context) (sub2api.Availability, error)
}

// Verifier performs one bounded, read-only observation of a snapshot's frozen
// checks. It has no store, no write action, no LLM, and no authority to
// declare a durable outcome. Missing data is "unavailable", never healthy.
type Verifier struct {
	registry   *tools.Registry
	httpClient *http.Client
	accounts   AccountReader
	probe      *sub2api.Probe
}

// NewVerifier takes nil accounts or probe when they are not configured; a
// snapshot that needs them then observes "unavailable".
func NewVerifier(registry *tools.Registry, accounts AccountReader, probe *sub2api.Probe) *Verifier {
	return &Verifier{registry: registry, httpClient: &http.Client{}, accounts: accounts, probe: probe}
}

// Check evaluates every frozen check, capped by the snapshot timeout and the
// remaining window. Any unavailable check makes the observation unavailable;
// otherwise any unhealthy check makes it unhealthy.
func (v *Verifier) Check(ctx context.Context, snapshot incident.ExecutionContext, remaining time.Duration) VerificationObservation {
	spec := snapshot.Verification
	if ctx.Err() != nil || remaining <= 0 || spec.TimeoutSeconds <= 0 || spec.TimeoutSeconds >= 30 {
		return VerificationObservation{Observation: "unavailable", Detail: "check canceled, window expired or timeout invalid"}
	}
	checkCtx, cancel := context.WithTimeout(ctx, min(time.Duration(spec.TimeoutSeconds)*time.Second, remaining))
	defer cancel()
	result := VerificationObservation{Observation: "healthy"}
	details := make([]string, 0, len(spec.Checks))
	for _, check := range spec.Checks {
		observation := v.check(checkCtx, check)
		details = append(details, check.Kind+": "+observation.Detail)
		switch {
		case observation.Observation == "unavailable":
			result.Observation = "unavailable"
		case observation.Observation == "unhealthy" && result.Observation == "healthy":
			result.Observation = "unhealthy"
		}
	}
	result.Detail = verifyText(strings.Join(details, "; "))
	return result
}

func observed(state, format string, args ...any) VerificationObservation {
	return VerificationObservation{Observation: state, Detail: fmt.Sprintf(format, args...)}
}

func (v *Verifier) check(ctx context.Context, check incident.Check) VerificationObservation {
	switch check.Kind {
	case incident.CheckHealth:
		var p tools.HealthCheck
		if json.Unmarshal(check.Params, &p) != nil {
			return observed("unavailable", "invalid params")
		}
		return readHTTPHealth(ctx, v.httpClient, p.BaseURL).VerificationObservation
	case incident.CheckContainer:
		return v.container(ctx, check.Params)
	case incident.CheckProbe:
		if v.probe == nil {
			return observed("unavailable", "business probe is not configured")
		}
		result := v.probe.Run(ctx)
		switch {
		case result.OK:
			return observed("healthy", "probe %s in %s", result.Detail, result.Latency.Round(time.Millisecond))
		case result.StatusCode == 0:
			return observed("unavailable", "%s", result.Detail)
		}
		return observed("unhealthy", "probe %s", result.Detail)
	case incident.CheckErrorRatio:
		return v.errorRatio(ctx, check.Params)
	case incident.CheckAccount:
		var p tools.AccountCheck
		if json.Unmarshal(check.Params, &p) != nil || v.accounts == nil {
			return observed("unavailable", "account state is not readable")
		}
		account, err := v.accounts.Account(ctx, p.AccountID)
		if err != nil {
			return observed("unavailable", "read account %d failed", p.AccountID)
		}
		if account.Schedulable != p.Schedulable {
			return observed("unhealthy", "account %d schedulable=%v, want %v", p.AccountID, account.Schedulable, p.Schedulable)
		}
		return observed("healthy", "account %d schedulable=%v", p.AccountID, account.Schedulable)
	case incident.CheckGroupAvailable:
		return v.groupAvailable(ctx, check.Params)
	}
	return observed("unavailable", "unsupported check")
}

func (v *Verifier) container(ctx context.Context, params json.RawMessage) VerificationObservation {
	var p tools.ContainerCheck
	if json.Unmarshal(params, &p) != nil {
		return observed("unavailable", "invalid params")
	}
	args, _ := json.Marshal(map[string]string{"name": p.Name})
	out, err := v.registry.Execute(ctx, tools.ToolDockerInspect, args)
	var live tools.ContainerInspect
	if err != nil || json.Unmarshal([]byte(out), &live) != nil {
		return observed("unavailable", "container %s cannot be inspected", p.Name)
	}
	switch {
	case p.ID != "" && live.ID != p.ID:
		return observed("unhealthy", "container %s is a different instance", p.Name)
	case !live.Running || live.Restarting:
		return observed("unhealthy", "container %s is %s", p.Name, live.Status)
	case !p.StartedAfter.IsZero() && !live.StartedAt.After(p.StartedAfter):
		return observed("unhealthy", "container %s has not restarted since the snapshot", p.Name)
	case p.RepoDigest != "" && !hasDigest(live.RepoDigests, p.RepoDigest):
		return observed("unhealthy", "container %s is not running the approved image %s", p.Name, p.RepoDigest)
	}
	return observed("healthy", "container %s running since %s", p.Name, live.StartedAt.UTC().Format(time.RFC3339))
}

func hasDigest(refs []string, digest string) bool {
	for _, ref := range refs {
		if strings.HasSuffix(ref, "@"+digest) {
			return true
		}
	}
	return false
}

// errorRatio reads the exporter's trailing five-minute SLA traffic. Too few
// real requests is not a pass: there is no evidence of recovery yet.
func (v *Verifier) errorRatio(ctx context.Context, params json.RawMessage) VerificationObservation {
	var p tools.ErrorRatioCheck
	if json.Unmarshal(params, &p) != nil {
		return observed("unavailable", "invalid params")
	}
	traffic, err := tools.ReadBusinessTraffic(ctx, v.registry)
	if err != nil {
		return observed("unavailable", "traffic unavailable: %v", err)
	}
	requests := traffic.Requests
	if requests < float64(p.MinRequests) || requests == 0 {
		return observed("unavailable", "only %.0f requests in 5m, need %d", requests, p.MinRequests)
	}
	ratio := traffic.ErrorRatio()
	if ratio > p.MaxRatio {
		return observed("unhealthy", "error ratio %.3f above %.3f over %.0f requests", ratio, p.MaxRatio, requests)
	}
	return observed("healthy", "error ratio %.3f over %.0f requests", ratio, requests)
}

func (v *Verifier) groupAvailable(ctx context.Context, params json.RawMessage) VerificationObservation {
	var p tools.GroupAvailableCheck
	if json.Unmarshal(params, &p) != nil || v.accounts == nil {
		return observed("unavailable", "group availability is not readable")
	}
	availability, err := v.accounts.Availability(ctx)
	if err != nil || !availability.Enabled {
		return observed("unavailable", "group availability is not readable")
	}
	for _, id := range p.GroupIDs {
		group, ok := availability.Groups[strconv.FormatInt(id, 10)]
		if !ok {
			return observed("unavailable", "group %d is not reported", id)
		}
		if group.AvailableCount < int64(p.Min) {
			return observed("unhealthy", "group %d has %d schedulable accounts, below %d", id, group.AvailableCount, p.Min)
		}
	}
	return observed("healthy", "groups %v keep at least %d schedulable accounts", p.GroupIDs, p.Min)
}
