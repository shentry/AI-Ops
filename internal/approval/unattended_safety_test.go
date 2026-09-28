package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

func TestUnattendedRecoveryRejectsChangedServiceConfiguration(t *testing.T) {
	for name, change := range map[string]func(*config.ServiceConfig){
		"container":   func(s *config.ServiceConfig) { s.Container = "replacement" },
		"origin":      func(s *config.ServiceConfig) { s.BaseURL = "http://replacement.invalid" },
		"probe":       func(s *config.ServiceConfig) { s.Probe.Model = "replacement-model" },
		"environment": func(s *config.ServiceConfig) { s.Env = "staging" },
	} {
		t.Run(name, func(t *testing.T) {
			row := approvedRow(t, 1)
			op := "interrupted-operation"
			row.Status, row.OperationID = "executing", &op
			db := newFakeExecStore(row)
			action := newRestartAction()
			// A call to the old target would misleadingly report a successful write.
			action.reconcile = func(tools.Operation) (tools.Outcome, error) { return tools.OutcomeWritten, nil }
			executor := testExecutor(t, db, action)
			service := testService
			change(&service)
			current, err := NewAuthority(service, testRemediation(incident.ModeAuto), executor.registry)
			if err != nil {
				t.Fatal(err)
			}
			executor.authority = current
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := executor.Start(ctx); err != nil {
				t.Fatal(err)
			}
			cancel()
			waitExecutor(t, executor)
			if len(action.executed) != 0 || len(action.reconciled) != 0 {
				t.Fatalf("revoked snapshot reached action: executed=%d reconciled=%d", len(action.executed), len(action.reconciled))
			}
			completion := db.committed[row.ID]
			result := resultOf(t, completion)
			if completion.Status != "failed" || !completion.ManualCheck || completion.Change != nil || result["outcome"] != string(tools.OutcomeUnknown) || result["written"] != false || result["error"] == nil {
				t.Fatalf("revoked recovery must require manual investigation: completion=%+v result=%v", completion, result)
			}
		})
	}
}

func TestUnattendedAutoExecutionRequiresCurrentMonitoring(t *testing.T) {
	for _, scenario := range []string{"not configured", "query failure", "missing sample", "stale sample", "exporter down", "fresh"} {
		t.Run(scenario, func(t *testing.T) {
			db := newFakeExecStore(approvedRow(t, 1))
			action := newRestartAction()
			registry := testRegistry(t, action)
			if scenario != "not configured" {
				err := registry.Register(tools.ToolSpec{Name: tools.ToolPromInstantQuery, Description: "test monitoring", Timeout: time.Second, Handler: func(_ context.Context, args json.RawMessage) (string, error) {
					if scenario == "query failure" {
						return "", errors.New("monitoring unavailable")
					}
					if scenario == "missing sample" {
						return `{"resultType":"vector","result":[]}`, nil
					}
					at, value := time.Now(), "1"
					if scenario == "stale sample" {
						at = at.Add(-10 * time.Minute)
					}
					if scenario == "exporter down" && strings.Contains(string(args), "sub2api_ops_up") {
						value = "0"
					}
					return fmt.Sprintf(`{"resultType":"vector","result":[{"value":[%d,%q]}]}`, at.Unix(), value), nil
				}})
				if err != nil {
					t.Fatal(err)
				}
			}
			executor := NewExecutor(db, registry, testAuthority(t, registry, testRemediation(incident.ModeAuto)), nil)
			if err := executor.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			completion := db.committed[1]
			result := resultOf(t, completion)
			if scenario == "fresh" {
				if len(action.executed) != 1 || completion.Status != "executed" {
					t.Fatalf("fresh monitoring blocked execution: %+v", completion)
				}
				return
			}
			if len(db.policies) != 1 || len(action.executed) != 0 || len(action.reconciled) != 0 || completion.Status != "aborted" || completion.ManualCheck || completion.Change != nil || result["written"] != false || result["outcome"] != string(tools.OutcomeNotWritten) || !strings.Contains(fmt.Sprint(result["error"]), "business metrics") {
				t.Fatalf("unobservable auto action: claims=%d executed=%d reconciled=%d completion=%+v result=%v", len(db.policies), len(action.executed), len(action.reconciled), completion, result)
			}
		})
	}
}

