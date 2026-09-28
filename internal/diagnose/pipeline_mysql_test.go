package diagnose

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestPipelineMySQLCriticalAuditGates(t *testing.T) {
	for _, tc := range []struct {
		name, table, predicate string
		publication            bool
		absentSeq              int
		collects, reasons      int
	}{
		{"evidence_started", "incident_event", "NEW.event_type = 'collector.started'", false, 2, 0, 0},
		{"llm_started", "incident_event", "NEW.event_type = 'llm.started'", false, 3, 1, 0},
		{"ordinary_evidence_step", "agent_run_step", "NEW.name = 'collect'", false, 2, 1, 0},
		{"ordinary_guard_step", "agent_run_step", "NEW.name = 'rules'", false, 4, 1, 1},
		{"ordinary_policy_step", "agent_run_step", "NEW.name = 'policy'", false, 5, 1, 1},
		{"tool_step_insert", "agent_run_step", "NEW.seq = 30", false, 30, 1, 1},
		// The tool step is inserted first; failing its event must roll back that
		// whole step+event batch rather than merely prevent the next stage.
		{"tool_step_batch_event", "incident_event", "NEW.event_type = 'llm.tool_called'", false, 30, 1, 1},
		{"approval_insert", "approval", "1 = 1", true, 0, 1, 1},
		// Run conclusion and approval row have both been written in this tx.
		{"approval_publication_event", "incident_event", "NEW.event_type = 'approval.created'", true, 0, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := diagnoseMySQLHealthServer(t, func(http.ResponseWriter, *http.Request) { t.Error("Pipeline must not verify or execute") })
			f := newDiagnoseMySQLFixture(t, server.URL, false)
			remove := f.inject(t, tc.table, fmt.Sprintf("NEW.run_id = %d AND (%s)", f.run.ID, tc.predicate))
			err := f.pipeline.Run(context.Background(), f.run)
			if err == nil || !strings.Contains(err.Error(), "diagnose integration injection") {
				t.Fatalf("required MySQL SIGNAL was not propagated: %v", err)
			}
			if f.count(t, "approval", "incident_id = ?", f.parent.ID) != 0 || f.reporter.calls != 0 || f.eventCount(t, "approval.created") != 0 || f.eventCount(t, "run.succeeded") != 0 {
				t.Fatal("critical audit failure published approval/conclusion or notified")
			}
			run := f.loadRun(t)
			wantStatus := "failed"
			if tc.publication {
				wantStatus = "running"
				if run.FinishedAt != nil {
					t.Fatalf("publication rollback retained finished_at: %+v", run)
				}
			}
			if run.Status != wantStatus || run.RCAText != nil || run.PlanJSON != nil || run.TokensIn != 0 || run.TokensOut != 0 {
				t.Fatalf("partial final conclusion after rollback: %+v", run)
			}
			if tc.absentSeq != 0 && f.count(t, "agent_run_step", "run_id = ? AND seq = ?", f.run.ID, tc.absentSeq) != 0 {
				t.Fatalf("failed audit batch retained seq=%d", tc.absentSeq)
			}
			if f.collector.calls != tc.collects || f.reasoner.calls != tc.reasons {
				t.Fatalf("continued external stages: collector=%d reasoner=%d", f.collector.calls, f.reasoner.calls)
			}
			// A different fixture succeeds even while injection is installed:
			// this is also an executable check that the trigger is not global.
			control := newDiagnoseMySQLFixture(t, server.URL, false)
			control.diagnose(t)
			remove()
			// Removing every injection then running a clean fixture proves this
			// was a storage fault, not an invalid target/policy/service setup.
			clean := newDiagnoseMySQLFixture(t, server.URL, false)
			clean.diagnose(t)
			if clean.reporter.calls != 1 || clean.eventCount(t, "approval.created") != 1 || clean.eventCount(t, "run.succeeded") != 1 {
				t.Fatal("clean pipeline did not publish its valid approval and conclusion")
			}
		})
	}
}

func TestPipelineMySQLUnsupportedScopeCannotPublishApproval(t *testing.T) {
	for _, name := range []string{"Sub2APISlow", "PostgresDown", "RedisDown", "mixed", "other_service"} {
		t.Run(name, func(t *testing.T) {
			server := diagnoseMySQLHealthServer(t, func(http.ResponseWriter, *http.Request) { t.Error("unsupported policy must not probe") })
			f := newDiagnoseMySQLFixture(t, server.URL, false)
			switch name {
			case "mixed":
				f.addMember(t, "Sub2APISlow", f.service)
			case "other_service":
				f.addMember(t, diagnoseAlert, "postgres")
			default:
				if err := f.db.Exec("UPDATE alert SET name = ? WHERE fingerprint = ?", name, f.fingerprints[0]).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := f.pipeline.Run(context.Background(), f.run); err != nil {
				t.Fatal(err)
			}
			if f.loadRun(t).Status != "succeeded" || f.count(t, "approval", "incident_id = ?", f.parent.ID) != 0 || f.count(t, "incident_problem", "incident_id = ? AND code = 'policy_blocked' AND status = 'open'", f.parent.ID) != 1 {
				t.Fatal("unsupported fault produced executable approval or lost manual-review problem")
			}
		})
	}
}

func TestPipelineMySQLScopeRecheckedBeforeExecutionClaim(t *testing.T) {
	for _, name := range []string{"resolved", "new_member"} {
		t.Run(name, func(t *testing.T) {
			server := diagnoseMySQLHealthServer(t, func(http.ResponseWriter, *http.Request) { t.Error("stale approval must not probe") })
			f := newDiagnoseMySQLFixture(t, server.URL, false)
			f.diagnose(t)
			if name == "resolved" {
				if err := f.db.Exec("UPDATE incident SET status = 'resolved' WHERE id = ?", f.parent.ID).Error; err != nil {
					t.Fatal(err)
				}
			} else {
				f.addMember(t, diagnoseAlert, f.service)
			}
			row, claimed, err := f.db.ClaimApprovalExecution(context.Background(), f.approval.ID, f.approval.CreatedAt, f.authority.Policy(f.approval.CreatedAt))
			if err != nil || claimed || row.Status != "expired" || f.eventCount(t, "execution.started") != 0 || f.count(t, "verify_task", "approval_id = ?", f.approval.ID) != 0 {
				t.Fatalf("stale execution admitted: row=%+v claimed=%v err=%v", row, claimed, err)
			}
		})
	}
}
