package diagnose

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/store"
)

func diagnoseMySQLRunOnce(t *testing.T, worker *VerificationWorker) {
	t.Helper()
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func diagnoseMySQLHealthServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/health" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected health request: %s %s", r.Method, r.URL)
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func (f *diagnoseMySQLFixture) assertTerminal(t *testing.T, status string, deadline time.Time, retries int64) {
	t.Helper()
	task := f.task(t)
	if task.Status != status || task.ClaimedAt != nil || task.FinishedAt == nil || !task.DeadlineAt.Equal(deadline) {
		t.Fatalf("terminal task=%+v want=%s original deadline=%s", task, status, deadline)
	}
	parent, err := f.db.GetIncident(context.Background(), f.parent.ID)
	if err != nil || parent.Status != "firing" {
		t.Fatalf("verification changed incident: %+v err=%v", parent, err)
	}
	approval, err := f.db.GetApproval(context.Background(), f.approval.ID)
	if err != nil || approval.Status != "executed" || approval.PlanHash != f.approval.PlanHash || approval.Verification == nil || approval.Verification.Status != status {
		t.Fatalf("execution/verification facts=%+v err=%v", approval, err)
	}
	if got := f.count(t, "agent_run", "incident_id = ? AND retry_of = ?", f.parent.ID, f.run.ID); got != retries {
		t.Fatalf("retry rows=%d want=%d", got, retries)
	}
	if got := f.eventCount(t, "retry.scheduled"); got != retries {
		t.Fatalf("retry events=%d want=%d", got, retries)
	}
	if got := f.count(t, "agent_run_step", "run_id = ? AND kind = 'verify'", f.run.ID); got != 1 {
		t.Fatalf("terminal verify steps=%d", got)
	}
	if got := f.eventCount(t, "verify."+status); got != 1 {
		t.Fatalf("terminal events=%d", got)
	}
}

