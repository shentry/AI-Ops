package diagnose

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

// These tests exercise the worker boundary and its requested atomic effects.
// SQL claim CAS, scope recheck and transaction rollback are store integration tests.
type verificationDB struct {
	task        store.VerifyTask
	approval    store.Approval
	run         store.AgentRun
	incident    store.Incident
	members     []incident.ExecutionMember
	steps       []store.AgentRunStep
	completions []store.VerificationCompletion
	staleBefore []time.Time
	fail        string
	finalStatus string
	duplicate   bool
	escalated   bool
}

func (d *verificationDB) err(stage string) error {
	if d.fail == stage {
		return errors.New("database unavailable: " + stage)
	}
	return nil
}
func (d *verificationDB) NextVerificationTask(_ context.Context, now time.Time) (store.VerifyTask, bool, error) {
	return d.task, d.task.Status == "pending" && !d.task.NextCheckAt.After(now), d.err("next")
}
func (d *verificationDB) ClaimVerificationTask(_ context.Context, id uint64, now time.Time) (store.VerifyTask, bool, error) {
	if err := d.err("claim"); err != nil {
		return store.VerifyTask{}, false, err
	}
	if d.task.ApprovalID != id || d.task.Status != "pending" {
		return store.VerifyTask{}, false, nil
	}
	d.task.Status, d.task.ClaimedAt = "running", &now
	return d.task, true, nil
}
func (d *verificationDB) RequeueStaleVerificationTasks(_ context.Context, before time.Time) (int64, error) {
	d.staleBefore = append(d.staleBefore, before)
	if err := d.err("requeue"); err != nil {
		return 0, err
	}
	if d.task.Status == "running" && d.task.ClaimedAt != nil && d.task.ClaimedAt.Before(before) {
		d.task.Status, d.task.ClaimedAt = "pending", nil
		return 1, nil
	}
	return 0, nil
}
func (d *verificationDB) FinalizeVerification(_ context.Context, c store.VerificationCompletion) (store.VerificationFinalization, error) {
	d.completions = append(d.completions, c)
	if err := d.err("finalize"); err != nil {
		return store.VerificationFinalization{}, err
	}
	if d.duplicate {
		return store.VerificationFinalization{}, nil
	}
	status := c.Status
	if d.finalStatus != "" {
		status = d.finalStatus
	}
	// Mirror the store's streak rule: only in-window observations move it.
	if c.CheckedAt.Before(d.task.DeadlineAt) {
		previous := d.task.ConsecutivePasses
		d.task.ConsecutivePasses = 0
		if c.Observation == "healthy" {
			d.task.ConsecutivePasses = previous + 1
		}
	}
	d.task.Status, d.task.NextCheckAt = status, c.NextCheckAt
	d.task.LastCheckedAt, d.task.ClaimedAt = &c.CheckedAt, nil
	d.task.LastResultJSON, _ = json.Marshal(VerificationObservation{Observation: c.Observation, Detail: c.Detail})
	return store.VerificationFinalization{Applied: true, Status: status, Phase: d.task.Phase, Escalated: d.escalated}, nil
}
func (d *verificationDB) GetApproval(context.Context, uint64) (store.Approval, error) {
	return d.approval, d.err("approval")
}
func (d *verificationDB) GetAgentRun(context.Context, uint64) (store.AgentRun, error) {
	return d.run, d.err("run")
}
func (d *verificationDB) GetIncident(context.Context, uint64) (store.Incident, error) {
	return d.incident, d.err("incident")
}
func (d *verificationDB) ListRunSteps(context.Context, uint64) ([]store.AgentRunStep, error) {
	return d.steps, d.err("steps")
}
func (d *verificationDB) ListIncidentExecutionMembers(context.Context, uint64) ([]incident.ExecutionMember, error) {
	return d.members, d.err("members")
}

type verificationNotifier struct {
	db    *verificationDB
	calls []notify.Notification
	fail  bool
}

