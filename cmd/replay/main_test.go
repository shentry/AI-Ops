package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"

	"oncall-agent/internal/diagnose"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

func recordedSnapshot(t *testing.T) store.DiagnosisSnapshot {
	t.Helper()
	catalog, _ := json.Marshal(diagnose.RecordedCatalog{
		Tools:   []llm.ToolDefinition{{Name: tools.ToolPromInstantQuery, Description: "instant query", Params: []tools.ParamSpec{{Name: "query", Required: true}}}},
		Actions: []tools.ActionDefinition{{Name: tools.ActionDockerRestart, Version: 2, TargetKind: "container", Description: "restart", Timeout: time.Minute}},
	})
	steps, _ := json.Marshal([]llm.StepLog{
		{Name: tools.ToolPromInstantQuery, Input: `{"query":"up"}`, Output: `{"result":"first"}`},
		{Name: tools.ToolPromInstantQuery, Input: `{ "query" : "up" }`, Output: `{"result":"second"}`},
		{Name: tools.ToolPromInstantQuery, Input: `{"query":"down"}`, Err: "prometheus unavailable"},
	})
	toolsJSON, callsJSON := datatypes.JSON(catalog), datatypes.JSON(steps)
	return store.DiagnosisSnapshot{ToolsJSON: &toolsJSON, ToolCallsJSON: &callsJSON}
}

func TestFrozenRegistryServesOnlyRecordedCalls(t *testing.T) {
	registry, calls, err := frozenRegistry(recordedSnapshot(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, want := range []string{"first", "second"} {
		out, err := registry.Execute(ctx, tools.ToolPromInstantQuery, json.RawMessage(`{"query":"up"}`))
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("recorded call = %q %v, want %s", out, err, want)
		}
	}
	if _, err := registry.Execute(ctx, tools.ToolPromInstantQuery, json.RawMessage(`{"query":"down"}`)); err == nil || !strings.Contains(err.Error(), "prometheus unavailable") {
		t.Fatalf("recorded failure not replayed: %v", err)
	}
	out, err := registry.Execute(ctx, tools.ToolPromInstantQuery, json.RawMessage(`{"query":"up"}`))
	if err != nil || !strings.Contains(out, "missing") {
		t.Fatalf("exhausted query must be missing, got %q %v", out, err)
	}
	if calls.total != 4 || calls.missing != 1 {
		t.Fatalf("calls = %d missing = %d", calls.total, calls.missing)
	}
	if _, err := registry.Execute(ctx, tools.ToolDockerInspect, json.RawMessage(`{"name":"sub2api"}`)); err == nil {
		t.Fatal("an unrecorded tool must not exist in a replay")
	}
}

// The model may plan a recorded action, but the replay can never write.
func TestFrozenRegistryActionsArePlanOnly(t *testing.T) {
	registry, _, err := frozenRegistry(recordedSnapshot(t))
	if err != nil {
		t.Fatal(err)
	}
	if defs := registry.PlannableActions(); len(defs) != 1 || defs[0].Name != tools.ActionDockerRestart || defs[0].Version != 2 {
		t.Fatalf("plannable = %+v", defs)
	}
	action, _ := registry.Action(tools.ActionDockerRestart)
	if _, err := action.Prepare(context.Background(), tools.PrepareRequest{}); err == nil {
		t.Fatal("frozen action prepared")
	}
	if _, err := action.Execute(context.Background(), tools.Operation{}); err == nil {
		t.Fatal("frozen action executed")
	}
}
