// Package topology turns the declared dependency graph into live node states:
// containers through docker_inspect, health expressions and firing alerts
// through prom_instant_query. Both go through the tool Registry, so timeouts,
// sanitizing and truncation are the same as for the model's own tool calls.
package topology

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/tools"
)

// Node states. down and missing are confirmed faults; unknown means the state
// could not be observed, and it never blocks anything.
const (
	StateUp      = "up"
	StateDown    = "down"
	StateUnknown = "unknown"
	StateMissing = "missing"
)

// cacheTTL bounds how often the page and diagnosis re-observe the graph.
const cacheTTL = 15 * time.Second

// firingAlerts keeps only the labels used for mapping, so the result stays
// small enough to arrive untruncated.
const firingAlerts = `count by (alertname, component, container, service) (ALERTS{alertstate="firing"})`

type Node struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Container    string   `json:"container,omitempty"`
	State        string   `json:"state"`
	Detail       string   `json:"detail,omitempty"`
	ContainerID  string   `json:"container_id,omitempty"`
	RestartCount int      `json:"restart_count,omitempty"`
	OOMKilled    bool     `json:"oom_killed,omitempty"`
	Alerts       []string `json:"alerts,omitempty"`
}

type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

// Snapshot is one observation of the whole graph. AlertsError is set when the
// firing alerts could not be read: empty alert lists then mean "not known".
type Snapshot struct {
	CheckedAt   time.Time `json:"checked_at"`
	Nodes       []Node    `json:"nodes"`
	Edges       []Edge    `json:"edges"`
	AlertsError string    `json:"alerts_error,omitempty"`
}

// BrokenDependencies returns the direct depends_on targets of the node running
// container that are confirmed down or missing.
func (s Snapshot) BrokenDependencies(container string) []Node {
	nodes := map[string]Node{}
	from := ""
	for _, node := range s.Nodes {
		nodes[node.ID] = node
		if from == "" && node.Container == container {
			from = node.ID
		}
	}
	var broken []Node
	for _, edge := range s.Edges {
		if edge.From == from && edge.Type == config.TopologyDependsOn {
			if target := nodes[edge.To]; target.State == StateDown || target.State == StateMissing {
				broken = append(broken, target)
			}
		}
	}
	return broken
}

type Topology struct {
	nodes    []config.TopologyNode
	edges    []Edge
	registry *tools.Registry
	now      func() time.Time

	mu     sync.Mutex
	cached Snapshot
}

// New builds the graph from validated configuration; the service node comes
// from the service description so its name and container are written once.
func New(cfg config.TopologyConfig, service config.ServiceConfig, registry *tools.Registry) *Topology {
	t := &Topology{
		nodes:    append([]config.TopologyNode{{ID: service.Name, Kind: "service", Container: service.Container}}, cfg.Nodes...),
		edges:    make([]Edge, 0, len(cfg.Edges)),
		registry: registry,
		now:      time.Now,
	}
	for _, edge := range cfg.Edges {
		t.edges = append(t.edges, Edge(edge))
	}
	return t
}

// NodeFor maps alert labels to a node: component names a node, then the
// container label, then service. Labels that match nothing map to "".
func (t *Topology) NodeFor(labels map[string]string) string {
	for _, key := range []string{"component", "container", "service"} {
		value := labels[key]
		if value == "" {
			continue
		}
		for _, node := range t.nodes {
			if (key == "container" && node.Container == value) || (key != "container" && node.ID == value) {
				return node.ID
			}
		}
	}
	return ""
}

// Snapshot returns the live graph, observed at most cacheTTL ago. The caller's
// cancellation does not reach the observation: a request abandoned halfway
// must not cache "unknown" states that later readers would take as current.
func (t *Topology) Snapshot(ctx context.Context) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.now().Sub(t.cached.CheckedAt) < cacheTTL {
		return t.cached
	}
	ctx = context.WithoutCancel(ctx)
	snapshot := Snapshot{Nodes: make([]Node, len(t.nodes)), Edges: t.edges}
	var wg sync.WaitGroup
	for i, node := range t.nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snapshot.Nodes[i] = t.observe(ctx, node)
		}()
	}
	alerts, err := t.query(ctx, firingAlerts)
	wg.Wait()
	if err != nil {
		snapshot.AlertsError = "firing alerts could not be read"
	}
	for _, alert := range alerts {
		id := t.NodeFor(alert.Metric)
		for i := range snapshot.Nodes {
			if node := &snapshot.Nodes[i]; node.ID == id && !slices.Contains(node.Alerts, alert.Metric["alertname"]) {
				node.Alerts = append(node.Alerts, alert.Metric["alertname"])
				slices.Sort(node.Alerts)
			}
		}
	}
	snapshot.CheckedAt = t.now().UTC()
	t.cached = snapshot
	return snapshot
}

