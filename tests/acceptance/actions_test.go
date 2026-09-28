package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/datatypes"

	"oncall-agent/internal/approval"
	"oncall-agent/internal/config"
	"oncall-agent/internal/diagnose"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/store"
	"oncall-agent/internal/sub2api"
	"oncall-agent/internal/tools"
)

// These tests use real MySQL transactions, policy, actions, executor and verifier.
// Only Docker/deployment, the admin API, metrics and diagnosis inputs are fixtures.
// No daemon, deployment command or upstream outside a temporary fixture is used.
func TestUnattendedRollbackActions(t *testing.T) {
	for _, lostReceipt := range []bool{false, true} {
		t.Run(fmt.Sprintf("reconcile_after_write=%v", lostReceipt), func(t *testing.T) {
			f := newActionFixture(t)
			// Keep the Unix socket short enough on macOS as well as Linux.
			dir, err := os.MkdirTemp("", "oa-action-")
			mustAction(t, err)
			t.Cleanup(func() { mustAction(t, os.RemoveAll(dir)) })
			current := "fixture/sub2api@sha256:" + strings.Repeat("a", 64)
			previous := "fixture/sub2api@sha256:" + strings.Repeat("b", 64)
			mustAction(t, os.WriteFile(filepath.Join(dir, "image"), []byte(current), 0600))
			exit := 0
			if lostReceipt {
				exit = 23 // Deployment applied, but its caller received an error.
			}
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"$1\" > image\nprintf x >> writes\nexit %d\n", exit)
			command := filepath.Join(dir, "deploy")
			mustAction(t, os.WriteFile(command, []byte(script), 0700))
			socket := filepath.Join(dir, "engine.sock")
			listener, err := net.Listen("unix", socket)
			mustAction(t, err)
			engine := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				image, err := os.ReadFile(filepath.Join(dir, "image"))
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				switch r.Method + " " + r.URL.Path {
				case "GET /containers/" + f.name + "/json":
					writeActionJSON(w, map[string]any{"Id": f.name, "Name": "/" + f.name, "Image": "sha256:fixture", "Config": map[string]string{"Image": string(image)}, "State": map[string]any{"Status": "running", "Running": true, "StartedAt": "2025-01-01T00:00:00Z"}})
				case "GET /images/sha256:fixture/json":
					writeActionJSON(w, map[string]any{"RepoDigests": []string{string(image)}})
				default:
					t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})}
			go func() { _ = engine.Serve(listener) }()
			t.Cleanup(func() { mustAction(t, engine.Close()) })
			docker, err := tools.NewDockerClient(socket)
			mustAction(t, err)
			mustAction(t, docker.RegisterTools(f.registry, 10))
			health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/health" {
					t.Errorf("unexpected health request: %s %s", r.Method, r.URL.Path)
				}
				writeActionJSON(w, map[string]string{"status": "ok"})
			}))
			t.Cleanup(health.Close)
			service := config.ServiceConfig{Name: f.name, Env: "acceptance", Container: f.name, BaseURL: health.URL,
				Release: config.ReleaseConfig{Command: []string{command}, WorkDir: dir, LockFile: filepath.Join(dir, "deploy.lock"), TimeoutSeconds: 2}}
			now := time.Now().UTC()
			releases := func(context.Context) ([]tools.Release, error) {
				return []tools.Release{{ID: "current-" + f.name, ImageRef: current, Migration: "none", OccurredAt: now},
					{ID: "previous-" + f.name, ImageRef: previous, Migration: "none", VerifiedAt: &now, OccurredAt: now.Add(-time.Hour)}}, nil
			}
			mustAction(t, f.registry.RegisterAction(tools.NewRollbackAction(docker, releases, service)))
			f.authorize(service, tools.ActionDeploymentRollback)
			params, err := json.Marshal(map[string]string{"release_id": "previous-" + f.name})
			mustAction(t, err)
			row := f.publish(tools.ActionDeploymentRollback, incident.Object{Kind: "service", Name: f.name}, params)
			f.execute()
			row = f.get(row.ID, "executed")
			if row.Verification == nil || row.Verification.Status != "pending" {
				t.Fatalf("rollback did not atomically queue verification: %+v", row.Verification)
			}
			var receipt struct {
				Written bool   `json:"written"`
				Outcome string `json:"outcome"`
				Error   string `json:"error"`
				Detail  string `json:"detail"`
			}
			if row.ResultJSON == nil {
				t.Fatal("missing execution receipt")
			}
			mustAction(t, json.Unmarshal(*row.ResultJSON, &receipt))
			if !receipt.Written || receipt.Outcome != string(tools.OutcomeWritten) || (receipt.Error != "") != lostReceipt {
				t.Fatalf("receipt=%+v", receipt)
			}
			if lostReceipt && !strings.Contains(receipt.Detail, "the target shows the write applied") {
				t.Fatalf("external success was not reconciled: %+v", receipt)
			}
			var changes []store.ChangeEvent
			mustAction(t, f.db.Where("approval_id = ?", row.ID).Find(&changes).Error)
			if len(changes) != 1 {
				t.Fatalf("want one persisted change, got %+v", changes)
			}
			change := changes[0]
			if change.ChangeType != "rollback" || change.Service != f.name || change.Env != "acceptance" || change.Actor != "system:approval" ||
				change.ReleaseID == nil || *change.ReleaseID != "previous-"+f.name || change.BeforeRef == nil || *change.BeforeRef != current || change.ImageRef == nil || *change.ImageRef != previous {
				t.Fatalf("normal and reconciled receipts must persist the same rollback facts: %+v", change)
			}
			f.verify(row.ID, nil, "passed")
			f.execute()
			writes, err := os.ReadFile(filepath.Join(dir, "writes"))
			mustAction(t, err)
			if string(writes) != "x" {
				t.Fatalf("deployment replayed: %q", writes)
			}
			f.count(&store.ChangeEvent{}, "approval_id = ?", row.ID, 1)
			f.count(&store.VerifyTask{}, "approval_id = ?", row.ID, 1)
			f.count(&store.FaultCmdHistory{}, "approval_id = ?", row.ID, 1)
		})
	}
}

