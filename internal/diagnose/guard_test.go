package diagnose

import (
	"testing"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/tools"
	"oncall-agent/internal/topology"
)

func restartPlan(target string) llm.Plan {
	return llm.Plan{Action: "docker_restart", Target: llm.PlanTarget{Kind: "container", Name: target}, Reason: "进程不可用"}
}

// restartEvidence 是一份可信的目标证据：docker_inspect 确认 sub2api 身份。
func restartEvidence(container ContainerFacts, health *HealthFacts) Evidence {
	items := []EvidenceItem{{Name: "docker_inspect", Status: ItemOK,
		Object: &ObjectRef{Kind: "container", Name: "sub2api", ID: "c0ffee"}, Container: &container}}
	if health != nil {
		status := ItemOK
		if health.Observation != "healthy" {
			status = ItemError
		}
		items = append(items, EvidenceItem{Name: "sub2api_health", Status: status, Object: &ObjectRef{Kind: "service", Name: "sub2api"}, Health: health})
	}
	items = append(items, EvidenceItem{Name: "postgres", Status: ItemOK}, EvidenceItem{Name: "redis", Status: ItemOK})
	return Evidence{IncidentID: 7, Items: items}
}

func exitedWithoutSelfHealing() ContainerFacts {
	return ContainerFacts{Status: "exited", ExitCode: 0, RestartPolicy: "no"}
}

func hungContainer() (ContainerFacts, *HealthFacts) {
	return ContainerFacts{Status: "running", Running: true, RestartPolicy: "unless-stopped"}, &HealthFacts{Observation: "unavailable"}
}

func TestGuardAllowsRestartWhenFactsSupportIt(t *testing.T) {
	running, unavailable := hungContainer()
	for name, evidence := range map[string]Evidence{
		"exited and not self-healing": restartEvidence(exitedWithoutSelfHealing(), nil),
		"running but probe fails":     restartEvidence(running, unavailable),
		"running but probe unhealthy": restartEvidence(running, &HealthFacts{Observation: "unhealthy", StatusCode: 503}),
	} {
		result := Guard("进程不可用", restartPlan("sub2api"), evidence)
		if result.Decision != DecisionAllow || result.Overridden || result.Plan.Target.Name != "sub2api" {
			t.Fatalf("%s: Guard() = %+v, want allow", name, result)
		}
	}
}

func TestGuardDeniesActionWithoutTarget(t *testing.T) {
	plan := llm.Plan{Action: "docker_restart", Target: llm.PlanTarget{Kind: "container", Name: ""}}
	result := Guard("某故障", plan, restartEvidence(exitedWithoutSelfHealing(), nil))
	if result.Decision != DecisionDeny || !result.Overridden {
		t.Fatalf("Guard() = %+v, want deny", result)
	}
	if result.Plan.Action != "none" || result.Plan.Target.Name != "none" {
		t.Fatalf("plan = %+v, want forced none", result.Plan)
	}
}

func TestGuardBlocksRestartOnConfigError(t *testing.T) {
	for _, rca := range []string{
		"配置错误导致网关启动失败",
		"镜像不存在，ImagePullBackOff",
		"credential unauthorized 认证失败",
	} {
		result := Guard(rca, restartPlan("sub2api"), restartEvidence(exitedWithoutSelfHealing(), nil))
		if result.Decision != DecisionEscalate || !result.Overridden || result.Plan.Action != "none" {
			t.Fatalf("Guard(%q) = %+v, want escalate + forced none", rca, result)
		}
	}
}

func TestGuardNoneActionPassesThrough(t *testing.T) {
	// action=none 的计划不需要 target 和证据，直接放行。
	plan := llm.Plan{Action: "none", Target: llm.PlanTarget{Kind: "none", Name: "none"}}
	result := Guard("证据不足", plan, Evidence{})
	if result.Decision != DecisionAllow || result.Overridden {
		t.Fatalf("Guard() = %+v, want allow", result)
	}
}

