package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/sub2api"
)

const (
	ActionUpstreamQuarantine = "upstream_quarantine"
	ActionUpstreamRestore    = "upstream_restore"
)

// upstreamAdmin is the sub2api surface the quarantine actions may call; the
// client behind it is built with exactly these endpoints allowed.
type upstreamAdmin interface {
	Account(ctx context.Context, id int64) (sub2api.Account, error)
	SetSchedulable(ctx context.Context, id int64, schedulable bool) (sub2api.Account, error)
	Availability(ctx context.Context) (sub2api.Availability, error)
}

// quarantineAction stops scheduling one upstream account through sub2api's
// admin API. sub2api's schedulable write has no version condition, so the
// action reads the state immediately before writing and records that race.
type quarantineAction struct {
	admin upstreamAdmin
	now   func() time.Time
}

// UpstreamEndpoints is the allowlist of the sub2api client handed to the
// quarantine actions.
var UpstreamEndpoints = []sub2api.Endpoint{sub2api.GetAccount, sub2api.SetSchedulable, sub2api.OpsAvailability}

func NewUpstreamActions(admin upstreamAdmin) []Action {
	return []Action{&quarantineAction{admin: admin, now: time.Now}, &restoreAction{admin: admin}}
}

func (a *quarantineAction) Definition() ActionDefinition {
	return ActionDefinition{
		Name: ActionUpstreamQuarantine, Version: 2, TargetKind: "upstream_account", Timeout: 30 * time.Second,
		Description: "Stop scheduling one upstream account (target.name is its numeric id) whose errors are concentrated on it while other accounts " +
			"in its groups stay healthy. Not when sub2api already unscheduled it temporarily, and never when most upstreams fail.",
	}
}

func accountID(object incident.Object) (int64, error) {
	id, err := strconv.ParseInt(object.Name, 10, 64)
	if err != nil || id <= 0 || object.Kind != "upstream_account" {
		return 0, refuse("target must be an upstream_account with a numeric id")
	}
	if object.ID != "" && object.ID != object.Name {
		return 0, refuse("target identity differs from evidence")
	}
	return id, nil
}

func schedulableRevision(value bool) string { return "schedulable=" + strconv.FormatBool(value) }

func (a *quarantineAction) Prepare(ctx context.Context, req PrepareRequest) (Prepared, error) {
	id, err := accountID(req.Target)
	if err != nil {
		return Prepared{}, err
	}
	if err := decodeParams(req.Params, &struct{}{}); err != nil {
		return Prepared{}, err
	}
	account, err := a.admin.Account(ctx, id)
	if err != nil {
		return Prepared{}, fmt.Errorf("read account %d: %w", id, err)
	}
	floor := max(req.Rule.MinAvailableAccounts, 1)
	if err := quarantineCapacity(ctx, a.admin, account, floor, a.now()); err != nil {
		return Prepared{}, err
	}
	target := incident.Object{Kind: "upstream_account", Name: req.Target.Name, ID: req.Target.Name}
	prepared := Prepared{
		Target:   target,
		Args:     mustJSON(map[string]any{"account_id": id, "schedulable": false}),
		Revision: schedulableRevision(true),
		PreState: mustJSON(schedulingState{Status: account.Status, Platform: account.Platform, GroupIDs: account.GroupIDs, MinAvailableAccounts: floor}),
		Checks: []incident.Check{
			check(incident.CheckAccount, AccountCheck{AccountID: id, Schedulable: false}),
			check(incident.CheckGroupAvailable, GroupAvailableCheck{GroupIDs: account.GroupIDs, Min: floor}),
			check(incident.CheckErrorRatio, ErrorRatioCheck{MaxRatio: req.Rule.MaxErrorRatio, MinRequests: req.Rule.MinRequests}),
		},
	}
	if req.Rule.Compensate {
		prepared.Compensation = &incident.Compensation{
			Action: ActionUpstreamRestore, ActionVersion: 2,
			Args:     mustJSON(map[string]any{"account_id": id, "schedulable": true}),
			Revision: schedulableRevision(false),
			Checks:   []incident.Check{check(incident.CheckAccount, AccountCheck{AccountID: id, Schedulable: true})},
		}
	}
	return prepared, nil
}

type schedulingState struct {
	Status               string  `json:"status"`
	Platform             string  `json:"platform"`
	GroupIDs             []int64 `json:"group_ids"`
	MinAvailableAccounts int     `json:"min_available_accounts"`
}