func (f *diagnoseMySQLFixture) memory(t *testing.T) store.FaultMemory {
	t.Helper()
	entry, err := f.db.GetFaultMemory(context.Background(), f.memoryFingerprint())
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestVerificationMySQL503Then2xxWithPipelineMemory(t *testing.T) {
	var calls atomic.Int64
	server := diagnoseMySQLHealthServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	f := newDiagnoseMySQLFixture(t, server.URL, false)
	f.diagnose(t) // Guard success evidence is emitted by the genuine Pipeline.
	worker := f.execute(t)
	initial := f.task(t)
	diagnoseMySQLRunOnce(t, worker)
	pending := f.task(t)
	if calls.Load() != 1 || pending.Status != "pending" || pending.LastCheckedAt == nil || !pending.LastCheckedAt.Equal(initial.CreatedAt) || !pending.NextCheckAt.Equal(initial.CreatedAt.Add(10*time.Second)) || !strings.Contains(string(pending.LastResultJSON), "HTTP 503") {
		t.Fatalf("503 task=%+v calls=%d", pending, calls.Load())
	}
	if _, err := f.db.GetFaultMemory(context.Background(), f.memoryFingerprint()); !errors.Is(err, store.ErrMemoryNotFound) {
		t.Fatalf("memory written before recovery: %v", err)
	}
	diagnoseMySQLRunOnce(t, worker)
	if calls.Load() != 1 {
		t.Fatal("worker probed before next_check_at")
	}
	f.clock.set(pending.NextCheckAt)
	diagnoseMySQLRunOnce(t, worker)
	f.assertTerminal(t, "passed", initial.DeadlineAt, 0)
	entry := f.memory(t)
	if calls.Load() != 2 || entry.Confidence != "high" || entry.TTLSeconds != 3600 || entry.RCAText != *f.run.RCAText || !entry.LastSuccess.Equal(f.clock.now()) {
		t.Fatalf("successful memory=%+v requests=%d", entry, calls.Load())
	}
	members, err := f.db.ListIncidentExecutionMembers(context.Background(), f.parent.ID)
	if err != nil || len(members) != 1 || members[0].Status != "firing" {
		t.Fatalf("resolved was invented: members=%+v err=%v", members, err)
	}
	diagnoseMySQLRunOnce(t, worker)
	f.assertTerminal(t, "passed", initial.DeadlineAt, 0)
	if calls.Load() != 2 || !reflect.DeepEqual(entry, f.memory(t)) {
		t.Fatal("terminal replay repeated HTTP or memory effects")
	}
}

func TestVerificationMySQLPersistent503AtDeadline(t *testing.T) {
	var calls atomic.Int64
	server := diagnoseMySQLHealthServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	f := newDiagnoseMySQLFixture(t, server.URL, true)
	f.diagnose(t)
	if f.run.Mode != "memory_hit" || f.reasoner.calls != 0 {
		t.Fatalf("expected genuine memory-hit pipeline: %+v llm=%d", f.run, f.reasoner.calls)
	}
	worker := f.execute(t)
	initial := f.task(t)
	var lastObserved time.Time
	for i := 0; i <= 12; i++ {
		diagnoseMySQLRunOnce(t, worker)
		task := f.task(t)
		if task.Status != "pending" {
			break
		}
		if task.LastCheckedAt == nil || !strings.Contains(string(task.LastResultJSON), "HTTP 503") {
			t.Fatalf("missing actual 503 observation: %+v", task)
		}
		lastObserved = *task.LastCheckedAt
		f.clock.set(task.NextCheckAt)
	}
	f.assertTerminal(t, "failed", initial.DeadlineAt, 1)
	final := f.task(t)
	if calls.Load() != 12 || !f.clock.now().Equal(initial.DeadlineAt) || final.LastCheckedAt == nil || !final.LastCheckedAt.Equal(lastObserved) || !lastObserved.Equal(initial.DeadlineAt.Add(-10*time.Second)) {
		t.Fatalf("expiry rewrote actual observation time: final=%+v last=%s calls=%d", final, lastObserved, calls.Load())
	}
	if entry := f.memory(t); entry.Confidence != "low" || entry.LastUsed == nil || !entry.LastUsed.Equal(initial.DeadlineAt) {
		t.Fatalf("memory not atomically demoted: %+v", entry)
	}
	diagnoseMySQLRunOnce(t, worker)
	f.assertTerminal(t, "failed", initial.DeadlineAt, 1)
	if calls.Load() != 12 {
		t.Fatal("terminal task probed again")
	}
}

func TestVerificationMySQLInconclusivePreservesObservationAndMemory(t *testing.T) {
	for _, name := range []string{"absent_backlog", "stale_503", "unavailable_read", "late_200"} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			var late atomic.Bool
			var f *diagnoseMySQLFixture
			server := diagnoseMySQLHealthServer(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				if late.Load() {
					// Advance only the test clock during a real HTTP request. A
					// completed 200 is now late, without a slow/flaky 120s sleep.
					f.clock.set(f.clock.now().Add(2 * time.Second))
					w.WriteHeader(http.StatusOK)
					return
				}
				if name == "unavailable_read" {
					w.Header().Set("Content-Length", "100")
					_, _ = w.Write([]byte("short body")) // genuine HTTP read failure
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			f = newDiagnoseMySQLFixture(t, server.URL, true)
			f.diagnose(t)
			worker := f.execute(t)
			initial, memoryBefore := f.task(t), f.memory(t)
			if name != "absent_backlog" {
				diagnoseMySQLRunOnce(t, worker)
			}
			previous := f.task(t)
			if name == "unavailable_read" && !strings.Contains(string(previous.LastResultJSON), "unavailable") {
				t.Fatalf("expected real read failure: %+v", previous)
			}
			wantCalls := calls.Load()
			f.clock.set(initial.DeadlineAt)
			if name == "late_200" {
				f.clock.set(initial.DeadlineAt.Add(-time.Second))
				late.Store(true)
				wantCalls++
			}
			diagnoseMySQLRunOnce(t, worker)
			f.assertTerminal(t, "inconclusive", initial.DeadlineAt, 0)
			final := f.task(t)
			if calls.Load() != wantCalls || !reflect.DeepEqual(final.LastCheckedAt, previous.LastCheckedAt) || !reflect.DeepEqual(memoryBefore, f.memory(t)) {
				t.Fatalf("expiry invented observation/memory effects: before=%+v after=%+v calls=%d", previous, final, calls.Load())
			}
			if f.count(t, "incident_problem", "incident_id = ? AND code = 'verify_inconclusive' AND status = 'open'", f.parent.ID) != 1 {
				t.Fatal("missing durable manual-verification problem")
			}
			diagnoseMySQLRunOnce(t, worker)
			f.assertTerminal(t, "inconclusive", initial.DeadlineAt, 0)
		})
	}
}

