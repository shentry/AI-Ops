package diagnose

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/store"
	"oncall-agent/internal/sub2api"
	"oncall-agent/internal/tools"
)

// stubRegistry 用假 handler 装一个真 Registry：超时/截断纪律走真实现，
// 外部调用走桩。
func stubRegistry(t *testing.T, handlers map[string]tools.Handler) *tools.Registry {
	t.Helper()
	registry := tools.NewRegistry()
	for name, handler := range handlers {
		if err := registry.Register(tools.ToolSpec{
			Name: name, Description: "stub",
			Timeout: time.Second, MaxOutput: 1024, Handler: handler,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func TestSnapshotCollectorRendersMembersAndAlerts(t *testing.T) {
	target := Target{
		Incident: store.Incident{ID: 9, GroupKey: "payments", Status: "firing", Severity: 5, AlertsCount: 1,
			StartedAt: time.Date(2026, 8, 19, 2, 0, 0, 0, time.UTC), LastSeenAt: time.Date(2026, 8, 19, 2, 1, 0, 0, time.UTC)},
		Members: []store.IncidentMember{{Fingerprint: "fp1", Name: "HighCPU", Status: "firing", Severity: 5}},
		Alerts: []store.Alert{{
			Fingerprint: "fp1", Name: "HighCPU", Status: "firing", Severity: 5,
			Labels:       []byte(`{"alertname":"HighCPU","token":"abc123"}`),
			Annotations:  []byte(`{"summary":"cpu hot"}`),
			GeneratorURL: "http://prom/graph?g0.expr=up",
		}},
	}
	item := NewSnapshotCollector().Collect(context.Background(), target)
	if item.Status != ItemOK {
		t.Fatalf("status = %s, err = %s", item.Status, item.Err)
	}
	for _, want := range []string{"group_key=payments", "fingerprint=fp1", "cpu hot", "generator_url"} {
		if !strings.Contains(item.Body, want) {
			t.Fatalf("snapshot body missing %q:\n%s", want, item.Body)
		}
	}
	// labels 里的 token 必须被脱敏。
	if strings.Contains(item.Body, "abc123") {
		t.Fatalf("snapshot leaked secret:\n%s", item.Body)
	}
}

func TestExtractGeneratorExpr(t *testing.T) {
	expr, err := ExtractGeneratorExpr("http://127.0.0.1:9090/graph?g0.expr=up%7Bjob%3D%22x%22%7D&g0.tab=1")
	if err != nil || expr != `up{job="x"}` {
		t.Fatalf("expr = %q, err = %v", expr, err)
	}
	if expr, _ := ExtractGeneratorExpr(""); expr != "" {
		t.Fatalf("empty url expr = %q", expr)
	}
	if _, err := ExtractGeneratorExpr("http://x/graph?g0.tab=1"); err == nil {
		t.Fatal("missing g0.expr should error")
	}
}

func TestPromReplayCollectorMissingGeneratorURL(t *testing.T) {
	registry := stubRegistry(t, nil)
	collector := NewPromReplayCollector(registry, 15)
	item := collector.Collect(context.Background(), Target{Incident: store.Incident{ID: 1}})
	if item.Status != ItemMissing {
		t.Fatalf("status = %s, want missing", item.Status)
	}
}

func TestPromReplayCollectorQueriesRange(t *testing.T) {
	var gotArgs map[string]string
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolPromRangeQuery: func(_ context.Context, raw json.RawMessage) (string, error) {
			if err := json.Unmarshal(raw, &gotArgs); err != nil {
				return "", err
			}
			return `{"resultType":"matrix"}`, nil
		},
	})
	startedAt := time.Date(2026, 8, 19, 3, 0, 0, 0, time.UTC)
	target := Target{
		Incident: store.Incident{ID: 1, StartedAt: startedAt},
		Alerts:   []store.Alert{{Name: "A", GeneratorURL: "http://prom/graph?g0.expr=up"}},
	}
	item := NewPromReplayCollector(registry, 15).Collect(context.Background(), target)
	if item.Status != ItemOK {
		t.Fatalf("status = %s, err = %s", item.Status, item.Err)
	}
	// range_minutes=15 → 半窗口钳制为 7.5 分钟，保证 [start,end] 不被工具截断。
	half := 7*time.Minute + 30*time.Second
	if gotArgs["query"] != "up" || gotArgs["start"] != startedAt.Add(-half).Format(time.RFC3339) || gotArgs["end"] != startedAt.Add(half).Format(time.RFC3339) {
		t.Fatalf("range args = %v", gotArgs)
	}
	if !strings.Contains(item.Body, "matrix") {
		t.Fatalf("body = %s", item.Body)
	}
}

// 窗口按每条告警自己的 firing 时刻算，不是 incident 的 StartedAt：
// 晚 20 分钟才 firing 的成员，用 incident 时刻算窗口会完全错过触发现场。
func TestPromReplayCollectorUsesPerAlertFiringTime(t *testing.T) {
	windows := map[string][2]string{}
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolPromRangeQuery: func(_ context.Context, raw json.RawMessage) (string, error) {
			var args map[string]string
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			windows[args["query"]] = [2]string{args["start"], args["end"]}
			return `{"resultType":"matrix"}`, nil
		},
	})
	incidentStart := time.Date(2026, 8, 19, 3, 0, 0, 0, time.UTC)
	lateFiring := incidentStart.Add(20 * time.Minute)
	target := Target{
		Incident: store.Incident{ID: 1, StartedAt: incidentStart},
		Alerts: []store.Alert{
			{Name: "First", GeneratorURL: "http://prom/graph?g0.expr=up", StartsAt: incidentStart},
			{Name: "Late", GeneratorURL: "http://prom/graph?g0.expr=down", StartsAt: lateFiring},
		},
	}
	item := NewPromReplayCollector(registry, 15).Collect(context.Background(), target)
	if item.Status != ItemOK {
		t.Fatalf("status = %s, err = %s", item.Status, item.Err)
	}
	half := 7*time.Minute + 30*time.Second
	if got := windows["down"]; got[0] != lateFiring.Add(-half).Format(time.RFC3339) || got[1] != lateFiring.Add(half).Format(time.RFC3339) {
		t.Fatalf("late alert window = %v, want centered on its own firing time %s", got, lateFiring)
	}
	if got := windows["up"]; got[0] != incidentStart.Add(-half).Format(time.RFC3339) {
		t.Fatalf("first alert window = %v", got)
	}
	// 正文里带 firing 时刻，回放时能核对窗口是怎么来的。
	if !strings.Contains(item.Body, "firing_at "+lateFiring.Format(time.RFC3339)) {
		t.Fatalf("body missing firing time:\n%s", item.Body)
	}
}

