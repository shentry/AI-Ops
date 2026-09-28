package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/sub2api"
)

// fakeEngine is a Docker Engine API on a unix socket with one container.
type fakeEngine struct {
	mu         sync.Mutex
	id         string
	started    time.Time
	restarting bool
	imageID    string
	digests    map[string][]string
	restarts   int
	restartErr bool
	// deployed, once the file exists, means the deploy entry recreated the
	// container on the approved image.
	deployed string
}

func newFakeEngine(t *testing.T) (*fakeEngine, *DockerClient) {
	t.Helper()
	engine := &fakeEngine{id: "c0ffee", started: time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC), imageID: "sha256:old",
		digests: map[string][]string{"sha256:old": {"weishaw/sub2api@sha256:bad"}, "sha256:new": {"weishaw/sub2api@sha256:good"}}}
	dir, err := os.MkdirTemp("", "dk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(engine.serve)}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	client, err := NewDockerClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	return engine, client
}

func (e *fakeEngine) serve(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/containers/sub2api/json":
		if _, err := os.Stat(e.deployed); e.deployed != "" && err == nil {
			e.imageID = "sha256:new"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": e.id, "Name": "/sub2api", "Image": e.imageID,
			"State":      map[string]any{"Status": "running", "Running": true, "Restarting": e.restarting, "StartedAt": e.started},
			"HostConfig": map[string]any{"RestartPolicy": map[string]any{"Name": "unless-stopped"}}})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/"):
		_ = json.NewEncoder(w).Encode(map[string]any{"RepoDigests": e.digests[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/json")]})
	case r.Method == http.MethodPost && r.URL.Path == "/containers/"+e.id+"/restart":
		if e.restartErr {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		e.restarts++
		e.started = e.started.Add(time.Minute)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func testService() config.ServiceConfig {
	return config.ServiceConfig{Name: "sub2api", Container: "sub2api", BaseURL: "http://127.0.0.1:8080"}
}

func operation(p Prepared) Operation {
	return Operation{ID: "op-1", Target: p.Target, Args: p.Args, Revision: p.Revision, PreState: p.PreState}
}

func TestRestartActionFreezesIdentityAndRefusesStaleSnapshots(t *testing.T) {
	engine, docker := newFakeEngine(t)
	action := NewRestartAction(docker, testService())
	ctx := context.Background()
	for name, target := range map[string]incident.Object{
		"other container": {Kind: "container", Name: "victim"},
		"not container":   {Kind: "service", Name: "sub2api"},
		"identity moved":  {Kind: "container", Name: "sub2api", ID: "beef"},
	} {
		if _, err := action.Prepare(ctx, PrepareRequest{Target: target}); !errors.Is(err, ErrActionRefused) {
			t.Fatalf("%s: prepare = %v", name, err)
		}
	}
	prepared, err := action.Prepare(ctx, PrepareRequest{Target: incident.Object{Kind: "container", Name: "sub2api", ID: "c0ffee"}})
	if err != nil || prepared.Target.ID != "c0ffee" || !strings.HasPrefix(prepared.Revision, "started_at=2026-09-24T01:00:00") || len(prepared.Checks) != 2 {
		t.Fatalf("prepared = %+v %v", prepared, err)
	}

	// Someone restarted it after approval: never restart twice.
	engine.started = engine.started.Add(time.Second)
	receipt, err := action.Execute(ctx, operation(prepared))
	if err != nil || receipt.Written || engine.restarts != 0 {
		t.Fatalf("stale execute = %+v %v restarts=%d", receipt, err, engine.restarts)
	}
	if outcome, _ := action.Reconcile(ctx, operation(prepared)); outcome.Outcome != OutcomeWritten {
		t.Fatalf("reconcile after external restart = %+v", outcome)
	}

	engine.started = engine.started.Add(-time.Second)
	if outcome, _ := action.Reconcile(ctx, operation(prepared)); outcome.Outcome != OutcomeUnknown {
		t.Fatalf("reconcile before restart = %+v", outcome)
	}
	receipt, err = action.Execute(ctx, operation(prepared))
	if err != nil || !receipt.Written || engine.restarts != 1 || receipt.After == receipt.Before {
		t.Fatalf("execute = %+v %v", receipt, err)
	}

	engine.restartErr = true
	fresh, _ := action.Prepare(ctx, PrepareRequest{Target: incident.Object{Kind: "container", Name: "sub2api"}})
	if _, err := action.Execute(ctx, operation(fresh)); err == nil {
		t.Fatal("failed restart reported success")
	}
}

func releaseScript(t *testing.T, engine *fakeEngine, body string) config.ServiceConfig {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "deploy.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	service := testService()
	service.Release = config.ReleaseConfig{Command: []string{script}, WorkDir: dir, LockFile: filepath.Join(dir, "deploy.lock"), TimeoutSeconds: 10}
	return service
}

func TestRollbackActionEnforcesReleasePreconditions(t *testing.T) {
	engine, docker := newFakeEngine(t)
	verified := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	releases := []Release{
		{ID: "r2", ImageRef: "weishaw/sub2api@sha256:bad", Migration: "none", OccurredAt: time.Date(2026, 9, 24, 0, 50, 0, 0, time.UTC)},
		{ID: "r1", ImageRef: "weishaw/sub2api@sha256:good", Migration: "none", VerifiedAt: &verified},
		{ID: "r0", ImageRef: "weishaw/sub2api@sha256:older", Migration: "none"},
	}
	source := func(context.Context) ([]Release, error) { return releases, nil }
	service := releaseScript(t, engine, `echo "deploying $1"`)
	action := NewRollbackAction(docker, source, service)
	target := incident.Object{Kind: "service", Name: "sub2api"}
	ctx := context.Background()
	prepare := func(releaseID string) (Prepared, error) {
		return action.Prepare(ctx, PrepareRequest{Target: target, Params: json.RawMessage(`{"release_id":"` + releaseID + `"}`), Rule: config.RuleConfig{MaxErrorRatio: 0.05, MinRequests: 20}})
	}
	if _, err := prepare("r0"); !errors.Is(err, ErrActionRefused) {
		t.Fatalf("unverified release accepted: %v", err)
	}
	if _, err := prepare("missing"); !errors.Is(err, ErrActionRefused) {
		t.Fatalf("unknown release accepted: %v", err)
	}
	releases[0].Migration = "incompatible"
	if _, err := prepare("r1"); !errors.Is(err, ErrActionRefused) || !strings.Contains(err.Error(), "migration") {
		t.Fatalf("incompatible migration accepted: %v", err)
	}
	releases[0].Migration = "unknown"
	if _, err := prepare("r1"); !errors.Is(err, ErrActionRefused) {
		t.Fatalf("unknown migration accepted: %v", err)
	}
	releases[0].Migration = "compatible"
	engine.imageID = "sha256:new"
	if _, err := prepare("r1"); !errors.Is(err, ErrActionRefused) {
		t.Fatalf("running image outside release records accepted: %v", err)
	}
	engine.imageID = "sha256:old"
	prepared, err := prepare("r1")
	if err != nil || prepared.Revision != "sha256:bad" || prepared.Target.ID != "r2" || len(prepared.Checks) != 3 {
		t.Fatalf("prepared = %+v %v", prepared, err)
	}

	// A concurrent deployment holds the shared lock: stop, do not race it.
	lock, _ := os.OpenFile(service.Release.LockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	receipt, err := action.Execute(ctx, operation(prepared))
	if err != nil || receipt.Written || !strings.Contains(receipt.Detail, "lock") {
		t.Fatalf("locked execute = %+v %v", receipt, err)
	}
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	lock.Close()

	// The entry ran but the approved image is not running: failure, reconciled by the executor.
	if _, err := action.Execute(ctx, operation(prepared)); err == nil {
		t.Fatal("rollback without the approved image reported success")
	}
	if outcome, _ := action.Reconcile(ctx, operation(prepared)); outcome.Outcome != OutcomeUnknown {
		t.Fatalf("reconcile = %+v", outcome)
	}
	engine.imageID = "sha256:new"
	if outcome, _ := action.Reconcile(ctx, operation(prepared)); outcome.Outcome != OutcomeWritten || outcome.Change == nil || outcome.Change.After != "weishaw/sub2api@sha256:good" {
		t.Fatalf("reconcile after rollback = %+v", outcome)
	}
	engine.imageID = "sha256:old"
	service = releaseScript(t, engine, `echo "$1" > ran`)
	engine.mu.Lock()
	engine.deployed = filepath.Join(service.Release.WorkDir, "ran")
	engine.mu.Unlock()
	action = NewRollbackAction(docker, source, service)
	receipt, err = action.Execute(ctx, operation(prepared))
	if err != nil || !receipt.Written || receipt.Change == nil || receipt.Change.After != "weishaw/sub2api@sha256:good" {
		t.Fatalf("rollback = %+v %v", receipt, err)
	}
	ran, _ := os.ReadFile(engine.deployed)
	if strings.TrimSpace(string(ran)) != "weishaw/sub2api@sha256:good" {
		t.Fatalf("deploy entry received %q, want the approved digest reference", ran)
	}
}

type fakeAdmin struct {
	account      sub2api.Account
	availability sub2api.Availability
	writes       []bool
}

func (f *fakeAdmin) Account(_ context.Context, id int64) (sub2api.Account, error) {
	account := f.account
	account.ID = id
	return account, nil
}

func (f *fakeAdmin) SetSchedulable(_ context.Context, id int64, value bool) (sub2api.Account, error) {
	f.writes = append(f.writes, value)
	f.account.Schedulable = value
	return f.account, nil
}

func (f *fakeAdmin) Availability(context.Context) (sub2api.Availability, error) {
	return f.availability, nil
}

func TestUpstreamQuarantineKeepsCapacityAndFreezesItsUndo(t *testing.T) {
	admin := &fakeAdmin{
		account: sub2api.Account{Status: "active", Schedulable: true, GroupIDs: []int64{2}},
		availability: sub2api.Availability{Enabled: true,
			Groups:   map[string]sub2api.GroupAvailability{"2": {GroupID: 2, AvailableCount: 2}},
			Accounts: map[string]sub2api.AccountAvailability{"7": {AccountID: 7, IsAvailable: true}}},
	}
	actions := NewUpstreamActions(admin)
	quarantine, restore := actions[0], actions[1]
	ctx := context.Background()
	target := incident.Object{Kind: "upstream_account", Name: "7"}
	rule := config.RuleConfig{MinAvailableAccounts: 1, Compensate: true}

	until := time.Now().Add(time.Minute)
	admin.account.TempUnschedulableUntil = &until
	if _, err := quarantine.Prepare(ctx, PrepareRequest{Target: target, Rule: rule}); !errors.Is(err, ErrActionRefused) {
		t.Fatalf("already auto-isolated account accepted: %v", err)
	}
	admin.account.TempUnschedulableUntil = nil
	if _, err := quarantine.Prepare(ctx, PrepareRequest{Target: target, Rule: config.RuleConfig{MinAvailableAccounts: 2}}); !errors.Is(err, ErrActionRefused) {
		t.Fatalf("quarantine below the capacity floor accepted: %v", err)
	}
	prepared, err := quarantine.Prepare(ctx, PrepareRequest{Target: target, Rule: rule})
	if err != nil || prepared.Revision != "schedulable=true" || prepared.Compensation == nil || prepared.Compensation.Revision != "schedulable=false" {
		t.Fatalf("prepared = %+v %v", prepared, err)
	}

	receipt, err := quarantine.Execute(ctx, operation(prepared))
	if err != nil || !receipt.Written || len(admin.writes) != 1 || admin.writes[0] {
		t.Fatalf("quarantine = %+v %v writes=%v", receipt, err, admin.writes)
	}
	undo := Operation{ID: "op-2", Target: prepared.Target, Args: prepared.Compensation.Args, Revision: prepared.Compensation.Revision, PreState: prepared.PreState}

	// Someone else re-enabled the account: the compensation must not overwrite them.
	admin.account.Schedulable = true
	receipt, err = restore.Execute(ctx, undo)
	if err != nil || receipt.Written || len(admin.writes) != 1 {
		t.Fatalf("restore over a foreign change = %+v %v", receipt, err)
	}
	admin.account.Schedulable = false
	receipt, err = restore.Execute(ctx, undo)
	if err != nil || !receipt.Written || !admin.writes[1] {
		t.Fatalf("restore = %+v %v", receipt, err)
	}
	if _, err := restore.Prepare(ctx, PrepareRequest{Target: target}); err == nil {
		t.Fatal("the model could plan a compensation directly")
	}
}
