package llm

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"oncall-agent/internal/tools"
)

// compactToolResult uses known tool formats only. Unknown, malformed or already
// truncated data is kept intact instead of guessing which facts can be dropped.
func compactToolResult(name, content string) string {
	var compacted string
	switch name {
	case tools.ToolPromRangeQuery:
		compacted = compactRangeResult(content)
	case tools.ToolDockerLogs:
		lines := strings.Split(content, "\n")
		type run struct {
			Line  string `json:"line"`
			Count int    `json:"count"`
		}
		var runs []run
		for _, line := range lines {
			if len(runs) > 0 && runs[len(runs)-1].Line == line {
				runs[len(runs)-1].Count++
			} else {
				runs = append(runs, run{Line: line, Count: 1})
			}
		}
		encoded, err := json.Marshal(struct {
			Compacted bool  `json:"compacted"`
			Runs      []run `json:"ordered_line_runs"`
		}{true, runs})
		if err == nil {
			compacted = string(encoded)
		}
	}
	if compacted != "" && len(compacted) < len(content) {
		return compacted
	}
	return content
}

// Keep the complete result envelope and each series' labels. Replace only valid
// numeric sample arrays with first/last/min/max points and the original count.
// Explicitly mark that intermediate samples were omitted, not that no anomaly
// occurred. NaN/Inf, histograms, unexpected formats and key collisions stay raw.
func compactRangeResult(content string) string {
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &envelope) != nil || envelope == nil {
		return ""
	}
	var series []map[string]json.RawMessage
	if json.Unmarshal(envelope["result"], &series) != nil {
		return ""
	}
	changed := false
	for _, item := range series {
		if item == nil || item["values_summary"] != nil {
			continue
		}
		var points []json.RawMessage
		if json.Unmarshal(item["values"], &points) != nil || len(points) < 8 {
			continue
		}
		minIndex, maxIndex := 0, 0
		minValue, maxValue := math.Inf(1), math.Inf(-1)
		valid := true
		for i, point := range points {
			var pair []json.RawMessage
			var timestamp float64
			var value string
			if json.Unmarshal(point, &pair) != nil || len(pair) != 2 ||
				json.Unmarshal(pair[0], &timestamp) != nil || string(pair[0]) == "null" ||
				json.Unmarshal(pair[1], &value) != nil {
				valid = false
				break
			}
			n, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
				valid = false
				break
			}
			if n < minValue {
				minValue, minIndex = n, i
			}
			if n > maxValue {
				maxValue, maxIndex = n, i
			}
		}
		if !valid {
			continue
		}
		summary, err := json.Marshal(map[string]any{
			"compacted": true, "intermediate_samples_omitted": true, "count": len(points),
			"first": points[0], "last": points[len(points)-1], "min": points[minIndex], "max": points[maxIndex],
		})
		if err != nil || len(summary) >= len(item["values"]) {
			continue
		}
		delete(item, "values")
		item["values_summary"] = summary
		changed = true
	}
	if !changed {
		return ""
	}
	encoded, err := json.Marshal(series)
	if err != nil {
		return ""
	}
	envelope["result"] = encoded
	encoded, err = json.Marshal(envelope)
	if err != nil {
		return ""
	}
	return string(encoded)
}