func (n *verificationNotifier) Send(_ context.Context, notification notify.Notification) (notify.Delivery, error) {
	if len(n.db.completions) == 0 || n.db.task.Status == "running" {
		panic("notification before commit")
	}
	n.calls = append(n.calls, notification)
	if n.fail {
		return notify.Delivery{}, errors.New("delivery failed")
	}
	return notify.Delivery{}, nil
}

func newVerificationFixture(t *testing.T, base string) (*VerificationWorker, *verificationDB, *time.Time, *verificationNotifier) {
	t.Helper()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	snapshot, _ := json.Marshal(verificationSnapshot(base))
	args := datatypes.JSON(`{"target_kind":"container","target_name":"sub2api"}`)
	hash, err := incident.PlanHash(tools.ActionDockerRestart, args, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	plan := datatypes.JSON(`{"action":"docker_restart","target":{"kind":"container","name":"sub2api"},"confidence":"high"}`)
	rca := "process exited"
	guard := datatypes.JSON(`"decision=allow overridden=false reason="`)
	db := &verificationDB{
		task:     store.VerifyTask{ApprovalID: 3, Status: "pending", Phase: "verify", NextCheckAt: now, DeadlineAt: now.Add(120 * time.Second), CreatedAt: now},
		approval: store.Approval{ID: 3, IncidentID: 7, RunID: 11, ToolName: tools.ActionDockerRestart, ArgsJSON: args, PlanHash: hash, ExecutionContext: snapshot, Status: "executed"},
		run:      store.AgentRun{ID: 11, IncidentID: 7, Status: "succeeded", Mode: "full", PlanJSON: &plan, RCAText: &rca},
		incident: store.Incident{ID: 7, GroupKey: "sub2api", Status: "firing"},
		members:  []incident.ExecutionMember{{Fingerprint: "fp", Name: "Sub2APIDown", Status: "firing", Service: "sub2api"}},
		steps:    []store.AgentRunStep{{RunID: 11, Kind: "guard", OutputJSON: &guard, FinishedAt: &now}},
	}
	notifier := &verificationNotifier{db: db}
	contract := verificationSnapshot(base)
	binding := incident.ExecutionBinding{Service: contract.Service, RulesVersion: contract.Rule.Version, Rules: map[string]incident.RuleRef{contract.Rule.ID: contract.Rule}, Actions: map[string]int{tools.ActionDockerRestart: contract.ActionVersion}}
	worker := NewVerificationWorker(db, NewVerifier(tools.NewRegistry(), nil, nil), 3600, notifier, log.New(io.Discard, "", 0), binding)
	worker.now = func() time.Time { return now }
	return worker, db, &now, notifier
}

func TestVerificationWorkerRedThenGreenWhileAlertStillFiring(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(503)
		}
	}))
	defer server.Close()
	worker, db, now, notifier := newVerificationFixture(t, server.URL)
	deadline := db.task.DeadlineAt
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || db.task.Status != "pending" || !db.task.NextCheckAt.Equal(now.Add(10*time.Second)) {
		t.Fatalf("task=%+v calls=%d", db.task, calls)
	}
	if c := db.completions[0]; c.Memory != nil || c.Retry || c.DemoteFingerprint != "" {
		t.Fatalf("pending effects: %+v", c)
	}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("checked before due time")
	}
	*now = db.task.NextCheckAt
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := db.completions[1]
	if calls != 2 || db.task.Status != "passed" || !db.task.DeadlineAt.Equal(deadline) || db.members[0].Status != "firing" {
		t.Fatalf("task=%+v calls=%d", db.task, calls)
	}
	if c.Memory == nil || c.Memory.Fingerprint != incident.FaultFingerprint("sub2api", "Sub2APIDown") || c.Memory.TTLSeconds != 3600 || !c.Memory.LastSuccess.Equal(*now) {
		t.Fatalf("memory=%+v", c.Memory)
	}
	if len(notifier.calls) != 1 || notifier.calls[0].Kind != notify.NotificationVerifyCompleted {
		t.Fatalf("notifications=%+v", notifier.calls)
	}
}

