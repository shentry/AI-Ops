package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/incident"
)

const ActionDeploymentRollback = "deployment_rollback"

// Release is one recorded deployment of the service. ImageRef carries the
// registry digest (repo@sha256:...), the only identity rollback trusts.
type Release struct {
	ID         string
	ImageRef   string
	Migration  string // none | compatible | incompatible | unknown
	VerifiedAt *time.Time
	OccurredAt time.Time
}

// ReleaseSource returns the service's release records, newest first.
type ReleaseSource func(ctx context.Context) ([]Release, error)

// rollbackAction returns the service to an earlier verified release through the
// configured deployment entry. It never uses sub2api's own /system/rollback:
// a container rebuild would silently undo that.
type rollbackAction struct {
	docker    *DockerClient
	releases  ReleaseSource
	service   config.ServiceConfig
	healthURL string
	probe     bool
}

func NewRollbackAction(docker *DockerClient, releases ReleaseSource, service config.ServiceConfig) Action {
	return &rollbackAction{docker: docker, releases: releases, service: service, healthURL: service.BaseURL,
		probe: strings.TrimSpace(service.Probe.APIKey) != ""}
}

func (a *rollbackAction) Definition() ActionDefinition {
	return ActionDefinition{
		Name: ActionDeploymentRollback, Version: 2, TargetKind: "service", Timeout: time.Duration(a.service.Release.TimeoutSeconds+60) * time.Second,
		Description: "Return the service to the immediately preceding verified release. Only when the fault started after the latest release " +
			"and logs or business metrics point at that release. Never skip a release or cross an incompatible or unknown database migration.",
		Params: []ParamSpec{{Name: "release_id", Description: "id of the immediately preceding verified release, from the release records evidence", Required: true}},
	}
}

func imageDigest(ref string) string {
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		return ref[at+1:]
	}
	return ""
}

// runningDigest is the registry digest of the running image that matches one
// of the known release digests; images without digests have no identity.
func runningDigest(live ContainerInspect) []string {
	digests := make([]string, 0, len(live.RepoDigests))
	for _, ref := range live.RepoDigests {
		if d := imageDigest(ref); d != "" {
			digests = append(digests, d)
		}
	}
	return digests
}

func runs(live ContainerInspect, digest string) bool {
	for _, d := range runningDigest(live) {
		if d == digest {
			return true
		}
	}
	return false
}

func (a *rollbackAction) Prepare(ctx context.Context, req PrepareRequest) (Prepared, error) {
	if req.Target.Kind != "service" || req.Target.Name != a.service.Name {
		return Prepared{}, refuse("rollback target must be the service %s", a.service.Name)
	}
	var params struct {
		ReleaseID string `json:"release_id"`
	}
	if err := decodeParams(req.Params, &params); err != nil {
		return Prepared{}, err
	}
	current, target, err := a.rollbackReleases(ctx, req.Target.ID, params.ReleaseID)
	if err != nil {
		return Prepared{}, err
	}
	live, err := a.docker.Inspect(ctx, a.service.Container)
	if err != nil {
		return Prepared{}, fmt.Errorf("inspect %s: %w", a.service.Container, err)
	}
	if imageDigest(current.ImageRef) == "" || !runs(live, imageDigest(current.ImageRef)) {
		return Prepared{}, refuse("the running image is not the latest recorded release; the current version is unknown")
	}
	checks := []incident.Check{
		check(incident.CheckContainer, ContainerCheck{Name: a.service.Container, RepoDigest: imageDigest(target.ImageRef)}),
		check(incident.CheckHealth, HealthCheck{BaseURL: a.healthURL}),
		check(incident.CheckErrorRatio, ErrorRatioCheck{MaxRatio: req.Rule.MaxErrorRatio, MinRequests: req.Rule.MinRequests}),
	}
	if a.probe {
		checks = append(checks, check(incident.CheckProbe, struct{}{}))
	}
	return Prepared{
		Target:   incident.Object{Kind: "service", Name: a.service.Name, ID: current.ID},
		Args:     mustJSON(map[string]string{"release_id": target.ID, "image_ref": target.ImageRef}),
		Revision: imageDigest(current.ImageRef),
		PreState: mustJSON(map[string]string{"release_id": current.ID, "image_ref": current.ImageRef}),
		Checks:   checks,
	}, nil
}

