// D02 ParseWebhook 测试：字段归一化、缺失 map、以及拒绝未知 version / status / 时间，而不是给默认值。
package ingest

import (
	"strings"
	"testing"
)

func TestParseWebhook(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		check   func(t *testing.T, alerts []NormalizedAlert)
		wantErr string
	}{
		{
			name: "normalizes alert fields",
			payload: `{
				"version":"4",
				"status":"firing",
				"alerts":[
					{
						"status":"firing",
						"labels":{"AlertName":"HighCPU","severity":"critical","instance":"node-1"},
						"annotations":{"Summary":"CPU high"},
						"startsAt":"2026-08-17T10:00:00+08:00",
						"endsAt":"0001-01-01T00:00:00Z",
						"generatorURL":"http://prometheus:9090/graph?g0.expr=vector(1)"
					}
				]
			}`,
			check: func(t *testing.T, alerts []NormalizedAlert) {
				t.Helper()
				if len(alerts) != 1 {
					t.Fatalf("got %d alerts, want 1", len(alerts))
				}
				alert := alerts[0]
				if alert.Source != SourceAlertmanager || alert.Name != "HighCPU" || alert.Status != "firing" {
					t.Fatalf("normalized identity = %#v", alert)
				}
				if alert.Labels["alertname"] != "HighCPU" || alert.Annotations["summary"] != "CPU high" {
					t.Fatalf("normalized maps = labels=%#v annotations=%#v", alert.Labels, alert.Annotations)
				}
				if alert.Severity != 5 || alert.StartsAt.IsZero() || !alert.EndsAt.IsZero() {
					t.Fatalf("normalized derived fields = severity=%d starts=%v ends=%v", alert.Severity, alert.StartsAt, alert.EndsAt)
				}
				if alert.Fingerprint == "" || alert.AlertHash == "" || alert.AlertHash != FullHash(alert) {
					t.Fatalf("derived hashes = fingerprint=%q alert_hash=%q", alert.Fingerprint, alert.AlertHash)
				}
				if !alert.ReceivedAt.IsZero() {
					t.Fatalf("ReceivedAt = %v, want parser zero value", alert.ReceivedAt)
				}
			},
		},
		{
			name:    "labels missing",
			payload: `{"version":"4","alerts":[{"status":"resolved"}]}`,
			check: func(t *testing.T, alerts []NormalizedAlert) {
				t.Helper()
				if len(alerts) != 1 || alerts[0].Labels == nil || alerts[0].Annotations == nil {
					t.Fatalf("missing maps were not normalized: %#v", alerts)
				}
				if alerts[0].Name != "" || alerts[0].Severity != 3 || alerts[0].Status != "resolved" {
					t.Fatalf("missing-label defaults = %#v", alerts[0])
				}
			},
		},
		{
			name:    "invalid json",
			payload: `{"alerts":`,
			wantErr: "decode webhook payload",
		},
		{
			name:    "unsupported version",
			payload: `{"version":"3","alerts":[]}`,
			wantErr: "unsupported webhook version",
		},
		{
			name:    "missing version",
			payload: `{"alerts":[]}`,
			wantErr: "unsupported webhook version",
		},
		{
			name:    "invalid timestamp",
			payload: `{"version":"4","alerts":[{"status":"firing","startsAt":"not-a-time"}]}`,
			wantErr: "invalid RFC3339 timestamp",
		},
		{
			name:    "invalid status",
			payload: `{"version":"4","alerts":[{"status":"pending"}]}`,
			wantErr: "unsupported value",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alerts, err := ParseWebhook([]byte(test.payload))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("ParseWebhook() error = %v, want substring %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWebhook() error = %v", err)
			}
			test.check(t, alerts)
		})
	}
}

func TestParseWebhookUsesTopLevelStatusAsFallback(t *testing.T) {
	alerts, err := ParseWebhook([]byte(`{"version":"4","status":"resolved","alerts":[{}]}`))
	if err != nil {
		t.Fatalf("ParseWebhook() error = %v", err)
	}
	if len(alerts) != 1 || alerts[0].Status != "resolved" {
		t.Fatalf("status = %#v, want resolved", alerts)
	}
}