// observe combines the node's checks: missing or down if any check says so,
// unknown if a check could not be read or none is declared, up otherwise.
func (t *Topology) observe(ctx context.Context, declared config.TopologyNode) Node {
	node := Node{ID: declared.ID, Kind: declared.Kind, Container: declared.Container}
	var states, details []string
	record := func(state, detail string) {
		states = append(states, state)
		if detail != "" {
			details = append(details, detail)
		}
	}
	if declared.Container != "" {
		record(t.container(ctx, &node))
	}
	if declared.Health != "" {
		record(t.health(ctx, declared.Health))
	}
	switch {
	case slices.Contains(states, StateMissing):
		node.State = StateMissing
	case slices.Contains(states, StateDown):
		node.State = StateDown
	case len(states) == 0:
		node.State, details = StateUnknown, []string{"no container or health check declared"}
	case slices.Contains(states, StateUnknown):
		node.State = StateUnknown
	default:
		node.State = StateUp
	}
	node.Detail = strings.Join(details, "; ")
	return node
}

func (t *Topology) container(ctx context.Context, node *Node) (string, string) {
	args, _ := json.Marshal(map[string]string{"name": node.Container})
	out, err := t.registry.Execute(ctx, tools.ToolDockerInspect, args)
	if errors.Is(err, tools.ErrContainerNotFound) {
		return StateMissing, "container " + node.Container + " does not exist"
	}
	var inspect tools.ContainerInspect
	if err != nil || json.Unmarshal([]byte(out), &inspect) != nil || inspect.ID == "" {
		return StateUnknown, "container " + node.Container + " cannot be inspected"
	}
	node.ContainerID, node.RestartCount, node.OOMKilled = inspect.ID, inspect.RestartCount, inspect.OOMKilled
	if !inspect.Running || inspect.Restarting || inspect.Health == "unhealthy" {
		status := inspect.Status
		if inspect.Health != "" {
			status += ", " + inspect.Health
		}
		return StateDown, fmt.Sprintf("container %s is %s", node.Container, status)
	}
	return StateUp, ""
}

func (t *Topology) health(ctx context.Context, expr string) (string, string) {
	samples, err := t.query(ctx, expr)
	if err != nil {
		return StateUnknown, "health query failed"
	}
	if len(samples) != 1 {
		return StateUnknown, fmt.Sprintf("health %s returned %d samples, want 1", expr, len(samples))
	}
	var value string
	_ = json.Unmarshal(samples[0].Value[1], &value)
	switch value {
	case "1":
		return StateUp, ""
	case "0":
		return StateDown, "health " + expr + " = 0"
	}
	return StateUnknown, fmt.Sprintf("health %s = %q, want 1 or 0", expr, value)
}

type sample struct {
	Metric map[string]string  `json:"metric"`
	Value  [2]json.RawMessage `json:"value"`
}

// query runs an instant query and accepts only a complete instant vector.
func (t *Topology) query(ctx context.Context, expr string) ([]sample, error) {
	args, _ := json.Marshal(map[string]string{"query": expr})
	out, meta, err := t.registry.ExecuteWithMetadata(ctx, tools.ToolPromInstantQuery, args)
	if err != nil {
		return nil, err
	}
	var data struct {
		ResultType string   `json:"resultType"`
		Result     []sample `json:"result"`
	}
	if meta.Truncated || json.Unmarshal([]byte(out), &data) != nil || data.ResultType != "vector" {
		return nil, errors.New("result is not a complete instant vector")
	}
	return data.Result, nil
}