// 同一 expr 来自多条告警时取最早的 firing 时刻（第一次触发的现场）。
func TestPromReplayCollectorTakesEarliestFiringPerExpr(t *testing.T) {
	var gotStart string
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolPromRangeQuery: func(_ context.Context, raw json.RawMessage) (string, error) {
			var args map[string]string
			_ = json.Unmarshal(raw, &args)
			gotStart = args["start"]
			return `{"resultType":"matrix"}`, nil
		},
	})
	early := time.Date(2026, 8, 19, 3, 0, 0, 0, time.UTC)
	target := Target{
		Incident: store.Incident{ID: 1, StartedAt: early},
		Alerts: []store.Alert{
			{Name: "B", GeneratorURL: "http://prom/graph?g0.expr=up", StartsAt: early.Add(10 * time.Minute)},
			{Name: "A", GeneratorURL: "http://prom/graph?g0.expr=up", StartsAt: early},
		},
	}
	NewPromReplayCollector(registry, 15).Collect(context.Background(), target)
	half := 7*time.Minute + 30*time.Second
	if gotStart != early.Add(-half).Format(time.RFC3339) {
		t.Fatalf("start = %s, want window around the earliest firing %s", gotStart, early)
	}
}

func TestGoldenMetricsCollectorPartialFailure(t *testing.T) {
	calls := 0
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolPromInstantQuery: func(_ context.Context, raw json.RawMessage) (string, error) {
			calls++
			var args map[string]string
			_ = json.Unmarshal(raw, &args)
			if strings.Contains(args["query"], "node_cpu") {
				return "", fmt.Errorf("prometheus down")
			}
			return `{"resultType":"vector","result":[{"value":[1,"42"]}]}`, nil
		},
	})
	item := NewGoldenMetricsCollector(registry).Collect(context.Background(), Target{})
	if item.Status != ItemPartial || item.Err != "1 of 5 queries failed" {
		t.Fatalf("item = %+v, want partial with the failure recorded", item)
	}
	if !strings.Contains(item.Body, "cpu_usage_percent") || !strings.Contains(item.Body, "error:") {
		t.Fatalf("body = %s", item.Body)
	}
	if calls != 5 {
		t.Fatalf("queries = %d, want 5", calls)
	}
}

