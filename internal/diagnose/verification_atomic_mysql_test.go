package diagnose

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/store"
)

func TestVerificationMySQLMemoryAndAuditRollback(t *testing.T) {
	for _, table := range []string{"incident_event", "fault_memory"} {
		t.Run(table, func(t *testing.T) {
			server := diagnoseMySQLHealthServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			f := newDiagnoseMySQLFixture(t, server.URL, false)
			f.diagnose(t)
			worker := f.execute(t)
			initial := f.task(t)
			condition := fmt.Sprintf("NEW.incident_id = %d AND NEW.event_type = 'verify.passed'", f.parent.ID)
			if table == "fault_memory" {
				condition = fmt.Sprintf("NEW.fingerprint = '%s'", f.memoryFingerprint())
			}
			remove := f.inject(t, table, condition)
			if err := worker.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "diagnose integration injection") {
				t.Fatalf("verification write failure swallowed: %v", err)
			}
			rolledBack := f.task(t)
			if rolledBack.Status != "running" || rolledBack.ClaimedAt == nil || rolledBack.LastCheckedAt != nil || rolledBack.FinishedAt != nil || len(rolledBack.LastResultJSON) != 0 || !rolledBack.DeadlineAt.Equal(initial.DeadlineAt) {
				t.Fatalf("partial terminal task after rollback: %+v", rolledBack)
			}
			if f.count(t, "agent_run_step", "run_id = ? AND kind = 'verify'", f.run.ID) != 0 || f.eventCount(t, "verify.passed") != 0 {
				t.Fatal("terminal audit escaped failed verification transaction")
			}
			if _, err := f.db.GetFaultMemory(context.Background(), f.memoryFingerprint()); !errors.Is(err, store.ErrMemoryNotFound) {
				t.Fatalf("memory escaped failed verification transaction: %v", err)
			}
			remove()
			// Genuine read-only lease recovery, still within the original window.
			// A new real 200 observation can now atomically finalize + write memory.
			f.clock.set(f.clock.now().Add(incident.VerificationLease + time.Second))
			diagnoseMySQLRunOnce(t, worker)
			f.assertTerminal(t, "passed", initial.DeadlineAt, 0)
			entry := f.memory(t)
			diagnoseMySQLRunOnce(t, worker)
			f.assertTerminal(t, "passed", initial.DeadlineAt, 0)
			if !reflect.DeepEqual(entry, f.memory(t)) {
				t.Fatal("memory side effects replayed")
			}
		})
	}
}

func TestVerificationMySQLRetryAndDemotionRollback(t *testing.T) {
	server := diagnoseMySQLHealthServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	f := newDiagnoseMySQLFixture(t, server.URL, true)
	f.diagnose(t)
	worker := f.execute(t)
	initial, memoryBefore := f.task(t), f.memory(t)
	// A genuine HTTP 503 at deadline-minus-one-interval, not a seeded JSON claim.
	f.clock.set(initial.DeadlineAt.Add(-10 * time.Second))
	diagnoseMySQLRunOnce(t, worker)
	observed := f.task(t)
	remove := f.inject(t, "incident_event", fmt.Sprintf("NEW.incident_id = %d AND NEW.event_type = 'retry.scheduled'", f.parent.ID))
	f.clock.set(initial.DeadlineAt)
	if err := worker.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "diagnose integration injection") {
		t.Fatalf("retry scheduling fault swallowed: %v", err)
	}
	rolledBack := f.task(t)
	if rolledBack.Status != "running" || rolledBack.FinishedAt != nil || !reflect.DeepEqual(rolledBack.LastCheckedAt, observed.LastCheckedAt) || !reflect.DeepEqual(rolledBack.LastResultJSON, observed.LastResultJSON) || !reflect.DeepEqual(memoryBefore, f.memory(t)) {
		t.Fatalf("failed task/demotion escaped retry rollback: task=%+v memory=%+v", rolledBack, f.memory(t))
	}
	if f.count(t, "agent_run", "incident_id = ? AND retry_of = ?", f.parent.ID, f.run.ID) != 0 || f.eventCount(t, "retry.scheduled") != 0 || f.eventCount(t, "verify.failed") != 0 || f.count(t, "incident_problem", "incident_id = ? AND code = 'verify_failed'", f.parent.ID) != 0 || f.count(t, "agent_run_step", "run_id = ? AND kind = 'verify'", f.run.ID) != 0 {
		t.Fatal("partial retry/task audit or problem committed")
	}
	remove()
	// Unlike an immediate DB commit retry, lease recovery occurs after the
	// original unhealthy observation is stale. It must NOT resurrect failed or
	// reset the deadline merely to obtain the desired retry/demotion outcome.
	f.clock.set(initial.DeadlineAt.Add(incident.VerificationLease + time.Second))
	diagnoseMySQLRunOnce(t, worker)
	f.assertTerminal(t, "inconclusive", initial.DeadlineAt, 0)
	if !reflect.DeepEqual(memoryBefore, f.memory(t)) || !reflect.DeepEqual(f.task(t).LastCheckedAt, observed.LastCheckedAt) {
		t.Fatal("expiry-only recovery changed memory or actual observation time")
	}
	// The same unmodified pipeline/worker with a fresh window does fail and
	// create exactly one retry after injection removal, ruling out bad setup.
	clean := newDiagnoseMySQLFixture(t, server.URL, true)
	clean.diagnose(t)
	cleanWorker := clean.execute(t)
	deadline := clean.task(t).DeadlineAt
	clean.clock.set(deadline.Add(-10 * time.Second))
	diagnoseMySQLRunOnce(t, cleanWorker)
	clean.clock.set(deadline)
	diagnoseMySQLRunOnce(t, cleanWorker)
	clean.assertTerminal(t, "failed", deadline, 1)
	if clean.memory(t).Confidence != "low" {
		t.Fatal("clean failed verification did not demote its memory")
	}
}