func quarantineCapacity(ctx context.Context, admin upstreamAdmin, account sub2api.Account, floor int, now time.Time) error {
	if !account.Schedulable || account.Status != "active" || len(account.GroupIDs) == 0 {
		return refuse("account %d is not active and schedulable in a known group", account.ID)
	}
	for _, until := range []*time.Time{account.TempUnschedulableUntil, account.RateLimitResetAt, account.OverloadUntil} {
		if until != nil && now.Before(*until) {
			return refuse("sub2api has already temporarily unscheduled account %d", account.ID)
		}
	}
	availability, err := admin.Availability(ctx)
	if err != nil {
		return fmt.Errorf("read account availability: %w", err)
	}
	if !availability.Enabled {
		return refuse("sub2api realtime availability is disabled; remaining capacity is unknown")
	}
	current, known := availability.Accounts[strconv.FormatInt(account.ID, 10)]
	for _, group := range account.GroupIDs {
		stats, ok := availability.Groups[strconv.FormatInt(group, 10)]
		if !ok || !known {
			return refuse("availability of group %d is unknown", group)
		}
		left := stats.AvailableCount
		if current.IsAvailable {
			left--
		}
		if left < int64(floor) || floor < 1 {
			return refuse("group %d would keep %d accounts, below the rule floor %d", group, left, floor)
		}
	}
	return nil
}

// setSchedulable writes want only while the account still holds the frozen
// revision: a change by anyone else since approval is never overwritten.
func setSchedulable(ctx context.Context, admin upstreamAdmin, op Operation, want bool) (Receipt, error) {
	id, err := accountID(op.Target)
	if err != nil {
		return Receipt{Detail: err.Error()}, nil
	}
	account, err := admin.Account(ctx, id)
	if err != nil {
		return Receipt{Before: op.Revision, Detail: "could not read the account before writing: " + err.Error()}, nil
	}
	if schedulableRevision(account.Schedulable) != op.Revision {
		return Receipt{Before: op.Revision, After: schedulableRevision(account.Schedulable), Detail: "account scheduling changed after approval; not overwritten"}, nil
	}
	var before schedulingState
	if json.Unmarshal(op.PreState, &before) != nil || len(before.GroupIDs) == 0 {
		return Receipt{Detail: "missing frozen account prerequisites"}, nil
	}
	groups, previous := slices.Clone(account.GroupIDs), slices.Clone(before.GroupIDs)
	slices.Sort(groups)
	slices.Sort(previous)
	if account.Status != before.Status || account.Platform != before.Platform || !slices.Equal(groups, previous) {
		return Receipt{Before: op.Revision, Detail: "account status, platform or groups changed after approval"}, nil
	}
	if !want {
		if err := quarantineCapacity(ctx, admin, account, before.MinAvailableAccounts, time.Now()); err != nil {
			return Receipt{Before: op.Revision, Detail: err.Error()}, nil
		}
	}
	if _, err := admin.SetSchedulable(ctx, id, want); err != nil {
		return Receipt{Before: op.Revision}, err
	}
	return Receipt{Written: true, Before: op.Revision, After: schedulableRevision(want),
		Detail: "sub2api scheduling writes are not version-conditional; a concurrent change between the read and this write cannot be excluded"}, nil
}

func reconcileSchedulable(ctx context.Context, admin upstreamAdmin, op Operation, want bool) (Reconciliation, error) {
	id, err := accountID(op.Target)
	if err != nil {
		return Reconciliation{Outcome: OutcomeUnknown}, err
	}
	account, err := admin.Account(ctx, id)
	if err != nil {
		return Reconciliation{Outcome: OutcomeUnknown}, err
	}
	if account.Schedulable == want {
		return Reconciliation{Outcome: OutcomeWritten}, nil
	}
	return Reconciliation{Outcome: OutcomeUnknown}, nil
}

func (a *quarantineAction) Execute(ctx context.Context, op Operation) (Receipt, error) {
	return setSchedulable(ctx, a.admin, op, false)
}

func (a *quarantineAction) Reconcile(ctx context.Context, op Operation) (Reconciliation, error) {
	return reconcileSchedulable(ctx, a.admin, op, false)
}

// restoreAction is the frozen compensation of a quarantine: it returns the
// account to schedulable only while it still holds the value we wrote.
type restoreAction struct {
	admin upstreamAdmin
}

func (a *restoreAction) Definition() ActionDefinition {
	return ActionDefinition{Name: ActionUpstreamRestore, Version: 2, TargetKind: "upstream_account", Compensation: true, Timeout: 30 * time.Second,
		Description: "Undo an upstream quarantine by making the account schedulable again."}
}

func (a *restoreAction) Prepare(context.Context, PrepareRequest) (Prepared, error) {
	return Prepared{}, errors.New("tools: upstream_restore is only planned as a frozen compensation")
}

func (a *restoreAction) Execute(ctx context.Context, op Operation) (Receipt, error) {
	return setSchedulable(ctx, a.admin, op, true)
}

func (a *restoreAction) Reconcile(ctx context.Context, op Operation) (Reconciliation, error) {
	return reconcileSchedulable(ctx, a.admin, op, true)
}