// 仅"存在目标对象"不足以允许动作：身份必须来自本次可信的 docker_inspect。
func TestGuardRequiresTrustedRestartIdentity(t *testing.T) {
	anonymous := restartEvidence(exitedWithoutSelfHealing(), nil)
	anonymous.Items[0].Object = nil
	noID := restartEvidence(exitedWithoutSelfHealing(), nil)
	noID.Items[0].Object = &ObjectRef{Kind: "container", Name: "sub2api"}
	failed := restartEvidence(exitedWithoutSelfHealing(), nil)
	failed.Items[0].Status = ItemError
	for name, tc := range map[string]struct {
		plan     llm.Plan
		evidence Evidence
	}{
		"no inspect evidence":     {restartPlan("sub2api"), Evidence{Items: []EvidenceItem{{Name: "alert_snapshot", Status: ItemOK, Body: "container=sub2api"}}}},
		"inspect failed":          {restartPlan("sub2api"), failed},
		"anonymous container":     {restartPlan("sub2api"), anonymous},
		"identity without id":     {restartPlan("sub2api"), noID},
		"target is another name":  {restartPlan("unrelated-victim"), restartEvidence(exitedWithoutSelfHealing(), nil)},
		"target is not container": {llm.Plan{Action: "docker_restart", Target: llm.PlanTarget{Kind: "service", Name: "sub2api"}}, restartEvidence(exitedWithoutSelfHealing(), nil)},
	} {
		result := Guard("", tc.plan, tc.evidence)
		if result.Decision != DecisionDeny || result.Plan.Action != "none" {
			t.Fatalf("%s: Guard() = %+v, want deny", name, result)
		}
	}
}

// 演练矩阵：无身份 OOM，旁边存在健康目标证据 —— Guard 不能依据匿名 OOM 对目标动作。
func TestGuardRejectsAnonymousOOMBesideHealthyTarget(t *testing.T) {
	evidence := restartEvidence(ContainerFacts{Status: "running", Running: true, RestartPolicy: "unless-stopped"}, &HealthFacts{Observation: "healthy", StatusCode: 200})
	evidence.Items = append(evidence.Items, EvidenceItem{Name: "golden_metrics", Status: ItemOK,
		Body: "external_log: container=batch-worker reason=OOMKilled exit_code=137"})
	result := Guard("sub2api 因 OOM 被杀，建议重启", restartPlan("sub2api"), evidence)
	if result.Decision != DecisionDeny || result.Plan.Action != "none" {
		t.Fatalf("Guard() = %+v, want deny", result)
	}
}

func TestGuardRestartPreconditions(t *testing.T) {
	running, unavailable := hungContainer()
	postgresDown := restartEvidence(running, unavailable)
	postgresDown.Items[2].Status = ItemError
	redisDown := restartEvidence(exitedWithoutSelfHealing(), nil)
	redisDown.Items[2].Status = ItemError
	for name, tc := range map[string]struct {
		evidence Evidence
		want     string
	}{
		"docker already restarting":   {restartEvidence(ContainerFacts{Status: "restarting", Restarting: true, RestartPolicy: "unless-stopped"}, nil), DecisionDeny},
		"postgres unavailable":        {postgresDown, DecisionEscalate},
		"redis unavailable":           {redisDown, DecisionEscalate},
		"deliberately stopped":        {restartEvidence(ContainerFacts{Status: "exited", RestartPolicy: "unless-stopped"}, nil), DecisionEscalate},
		"running without health fact": {restartEvidence(running, nil), DecisionDeny},
		"running and healthy":         {restartEvidence(running, &HealthFacts{Observation: "healthy", StatusCode: 200}), DecisionDeny},
	} {
		result := Guard("", restartPlan("sub2api"), tc.evidence)
		if result.Decision != tc.want || result.Plan.Action != "none" || result.Reason == "" {
			t.Fatalf("%s: Guard() = %+v, want %s", name, result, tc.want)
		}
	}
}

