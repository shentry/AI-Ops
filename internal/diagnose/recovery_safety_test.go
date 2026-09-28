package diagnose

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

func TestWatchNeverInventsStabilityAcrossObservationGaps(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-40 * time.Minute)
	result := evaluateWatch(store.VerifyTask{Phase: "watch", DeadlineAt: now.Add(-10 * time.Minute), LastCheckedAt: &old, LastResultJSON: []byte(`{"observation":"healthy"}`)}, incident.VerificationSpec{IntervalSeconds: 10, RequiredPasses: 3, WatchSeconds: 1800}, now, nil)
	if result.Status != "inconclusive" {
		t.Fatalf("old health proved stability: %+v", result)
	}
}

func TestMissingMetricsCannotAuthorizeRollback(t *testing.T) {
	registry := stubRegistry(t, map[string]tools.Handler{tools.ToolPromInstantQuery: func(context.Context, json.RawMessage) (string, error) {
		return `{"resultType":"vector","result":[]}`, nil
	}})
	item := NewSub2APIMetricsCollector(registry).Collect(context.Background(), Target{})
	evidence := rollbackEvidence(true, "")
	evidence.Items = append(evidence.Items, item)
	result := Guard("possible release regression", llm.Plan{Action: tools.ActionDeploymentRollback, Target: llm.PlanTarget{Kind: "service", Name: "sub2api"}}, evidence)
	if item.Status == ItemOK || observationOK(evidence) || result.Decision == DecisionAllow {
		t.Fatalf("missing metrics authorized: item=%s observation=%v guard=%s", item.Status, observationOK(evidence), result.Decision)
	}
}
