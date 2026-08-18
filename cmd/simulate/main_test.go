package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"oncall-agent/internal/ingest"
)

func TestSimulateSendsBearerAndAccepts202(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var payload webhookEnvelope
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Version != "4" || len(payload.Alerts) != 1 {
			t.Errorf("payload = %#v", payload)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	if err := simulate(server.URL, "secret", 1, 0, false); err != nil {
		t.Fatal(err)
	}
}

func TestBuildPayloadUsesAlertmanagerV4AndDuplicateRatio(t *testing.T) {
	payload, err := buildPayload(100, 0.6, false, time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var envelope webhookEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Version != "4" || len(envelope.Alerts) != 100 {
		t.Fatalf("envelope = %#v", envelope)
	}
	seen := make(map[string]struct{})
	for _, alert := range envelope.Alerts {
		encoded, err := json.Marshal(webhookEnvelope{Version: "4", Status: alert.Status, Alerts: []simulateAlert{alert}})
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ingest.ParseWebhook(encoded)
		if err != nil {
			t.Fatal(err)
		}
		seen[parsed[0].Fingerprint] = struct{}{}
		if alert.GeneratorURL != "http://127.0.0.1:9090/graph?g0.expr=vector(1)" {
			t.Fatalf("generatorURL = %q", alert.GeneratorURL)
		}
	}
	if len(seen) != 40 {
		t.Fatalf("unique fingerprints = %d, want 40", len(seen))
	}
}

func TestBuildPayloadResolvedChangesHashButKeepsFingerprint(t *testing.T) {
	now := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	firing, err := buildPayload(1, 0, false, now)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := buildPayload(1, 0, true, now)
	if err != nil {
		t.Fatal(err)
	}
	firingAlerts, err := ingest.ParseWebhook(firing)
	if err != nil {
		t.Fatal(err)
	}
	resolvedAlerts, err := ingest.ParseWebhook(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if firingAlerts[0].Fingerprint != resolvedAlerts[0].Fingerprint || firingAlerts[0].AlertHash == resolvedAlerts[0].AlertHash {
		t.Fatalf("firing=%#v resolved=%#v", firingAlerts[0], resolvedAlerts[0])
	}
}
