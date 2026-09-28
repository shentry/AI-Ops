package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/incident"
)

const ActionDockerRestart = "docker_restart"

// restartAction restarts the one configured service container. Its revision is
// the container's start time: a restart by anyone else since the snapshot
// changes it, and execution then refuses instead of restarting twice.
type restartAction struct {
	docker    *DockerClient
	container string
	healthURL string
	probe     bool
}

func NewRestartAction(docker *DockerClient, service config.ServiceConfig) Action {
	return &restartAction{docker: docker, container: service.Container, healthURL: service.BaseURL,
		probe: strings.TrimSpace(service.Probe.APIKey) != ""}
}

func (a *restartAction) Definition() ActionDefinition {
	return ActionDefinition{
		Name: ActionDockerRestart, Version: 3, TargetKind: "container", Timeout: time.Minute,
		Description: "Restart the service container once. Only for a process that exited or hangs (running while its health probe fails) " +
			"and that Docker's restart policy is not already recovering. Never for configuration, dependency, upstream or resource faults.",
	}
}

type restartState struct {
	Status       string    `json:"status"`
	Running      bool      `json:"running"`
	StartedAt    time.Time `json:"started_at"`
	RestartCount int       `json:"restart_count"`
}

func startedRevision(at time.Time) string { return "started_at=" + at.UTC().Format(time.RFC3339Nano) }

func (a *restartAction) Prepare(ctx context.Context, req PrepareRequest) (Prepared, error) {
	if req.Target.Kind != "container" || req.Target.Name != a.container {
		return Prepared{}, refuse("restart target must be the service container %s", a.container)
	}
	if err := decodeParams(req.Params, &struct{}{}); err != nil {
		return Prepared{}, err
	}
	live, err := a.docker.Inspect(ctx, a.container)
	if err != nil {
		return Prepared{}, fmt.Errorf("inspect %s: %w", a.container, err)
	}
	if req.Target.ID != "" && live.ID != req.Target.ID {
		return Prepared{}, refuse("container identity changed since evidence was collected")
	}
	if live.Restarting {
		return Prepared{}, refuse("docker is already restarting the container")
	}
	checks := []incident.Check{
		check(incident.CheckContainer, ContainerCheck{Name: a.container, ID: live.ID, StartedAfter: live.StartedAt}),
		check(incident.CheckHealth, HealthCheck{BaseURL: a.healthURL}),
	}
	if a.probe {
		checks = append(checks, check(incident.CheckProbe, struct{}{}))
	}
	return Prepared{
		Target:   incident.Object{Kind: "container", Name: a.container, ID: live.ID},
		Args:     mustJSON(map[string]string{"container": a.container}),
		Revision: startedRevision(live.StartedAt),
		PreState: mustJSON(restartState{Status: live.Status, Running: live.Running, StartedAt: live.StartedAt, RestartCount: live.RestartCount}),
		Checks:   checks,
	}, nil
}

func (a *restartAction) Execute(ctx context.Context, op Operation) (Receipt, error) {
	if op.Target.Kind != "container" || op.Target.Name != a.container {
		return Receipt{Detail: "container target is no longer configured"}, nil
	}
	var before restartState
	if decodeParams(op.PreState, &before) != nil || before.StartedAt.IsZero() {
		return Receipt{Detail: "missing frozen container state"}, nil
	}
	live, err := a.docker.Inspect(ctx, op.Target.Name)
	if err != nil {
		return Receipt{Detail: "could not read the container before restarting: " + err.Error()}, nil
	}
	switch {
	case live.ID != op.Target.ID:
		return Receipt{Before: op.Revision, Detail: "container identity changed after approval; not restarted"}, nil
	case startedRevision(live.StartedAt) != op.Revision:
		return Receipt{Before: op.Revision, After: startedRevision(live.StartedAt), Detail: "container restarted by someone else after approval; not restarted again"}, nil
	case live.Running != before.Running || live.Status != before.Status:
		return Receipt{Before: op.Revision, Detail: "container process state changed after approval"}, nil
	case !live.Running && (live.RestartPolicy == "always" || live.RestartPolicy == "unless-stopped"):
		return Receipt{Before: op.Revision, Detail: "container is deliberately stopped under an automatic restart policy"}, nil
	case live.Restarting:
		return Receipt{Before: op.Revision, Detail: "docker is already restarting the container; not restarted"}, nil
	}
	if err := a.docker.Restart(ctx, op.Target.ID); err != nil {
		return Receipt{Before: op.Revision}, err
	}
	receipt := Receipt{Written: true, Before: op.Revision, Detail: "restarted container " + op.Target.Name}
	if after, err := a.docker.Inspect(ctx, op.Target.Name); err == nil {
		receipt.After = startedRevision(after.StartedAt)
	}
	return receipt, nil
}

func (a *restartAction) Reconcile(ctx context.Context, op Operation) (Reconciliation, error) {
	before, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(op.Revision, "started_at="))
	if err != nil {
		return Reconciliation{Outcome: OutcomeUnknown}, errors.New("restart revision is not a start time")
	}
	live, err := a.docker.Inspect(ctx, op.Target.Name)
	if err != nil || live.ID != op.Target.ID {
		return Reconciliation{Outcome: OutcomeUnknown}, err
	}
	if live.StartedAt.After(before) {
		return Reconciliation{Outcome: OutcomeWritten}, nil
	}
	return Reconciliation{Outcome: OutcomeUnknown}, nil
}