func TestUnattendedUpstreamActions(t *testing.T) {
	t.Run("capacity_lost_after_approval", func(t *testing.T) {
		f := newActionFixture(t)
		upstream, client := newActionUpstream(t)
		f.upstream(client)
		row := f.publish(tools.ActionUpstreamQuarantine, incident.Object{Kind: "upstream_account", Name: "41"}, nil)
		upstream.mu.Lock()
		upstream.available = 1
		upstream.mu.Unlock()
		f.execute()
		row = f.get(row.ID, "aborted")
		if row.Verification != nil || row.ResultJSON == nil || !strings.Contains(string(*row.ResultJSON), "below the rule floor") {
			t.Fatalf("capacity refusal=%+v", row)
		}
		upstream.assert(t, true, nil, []int64{7}, "anthropic")
		f.count(&store.VerifyTask{}, "approval_id = ?", row.ID, 0)
		f.count(&store.Approval{}, "parent_approval_id = ?", row.ID, 0)
	})

	for _, scenario := range []struct{ name, verdict, drift string }{
		{"failed_restores", "failed", ""},
		{"inconclusive_restores", "inconclusive", ""},
		{"changed_groups_refuses_restore", "failed", "groups"},
		{"changed_platform_refuses_restore", "failed", "platform"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newActionFixture(t)
			upstream, client := newActionUpstream(t)
			f.upstream(client)
			parent := f.publish(tools.ActionUpstreamQuarantine, incident.Object{Kind: "upstream_account", Name: "41"}, nil)
			f.execute()
			parent = f.get(parent.ID, "executed")
			upstream.assert(t, false, []bool{false}, []int64{7}, "anthropic")
			// The executor's fresh-metrics gate passed. Recovery then sees either
			// persistent SLA errors or unavailable metrics, through the real verifier.
			f.metrics.Store(1)
			if scenario.verdict == "inconclusive" {
				f.metrics.Store(2)
			}
			f.verify(parent.ID, client, scenario.verdict)
			var children []store.Approval
			mustAction(t, f.db.Where("parent_approval_id = ?", parent.ID).Find(&children).Error)
			if len(children) != 1 {
				t.Fatalf("want one compensation, got %+v", children)
			}
			child := children[0]
			if child.Status != "approved" || child.ToolName != tools.ActionUpstreamRestore || child.DecidedBy == nil || *child.DecidedBy != "system:compensation" {
				t.Fatalf("compensation was not preauthorized: %+v", child)
			}
			original, err := incident.ParseExecutionContext(parent.ExecutionContext)
			mustAction(t, err)
			undo, err := incident.ParseExecutionContext(child.ExecutionContext)
			mustAction(t, err)
			if undo.Kind != incident.KindCompensation || !bytes.Equal(undo.PreState, original.PreState) || undo.Target != original.Target || undo.Revision != "schedulable=false" {
				t.Fatalf("compensation lost frozen prerequisites: parent=%s child=%s", parent.ExecutionContext, child.ExecutionContext)
			}
			groups, platform := []int64{7}, "anthropic"
			upstream.mu.Lock()
			switch scenario.drift {
			case "groups":
				groups = []int64{99}
				upstream.account.GroupIDs = groups
			case "platform":
				platform = "openai"
				upstream.account.Platform = platform
			}
			upstream.mu.Unlock()
			f.execute()
			if scenario.drift == "" {
				f.get(child.ID, "executed")
				upstream.assert(t, true, []bool{false, true}, groups, platform)
				f.verify(child.ID, client, "passed")
			} else {
				refused := f.get(child.ID, "aborted")
				if refused.Verification != nil || refused.ResultJSON == nil || !strings.Contains(string(*refused.ResultJSON), "changed after approval") {
					t.Fatalf("changed account was not refused: %+v", refused)
				}
				upstream.assert(t, false, []bool{false}, groups, platform)
			}
			f.execute()
			f.count(&store.Approval{}, "parent_approval_id = ?", parent.ID, 1)
			f.count(&store.Approval{}, "parent_approval_id = ?", child.ID, 0)
		})
	}
}

