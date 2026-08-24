package ingest

import (
	"context"
	"testing"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/store"
)

func TestGroupKey(t *testing.T) {
	tests := []struct {
		name   string
		group  []string
		labels map[string]string
		want   string
	}{
		{name: "labels prefix", group: []string{"labels.service"}, labels: map[string]string{"service": "payments"}, want: "payments"},
		{name: "bare field", group: []string{"service"}, labels: map[string]string{"service": "payments"}, want: "payments"},
		{name: "configured order", group: []string{"labels.zone", "service"}, labels: map[string]string{"service": "payments", "zone": "prod"}, want: "prod,payments"},
		{name: "empty group", labels: map[string]string{"service": "payments"}, want: "name:HighCPU"},
		{name: "missing label", group: []string{"service"}, labels: map[string]string{"instance": "node-1"}, want: "name:HighCPU"},
		{name: "blank label", group: []string{"service"}, labels: map[string]string{"service": "  "}, want: "name:HighCPU"},
		{name: "blank configured field", group: []string{"labels."}, labels: map[string]string{"service": "payments"}, want: "name:HighCPU"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := GroupKey(CorrelationInput{Name: "HighCPU", Labels: test.labels}, config.CorrelateConfig{GroupBy: test.group})
			if got != test.want {
				t.Fatalf("GroupKey() = %q, want %q", got, test.want)
			}
		})
	}
}

type recordingIncidentTx struct {
	input  store.IncidentInput
	window time.Duration
	min    int
}

func (r *recordingIncidentTx) AssignIncident(_ context.Context, input store.IncidentInput, window time.Duration, min int) (store.IncidentAssignment, error) {
	r.input = input
	r.window = window
	r.min = min
	return store.IncidentAssignment{IncidentID: 42, Status: "firing", Created: true, Promoted: true}, nil
}

func (r *recordingIncidentTx) TouchIncident(context.Context, uint64, time.Time, int) error {
	return nil
}

func (r *recordingIncidentTx) ResolveIncident(context.Context, uint64, time.Time) (bool, error) {
	return false, nil
}

func (r *recordingIncidentTx) EnqueueAgentRun(context.Context, *store.AgentRun) error {
	return nil
}

func (r *recordingIncidentTx) AppendIncidentEvent(_ context.Context, event store.IncidentEvent) (store.IncidentEvent, error) {
	return event, nil
}

func (r *recordingIncidentTx) OpenIncidentProblem(_ context.Context, problem store.IncidentProblem) (store.IncidentProblem, error) {
	return problem, nil
}

func (r *recordingIncidentTx) ResolveIncidentProblem(context.Context, uint64, string, *uint64, time.Time) (bool, error) {
	return false, nil
}

func TestCorrelatorAssignForwardsNormalizedAlert(t *testing.T) {
	observedAt := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	tx := &recordingIncidentTx{}
	correlator := NewCorrelator(config.CorrelateConfig{GroupBy: []string{"labels.service"}, WindowMinutes: 15, MinAlerts: 3})
	assignment, err := correlator.Assign(context.Background(), tx, CorrelationInput{
		Fingerprint: "fingerprint",
		Name:        "HighCPU",
		Labels:      map[string]string{"service": "payments"},
		Severity:    5,
		ObservedAt:  observedAt,
	})
	if err != nil {
		t.Fatalf("Assign() error = %v", err)
	}
	if assignment.IncidentID != 42 || !assignment.Created || !assignment.Promoted {
		t.Fatalf("assignment = %#v", assignment)
	}
	if tx.input.GroupKey != "payments" || tx.input.Fingerprint != "fingerprint" || tx.input.Name != "HighCPU" || tx.input.Severity != 5 || !tx.input.ObservedAt.Equal(observedAt) {
		t.Fatalf("incident input = %#v", tx.input)
	}
	if tx.window != 15*time.Minute || tx.min != 3 {
		t.Fatalf("correlate config = window %v min %d", tx.window, tx.min)
	}
}

func TestCorrelatorRejectsWindowOverflow(t *testing.T) {
	correlator := NewCorrelator(config.CorrelateConfig{WindowMinutes: int(maxCorrelationWindowMinutes + 1), MinAlerts: 1})
	_, err := correlator.Assign(context.Background(), &recordingIncidentTx{}, CorrelationInput{
		Fingerprint: "fingerprint",
		Name:        "HighCPU",
		ObservedAt:  time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatal("Assign() error = nil, want window overflow validation")
	}
}
