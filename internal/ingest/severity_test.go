package ingest

import "testing"

func TestSeverity(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		want   int
		labels map[string]string
	}{
		{name: "critical", value: "critical", want: 5},
		{name: "error alias", value: "error", want: 4},
		{name: "high", value: "HIGH", want: 4},
		{name: "warning", value: "warning", want: 3},
		{name: "medium alias", value: "medium", want: 3},
		{name: "info", value: "info", want: 2},
		{name: "low", value: " low ", want: 1},
		{name: "unknown defaults warning", value: "notice", want: 3},
		{name: "missing defaults warning", want: 3},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			labels := test.labels
			if labels == nil && test.value != "" {
				labels = map[string]string{"severity": test.value}
			}
			if got := Severity(labels, "severity"); got != test.want {
				t.Fatalf("Severity() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestSeverityUsesConfiguredLabel(t *testing.T) {
	if got := Severity(map[string]string{"priority": "critical"}, "priority"); got != 5 {
		t.Fatalf("Severity() = %d, want 5", got)
	}
	if got := Severity(map[string]string{"priority": "critical"}, "PRIORITY"); got != 5 {
		t.Fatalf("case-insensitive label lookup = %d, want 5", got)
	}
}