// requirePasses re-freezes the fixture approval with a stricter recovery rule.
func requirePasses(t *testing.T, db *verificationDB, base string, passes int) {
	t.Helper()
	refreeze(t, db, base, func(s *incident.ExecutionContext) { s.Verification.RequiredPasses = passes })
}

func refreeze(t *testing.T, db *verificationDB, base string, change func(*incident.ExecutionContext)) {
	t.Helper()
	snapshot := verificationSnapshot(base)
	change(&snapshot)
	raw, _ := json.Marshal(snapshot)
	hash, err := incident.PlanHash(tools.ActionDockerRestart, db.approval.ArgsJSON, raw)
	if err != nil {
		t.Fatal(err)
	}
	db.approval.ExecutionContext, db.approval.PlanHash = raw, hash
}

// One healthy read is not stable recovery: a flap resets the streak, and only
// the required number of consecutive healthy observations passes.
func TestVerificationWorkerRequiresConsecutiveHealthyObservations(t *testing.T) {
	sequence := []int{200, 200, 503, 200, 200, 200}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(sequence[calls])
		calls++
	}))
	defer server.Close()
	worker, db, now, notifier := newVerificationFixture(t, server.URL)
	requirePasses(t, db, server.URL, 3)
	var statuses []string
	for db.task.Status == "pending" {
		if err := worker.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, db.task.Status)
		*now = db.task.NextCheckAt
	}
	want := []string{"pending", "pending", "pending", "pending", "pending", "passed"}
	if strings.Join(statuses, ",") != strings.Join(want, ",") || calls != len(sequence) {
		t.Fatalf("statuses=%v calls=%d", statuses, calls)
	}
	if detail := db.completions[1].Detail; !strings.Contains(detail, "2 of 3 consecutive healthy observations") {
		t.Fatalf("pending detail = %q", detail)
	}
	if len(notifier.calls) != 1 || db.completions[5].Memory == nil {
		t.Fatalf("notifications=%d memory=%v", len(notifier.calls), db.completions[5].Memory)
	}
}

// A healthy streak cut off by the window is neither recovered nor failed.
func TestVerificationWorkerUnfinishedStreakAtExpiryIsInconclusive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	worker, db, now, _ := newVerificationFixture(t, server.URL)
	requirePasses(t, db, server.URL, 3)
	*now = db.task.DeadlineAt.Add(-15 * time.Second)
	for db.task.Status == "pending" {
		if err := worker.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		*now = db.task.NextCheckAt
	}
	last := db.completions[len(db.completions)-1]
	if db.task.Status != "inconclusive" || last.Memory != nil || last.Retry {
		t.Fatalf("task=%+v completion=%+v", db.task, last)
	}
}

func TestVerificationWorkerFreshUnhealthyAtExpiryRequestsAtomicRetryAndDemotion(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(503) }))
	defer server.Close()
	worker, db, now, _ := newVerificationFixture(t, server.URL)
	db.run.Mode = "memory_hit"
	for db.task.Status == "pending" {
		if err := worker.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if db.task.Status == "pending" {
			*now = db.task.NextCheckAt
		}
	}
	c := db.completions[len(db.completions)-1]
	if db.task.Status != "failed" || calls != 12 || !c.Retry || c.Memory != nil || c.DemoteFingerprint != incident.FaultFingerprint("sub2api", "Sub2APIDown") {
		t.Fatalf("result=%+v calls=%d", c, calls)
	}
}

