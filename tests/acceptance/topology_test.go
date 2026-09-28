package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/diagnose"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/tools"
	"oncall-agent/internal/topology"
)

// Explicitly opt in: this test creates, stops and removes only its own two
// containers. It checks the step-2 acceptance: a stopped dependency shows down
// in the topology and the Guard escalates a restart of the service using it.
func TestTopologyDependencyBlocksRestartThroughRealDocker(t *testing.T) {
	socket := os.Getenv("TEST_DOCKER_SOCKET")
	if socket == "" {
		t.Skip("requires TEST_DOCKER_SOCKET")
	}
	image := os.Getenv("TEST_DOCKER_IMAGE")
	if image == "" {
		image = "python:3.12-alpine"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	command := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %s: %v", args[0], out, err)
		}
		return strings.TrimSpace(string(out))
	}
	suffix := time.Now().UnixNano()
	app, db := fmt.Sprintf("oncall-topology-app-%d", suffix), fmt.Sprintf("oncall-topology-db-%d", suffix)
	for _, name := range []string{app, db} {
		id := command("run", "-d", "--name", name, "--label", "oncall.test=topology", "--restart=no", image, "python", "-c", "import time; time.sleep(600)")
		t.Cleanup(func() {
			// Removal uses the immutable ID returned by our own create operation.
			rmCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			_ = exec.CommandContext(rmCtx, "docker", "rm", "-f", "-v", id).Run()
		})
	}

	client, err := tools.NewDockerClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	if err := client.RegisterTools(registry, 20); err != nil {
		t.Fatal(err)
	}
	// Docker is real; Prometheus answers "no firing alerts" so only container
	// state decides the node states.
	if err := registry.Register(tools.ToolSpec{Name: tools.ToolPromInstantQuery, Description: "fixture: no alerts", Timeout: time.Second, Handler: func(context.Context, json.RawMessage) (string, error) {
		return `{"resultType":"vector","result":[]}`, nil
	}}); err != nil {
		t.Fatal(err)
	}
	service := config.ServiceConfig{Name: "app", Container: app}
	graph := config.TopologyConfig{
		Nodes: []config.TopologyNode{{ID: "db", Kind: "datastore", Container: db}},
		Edges: []config.TopologyEdge{{From: "app", To: "db", Type: config.TopologyDependsOn}},
	}
	restart := llm.Plan{Action: tools.ActionDockerRestart, Target: llm.PlanTarget{Kind: "container", Name: app}, Reason: "进程不可用"}
	// Each observation uses a fresh graph: the 15-second cache is per instance.
	observe := func() (topology.Node, diagnose.GuardResult) {
		t.Helper()
		evidence := diagnose.BuildEvidence(ctx, []diagnose.Collector{
			diagnose.NewDockerInspectCollector(service, registry),
			diagnose.NewTopologyCollector(topology.New(graph, service, registry)),
		}, diagnose.Target{})
		item, _ := evidence.Item("topology")
		return item.Topology.Nodes[1], diagnose.Guard("进程不可用", restart, evidence)
	}

	if node, _ := observe(); node.State != topology.StateUp || node.ContainerID == "" {
		t.Fatalf("running dependency = %+v", node)
	}
	command("stop", "-t", "1", db)
	node, result := observe()
	if node.State != topology.StateDown || result.Decision != diagnose.DecisionEscalate || result.Reason != "dependency db is down; restarting "+app+" does not fix it" {
		t.Fatalf("stopped dependency: node=%+v guard=%+v", node, result)
	}
	command("rm", "-f", "-v", db)
	node, result = observe()
	if node.State != topology.StateMissing || result.Decision != diagnose.DecisionEscalate {
		t.Fatalf("removed dependency: node=%+v guard=%+v", node, result)
	}
	t.Logf("real Docker: running dependency up; stopped -> down and restart escalated; removed -> missing")
}
