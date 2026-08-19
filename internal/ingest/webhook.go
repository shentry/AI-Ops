package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Alertmanager v4 信封。version 必填，必须是字符串 "4"。
type webhookPayload struct {
	Version string         `json:"version"`
	Status  string         `json:"status"` // 单条告警缺 status 时的回退
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

// ParseWebhook 把一条 Alertmanager v4 正文解成 NormalizedAlert。
// 无副作用：不碰库、不调 time.Now()、不读配置。D03 套上 ingest 配置后
// 必须重算 Fingerprint / Severity / AlertHash。
func ParseWebhook(payload []byte) ([]NormalizedAlert, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, fmt.Errorf("ingest: webhook payload is empty")
	}
	// ParseWebhook 把一条 Alertmanager v4 正文解成 NormalizedAlert。
	var envelope webhookPayload
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("ingest: decode webhook payload: %w", err)
	}
	// 缺 version 同样拒绝。D02 只承诺 Alertmanager v4。
	if envelope.Version != "4" {
		return nil, fmt.Errorf("ingest: unsupported webhook version %q", envelope.Version)
	}
	if envelope.Alerts == nil {
		return nil, fmt.Errorf("ingest: webhook alerts is required")
	}
	// 单条告警缺 status 时的回退
	fallbackStatus := ""
	if strings.TrimSpace(envelope.Status) != "" {
		var err error
		fallbackStatus, err = normalizeStatus(envelope.Status)
		if err != nil {
			return nil, fmt.Errorf("ingest: invalid webhook status: %w", err)
		}
	}
	// 逐条解析告警，status 缺省时用 fallbackStatus。
	alerts := make([]NormalizedAlert, 0, len(envelope.Alerts))
	for index, raw := range envelope.Alerts {
		status := strings.TrimSpace(raw.Status)
		if status == "" {
			status = fallbackStatus
		}
		// 逐条解析告警，status 缺省时用 fallbackStatus。
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
		// labels 规范化后传给 NormalizedAlert。
		labels := normalizeLabels(raw.Labels)
		annotations := normalizeLabels(raw.Annotations)
		// ReceivedAt 保持零值，同一份 payload 在单测里哈希稳定。
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

// 未知 status 报错，绝不默认成 firing。
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

// key 转小写，AlertName 和 alertname 会合并。value 不动。
func normalizeLabels(labels map[string]string) map[string]string {
	normalized := make(map[string]string, len(labels))
	for key, value := range labels {
		normalized[strings.ToLower(key)] = value
	}
	return normalized
}

// 空串保持零值。Alertmanager 的 0001-01-01T00:00:00Z 也会解析成零值。
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