func TestUnattendedLeaseFailureStopsClaimsAndRecovery(t *testing.T) {
	for _, startup := range []bool{false, true} {
		t.Run(fmt.Sprintf("startup=%v", startup), func(t *testing.T) {
			interrupted := approvedRow(t, 2)
			op := "interrupted"
			interrupted.Status, interrupted.OperationID = "executing", &op
			db := newFakeExecStore(approvedRow(t, 1), interrupted)
			db.leaseErr = errors.New("executor lease lost")
			action := newRestartAction()
			executor := testExecutor(t, db, action)
			var err error
			if startup {
				err = executor.Start(context.Background())
				if err == nil {
					t.Fatal("startup accepted a lost lease")
				}
				waitExecutor(t, executor)
			} else {
				err = executor.RunOnce(context.Background())
			}
			if !errors.Is(err, db.leaseErr) || len(db.policies) != 0 || len(db.attempts) != 0 || len(action.executed) != 0 || len(action.reconciled) != 0 || db.rows[0].Status != "approved" || db.rows[1].Status != "executing" {
				t.Fatalf("lease failure consumed work: err=%v claims=%d completions=%d executed=%d reconciled=%d rows=%+v", err, len(db.policies), len(db.attempts), len(action.executed), len(action.reconciled), db.rows)
			}
		})
	}
}

func TestUnattendedLeaseLostAfterClaimPreventsWrite(t *testing.T) {
	db := newFakeExecStore(approvedRow(t, 1))
	lost := errors.New("executor lease lost after claim")
	db.claim = func(*store.Approval) bool { db.leaseErr = lost; return true }
	action := newRestartAction()
	err := testExecutor(t, db, action).RunOnce(context.Background())
	completion := db.committed[1]
	result := resultOf(t, completion)
	if !errors.Is(err, lost) || len(db.policies) != 1 || len(action.executed) != 0 || len(action.reconciled) != 0 || completion.Status != "aborted" || result["written"] != false || result["outcome"] != string(tools.OutcomeNotWritten) || !strings.Contains(fmt.Sprint(result["error"]), lost.Error()) {
		t.Fatalf("lost lease allowed a write: err=%v completion=%+v result=%v executed=%d reconciled=%d", err, completion, result, len(action.executed), len(action.reconciled))
	}
}

// Keep the existing action fixture while supplying a reconciliation receipt
// that carries the deployment change discovered after an interrupted write.
type changeReconcilingAction struct {
	*fakeAction
	change *tools.Change
}

func (a *changeReconcilingAction) Reconcile(_ context.Context, op tools.Operation) (tools.Reconciliation, error) {
	a.reconciled = append(a.reconciled, op)
	return tools.Reconciliation{Outcome: tools.OutcomeWritten, Change: a.change}, nil
}

func TestUnattendedReconciliationPreservesDeploymentChange(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovery=%v", recovery), func(t *testing.T) {
			row := approvedRow(t, 1)
			if recovery {
				op := "interrupted"
				row.Status, row.OperationID = "executing", &op
			}
			db := newFakeExecStore(row)
			action := &changeReconcilingAction{fakeAction: newRestartAction(), change: &tools.Change{Type: "rollback", ReleaseID: "r1", Before: "repo@sha256:bad", After: "repo@sha256:good"}}
			action.execute = func(tools.Operation) (tools.Receipt, error) {
				return tools.Receipt{}, errors.New("write response lost")
			}
			executor := testExecutor(t, db, newRestartAction())
			registry := tools.NewRegistry()
			if err := registry.RegisterAction(action); err != nil {
				t.Fatal(err)
			}
			// Reuse the fixture's current metrics tool with the receipt-bearing action.
			if err := registry.Register(tools.ToolSpec{Name: tools.ToolPromInstantQuery, Description: "test monitoring", Timeout: time.Second, Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
				return executor.registry.Execute(ctx, tools.ToolPromInstantQuery, args)
			}}); err != nil {
				t.Fatal(err)
			}
			worker := NewExecutor(db, registry, executor.authority, executor.logger)
			if recovery {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if err := worker.Start(ctx); err != nil {
					t.Fatal(err)
				}
				cancel()
				waitExecutor(t, worker)
			} else if err := worker.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			completion := db.committed[1]
			change := completion.Change
			wantExec := 1
			if recovery {
				wantExec = 0
			}
			if completion.Status != "executed" || len(action.executed) != wantExec || len(action.reconciled) != 1 || change == nil {
				t.Fatalf("reconciliation lost write: completion=%+v executed=%d reconciled=%d", completion, len(action.executed), len(action.reconciled))
			}
			if change.ChangeType != "rollback" || change.ReleaseID == nil || *change.ReleaseID != "r1" || change.BeforeRef == nil || *change.BeforeRef != action.change.Before || change.ImageRef == nil || *change.ImageRef != action.change.After || change.Service != "sub2api" || change.Env != "prod" {
				t.Fatalf("reconciliation lost deployment audit: %+v", change)
			}
		})
	}
}
