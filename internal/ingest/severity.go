package ingest

import "strings"

// Severity 把标签值映射成 5..1。
// critical=5，error/high=4，warning/medium=3，info=2，low=1。
// 缺失或未知默认 warning(3)，不是 info。
// 这不是 alert status。D03 按 cfg.Ingest.SeverityLabel 重算。
func Severity(labels map[string]string, severityLabel string) int {
	severity := ""
	if labels != nil {
		severity = labels[strings.ToLower(strings.TrimSpace(severityLabel))]
	}

	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical":
		return 5
	case "error", "high":
		return 4
	case "warning", "medium":
		return 3
	case "info":
		return 2
	case "low":
		return 1
	default:
		return 3
	}
}