func rollbackEvidence(deployedBeforeFault bool, metrics string) Evidence {
	items := []EvidenceItem{{Name: "recent_changes", Status: ItemOK, Object: &ObjectRef{Kind: "service", Name: "sub2api", ID: "v2"},
		Release: &ReleaseFacts{CurrentID: "v2", CurrentMigration: "compatible", DeployedBeforeFault: deployedBeforeFault}}}
	if metrics != "" {
		items = append(items, EvidenceItem{Name: "sub2api_metrics", Source: "prometheus:query", Status: metrics, Business: &tools.BusinessTraffic{Requests: 100, Errors: 20, SampledAt: time.Now()}})
	}
	return Evidence{Items: items}
}

func TestGuardRollbackPreconditions(t *testing.T) {
	plan := llm.Plan{Action: "deployment_rollback", Target: llm.PlanTarget{Kind: "service", Name: "sub2api"}}
	if result := Guard("新版本业务 5xx", plan, rollbackEvidence(true, ItemOK)); result.Decision != DecisionAllow || result.Target.ID != "v2" {
		t.Fatalf("Guard() = %+v, want allow with current release as identity", result)
	}
	noRecords := Evidence{Items: []EvidenceItem{{Name: "recent_changes", Status: ItemOK, Release: &ReleaseFacts{}}}}
	other := plan
	other.Target.Name = "postgres"
	for name, tc := range map[string]struct {
		plan     llm.Plan
		evidence Evidence
		want     string
	}{
		"no release records":       {plan, noRecords, DecisionDeny},
		"other service":            {other, rollbackEvidence(true, ItemOK), DecisionDeny},
		"release not before fault": {plan, rollbackEvidence(false, ItemOK), DecisionDeny},
		"business metrics unread":  {plan, rollbackEvidence(true, ""), DecisionEscalate},
		"business metrics partial": {plan, rollbackEvidence(true, ItemPartial), DecisionEscalate},
		"database really is the cause": {plan, func() Evidence {
			e := rollbackEvidence(true, ItemOK)
			e.Items = append(e.Items, EvidenceItem{Name: "postgres", Status: ItemError})
			return e
		}(), DecisionEscalate},
	} {
		if result := Guard("", tc.plan, tc.evidence); result.Decision != tc.want || result.Plan.Action != "none" {
			t.Fatalf("%s: Guard() = %+v, want %s", name, result, tc.want)
		}
	}
}

func upstreamEvidence(accounts ...UpstreamAccountFacts) Evidence {
	return Evidence{Items: []EvidenceItem{{Name: "upstream_accounts", Status: ItemOK, Upstream: &UpstreamFacts{RealtimeEnabled: true, Accounts: accounts}}}}
}