// Only queue selection is scoped to this fixture. Claims, policy revalidation,
// completion and verification finalization all use the original store methods.
// This prevents a test worker from consuming another suite's durable work.
type actionQueue struct {
	*store.DB
	service string
}

func (q actionQueue) NextApprovedApproval(ctx context.Context, now time.Time) (store.Approval, bool, error) {
	scoped := *q.DB
	scoped.DB = q.DB.Where("service = ?", q.service)
	return scoped.NextApprovedApproval(ctx, now)
}

func (q actionQueue) NextVerificationTask(ctx context.Context, now time.Time) (store.VerifyTask, bool, error) {
	scoped := *q.DB
	scoped.DB = q.DB.Where("approval_id IN (?)", q.DB.Model(&store.Approval{}).Select("id").Where("service = ?", q.service))
	return scoped.NextVerificationTask(ctx, now)
}

func (q actionQueue) RequeueStaleVerificationTasks(ctx context.Context, before time.Time) (int64, error) {
	// These synchronous workers never abandon a claim; stale-lease recovery is
	// outside this test. Do not sweep other suites' verification tasks.
	var count int64
	err := q.DB.WithContext(ctx).Model(&store.VerifyTask{}).Where("approval_id IN (?)", q.DB.Model(&store.Approval{}).Select("id").Where("service = ?", q.service)).
		Where("status = ? AND claimed_at < ?", "running", before).Count(&count).Error
	if err == nil && count != 0 {
		err = fmt.Errorf("fixture unexpectedly abandoned %d verification claims", count)
	}
	return 0, err
}

type actionFixture struct {
	t         *testing.T
	db        *store.DB
	ctx       context.Context
	name      string
	parent    store.Incident
	registry  *tools.Registry
	authority *approval.Authority
	metrics   atomic.Int64 // 0 healthy, 1 unhealthy, 2 unavailable
}