func TestVerificationMySQLScopeRecheckedBeforeHTTP(t *testing.T) {
	for _, name := range []string{"address_drift", "allowlist_removed", "new_slow", "new_postgres", "new_supported_member"} {
		t.Run(name, func(t *testing.T) {
			var oldCalls, newCalls atomic.Int64
			oldServer := diagnoseMySQLHealthServer(t, func(http.ResponseWriter, *http.Request) { oldCalls.Add(1) })
			newServer := diagnoseMySQLHealthServer(t, func(http.ResponseWriter, *http.Request) { newCalls.Add(1) })
			f := newDiagnoseMySQLFixture(t, oldServer.URL, true)
			f.diagnose(t)
			worker := f.execute(t)
			initial, beforeMemory := f.task(t), f.memory(t)
			switch name {
			case "address_drift":
				worker.verifier.binding.BaseURL = newServer.URL
			case "allowlist_removed":
				worker.verifier.binding.AllowedContainers = nil
			case "new_slow":
				f.addMember(t, "Sub2APISlow", "sub2api", "sub2api")
			case "new_postgres":
				f.addMember(t, "PostgresDown", "postgres", "postgres")
			case "new_supported_member":
				f.addMember(t, incident.SupportedAlert, "sub2api", "sub2api")
			}
			diagnoseMySQLRunOnce(t, worker)
			f.assertTerminal(t, "inconclusive", initial.DeadlineAt, 0)
			if oldCalls.Load() != 0 || newCalls.Load() != 0 || !reflect.DeepEqual(beforeMemory, f.memory(t)) {
				t.Fatalf("scope/drift bypass: old=%d new=%d", oldCalls.Load(), newCalls.Load())
			}
		})
	}
}

func TestVerificationMySQLScopeRecheckedAfterHTTPInTransaction(t *testing.T) {
	requested, release := make(chan struct{}, 1), make(chan struct{})
	server := diagnoseMySQLHealthServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requested <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	f := newDiagnoseMySQLFixture(t, server.URL, false)
	f.diagnose(t)
	worker := f.execute(t)
	initial := f.task(t)
	done := make(chan error, 1)
	go func() { done <- worker.RunOnce(context.Background()) }()
	// Always release/join the HTTP worker before fixture cleanup, including fail.
	released := false
	defer func() {
		if !released {
			close(release)
			<-done
		}
	}()
	select {
	case <-requested:
	case <-time.After(10 * time.Second):
		t.Fatal("health request not reached")
	}
	// Scope was valid when HTTP started; the MySQL finalization must not trust
	// that earlier read. This member commits before HTTP completes.
	f.addMember(t, "Sub2APISlow", "sub2api", "sub2api")
	close(release)
	released = true
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.assertTerminal(t, "inconclusive", initial.DeadlineAt, 0)
	if _, err := f.db.GetFaultMemory(context.Background(), f.memoryFingerprint()); !errors.Is(err, store.ErrMemoryNotFound) {
		t.Fatalf("healthy proposal leaked successful memory despite changed scope: %v", err)
	}
	if f.eventCount(t, "verify.passed") != 0 {
		t.Fatal("pre-transaction HTTP success leaked into durable facts")
	}
}

func TestVerificationMySQLDoesNotFollowRedirect(t *testing.T) {
	var redirected, calls atomic.Int64
	target := diagnoseMySQLHealthServer(t, func(http.ResponseWriter, *http.Request) { redirected.Add(1) })
	server := diagnoseMySQLHealthServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, target.URL+"/health", http.StatusFound)
	})
	f := newDiagnoseMySQLFixture(t, server.URL, false)
	f.diagnose(t)
	worker := f.execute(t)
	diagnoseMySQLRunOnce(t, worker)
	task := f.task(t)
	var result VerificationObservation
	if err := json.Unmarshal(task.LastResultJSON, &result); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || redirected.Load() != 0 || task.Status != "pending" || result.Observation != "unhealthy" || !strings.Contains(result.Detail, "302") {
		t.Fatalf("redirect bypass task=%+v result=%+v requests=%d/%d", task, result, calls.Load(), redirected.Load())
	}
}