func TestVerificationWorkerExpiryWithoutFreshUnhealthyIsInconclusive(t *testing.T) {
	for _, tc := range []struct {
		name, observation string
		age               time.Duration
		missing           bool
	}{
		{"missing", "", 0, true}, {"stale", "unhealthy", 11 * time.Second, false},
		{"unavailable", "unavailable", time.Second, false}, {"healthy", "healthy", time.Second, false},
		{"at deadline", "unhealthy", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker, db, now, _ := newVerificationFixture(t, "http://approved.invalid")
			*now = db.task.DeadlineAt
			if !tc.missing {
				at := now.Add(-tc.age)
				db.task.LastCheckedAt = &at
				db.task.LastResultJSON, _ = json.Marshal(VerificationObservation{Observation: tc.observation})
			}
			worker.verifier.httpClient.Transport = healthRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("expired task must not probe"); return nil, nil })
			if err := worker.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			c := db.completions[0]
			if c.Status != "inconclusive" || c.Retry || c.Memory != nil || c.DemoteFingerprint != "" {
				t.Fatalf("result=%+v", c)
			}
		})
	}
}

func TestVerificationWorkerLateHealthyCannotPass(t *testing.T) {
	for _, previousUnhealthy := range []bool{false, true} {
		worker, db, now, _ := newVerificationFixture(t, "http://approved.invalid")
		*now = db.task.DeadlineAt.Add(-time.Second)
		want := "inconclusive"
		if previousUnhealthy {
			at := db.task.DeadlineAt.Add(-10 * time.Second)
			db.task.LastCheckedAt = &at
			db.task.LastResultJSON = datatypes.JSON(`{"observation":"unhealthy","detail":"health returned HTTP 503"}`)
			want = "failed"
		}
		worker.verifier.httpClient.Transport = healthRoundTripFunc(func(*http.Request) (*http.Response, error) {
			*now = db.task.DeadlineAt
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})
		if err := worker.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if db.task.Status != want || db.completions[0].Memory != nil {
			t.Fatalf("task=%+v want=%s", db.task, want)
		}
	}
}

func TestVerificationWorkerShutdownLeavesRecoverableClaimAndOriginalDeadline(t *testing.T) {
	worker, db, now, _ := newVerificationFixture(t, "http://approved.invalid")
	deadline := db.task.DeadlineAt
	ctx, cancel := context.WithCancel(context.Background())
	worker.verifier.httpClient.Transport = healthRoundTripFunc(func(*http.Request) (*http.Response, error) { cancel(); return nil, context.Canceled })
	if err := worker.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if db.task.Status != "running" || len(db.completions) != 0 {
		t.Fatalf("task=%+v", db.task)
	}
	*now = now.Add(31 * time.Second)
	worker.verifier.httpClient.Transport = healthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if db.task.Status != "passed" || !db.task.DeadlineAt.Equal(deadline) || len(db.staleBefore) != 2 || !db.staleBefore[1].Equal(now.Add(-30*time.Second)) {
		t.Fatalf("task=%+v recovery=%+v", db.task, db.staleBefore)
	}
}

func TestVerificationWorkerRejectsInvalidScopeOrSnapshotBeforeHTTP(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*VerificationWorker, *verificationDB)
	}{
		{"mixed alert", func(_ *VerificationWorker, d *verificationDB) {
			d.members = append(d.members, incident.ExecutionMember{Fingerprint: "new", Name: "Sub2APISlow", Status: "firing", Service: "sub2api"})
		}},
		{"expanded scope", func(_ *VerificationWorker, d *verificationDB) {
			m := d.members[0]
			m.Fingerprint = "new"
			d.members = append(d.members, m)
		}},
		{"missing members", func(_ *VerificationWorker, d *verificationDB) { d.members = nil }},
		{"hash mismatch", func(_ *VerificationWorker, d *verificationDB) { d.approval.PlanHash = "tampered" }},
		{"missing snapshot", func(_ *VerificationWorker, d *verificationDB) { d.approval.ExecutionContext = nil }},
		{"older snapshot version", func(_ *VerificationWorker, d *verificationDB) {
			d.approval.ExecutionContext = datatypes.JSON(`{"version":2,"safety_level":"L2","dry_run":false}`)
		}},
		{"not executed", func(_ *VerificationWorker, d *verificationDB) { d.approval.Status = "simulated" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			worker, db, _, _ := newVerificationFixture(t, "http://approved.invalid")
			change.apply(worker, db)
			worker.verifier.httpClient.Transport = healthRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected HTTP call"); return nil, nil })
			if err := worker.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			c := db.completions[0]
			if c.Status != "inconclusive" || c.Memory != nil || c.Retry || c.DemoteFingerprint != "" {
				t.Fatalf("result=%+v", c)
			}
		})
	}
}

