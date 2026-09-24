package diagnose

import (
	"context"
	"strings"
	"testing"

	"gorm.io/datatypes"

	"oncall-agent/internal/llm"
	"oncall-agent/internal/store"
)

// Retry admission/budget/atomic scheduling are tested at store.FinalizeVerification.
// Pipeline only enriches a newly admitted retry with the previous attempt's facts.
func TestPipelineInjectsRetryContext(t *testing.T) {
	db := newFakeRunStore()
	rca := "上次认为容器退出"
	planJSON := datatypes.JSON(`{"action":"docker_restart"}`)
	db.runs[10] = store.AgentRun{ID: 10, Status: "succeeded", RCAText: &rca, PlanJSON: &planJSON}
	verifyOutput := datatypes.JSON(`"outcome=failed detail=health remained unhealthy"`)
	db.steps = append(db.steps, store.AgentRunStep{RunID: 10, Seq: 90, Kind: "verify", Name: "sub2api_http_health", OutputJSON: &verifyOutput})
	var evidence string
	reasoner := &capturingReasoner{inner: &fakeReasoner{result: &llm.DiagnoseResult{RCA: "新结论", Confidence: "medium"}}, out: &evidence}
	pipeline := NewPipeline(db, fakeEvidenceBuilder{evidence: testEvidence()}, reasoner, allowPolicy(), &fakeApprovals{}, &fakeReporter{}, nil, 0)
	retryOf := uint64(10)
	run := store.AgentRun{ID: 11, IncidentID: 7, Mode: "full", Status: "running", RetryOf: &retryOf}
	if err := pipeline.Run(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"上次认为容器退出", "previous_run_id: 10", "docker_restart", "previous_verify", "health remained unhealthy"} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("retry evidence missing %q:\n%s", want, evidence)
		}
	}
}
