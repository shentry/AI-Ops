package topology

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/tools"
)

var testConfig = config.TopologyConfig{
	Nodes: []config.TopologyNode{
		{ID: "postgres", Kind: "datastore", Container: "sub2api-postgres", Health: "max(pg_up)"},
		{ID: "redis", Kind: "datastore", Container: "sub2api-redis"},
		{ID: "upstream", Kind: "upstream"},
		{ID: "host", Kind: "host", Health: "min(up{job=\"node-exporter\"})"},
	},
	Edges: []config.TopologyEdge{
		{From: "sub2api", To: "postgres", Type: config.TopologyDependsOn},
		{From: "sub2api", To: "redis", Type: config.TopologyDependsOn},
		{From: "sub2api", To: "upstream", Type: config.TopologyDependsOn},
		{From: "sub2api", To: "host", Type: config.TopologyRunsOn},
	},
}

// fakeSources answers docker_inspect and prom_instant_query from fixed tables
// and counts calls. A handler seeing a canceled context fails the call.
type fakeSources struct {
	containers map[string]any // tools.ContainerInspect or error
	queries    map[string]any // vector result JSON or error
	calls      atomic.Int32
}

func (f *fakeSources) registry(t *testing.T) *tools.Registry {
	t.Helper()
	registry := tools.NewRegistry()
	for name, lookup := range map[string]func(map[string]string) (any, bool){
		tools.ToolDockerInspect:    func(args map[string]string) (any, bool) { v, ok := f.containers[args["name"]]; return v, ok },
		tools.ToolPromInstantQuery: func(args map[string]string) (any, bool) { v, ok := f.queries[args["query"]]; return v, ok },
	} {
		if err := registry.Register(tools.ToolSpec{Name: name, Description: "fake", Timeout: time.Second, Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			f.calls.Add(1)
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			var args map[string]string
			_ = json.Unmarshal(raw, &args)
			value, ok := lookup(args)
			switch v := value.(type) {
			case error:
				return "", v
			case string:
				return v, nil
			case nil:
				if !ok {
					return "", fmt.Errorf("unexpected call %v", args)
				}
			}
			out, _ := json.Marshal(value)
			return string(out), nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func vector(samples ...string) string {
	return `{"resultType":"vector","result":[` + strings.Join(samples, ",") + `]}`
}

func alert(name, labels string) string {
	return `{"metric":{"alertname":"` + name + `"` + labels + `},"value":[1,"1"]}`
}

func newFake() *fakeSources {
	return &fakeSources{
		containers: map[string]any{
			"sub2api":          tools.ContainerInspect{ID: "c1", Name: "sub2api", Status: "running", Running: true, RestartCount: 2},
			"sub2api-postgres": tools.ContainerInspect{ID: "c2", Name: "sub2api-postgres", Status: "running", Running: true},
			"sub2api-redis":    fmt.Errorf("wrapped: %w", tools.ErrContainerNotFound),
		},
		queries: map[string]any{
			"max(pg_up)":                   vector(`{"metric":{},"value":[1,"0"]}`),
			`min(up{job="node-exporter"})`: errors.New("prometheus unreachable"),
			firingAlerts: vector(
				alert("Sub2APIPostgresUnreachable", `,"service":"sub2api","component":"postgres"`),
				alert("Sub2APIContainerOOM", `,"service":"sub2api","container":"sub2api"`),
				alert("Sub2APIBusinessErrors", `,"service":"sub2api"`),
				alert("HostDiskAlmostFull", ``),
			),
		},
	}
}

func TestSnapshotObservesEveryNode(t *testing.T) {
	fake := newFake()
	topology := New(testConfig, config.ServiceConfig{Name: "sub2api", Container: "sub2api"}, fake.registry(t))
	snapshot := topology.Snapshot(context.Background())
	got := map[string]Node{}
	for _, node := range snapshot.Nodes {
		got[node.ID] = node
	}
	want := map[string]Node{
		"sub2api":  {ID: "sub2api", Kind: "service", Container: "sub2api", State: StateUp, ContainerID: "c1", RestartCount: 2, Alerts: []string{"Sub2APIBusinessErrors", "Sub2APIContainerOOM"}},
		"postgres": {ID: "postgres", Kind: "datastore", Container: "sub2api-postgres", State: StateDown, Detail: "health max(pg_up) = 0", ContainerID: "c2", Alerts: []string{"Sub2APIPostgresUnreachable"}},
		"redis":    {ID: "redis", Kind: "datastore", Container: "sub2api-redis", State: StateMissing, Detail: "container sub2api-redis does not exist"},
		"upstream": {ID: "upstream", Kind: "upstream", State: StateUnknown, Detail: "no container or health check declared"},
		"host":     {ID: "host", Kind: "host", State: StateUnknown, Detail: "health query failed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nodes:\n got %+v\nwant %+v", got, want)
	}
	if snapshot.AlertsError != "" || len(snapshot.Edges) != 4 || snapshot.CheckedAt.IsZero() {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	var broken []string
	for _, node := range snapshot.BrokenDependencies("sub2api") {
		broken = append(broken, node.ID)
	}
	// upstream is unknown and host is placement, not a dependency: neither blocks.
	if !reflect.DeepEqual(broken, []string{"postgres", "redis"}) || snapshot.BrokenDependencies("sub2api-postgres") != nil {
		t.Fatalf("broken dependencies = %v", broken)
	}
}

func TestContainerStatesAndHealthValues(t *testing.T) {
	for name, test := range map[string]struct {
		inspect any
		health  any
		want    string
	}{
		"exited":             {tools.ContainerInspect{ID: "c", Status: "exited", Running: false}, vector(`{"metric":{},"value":[1,"1"]}`), StateDown},
		"restarting":         {tools.ContainerInspect{ID: "c", Status: "restarting", Running: true, Restarting: true}, vector(`{"metric":{},"value":[1,"1"]}`), StateDown},
		"unhealthy":          {tools.ContainerInspect{ID: "c", Status: "running", Running: true, Health: "unhealthy"}, vector(`{"metric":{},"value":[1,"1"]}`), StateDown},
		"docker down":        {errors.New("dial unix: connection refused"), vector(`{"metric":{},"value":[1,"1"]}`), StateUnknown},
		"two samples":        {tools.ContainerInspect{ID: "c", Running: true}, vector(`{"metric":{"a":"1"},"value":[1,"1"]}`, `{"metric":{"a":"2"},"value":[1,"1"]}`), StateUnknown},
		"not boolean":        {tools.ContainerInspect{ID: "c", Running: true}, vector(`{"metric":{},"value":[1,"0.5"]}`), StateUnknown},
		"no data":            {tools.ContainerInspect{ID: "c", Running: true}, vector(), StateUnknown},
		"down beats unknown": {errors.New("docker down"), vector(`{"metric":{},"value":[1,"0"]}`), StateDown},
		"healthy":            {tools.ContainerInspect{ID: "c", Running: true, Health: "healthy"}, vector(`{"metric":{},"value":[1,"1"]}`), StateUp},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeSources{
				containers: map[string]any{"app": tools.ContainerInspect{ID: "a", Running: true}, "db": test.inspect},
				queries:    map[string]any{"h": test.health, firingAlerts: vector()},
			}
			nodes := config.TopologyConfig{Nodes: []config.TopologyNode{{ID: "db", Kind: "datastore", Container: "db", Health: "h"}}}
			topology := New(nodes, config.ServiceConfig{Name: "app", Container: "app"}, fake.registry(t))
			if got := topology.Snapshot(context.Background()).Nodes[1]; got.State != test.want {
				t.Fatalf("state = %s (%s), want %s", got.State, got.Detail, test.want)
			}
		})
	}
}

func TestSnapshotCachesAndIgnoresCallerCancellation(t *testing.T) {
	fake := newFake()
	topology := New(testConfig, config.ServiceConfig{Name: "sub2api", Container: "sub2api"}, fake.registry(t))
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	topology.now = func() time.Time { return now }
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	// An abandoned request still observes the real states, which are cached.
	if first := topology.Snapshot(canceled); first.Nodes[0].State != StateUp {
		t.Fatalf("canceled caller cached %+v", first.Nodes[0])
	}
	calls := fake.calls.Load()
	now = now.Add(cacheTTL - time.Second)
	topology.Snapshot(context.Background())
	if fake.calls.Load() != calls {
		t.Fatalf("snapshot within %s re-observed: %d calls", cacheTTL, fake.calls.Load()-calls)
	}
	now = now.Add(time.Second)
	topology.Snapshot(context.Background())
	if fake.calls.Load() == calls {
		t.Fatal("expired snapshot was not re-observed")
	}
}

func TestAlertsFailureIsReportedNotHidden(t *testing.T) {
	fake := newFake()
	fake.queries[firingAlerts] = errors.New("prometheus down")
	snapshot := New(testConfig, config.ServiceConfig{Name: "sub2api", Container: "sub2api"}, fake.registry(t)).Snapshot(context.Background())
	if snapshot.AlertsError == "" || snapshot.Nodes[0].Alerts != nil {
		t.Fatalf("alerts failure hidden: %+v", snapshot)
	}
}

func TestNodeForPrefersComponentThenContainerThenService(t *testing.T) {
	topology := New(testConfig, config.ServiceConfig{Name: "sub2api", Container: "sub2api"}, tools.NewRegistry())
	for labels, want := range map[string]string{
		`{"service":"sub2api","component":"postgres"}`:      "postgres",
		`{"service":"sub2api","container":"sub2api-redis"}`: "redis",
		`{"service":"sub2api","component":"unknown-thing"}`: "sub2api",
		`{"container":"sub2api"}`:                           "sub2api",
		`{"alertname":"HostDiskAlmostFull"}`:                "",
		`{"service":"other","container":"other"}`:           "",
	} {
		var parsed map[string]string
		_ = json.Unmarshal([]byte(labels), &parsed)
		if got := topology.NodeFor(parsed); got != want {
			t.Errorf("NodeFor(%s) = %q, want %q", labels, got, want)
		}
	}
}
