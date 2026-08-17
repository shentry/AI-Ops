package ingest

import "strings"

// Severity maps a configured label value to the numeric severity used by the
// alert schema. Unknown and missing values use warning (3).
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
