package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type webhookPayload struct {
	Version string         `json:"version"`
	Status  string         `json:"status"`
	Alerts  []webhookAlert `json:"alerts"`
}

type webhookAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
}

// ParseWebhook parses one Alertmanager v4 webhook body without touching the
// database. Each alert gets a computed fingerprint and full-dedup hash.
func ParseWebhook(payload []byte) ([]NormalizedAlert, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, fmt.Errorf("ingest: webhook payload is empty")
	}

	var envelope webhookPayload
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("ingest: decode webhook payload: %w", err)
	}
	if envelope.Version != "4" {
		return nil, fmt.Errorf("ingest: unsupported webhook version %q", envelope.Version)
	}
	if envelope.Alerts == nil {
		return nil, fmt.Errorf("ingest: webhook alerts is required")
	}

	fallbackStatus := ""
	if strings.TrimSpace(envelope.Status) != "" {
		var err error
		fallbackStatus, err = normalizeStatus(envelope.Status)
		if err != nil {
			return nil, fmt.Errorf("ingest: invalid webhook status: %w", err)
		}
	}

	alerts := make([]NormalizedAlert, 0, len(envelope.Alerts))
	for index, raw := range envelope.Alerts {
		status := strings.TrimSpace(raw.Status)
		if status == "" {
			status = fallbackStatus
		}
		normalizedStatus, err := normalizeStatus(status)
		if err != nil {
			return nil, fmt.Errorf("ingest: alerts[%d].status: %w", index, err)
		}

		startsAt, err := parseWebhookTime(raw.StartsAt, index, "startsAt")
		if err != nil {
			return nil, err
		}
		endsAt, err := parseWebhookTime(raw.EndsAt, index, "endsAt")
		if err != nil {
			return nil, err
		}

		labels := normalizeLabels(raw.Labels)
		annotations := normalizeLabels(raw.Annotations)
		alert := NormalizedAlert{
			Source:       SourceAlertmanager,
			Name:         labels["alertname"],
			Status:       normalizedStatus,
			Labels:       labels,
			Annotations:  annotations,
			StartsAt:     startsAt,
			EndsAt:       endsAt,
			GeneratorURL: raw.GeneratorURL,
			Severity:     Severity(labels, DefaultSeverityLabel),
			Fingerprint:  Fingerprint(labels, nil),
		}
		alert.AlertHash = FullHash(alert)
		alerts = append(alerts, alert)
	}

	return alerts, nil
}

func normalizeStatus(status string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "firing":
		return "firing", nil
	case "resolved":
		return "resolved", nil
	default:
		if strings.TrimSpace(status) == "" {
			return "", fmt.Errorf("status is required")
		}
		return "", fmt.Errorf("unsupported value %q", status)
	}
}

func normalizeLabels(labels map[string]string) map[string]string {
	normalized := make(map[string]string, len(labels))
	for key, value := range labels {
		normalized[strings.ToLower(key)] = value
	}
	return normalized
}

func parseWebhookTime(value string, index int, field string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("ingest: alerts[%d].%s: invalid RFC3339 timestamp: %w", index, field, err)
	}
	return parsed.UTC(), nil
}
