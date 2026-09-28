// Command replay re-runs one recorded diagnosis on its frozen snapshot. The
// model receives the recorded input and can plan only the recorded actions;
// tools return only what was recorded, and an unrecorded query returns an
// explicit "missing" instead of reading today's live state. Actions are never
// prepared or executed, and nothing is written.
//
//	go run ./cmd/replay -config config.yaml -run 123
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/diagnose"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

func main() {
	configPath := flag.String("config", "config.yaml", "configuration file (model and database)")
	runID := flag.Uint64("run", 0, "agent_run id to replay")
	flag.Parse()
	if err := replay(context.Background(), *configPath, *runID, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		os.Exit(1)
	}
}

func replay(ctx context.Context, configPath string, runID uint64, out io.Writer) error {
	if runID == 0 {
		return errors.New("-run is required")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.MySQL.DSN)
	if err != nil {
		return err
	}
	defer db.Close()
	snapshot, err := db.GetDiagnosisSnapshot(ctx, runID)
	if err != nil {
		return err
	}
	run, err := db.GetAgentRun(ctx, runID)
	if err != nil {
		return err
	}
	if snapshot.InputText == nil || snapshot.ToolsJSON == nil {
		return errors.New("the run has no recorded model input (memory hit or failure before the model call)")
	}
	var evidence diagnose.Evidence
	if err := json.Unmarshal(snapshot.EvidenceJSON, &evidence); err != nil {
		return fmt.Errorf("recorded evidence: %w", err)
	}
	registry, calls, err := frozenRegistry(snapshot)
	if err != nil {
		return err
	}
	reasoner := llm.NewReasoner(llm.NewFactory(cfg.LLM), registry, cfg.Diagnose.Budget)
	var input llm.DiagnosisInput
	result, diagnoseErr := reasoner.Diagnose(ctx, *snapshot.InputText, run.Mode, func(in llm.DiagnosisInput) error { input = in; return nil })
	report := map[string]any{
		"run_id": runID, "recorded_code_version": snapshot.CodeVersion, "recorded_model": snapshot.Model, "replay_model": input.Model,
		// A different prompt digest means the code or action catalog changed:
		// the replay is then a regression check, not a reproduction.
		"prompt_matches":   snapshot.PromptSHA256 != nil && *snapshot.PromptSHA256 == input.PromptSHA256,
		"original":         map[string]any{"rca": run.RCAText, "plan": run.PlanJSON},
		"tool_calls":       calls.total,
		"unrecorded_calls": calls.missing,
	}
	if diagnoseErr != nil {
		report["error"] = tools.Sanitize(diagnoseErr.Error())
	}
	if result != nil {
		guard := diagnose.Guard(result.RCA, result.Plan, evidence)
		report["replay"] = map[string]any{"rca": result.RCA, "confidence": result.Confidence, "plan": result.Plan,
			"guard": map[string]any{"decision": guard.Decision, "overridden": guard.Overridden, "reason": guard.Reason}}
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

// callLog counts replayed tool calls; Eino runs a batch concurrently.
type callLog struct {
	mu             sync.Mutex
	total, missing int
}

// frozenRegistry serves each recorded tool call's output, in recorded order
// per identical query, and registers the recorded actions as plan-only.
func frozenRegistry(snapshot store.DiagnosisSnapshot) (*tools.Registry, *callLog, error) {
	var catalog diagnose.RecordedCatalog
	if err := json.Unmarshal(*snapshot.ToolsJSON, &catalog); err != nil {
		return nil, nil, fmt.Errorf("recorded tool catalog: %w", err)
	}
	var steps []llm.StepLog
	if snapshot.ToolCallsJSON != nil {
		if err := json.Unmarshal(*snapshot.ToolCallsJSON, &steps); err != nil {
			return nil, nil, fmt.Errorf("recorded tool calls: %w", err)
		}
	}
	recorded := map[string][]llm.StepLog{}
	for _, step := range steps {
		key := callKey(step.Name, []byte(step.Input))
		recorded[key] = append(recorded[key], step)
	}
	calls := &callLog{}
	registry := tools.NewRegistry()
	for _, def := range catalog.Tools {
		name := def.Name
		err := registry.Register(tools.ToolSpec{Name: name, Description: def.Description, Params: def.Params, Timeout: time.Second, MaxOutput: 1 << 20,
			Handler: func(_ context.Context, args json.RawMessage) (string, error) {
				calls.mu.Lock()
				defer calls.mu.Unlock()
				calls.total++
				key := callKey(name, args)
				queue := recorded[key]
				if len(queue) == 0 {
					calls.missing++
					return `{"missing":"this query was not recorded in the frozen snapshot; live state is not read during replay"}`, nil
				}
				step := queue[0]
				recorded[key] = queue[1:]
				if step.Err != "" {
					return "", errors.New(step.Err)
				}
				return step.Output, nil
			}})
		if err != nil {
			return nil, nil, err
		}
	}
	for _, def := range catalog.Actions {
		if err := registry.RegisterAction(frozenAction{def}); err != nil {
			return nil, nil, err
		}
	}
	return registry, calls, nil
}

func callKey(name string, args []byte) string {
	if canonical, err := incident.CanonicalJSON(args); err == nil {
		args = canonical
	}
	return name + "\x00" + string(args)
}

// frozenAction lets the model plan a recorded action; it can never write.
type frozenAction struct{ def tools.ActionDefinition }

var errFrozen = errors.New("replay: actions are never prepared or executed")

func (a frozenAction) Definition() tools.ActionDefinition { return a.def }
func (frozenAction) Prepare(context.Context, tools.PrepareRequest) (tools.Prepared, error) {
	return tools.Prepared{}, errFrozen
}
func (frozenAction) Execute(context.Context, tools.Operation) (tools.Receipt, error) {
	return tools.Receipt{}, errFrozen
}
func (frozenAction) Reconcile(context.Context, tools.Operation) (tools.Reconciliation, error) {
	return tools.Reconciliation{Outcome: tools.OutcomeUnknown}, errFrozen
}
