package approval

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/store"
)

func preparedDecision(t *testing.T, mode string) Decision {
	t.Helper()
	d := testPolicy(t, newRestartAction(), testRemediation(mode), &fakeState{}).Decide(context.Background(), llm.Plan{Action: "docker_restart"}, testInput())
	if d.PlanHash == "" {
		t.Fatalf("decision = %+v", d)
	}
	return d
}

func TestServicePrepareValidatesAndDoesNotWrite(t *testing.T) {
	// No store is needed: Prepare must only construct a draft for CompleteRun.
	svc := NewService(nil)
	for _, mode := range []string{incident.ModeManual, incident.ModeAuto} {
		d := preparedDecision(t, mode)
		a, err := svc.Prepare(7, 11, d, "safe reason")
		if err != nil {
			t.Fatal(err)
		}
		if a.ID != 0 || a.IncidentID != 7 || a.RunID != 11 || a.PlanHash != d.PlanHash || !a.ExpiresAt.Equal(d.ExpiresAt) || *a.Service != "sub2api" || *a.RuleID != "restart" || a.ParentApprovalID != nil {
			t.Fatalf("draft = %+v", a)
		}
		if mode == incident.ModeManual {
			if a.Status != "pending" || a.DecidedBy != nil {
				t.Fatalf("manual draft = %+v", a)
			}
		} else if a.Status != "approved" || *a.DecidedBy != "system:rule:restart" || *a.DecisionSource != "rule" || a.DecidedAt == nil || *a.DecisionReason != "safe reason" {
			t.Fatalf("automatic draft = %+v", a)
		}
		// The draft owns its bytes: later caller changes cannot alter the snapshot.
		d.Args[0] = 'x'
		d.ExecutionContext[0] = 'x'
		if hash, err := incident.PlanHash(a.ToolName, a.ArgsJSON, a.ExecutionContext); err != nil || hash != a.PlanHash {
			t.Fatalf("draft hash = %s, %v", hash, err)
		}
	}
}

func TestServicePrepareRejectsMalformedOrMismatchedSnapshot(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Decision)
	}{
		{"missing snapshot", func(d *Decision) { d.ExecutionContext = nil }},
		{"malformed snapshot", func(d *Decision) { d.ExecutionContext = []byte(`{"version":3}`) }},
		{"escalated mode", func(d *Decision) {
			d.ExecutionContext = []byte(strings.Replace(string(d.ExecutionContext), `"mode":"manual"`, `"mode":"auto"`, 1))
		}},
		{"changed target", func(d *Decision) { d.Args = []byte(`{"target_kind":"container","target_name":"other"}`) }},
		{"bad hash", func(d *Decision) { d.PlanHash = "wrong" }},
		{"manual snapshot as auto", func(d *Decision) { d.Kind = DecisionAuto }},
		{"observe", func(d *Decision) { d.Kind = DecisionObserve }},
		{"denied", func(d *Decision) { d.Kind = DecisionDenied }},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := preparedDecision(t, incident.ModeManual)
			test.change(&d)
			if _, err := NewService(nil).Prepare(7, 11, d, "reason"); err == nil {
				t.Fatal("unsafe approval draft accepted")
			}
		})
	}
	for _, test := range []struct {
		incidentID, runID uint64
		reason            string
	}{{0, 11, "r"}, {7, 0, "r"}, {7, 11, " "}} {
		if _, err := NewService(nil).Prepare(test.incidentID, test.runID, preparedDecision(t, incident.ModeManual), test.reason); err == nil {
			t.Fatalf("invalid draft accepted: %+v", test)
		}
	}
}

type fakeApprovalStore struct {
	row                                 store.Approval
	id                                  uint64
	status, hash, actor, reason, source string
	err                                 error
}

func (f *fakeApprovalStore) GetApproval(context.Context, uint64) (store.Approval, error) {
	return f.row, f.err
}
func (f *fakeApprovalStore) ListApprovals(_ context.Context, status string) ([]store.Approval, error) {
	f.status = status
	return []store.Approval{f.row}, f.err
}
func (f *fakeApprovalStore) DecideApproval(_ context.Context, id uint64, status, hash, actor, reason, source string, _ time.Time) (store.Approval, error) {
	f.id, f.status, f.hash, f.actor, f.reason, f.source = id, status, hash, actor, reason, source
	return f.row, f.err
}

func TestServiceForwardsExpectedHashToAtomicDecision(t *testing.T) {
	for _, approve := range []bool{true, false} {
		for _, dbErr := range []error{nil, store.ErrApprovalConflict, store.ErrApprovalNotFound} {
			db := &fakeApprovalStore{row: store.Approval{ID: 42}, err: dbErr}
			row, err := NewService(db).Decide(context.Background(), 42, approve, "expected-hash", "ops", "operator note", "web")
			if !errors.Is(err, dbErr) || row.ID != 42 || db.id != 42 || db.hash != "expected-hash" || db.actor != "ops" || db.reason != "operator note" || db.source != "web" {
				t.Fatalf("row=%+v err=%v db=%+v", row, err, db)
			}
			want := "denied"
			if approve {
				want = "approved"
			}
			if db.status != want {
				t.Fatal(db.status)
			}
		}
	}
}

func TestServiceGetAndList(t *testing.T) {
	db := &fakeApprovalStore{row: store.Approval{ID: 42}}
	svc := NewService(db)
	if row, err := svc.Get(context.Background(), 42); err != nil || row.ID != 42 {
		t.Fatalf("Get = %+v, %v", row, err)
	}
	if rows, err := svc.List(context.Background(), "pending"); err != nil || len(rows) != 1 || db.status != "pending" {
		t.Fatalf("List = %+v, %v", rows, err)
	}
}