func TestVerificationWorkerMemoryRequiresOriginalHighConfidenceExplicitGuard(t *testing.T) {
	for _, tc := range []struct {
		name     string
		apply    func(*verificationDB)
		eligible bool
	}{
		{"eligible", func(*verificationDB) {}, true},
		{"retry", func(d *verificationDB) { id := uint64(1); d.run.RetryOf = &id }, false},
		{"memory hit", func(d *verificationDB) { d.run.Mode = "memory_hit" }, false},
		{"low confidence", func(d *verificationDB) { raw := datatypes.JSON(`{"confidence":"low"}`); d.run.PlanJSON = &raw }, false},
		{"missing plan", func(d *verificationDB) { d.run.PlanJSON = nil }, false},
		{"missing guard", func(d *verificationDB) { d.steps = nil }, false},
		{"missing guard output", func(d *verificationDB) { d.steps[0].OutputJSON = nil }, false},
		{"overridden guard", func(d *verificationDB) {
			raw := datatypes.JSON(`"decision=allow overridden=true reason=x"`)
			d.steps[0].OutputJSON = &raw
		}, false},
		{"malformed guard", func(d *verificationDB) {
			raw := datatypes.JSON(`"missing decision, overridden=false"`)
			d.steps[0].OutputJSON = &raw
		}, false},
		{"guard error", func(d *verificationDB) { err := "write failed"; d.steps[0].Error = &err }, false},
		{"unfinished guard", func(d *verificationDB) { d.steps[0].FinishedAt = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker, db, _, _ := newVerificationFixture(t, "http://approved.invalid")
			tc.apply(db)
			worker.verifier.httpClient.Transport = healthRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})
			if err := worker.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if c := db.completions[0]; c.Status != "passed" || (c.Memory != nil) != tc.eligible {
				t.Fatalf("result=%+v", c)
			}
		})
	}
}

func TestVerificationWorkerDBErrorsDoNotTerminalizeOrNotify(t *testing.T) {
	for _, stage := range []string{"requeue", "next", "claim", "approval", "members", "run", "steps", "incident", "finalize"} {
		t.Run(stage, func(t *testing.T) {
			worker, db, _, notifier := newVerificationFixture(t, "http://approved.invalid")
			db.fail = stage
			worker.verifier.httpClient.Transport = healthRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})
			if err := worker.RunOnce(context.Background()); err == nil {
				t.Fatal("database error swallowed")
			}
			if db.task.Status != "running" && db.task.Status != "pending" {
				t.Fatalf("terminalized: %+v", db.task)
			}
			if len(notifier.calls) != 0 {
				t.Fatal("notified after failed DB operation")
			}
		})
	}
}

func TestVerificationWorkerNotificationsUseCommittedStatusAndNeverRepeat(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		duplicate, escalate, fail bool
		status                    string
		notifications             int
	}{
		{"duplicate", true, false, false, "", 0},
		{"postcheck drift uses durable delivery", false, false, false, "inconclusive", 0},
		{"failed delivery", false, false, true, "", 1},
		{"failure uses durable delivery", false, true, false, "failed", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker, db, _, notifier := newVerificationFixture(t, "http://approved.invalid")
			db.duplicate, db.escalated, db.finalStatus, notifier.fail = tc.duplicate, tc.escalate, tc.status, tc.fail
			worker.verifier.httpClient.Transport = healthRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})
			if err := worker.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(notifier.calls) != tc.notifications {
				t.Fatalf("calls=%+v", notifier.calls)
			}
			if tc.status != "" && db.task.Status != tc.status {
				t.Fatalf("committed outcome lost: %+v", db.task)
			}
			if err := worker.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(notifier.calls) != tc.notifications {
				t.Fatal("notification repeated")
			}
		})
	}
}