func TestGuardQuarantinePreconditions(t *testing.T) {
	plan := llm.Plan{Action: "upstream_quarantine", Target: llm.PlanTarget{Kind: "upstream_account", Name: "7"}}
	healthyPeer := UpstreamAccountFacts{ID: 8, GroupID: 2, Available: true}
	failing := UpstreamAccountFacts{ID: 7, GroupID: 2, Available: true, Errors: 30}
	if result := Guard("账号 7 持续 529", plan, upstreamEvidence(failing, healthyPeer, UpstreamAccountFacts{ID: 9, GroupID: 2, Available: true})); result.Decision != DecisionAllow || result.Target.ID != "7" {
		t.Fatalf("Guard() = %+v, want allow", result)
	}
	auto := failing
	auto.TempUnschedulable = true
	few := failing
	few.Errors = 3
	spread := UpstreamAccountFacts{ID: 8, GroupID: 2, Available: true, Errors: 40}
	notID := plan
	notID.Target.Name = "claude-main"
	for name, tc := range map[string]struct {
		plan     llm.Plan
		evidence Evidence
		want     string
	}{
		"state unreadable":               {plan, Evidence{}, DecisionDeny},
		"realtime disabled":              {plan, Evidence{Items: []EvidenceItem{{Name: "upstream_accounts", Status: ItemOK, Upstream: &UpstreamFacts{}}}}, DecisionDeny},
		"target not an id":               {notID, upstreamEvidence(failing, healthyPeer), DecisionDeny},
		"unknown account":                {plan, upstreamEvidence(healthyPeer), DecisionDeny},
		"already unscheduled by sub2api": {plan, upstreamEvidence(auto, healthyPeer), DecisionDeny},
		"too few errors":                 {plan, upstreamEvidence(few, healthyPeer), DecisionDeny},
		"errors not concentrated":        {plan, upstreamEvidence(failing, spread, healthyPeer), DecisionDeny},
		"whole group failing":            {plan, upstreamEvidence(failing, UpstreamAccountFacts{ID: 8, GroupID: 2, Errors: 5}), DecisionEscalate},
		"no peer to take over":           {plan, upstreamEvidence(failing), DecisionEscalate},
	} {
		if result := Guard("", tc.plan, tc.evidence); result.Decision != tc.want || result.Plan.Action != "none" {
			t.Fatalf("%s: Guard() = %+v, want %s", name, result, tc.want)
		}
	}
}

func TestGuardDeniesActionsWithoutRules(t *testing.T) {
	result := Guard("", llm.Plan{Action: "config_restore", Target: llm.PlanTarget{Kind: "service", Name: "sub2api"}}, Evidence{})
	if result.Decision != DecisionDeny || result.Plan.Action != "none" {
		t.Fatalf("Guard() = %+v, want fail-closed deny", result)
	}
}

// 依赖故障时重启下游无效：拓扑确认 down/missing 才拦截，unknown 与 runs_on 不拦截。
func TestGuardEscalatesRestartWhenDependencyIsBroken(t *testing.T) {
	withTopology := func(states map[string]string) Evidence {
		evidence := restartEvidence(exitedWithoutSelfHealing(), nil)
		snapshot := &topology.Snapshot{
			Nodes: []topology.Node{{ID: "sub2api", Container: "sub2api", State: topology.StateDown}},
			Edges: []topology.Edge{{From: "sub2api", To: "host", Type: config.TopologyRunsOn}},
		}
		for id, state := range states {
			snapshot.Nodes = append(snapshot.Nodes, topology.Node{ID: id, State: state})
			if id != "host" {
				snapshot.Edges = append(snapshot.Edges, topology.Edge{From: "sub2api", To: id, Type: config.TopologyDependsOn})
			}
		}
		evidence.Items = append(evidence.Items, EvidenceItem{Name: "topology", Status: ItemOK, Topology: snapshot})
		return evidence
	}
	for name, test := range map[string]struct {
		states map[string]string
		want   string
	}{
		"postgres down":       {map[string]string{"postgres": topology.StateDown}, DecisionEscalate},
		"redis missing":       {map[string]string{"redis": topology.StateMissing}, DecisionEscalate},
		"upstream unknown":    {map[string]string{"upstream": topology.StateUnknown}, DecisionAllow},
		"host down (runs_on)": {map[string]string{"host": topology.StateDown}, DecisionAllow},
		"dependencies up":     {map[string]string{"postgres": topology.StateUp}, DecisionAllow},
	} {
		result := Guard("进程不可用", restartPlan("sub2api"), withTopology(test.states))
		if result.Decision != test.want {
			t.Errorf("%s: Guard() = %+v, want %s", name, result, test.want)
		}
	}
	result := Guard("进程不可用", restartPlan("sub2api"), withTopology(map[string]string{"postgres": topology.StateDown}))
	if !result.Overridden || result.Plan.Action != "none" || result.Reason != "dependency postgres is down; restarting sub2api does not fix it" {
		t.Fatalf("escalation = %+v", result)
	}
}
