package diagnose

import (
	"testing"

	"oncall-agent/internal/llm"
)

func TestGuardAllowsNormalPlan(t *testing.T) {
	plan := llm.Plan{Action: "restart_container", Target: llm.PlanTarget{Kind: "container", Name: "sub2api"}, Reason: "进程退出"}
	result := Guard("容器进程退出，OOMKilled=false", plan)
	if result.Decision != DecisionAllow || result.Overridden {
		t.Fatalf("Guard() = %+v, want allow", result)
	}
	if result.Plan.Action != "restart_container" || result.Plan.Target.Name != "sub2api" {
		t.Fatalf("plan rewritten unexpectedly: %+v", result.Plan)
	}
}

func TestGuardDeniesActionWithoutTarget(t *testing.T) {
	plan := llm.Plan{Action: "restart_container", Target: llm.PlanTarget{Kind: "container", Name: ""}}
	result := Guard("某故障", plan)
	if result.Decision != DecisionDeny || !result.Overridden {
		t.Fatalf("Guard() = %+v, want deny", result)
	}
	if result.Plan.Action != "none" || result.Plan.Target.Name != "none" {
		t.Fatalf("plan = %+v, want forced none", result.Plan)
	}
}

func TestGuardBlocksRestartOnConfigError(t *testing.T) {
	plan := llm.Plan{Action: "restart_container", Target: llm.PlanTarget{Kind: "container", Name: "sub2api"}}
	for _, rca := range []string{
		"配置错误导致网关启动失败",
		"镜像不存在，ImagePullBackOff",
		"credential unauthorized 认证失败",
	} {
		result := Guard(rca, plan)
		if result.Decision != DecisionEscalate || !result.Overridden || result.Plan.Action != "none" {
			t.Fatalf("Guard(%q) = %+v, want escalate + forced none", rca, result)
		}
	}
}

func TestGuardNoneActionPassesThrough(t *testing.T) {
	// action=none 的计划不需要 target，直接放行。
	plan := llm.Plan{Action: "none", Target: llm.PlanTarget{Kind: "none", Name: "none"}}
	result := Guard("证据不足", plan)
	if result.Decision != DecisionAllow || result.Overridden {
		t.Fatalf("Guard() = %+v, want allow", result)
	}
}
