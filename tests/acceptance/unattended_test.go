package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/api"
	"oncall-agent/internal/approval"
	"oncall-agent/internal/config"
	"oncall-agent/internal/diagnose"
	"oncall-agent/internal/ingest"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/store"
	"oncall-agent/internal/sub2api"
	"oncall-agent/internal/tools"
)

// Explicitly opt in: this test creates, stops, restarts and removes only its own
// Docker container. Use a disposable migrated oncall_acceptance* database.
func TestUnattendedRestartThroughRealDockerAndMySQL(t *testing.T) {
	dsn, socket := os.Getenv("TEST_UNATTENDED_DSN"), os.Getenv("TEST_DOCKER_SOCKET")
	if dsn == "" || socket == "" {
		t.Skip("requires TEST_UNATTENDED_DSN and TEST_DOCKER_SOCKET")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var database string
	if err := db.Raw("SELECT DATABASE()").Scan(&database).Error; err != nil || !strings.HasPrefix(database, "oncall_acceptance") {
		t.Fatal("refusing a non-acceptance database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	name := fmt.Sprintf("oncall-unattended-%d", time.Now().UnixNano())
	command := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %s: %v", args[0], out, err)
		}
		return strings.TrimSpace(string(out))
	}
	program := "from http.server import BaseHTTPRequestHandler,HTTPServer\nclass H(BaseHTTPRequestHandler):\n def do_GET(self):\n  self.send_response(200);self.end_headers();self.wfile.write(b'ok')\n def do_POST(self):\n  self.rfile.read(int(self.headers.get('Content-Length',0)));self.send_response(200);self.end_headers();self.wfile.write(b'{\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}')\nHTTPServer(('0.0.0.0',8080),H).serve_forever()"
	image := os.Getenv("TEST_DOCKER_IMAGE")
	if image == "" {
		image = "python:3.12-alpine"
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	id := command("run", "-d", "--name", name, "--label", "oncall.test=unattended", "--restart=no", "-p", address+":8080", image, "python", "-u", "-c", program)
	t.Cleanup(func() {
		// Removal uses the immutable ID returned by our own create operation.
		rmCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if out, err := exec.CommandContext(rmCtx, "docker", "rm", "-f", "-v", id).CombinedOutput(); err != nil {
			t.Errorf("cleanup: %s %v", out, err)
		}
	})
	baseURL := "http://" + command("port", id, "8080/tcp")
	waitUntil(t, ctx, func() bool {
		resp, err := http.Get(baseURL + "/health")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	client, err := tools.NewDockerClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	before, err := client.Inspect(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	command("stop", "-t", "1", id)

	registry := tools.NewRegistry()
	if err := client.RegisterTools(registry, 20); err != nil {
		t.Fatal(err)
	}
	// The model and business metric source are deterministic boundaries; Docker,
	// HTTP health/probe, SQL transactions, policy and all workers are real.
	if err := registry.Register(tools.ToolSpec{Name: tools.ToolPromInstantQuery, Description: "fixture SLA metrics", Timeout: time.Second, Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
		var args map[string]string
		_ = json.Unmarshal(raw, &args)
		value := "1"
		if strings.Contains(args["query"], "requests") {
			value = "100"
		}
		if strings.Contains(args["query"], "errors") {
			value = "0"
		}
		return fmt.Sprintf(`{"resultType":"vector","result":[{"value":[%d,%q]}]}`, time.Now().Unix(), value), nil
	}}); err != nil {
		t.Fatal(err)
	}
	service := config.ServiceConfig{Name: name, Env: "acceptance", Container: name, BaseURL: baseURL, Probe: config.ProbeConfig{APIKey: "fixture-key", Path: "/v1/messages", Model: "fixture"}}
	if err := registry.RegisterAction(tools.NewRestartAction(client, service)); err != nil {
		t.Fatal(err)
	}
	rules := config.RemediationConfig{RulesVersion: "acceptance", Rules: []config.RuleConfig{{ID: name, Action: tools.ActionDockerRestart, Mode: "auto", Alerts: []string{"ProcessDown"}, MaxExecutions: 1, WindowMinutes: 60}}, Verification: config.VerificationConfig{IntervalSeconds: 2, TimeoutSeconds: 1, WindowSeconds: 12, RequiredPasses: 2, WatchSeconds: 6}}
	authority, err := approval.NewAuthority(service, rules, registry)
	if err != nil {
		t.Fatal(err)
	}
	release, err := db.AcquireExecutorLock(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	worker := ingest.NewWorker(db, config.IngestConfig{SeverityLabel: "severity"}, config.CorrelateConfig{GroupBy: []string{"labels.service"}, WindowMinutes: 15, MinAlerts: 1}, map[string]string{"critical": "full"}, log.New(io.Discard, "", 0))
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); worker.Wait() })
	auth, err := api.NewAuth("fixture-machine", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	webhook := httptest.NewServer(api.NewAlertmanagerWebhook(db, worker, auth))
	defer webhook.Close()
	started := time.Now().UTC().Format(time.RFC3339Nano)
	send := func(alertName, status string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"version": "4", "status": status, "alerts": []any{map[string]any{"labels": map[string]string{"alertname": alertName, "service": name, "severity": "critical"}, "startsAt": started}}})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, webhook.URL, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer fixture-machine")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 202 {
			t.Fatalf("webhook status %d", resp.StatusCode)
		}
	}
	send(strings.Repeat("x", 256), "firing")
	send("ProcessDown", "firing")
	var run store.AgentRun
	waitUntil(t, ctx, func() bool {
		r, found, err := db.NextPendingAgentRun(ctx)
		if err != nil {
			t.Fatal(err)
		}
		run = r
		return found
	})
	var rawFailed int64
	if err := db.Model(&store.RawEvent{}).Where("status = ? AND payload LIKE ?", "failed", "%"+name+"%").Count(&rawFailed).Error; err != nil || rawFailed != 1 {
		t.Fatalf("poison isolation failed: %d %v", rawFailed, err)
	}
	if ok, err := db.ClaimAgentRun(ctx, run.ID, time.Now()); err != nil || !ok {
		t.Fatalf("run claim %v %v", ok, err)
	}
	builder := diagnose.NewEvidenceBuilder(db, []diagnose.Collector{diagnose.NewDockerInspectCollector(service, registry), diagnose.NewSub2APIHealthCollector(service, config.EvidenceConfig{TimeoutSeconds: 1}), diagnose.NewSub2APIMetricsCollector(registry)})
	pipeline := diagnose.NewPipeline(db, builder, plannedRestart{name: name}, approval.NewPolicy(authority, registry, time.Minute, db), approval.NewService(db), quietReport{}, nil, 0, nil)
	if err := pipeline.Run(ctx, run); err != nil {
		t.Fatal(err)
	}
	executor := approval.NewExecutor(db, registry, authority, log.New(io.Discard, "", 0))
	if err := executor.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var action store.Approval
	if err := db.Where("run_id = ?", run.ID).First(&action).Error; err != nil || action.Status != "executed" {
		t.Fatalf("automatic execution status=%s err=%v", action.Status, err)
	}
	after, err := client.Inspect(ctx, name)
	if err != nil || after.ID != before.ID || !after.StartedAt.After(before.StartedAt) {
		t.Fatalf("real restart not observed: %+v %v", after, err)
	}
	probe, err := sub2api.NewProbe(baseURL, "/v1/messages", "fixture-key", "fixture", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	verify := diagnose.NewVerificationWorker(db, diagnose.NewVerifier(registry, nil, probe), 3600, notify.NoopNotifier{}, log.New(io.Discard, "", 0), authority.Binding())
	waitUntil(t, ctx, func() bool {
		if err := verify.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		row, err := db.GetApproval(ctx, action.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Verification != nil && row.Verification.Status != "pending" && row.Verification.Status != "running" && row.Verification.Status != "stable" {
			t.Fatalf("verification=%+v", row.Verification)
		}
		return row.Verification != nil && row.Verification.Status == "stable"
	})
	if err := executor.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	unchanged, err := client.Inspect(ctx, name)
	if err != nil || !unchanged.StartedAt.Equal(after.StartedAt) {
		t.Fatal("a finished action restarted again")
	}
	send("ProcessDown", "resolved")
	waitUntil(t, ctx, func() bool {
		parent, err := db.GetIncident(ctx, run.IncidentID)
		if err != nil {
			t.Fatal(err)
		}
		return parent.Status == "resolved"
	})
	t.Logf("verified webhook poison isolation -> automatic approval -> real Docker restart -> business probe -> stable watch -> no duplicate action -> resolved incident %d", run.IncidentID)
}

type plannedRestart struct{ name string }

func (r plannedRestart) Diagnose(_ context.Context, evidence, mode string, record func(llm.DiagnosisInput) error) (*llm.DiagnoseResult, error) {
	if err := record(llm.DiagnosisInput{Model: "deterministic-fixture", Evidence: evidence, PromptSHA256: strings.Repeat("0", 64)}); err != nil {
		return nil, err
	}
	return &llm.DiagnoseResult{RCA: "process exited", Confidence: "high", Plan: llm.Plan{Action: tools.ActionDockerRestart, Target: llm.PlanTarget{Kind: "container", Name: r.name}, Confidence: "high", Reason: "restart exited process", EvidenceRefs: []string{"docker_inspect"}}}, nil
}

type quietReport struct{}

func (quietReport) NotifyDiagnosis(context.Context, diagnose.DiagnosisReport) error { return nil }
func waitUntil(t *testing.T, ctx context.Context, condition func() bool) {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}