func TestVerificationWorkerStartAndWait(t *testing.T) {
	worker, db, _, _ := newVerificationFixture(t, "http://approved.invalid")
	db.fail = "next"
	if err := worker.Start(context.Background()); err == nil {
		t.Fatal("startup read failure ignored")
	}
	db.fail, db.task.Status = "", "passed"
	ctx, cancel := context.WithCancel(context.Background())
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	worker.Wait()
}

// A recovery with a watch window is not remembered at passed; the watch decides.
func TestVerificationWorkerWatchDecidesMemoryAndRecurrence(t *testing.T) {
	healthy := healthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	worker, db, now, notifier := newVerificationFixture(t, "http://approved.invalid")
	refreeze(t, db, "http://approved.invalid", func(s *incident.ExecutionContext) { s.Verification.WatchSeconds = 60 })
	worker.verifier.httpClient.Transport = healthy
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := db.completions[0]; c.Status != "passed" || c.Memory != nil {
		t.Fatalf("passed with a watch must not remember yet: %+v", c)
	}

	// Watch: recurrence after the required consecutive unhealthy observations.
	lastObserved := now.Add(-10 * time.Second)
	db.task = store.VerifyTask{ApprovalID: 3, Status: "pending", Phase: "watch", NextCheckAt: *now, DeadlineAt: now.Add(time.Minute), CreatedAt: *now, LastCheckedAt: &lastObserved}
	worker.verifier.httpClient.Transport = healthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("down"))}, nil
	})
	db.escalated = true
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := db.completions[1]; c.Status != "recurred" || c.Memory != nil || c.Retry {
		t.Fatalf("recurrence=%+v", c)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("recurrence must use the durable delivery task only: %+v", notifier.calls)
	}

	// Watch end after a healthy observation: stable, and only now remembered.
	db.escalated = false
	db.task = store.VerifyTask{ApprovalID: 3, Status: "pending", Phase: "watch", NextCheckAt: *now, DeadlineAt: *now, CreatedAt: *now,
		LastCheckedAt: &lastObserved, LastResultJSON: datatypes.JSON(`{"observation":"healthy","detail":"HTTP 200"}`)}
	worker.verifier.httpClient.Transport = healthRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("ended watch must not probe"); return nil, nil })
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := db.completions[2]; c.Status != "stable" || c.Memory == nil || c.Memory.AlertName != "Sub2APIDown" {
		t.Fatalf("stable=%+v", c)
	}
}

// A compensation undoes our own write: it never retries, remembers or demotes,
// and anything but a pass needs a person.
func TestVerificationWorkerCompensationHasNoEffectsAndEscalates(t *testing.T) {
	worker, db, now, notifier := newVerificationFixture(t, "http://approved.invalid")
	db.run.Mode = "memory_hit"
	refreeze(t, db, "http://approved.invalid", func(s *incident.ExecutionContext) { s.Kind, s.Members = incident.KindCompensation, nil })
	*now = db.task.DeadlineAt
	at := now.Add(-time.Second)
	db.task.LastCheckedAt = &at
	db.task.LastResultJSON = datatypes.JSON(`{"observation":"unhealthy","detail":"HTTP 503"}`)
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := db.completions[0]
	if c.Status != "failed" || c.Retry || c.Memory != nil || c.DemoteFingerprint != "" {
		t.Fatalf("compensation effects=%+v", c)
	}
	if len(notifier.calls) != 0 {
		t.Fatalf("compensation failure must use durable delivery only: %+v", notifier.calls)
	}
}
