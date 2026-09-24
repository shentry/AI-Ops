package incident_test

import (
	"testing"

	"oncall-agent/internal/incident"
)

func TestFaultFingerprintStable(t *testing.T) {
	a := incident.FaultFingerprint("payments", "HighCPU")
	b := incident.FaultFingerprint("payments", "HighCPU")
	if a != b || len(a) != 12 {
		t.Fatalf("fingerprint = %q vs %q", a, b)
	}
	if incident.FaultFingerprint("payments", "Other") == a {
		t.Fatal("different alert name produced same fingerprint")
	}
}