// rollbackReleases is shared by preparation and the check under the deploy lock.
// Only the adjacent release is eligible, so intermediate schema changes cannot be skipped.
func (a *rollbackAction) rollbackReleases(ctx context.Context, currentID, targetID string) (Release, Release, error) {
	releases, err := a.releases(ctx)
	if err != nil {
		return Release{}, Release{}, fmt.Errorf("read release records: %w", err)
	}
	if len(releases) < 2 {
		return Release{}, Release{}, refuse("no earlier release is recorded")
	}
	current, target := releases[0], releases[1]
	if currentID != "" && current.ID != currentID {
		return current, target, refuse("a new release was recorded after the snapshot")
	}
	if current.Migration != "none" && current.Migration != "compatible" {
		return current, target, refuse("current release %s declares a %s database migration", current.ID, current.Migration)
	}
	if target.ID != targetID || target.ID == current.ID || target.ImageRef == current.ImageRef {
		return current, target, refuse("rollback must target the immediately preceding distinct release")
	}
	if target.VerifiedAt == nil || imageDigest(target.ImageRef) == "" {
		return current, target, refuse("release %s was never verified or has no image digest", target.ID)
	}
	return current, target, nil
}

type rollbackArgs struct {
	ReleaseID string `json:"release_id"`
	ImageRef  string `json:"image_ref"`
}

func (a *rollbackAction) Execute(ctx context.Context, op Operation) (Receipt, error) {
	var args rollbackArgs
	if err := decodeParams(op.Args, &args); err != nil || imageDigest(args.ImageRef) == "" {
		return Receipt{Detail: "invalid frozen rollback args"}, nil
	}
	if op.Target.Kind != "service" || op.Target.Name != a.service.Name {
		return Receipt{Detail: "service target is no longer configured"}, nil
	}
	release := a.service.Release
	lock, err := os.OpenFile(release.LockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Receipt{Detail: "cannot open deploy lock: " + err.Error()}, nil
	}
	defer lock.Close()
	// The deployment entry and CI/CD must take the same lock; a held lock means
	// a concurrent deployment, so we stop instead of racing it.
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return Receipt{Before: op.Revision, Detail: "deploy lock is held by a concurrent deployment; not rolled back"}, nil
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	current, target, err := a.rollbackReleases(ctx, op.Target.ID, args.ReleaseID)
	if err != nil {
		return Receipt{Before: op.Revision, Detail: err.Error()}, nil
	}
	if imageDigest(current.ImageRef) != op.Revision || target.ImageRef != args.ImageRef {
		return Receipt{Before: op.Revision, Detail: "release records changed after approval"}, nil
	}
	live, err := a.docker.Inspect(ctx, a.service.Container)
	if err != nil {
		return Receipt{Before: op.Revision, Detail: "could not read the running image before rollback: " + err.Error()}, nil
	}
	if !runs(live, op.Revision) {
		return Receipt{Before: op.Revision, Detail: "the running image changed after approval (new release); not rolled back"}, nil
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(release.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, release.Command[0], append(append([]string{}, release.Command[1:]...), args.ImageRef)...)
	cmd.Dir = release.WorkDir
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	runErr := cmd.Run()
	tail := lastRunes(Sanitize(ToSafeText(output.String())), 512)
	if runErr != nil {
		return Receipt{Before: op.Revision, Detail: tail}, fmt.Errorf("release command failed: %w", runErr)
	}
	after, err := a.docker.Inspect(ctx, a.service.Container)
	if err != nil {
		return Receipt{Before: op.Revision, Detail: tail}, fmt.Errorf("release command finished but the result cannot be read: %w", err)
	}
	if !runs(after, imageDigest(args.ImageRef)) {
		return Receipt{Before: op.Revision, Detail: tail}, errors.New("release command finished but the running image is not the approved release")
	}
	return Receipt{Written: true, Before: op.Revision, After: imageDigest(args.ImageRef), Detail: tail,
		Change: &Change{Type: "rollback", ReleaseID: args.ReleaseID, Before: current.ImageRef, After: args.ImageRef}}, nil
}

func (a *rollbackAction) Reconcile(ctx context.Context, op Operation) (Reconciliation, error) {
	var args, before rollbackArgs
	if err := decodeParams(op.Args, &args); err != nil {
		return Reconciliation{Outcome: OutcomeUnknown}, err
	}
	if err := decodeParams(op.PreState, &before); err != nil {
		return Reconciliation{Outcome: OutcomeUnknown}, err
	}
	live, err := a.docker.Inspect(ctx, a.service.Container)
	if err != nil {
		return Reconciliation{Outcome: OutcomeUnknown}, err
	}
	if runs(live, imageDigest(args.ImageRef)) {
		return Reconciliation{Outcome: OutcomeWritten, Change: &Change{Type: "rollback", ReleaseID: args.ReleaseID, Before: before.ImageRef, After: args.ImageRef}}, nil
	}
	// An unchanged image cannot prove that a timed-out deployment will not finish later.
	return Reconciliation{Outcome: OutcomeUnknown}, nil
}

func lastRunes(text string, n int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= n {
		return string(runes)
	}
	return "…" + string(runes[len(runes)-n:])
}