func TestSub2APIHealthCollectorMissingConfig(t *testing.T) {
	item := NewSub2APIHealthCollector(config.ServiceConfig{Name: "sub2api"}, config.EvidenceConfig{TimeoutSeconds: 5}).Collect(context.Background(), Target{})
	if item.Status != ItemMissing || item.Health != nil {
		t.Fatalf("item = %+v, want missing without facts", item)
	}
}

func TestSub2APIHealthCollectorHealthyFacts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	item := NewSub2APIHealthCollector(config.ServiceConfig{Name: "sub2api", BaseURL: server.URL}, config.EvidenceConfig{TimeoutSeconds: 5}).Collect(context.Background(), Target{})
	if item.Status != ItemOK || item.Health == nil || item.Health.Observation != "healthy" || item.Health.StatusCode != 200 {
		t.Fatalf("item = %+v health = %+v", item, item.Health)
	}
	if item.Object == nil || *item.Object != (ObjectRef{Kind: "service", Name: "sub2api"}) {
		t.Fatalf("object = %+v", item.Object)
	}
	if !strings.Contains(item.Body, "status=200") {
		t.Fatalf("body = %s", item.Body)
	}
}

// /health 返回 5xx 时采集状态必须是 error，不能报 ok ——
// 否则下游会把"网关挂了"的证据当成"网关正常"的证据读。正文照样保留。
func TestSub2APIHealthCollectorUnhealthyMarksError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"status":"degraded"}`)
	}))
	defer server.Close()
	item := NewSub2APIHealthCollector(config.ServiceConfig{Name: "sub2api", BaseURL: server.URL}, config.EvidenceConfig{TimeoutSeconds: 5}).Collect(context.Background(), Target{})
	if item.Status != ItemError || !strings.Contains(item.Err, "503") {
		t.Fatalf("item = %+v, want error with status code", item)
	}
	if item.Health == nil || item.Health.Observation != "unhealthy" || item.Health.StatusCode != 503 {
		t.Fatalf("health = %+v", item.Health)
	}
	for _, want := range []string{"status=503", "degraded"} {
		if !strings.Contains(item.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, item.Body)
		}
	}
}

// 连不上是观测失败：状态 error，事实 unavailable —— 既不是健康，也不是已确认的故障。
func TestSub2APIHealthCollectorUnreachableIsUnavailable(t *testing.T) {
	item := NewSub2APIHealthCollector(config.ServiceConfig{Name: "sub2api", BaseURL: "http://127.0.0.1:1"}, config.EvidenceConfig{TimeoutSeconds: 1}).Collect(context.Background(), Target{})
	if item.Status != ItemError || item.Health == nil || item.Health.Observation != "unavailable" || item.Health.StatusCode != 0 {
		t.Fatalf("item = %+v health = %+v", item, item.Health)
	}
}

func TestSub2APIMetricsCollectorReadsExporterWithAlertBaseline(t *testing.T) {
	var times []string
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolPromInstantQuery: func(_ context.Context, raw json.RawMessage) (string, error) {
			var args map[string]string
			_ = json.Unmarshal(raw, &args)
			times = append(times, args["time"])
			if strings.Contains(args["query"], "sub2api_probe_success") {
				return "", fmt.Errorf("prometheus down")
			}
			value := "1"
			if strings.Contains(args["query"], "requests") {
				value = "100"
			}
			return fmt.Sprintf(`{"resultType":"vector","result":[{"value":[%d,%q]}]}`, time.Now().Unix(), value), nil
		},
	})
	started := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	item := NewSub2APIMetricsCollector(registry).Collect(context.Background(), Target{Incident: store.Incident{StartedAt: started}})
	if item.Status != ItemPartial || item.Err != "1 of 8 queries failed" || item.Business == nil {
		t.Fatalf("item = %+v, want partial", item)
	}
	for _, want := range []string{"sla_error_ratio_5m: ", "sla_error_ratio_5m@alert-15m: ", "top_upstream_accounts_5m: ", "business_probe_success: error"} {
		if !strings.Contains(item.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, item.Body)
		}
	}
	baselines := 0
	for _, at := range times {
		if at == "2026-09-24T02:45:00Z" {
			baselines++
		}
	}
	if baselines != 2 {
		t.Fatalf("baseline query times = %v", times)
	}
}

func TestDockerInspectCollectorRecordsIdentityAndFacts(t *testing.T) {
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolDockerInspect: func(_ context.Context, raw json.RawMessage) (string, error) {
			if string(raw) != `{"name":"sub2api"}` {
				t.Errorf("inspect args = %s", raw)
			}
			return `{"id":"c0ffee","name":"sub2api","image":"weishaw/sub2api:v0.1.164","image_id":"sha256:a94c","status":"running","running":true,"restart_count":2,"restart_policy":"unless-stopped","oom_killed":true,"repo_digests":["weishaw/sub2api@sha256:1234"]}`, nil
		},
	})
	item := NewDockerInspectCollector(config.ServiceConfig{Container: "sub2api"}, registry).Collect(context.Background(), Target{})
	if item.Status != ItemOK || item.Object == nil || *item.Object != (ObjectRef{Kind: "container", Name: "sub2api", ID: "c0ffee"}) {
		t.Fatalf("item = %+v object = %+v", item, item.Object)
	}
	c := item.Container
	if c == nil || !c.Running || !c.OOMKilled || c.RestartCount != 2 || c.RestartPolicy != "unless-stopped" || c.ImageID != "sha256:a94c" || len(c.RepoDigests) != 1 {
		t.Fatalf("container facts = %+v", c)
	}
	rendered := (Evidence{Items: []EvidenceItem{item}}).Render()
	for _, want := range []string{"- object: container sub2api id=c0ffee", `"RestartPolicy":"unless-stopped"`} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render missing %q:\n%s", want, rendered)
		}
	}
}

// inspect 失败或身份对不上都是 error：不能用一个 ok 把"没取到对象"藏起来。
func TestDockerInspectCollectorFailuresAreErrors(t *testing.T) {
	for name, handler := range map[string]tools.Handler{
		"tool error":  func(context.Context, json.RawMessage) (string, error) { return "", fmt.Errorf("container not found") },
		"undecodable": func(context.Context, json.RawMessage) (string, error) { return "not json", nil },
		"no id": func(context.Context, json.RawMessage) (string, error) {
			return `{"name":"sub2api","status":"running"}`, nil
		},
		"other object": func(context.Context, json.RawMessage) (string, error) { return `{"id":"x","name":"batch-worker"}`, nil },
	} {
		registry := stubRegistry(t, map[string]tools.Handler{tools.ToolDockerInspect: handler})
		item := NewDockerInspectCollector(config.ServiceConfig{Container: "sub2api"}, registry).Collect(context.Background(), Target{})
		if item.Status != ItemError || item.Object != nil || item.Container != nil {
			t.Fatalf("%s: item = %+v, want error without identity", name, item)
		}
	}
}

func TestDockerLogsCollectorLooksBackBeforeIncident(t *testing.T) {
	var since string
	registry := stubRegistry(t, map[string]tools.Handler{
		tools.ToolDockerLogs: func(_ context.Context, raw json.RawMessage) (string, error) {
			var args map[string]any
			_ = json.Unmarshal(raw, &args)
			since, _ = args["since"].(string)
			return "lines=1 patterns=1 (most recent first)\ncount=1 first=x last=x | boom\n", nil
		},
	})
	service, evidence := config.ServiceConfig{Container: "sub2api"}, config.EvidenceConfig{LogMaxLines: 50}
	target := Target{Incident: store.Incident{ID: 1, StartedAt: time.Date(2026, 8, 19, 4, 0, 0, 0, time.UTC)}}
	item := NewDockerLogsCollector(service, evidence, registry).Collect(context.Background(), target)
	if item.Status != ItemOK || !strings.Contains(item.Body, "boom") || item.Object == nil || item.Object.Name != "sub2api" {
		t.Fatalf("item = %+v", item)
	}
	if since != "2026-08-19T03:45:00Z" {
		t.Fatalf("since = %s, want 15 minutes before the incident", since)
	}

	failing := stubRegistry(t, map[string]tools.Handler{
		tools.ToolDockerLogs: func(context.Context, json.RawMessage) (string, error) { return "", fmt.Errorf("container not found") },
	})
	if item := NewDockerLogsCollector(service, evidence, failing).Collect(context.Background(), target); item.Status != ItemError || item.Object != nil {
		t.Fatalf("failed logs item = %+v, want error", item)
	}
}

func TestDockerCollectorsMissingContainer(t *testing.T) {
	for _, collector := range []Collector{
		NewDockerInspectCollector(config.ServiceConfig{}, stubRegistry(t, nil)),
		NewDockerLogsCollector(config.ServiceConfig{}, config.EvidenceConfig{}, stubRegistry(t, nil)),
	} {
		if item := collector.Collect(context.Background(), Target{}); item.Status != ItemMissing {
			t.Fatalf("%s status = %s, want missing", collector.Name(), item.Status)
		}
	}
}

func TestPostgresCollectorMissingConfig(t *testing.T) {
	item := NewPostgresCollector(config.ServiceConfig{}, config.EvidenceConfig{}).Collect(context.Background(), Target{})
	if item.Status != ItemMissing || !strings.Contains(item.Err, "not configured") {
		t.Fatalf("postgres item = %+v, want missing", item)
	}
}

func TestRedisCollectorMissingConfig(t *testing.T) {
	item := NewRedisCollector(config.ServiceConfig{}, config.EvidenceConfig{}).Collect(context.Background(), Target{})
	if item.Status != ItemMissing || !strings.Contains(item.Err, "not configured") {
		t.Fatalf("redis item = %+v, want missing", item)
	}
}

func TestRedisInfoValue(t *testing.T) {
	info := "# Memory\r\nused_memory:1048576\r\nused_memory_human:1.00M\r\n"
	if got := redisInfoValue(info, "used_memory"); got != "1048576" {
		t.Fatalf("redisInfoValue(used_memory) = %q", got)
	}
	if got := redisInfoValue(info, "missing_key"); got != "unknown" {
		t.Fatalf("redisInfoValue(missing) = %q", got)
	}
}

type fakeUpstream struct {
	availability sub2api.Availability
	errs         sub2api.UpstreamErrors
	err          error
}

func (f fakeUpstream) Availability(context.Context) (sub2api.Availability, error) {
	return f.availability, f.err
}

func (f fakeUpstream) UpstreamErrors(_ context.Context, window string, _ int) (sub2api.UpstreamErrors, error) {
	if window != "5m" {
		return sub2api.UpstreamErrors{}, fmt.Errorf("window %s", window)
	}
	return f.errs, f.err
}

func TestUpstreamAccountsCollectorAttributesErrorsToAccounts(t *testing.T) {
	now := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	later := now.Add(time.Minute)
	seven, nine := int64(7), int64(9)
	ops := fakeUpstream{
		availability: sub2api.Availability{Enabled: true,
			Groups: map[string]sub2api.GroupAvailability{"2": {GroupID: 2, GroupName: "claude", TotalAccounts: 3, AvailableCount: 2}},
			Accounts: map[string]sub2api.AccountAvailability{
				"7": {AccountID: 7, GroupID: 2, IsAvailable: true},
				"8": {AccountID: 8, GroupID: 2, IsAvailable: true},
				"9": {AccountID: 9, GroupID: 2, TempUnschedulableUntil: &later},
			}},
		errs: sub2api.UpstreamErrors{Total: 5, Items: []sub2api.UpstreamError{{AccountID: &seven, StatusCode: 529}, {AccountID: &seven, StatusCode: 529}, {AccountID: &nine, StatusCode: 429}}},
	}
	collector := NewUpstreamAccountsCollector(ops).(*upstreamAccountsCollector)
	collector.now = func() time.Time { return now }
	item := collector.Collect(context.Background(), Target{})
	facts := item.Upstream
	if item.Status != ItemOK || facts == nil || !facts.RealtimeEnabled || !facts.Sampled || facts.TotalErrors != 5 || len(facts.Accounts) != 3 {
		t.Fatalf("item=%+v facts=%+v", item, facts)
	}
	if top := facts.Accounts[0]; top.ID != 7 || top.Errors != 2 || !top.Available {
		t.Fatalf("most failing account first: %+v", facts.Accounts)
	}
	if facts.Accounts[1].ID != 9 || !facts.Accounts[1].TempUnschedulable {
		t.Fatalf("temporary unschedulable account = %+v", facts.Accounts[1])
	}
	if !strings.Contains(item.Body, `group 2 "claude": total=3 available=2`) {
		t.Fatalf("body = %s", item.Body)
	}
	if item := NewUpstreamAccountsCollector(nil).Collect(context.Background(), Target{}); item.Status != ItemMissing {
		t.Fatalf("unconfigured ops = %+v", item)
	}
	if item := NewUpstreamAccountsCollector(fakeUpstream{err: fmt.Errorf("401")}).Collect(context.Background(), Target{}); item.Status != ItemError || item.Upstream != nil {
		t.Fatalf("failed ops = %+v", item)
	}
}

type fakeChanges struct {
	changes, releases []store.ChangeEvent
	since             time.Time
}

func (f *fakeChanges) ListChanges(_ context.Context, _ string, since time.Time, _ int) ([]store.ChangeEvent, error) {
	f.since = since
	return f.changes, nil
}

func (f *fakeChanges) ListReleases(context.Context, string, int) ([]store.ChangeEvent, error) {
	return f.releases, nil
}

func TestRecentChangesCollectorIdentifiesCurrentRelease(t *testing.T) {
	start := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	v1, v2, image := "v1", "v2", "repo@sha256:"+strings.Repeat("a", 64)
	verified := start.Add(-24 * time.Hour)
	current := store.ChangeEvent{ChangeType: "release", ReleaseID: &v2, ImageRef: &image, DBMigration: "compatible", OccurredAt: start.Add(-10 * time.Minute), Source: "ci", Actor: "pipeline"}
	previous := store.ChangeEvent{ChangeType: "release", ReleaseID: &v1, ImageRef: &image, DBMigration: "none", VerifiedAt: &verified, OccurredAt: start.Add(-48 * time.Hour)}
	db := &fakeChanges{changes: []store.ChangeEvent{current}, releases: []store.ChangeEvent{current, previous}}
	item := NewRecentChangesCollector(db, config.ServiceConfig{Name: "sub2api"}).Collect(context.Background(), Target{Incident: store.Incident{StartedAt: start}})
	r := item.Release
	if item.Status != ItemOK || r == nil || r.CurrentID != "v2" || r.CurrentMigration != "compatible" || !r.DeployedBeforeFault || r.Changes != 1 {
		t.Fatalf("item=%+v release=%+v", item, r)
	}
	if item.Object == nil || *item.Object != (ObjectRef{Kind: "service", Name: "sub2api", ID: "v2"}) || !db.since.Equal(start.Add(-time.Hour)) {
		t.Fatalf("object=%+v since=%s", item.Object, db.since)
	}
	if !strings.Contains(item.Body, "release_id=v1 release") || !strings.Contains(item.Body, "verified="+verified.Format(time.RFC3339)) {
		t.Fatalf("body = %s", item.Body)
	}
}
