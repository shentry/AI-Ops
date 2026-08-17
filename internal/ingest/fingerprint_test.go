package ingest

import (
	"testing"
	"time"
)

func TestFingerprintIsIndependentOfLabelOrder(t *testing.T) {
	first := Fingerprint(map[string]string{
		"instance":  "node-1",
		"alertname": "HighCPU",
		"service":   "payments",
	}, nil)
	second := Fingerprint(map[string]string{
		"service":   "payments",
		"alertname": "HighCPU",
		"instance":  "node-1",
	}, nil)
	if first != second {
		t.Fatalf("fingerprints differ for same labels: %q != %q", first, second)
	}
}

func TestFingerprintCanSelectAndSortFields(t *testing.T) {
	labels := map[string]string{
		"alertname": "HighCPU",
		"instance":  "node-1",
		"service":   "payments",
	}
	selected := Fingerprint(labels, []string{"service", "labels.alertname", "service"})
	changedUnselected := Fingerprint(map[string]string{
		"alertname": "HighCPU",
		"instance":  "node-2",
		"service":   "payments",
	}, []string{"service", "labels.alertname"})
	changedSelected := Fingerprint(map[string]string{
		"alertname": "HighCPU-v2",
		"instance":  "node-1",
		"service":   "payments",
	}, []string{"service", "labels.alertname"})

	if selected != changedUnselected {
		t.Fatalf("unselected label changed fingerprint: %q != %q", selected, changedUnselected)
	}
	if selected == changedSelected {
		t.Fatalf("selected label did not change fingerprint: %q", selected)
	}
}

func TestFiringAndResolvedAlertsShareFingerprint(t *testing.T) {
	firing := `{"version":"4","alerts":[{"status":"firing","labels":{"alertname":"HighCPU","instance":"node-1"}}]}`
	resolved := `{"version":"4","alerts":[{"status":"resolved","labels":{"instance":"node-1","alertname":"HighCPU"}}]}`

	firingAlerts, err := ParseWebhook([]byte(firing))
	if err != nil {
		t.Fatalf("parse firing: %v", err)
	}
	resolvedAlerts, err := ParseWebhook([]byte(resolved))
	if err != nil {
		t.Fatalf("parse resolved: %v", err)
	}
	if firingAlerts[0].Fingerprint != resolvedAlerts[0].Fingerprint {
		t.Fatalf("firing/resolved fingerprints differ: %q != %q", firingAlerts[0].Fingerprint, resolvedAlerts[0].Fingerprint)
	}
	if firingAlerts[0].AlertHash == resolvedAlerts[0].AlertHash {
		t.Fatalf("firing/resolved full hashes unexpectedly match: %q", firingAlerts[0].AlertHash)
	}
}

func TestFullHashIgnoresTimestampFields(t *testing.T) {
	base := NormalizedAlert{
		Source:       SourceAlertmanager,
		Name:         "HighCPU",
		Status:       "firing",
		Labels:       map[string]string{"alertname": "HighCPU", "instance": "node-1"},
		Annotations:  map[string]string{"summary": "CPU high"},
		GeneratorURL: "http://prometheus:9090/graph?g0.expr=vector(1)",
		Severity:     3,
		Fingerprint:  "same-fingerprint",
		StartsAt:     time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC),
		EndsAt:       time.Date(2026, 8, 17, 10, 5, 0, 0, time.UTC),
		ReceivedAt:   time.Date(2026, 8, 17, 10, 1, 0, 0, time.UTC),
	}
	changedTimes := base
	changedTimes.StartsAt = base.StartsAt.Add(10 * time.Minute)
	changedTimes.EndsAt = base.EndsAt.Add(10 * time.Minute)
	changedTimes.ReceivedAt = base.ReceivedAt.Add(10 * time.Minute)

	if FullHash(base) != FullHash(changedTimes) {
		t.Fatalf("timestamp changes altered full hash: %q != %q", FullHash(base), FullHash(changedTimes))
	}

	changedStatus := base
	changedStatus.Status = "resolved"
	if FullHash(base) == FullHash(changedStatus) {
		t.Fatalf("status change did not alter full hash: %q", FullHash(base))
	}
}