func newActionFixture(t *testing.T) *actionFixture {
	t.Helper()
	dsn := os.Getenv("TEST_UNATTENDED_DSN")
	if dsn == "" {
		t.Skip("requires TEST_UNATTENDED_DSN pointing to a migrated oncall_acceptance* database")
	}
	db, err := store.Open(dsn)
	mustAction(t, err)
	t.Cleanup(func() { mustAction(t, db.Close()) })
	var database string
	mustAction(t, db.Raw("SELECT DATABASE()").Scan(&database).Error)
	if !strings.HasPrefix(database, "oncall_acceptance") {
		t.Fatal("refusing a non-acceptance database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	t.Cleanup(cancel)
	f := &actionFixture{t: t, db: db, ctx: ctx, name: fmt.Sprintf("actions-%d", time.Now().UnixNano()), registry: tools.NewRegistry()}
	release, err := db.AcquireExecutorLock(ctx, f.name)
	mustAction(t, err)
	t.Cleanup(release)
	now := time.Now().UTC().Truncate(time.Millisecond)
	f.parent = store.Incident{GroupKey: f.name, Title: f.name, Status: "firing", Severity: 5, AlertsCount: 1, StartedAt: now, LastSeenAt: now}
	mustAction(t, db.Create(&f.parent).Error)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(f.name)))
	// Register cleanup before creating dependents so failed setup cleans its rows too.
	t.Cleanup(func() {
		for _, query := range []string{
			"DELETE FROM notification_task WHERE event_id IN (SELECT id FROM incident_event WHERE incident_id = ?)",
			"DELETE FROM verify_task WHERE approval_id IN (SELECT id FROM approval WHERE incident_id = ?)",
			"DELETE FROM fault_cmd_history WHERE approval_id IN (SELECT id FROM approval WHERE incident_id = ?)",
			"DELETE FROM agent_run_step WHERE run_id IN (SELECT id FROM agent_run WHERE incident_id = ?)",
			"DELETE FROM incident_event WHERE incident_id = ?", "DELETE FROM incident_problem WHERE incident_id = ?",
			"DELETE FROM approval WHERE incident_id = ?", "DELETE FROM agent_run WHERE incident_id = ?",
			"DELETE FROM incident_alert WHERE incident_id = ?", "DELETE FROM last_alert WHERE incident_id = ?",
		} {
			if err := db.Exec(query, f.parent.ID).Error; err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
		for _, deletion := range []struct {
			model any
			where string
			value any
		}{
			{&store.Alert{}, "fingerprint = ?", fingerprint}, {&store.Incident{}, "id = ?", f.parent.ID},
			{&store.ChangeEvent{}, "service = ?", f.name}, {&store.ServiceLock{}, "service = ?", f.name},
			{&store.ControlEvent{}, "rule_id = ?", f.name}, {&store.FaultMemory{}, "group_key = ?", f.name},
		} {
			if err := db.Where(deletion.where, deletion.value).Delete(deletion.model).Error; err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	labels, err := json.Marshal(map[string]string{"service": f.name})
	mustAction(t, err)
	alert := store.Alert{Fingerprint: fingerprint, AlertHash: fingerprint[:32], Source: "acceptance", Name: "ActionFault", Severity: 5, Status: "firing", Labels: labels, Annotations: datatypes.JSON(`{}`), StartsAt: now, ReceivedAt: now}
	mustAction(t, db.Create(&alert).Error)
	mustAction(t, db.Create(&store.LastAlert{Fingerprint: fingerprint, AlertID: alert.ID, AlertHash: alert.AlertHash, Status: "firing", Severity: 5, FirstSeen: now, LastSeen: now, IncidentID: &f.parent.ID}).Error)
	mustAction(t, db.Create(&store.IncidentAlert{IncidentID: f.parent.ID, Fingerprint: fingerprint, LinkedAt: now}).Error)
	mustAction(t, f.registry.Register(tools.ToolSpec{Name: tools.ToolPromInstantQuery, Description: "deterministic SLA metrics", Timeout: time.Second,
		Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
			var args map[string]string
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if f.metrics.Load() == 2 {
				return "", fmt.Errorf("fixture metrics unavailable")
			}
			value := "1"
			switch {
			case strings.Contains(args["query"], "sub2api_requests_5m"):
				value = "100"
			case strings.Contains(args["query"], "sub2api_errors_5m"):
				value = "0"
				if f.metrics.Load() == 1 {
					value = "50"
				}
			case !strings.Contains(args["query"], "sub2api_ops_up"):
				return "", fmt.Errorf("unexpected query: %s", args["query"])
			}
			return fmt.Sprintf(`{"resultType":"vector","result":[{"value":[%d,%q]}]}`, time.Now().Unix(), value), nil
		}}))
	return f
}

func (f *actionFixture) authorize(service config.ServiceConfig, action string) {
	f.t.Helper()
	var err error
	f.authority, err = approval.NewAuthority(service, config.RemediationConfig{RulesVersion: f.name,
		Rules:        []config.RuleConfig{{ID: f.name, Action: action, Mode: "auto", Alerts: []string{"ActionFault"}, MaxExecutions: 1, WindowMinutes: 60, MinAvailableAccounts: 1, MinRequests: 10, MaxErrorRatio: .1, Compensate: true}},
		Verification: config.VerificationConfig{IntervalSeconds: 2, TimeoutSeconds: 1, WindowSeconds: 5, RequiredPasses: 2}}, f.registry)
	mustAction(f.t, err)
}

func (f *actionFixture) upstream(client *sub2api.Client) {
	for _, action := range tools.NewUpstreamActions(client) {
		mustAction(f.t, f.registry.RegisterAction(action))
	}
	f.authorize(config.ServiceConfig{Name: f.name, Env: "acceptance"}, tools.ActionUpstreamQuarantine)
}

func (f *actionFixture) publish(action string, target incident.Object, params json.RawMessage) store.Approval {
	f.t.Helper()
	run, created, err := f.db.RequestRun(f.ctx, store.RunRequest{IncidentID: f.parent.ID, Mode: "full", Trigger: store.RunTriggerAlert, RequestedAt: time.Now()})
	mustAction(f.t, err)
	if !created {
		f.t.Fatal("run not admitted")
	}
	claimed, err := f.db.ClaimAgentRun(f.ctx, run.ID, time.Now())
	mustAction(f.t, err)
	if !claimed {
		f.t.Fatal("run not claimed")
	}
	members, err := f.db.ListIncidentExecutionMembers(f.ctx, f.parent.ID)
	mustAction(f.t, err)
	plan := llm.Plan{Action: action, Target: llm.PlanTarget{Kind: target.Kind, Name: target.Name}, Params: params, Confidence: "high"}
	decision := approval.NewPolicy(f.authority, f.registry, time.Minute, f.db).Decide(f.ctx, plan, approval.PolicyInput{IncidentID: f.parent.ID, Members: members, FaultAlert: "ActionFault", Target: target, EvidenceRefs: []string{"fixture-evidence"}, ObservationOK: true})
	if decision.Kind != approval.DecisionAuto {
		f.t.Fatalf("policy did not authorize: %+v", decision)
	}
	draft, err := approval.NewService(f.db).Prepare(f.parent.ID, run.ID, decision, "acceptance action remediation")
	mustAction(f.t, err)
	raw, err := json.Marshal(plan)
	mustAction(f.t, err)
	mustAction(f.t, f.db.CompleteRun(f.ctx, store.RunCompletion{RunID: run.ID, Status: "succeeded", RCA: "fixture fault", PlanJSON: raw, FinishedAt: time.Now(), Approval: &draft}))
	if draft.ID == 0 {
		f.t.Fatal("CompleteRun did not publish approval")
	}
	return f.get(draft.ID, "approved")
}

func (f *actionFixture) execute() {
	f.t.Helper()
	mustAction(f.t, approval.NewExecutor(actionQueue{f.db, f.name}, f.registry, f.authority, log.New(io.Discard, "", 0)).RunOnce(f.ctx))
}

func (f *actionFixture) get(id uint64, status string) store.Approval {
	f.t.Helper()
	row, err := f.db.GetApproval(f.ctx, id)
	mustAction(f.t, err)
	if row.Status != status {
		f.t.Fatalf("approval %d: want %s, got %s result=%v", id, status, row.Status, row.ResultJSON)
	}
	return row
}

func (f *actionFixture) verify(id uint64, accounts diagnose.AccountReader, want string) {
	f.t.Helper()
	worker := diagnose.NewVerificationWorker(actionQueue{f.db, f.name}, diagnose.NewVerifier(f.registry, accounts, nil), 3600, notify.NoopNotifier{}, log.New(io.Discard, "", 0), f.authority.Binding())
	waitUntil(f.t, f.ctx, func() bool {
		mustAction(f.t, worker.RunOnce(f.ctx))
		row := f.get(id, "executed")
		if row.Verification == nil {
			f.t.Fatal("verification missing")
		}
		status := row.Verification.Status
		if status == "pending" || status == "running" {
			return false
		}
		if status != want {
			f.t.Fatalf("verification want %s, got %+v", want, row.Verification)
		}
		return true
	})
}

func (f *actionFixture) count(model any, where string, id uint64, want int64) {
	f.t.Helper()
	var got int64
	mustAction(f.t, f.db.Model(model).Where(where, id).Count(&got).Error)
	if got != want {
		f.t.Fatalf("%T %s id=%d: want %d rows, got %d", model, where, id, want, got)
	}
}

type actionUpstream struct {
	mu        sync.Mutex
	account   sub2api.Account
	available int64
	writes    []bool
}

func newActionUpstream(t *testing.T) (*actionUpstream, *sub2api.Client) {
	t.Helper()
	u := &actionUpstream{account: sub2api.Account{ID: 41, Status: "active", Platform: "anthropic", Schedulable: true, GroupIDs: []int64{7}}, available: 2}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		if r.Header.Get("x-api-key") != "fixture-admin" {
			t.Error("missing admin authentication")
			http.Error(w, "unauthorized", 401)
			return
		}
		var data any
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/admin/accounts/41":
			data = u.account
		case "POST /api/v1/admin/accounts/41/schedulable":
			var body map[string]bool
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 {
				t.Errorf("invalid scheduling request: %v %v", body, err)
				http.Error(w, "bad request", 400)
				return
			}
			value, ok := body["schedulable"]
			if !ok {
				t.Error("write changed fields other than schedulable")
				http.Error(w, "bad request", 400)
				return
			}
			u.account.Schedulable = value
			u.writes = append(u.writes, value)
			data = u.account
		case "GET /api/v1/admin/ops/account-availability":
			available := u.available
			if !u.account.Schedulable {
				available--
			}
			data = sub2api.Availability{Enabled: true, Groups: map[string]sub2api.GroupAvailability{"7": {GroupID: 7, AvailableCount: available}}, Accounts: map[string]sub2api.AccountAvailability{"41": {AccountID: 41, IsAvailable: u.account.Schedulable}}}
		default:
			t.Errorf("unexpected admin request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeActionJSON(w, map[string]any{"code": 0, "data": data})
	}))
	t.Cleanup(server.Close)
	client, err := sub2api.New(server.URL, "fixture-admin", time.Second, tools.UpstreamEndpoints...)
	mustAction(t, err)
	return u, client
}

func (u *actionUpstream) assert(t *testing.T, schedulable bool, writes []bool, groups []int64, platform string) {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.account.Schedulable != schedulable || !reflect.DeepEqual(u.writes, writes) || !reflect.DeepEqual(u.account.GroupIDs, groups) || u.account.Platform != platform || u.account.Status != "active" {
		t.Fatalf("unexpected upstream mutation: account=%+v writes=%v; want schedulable=%v writes=%v groups=%v platform=%s", u.account, u.writes, schedulable, writes, groups, platform)
	}
}

func writeActionJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func mustAction(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
