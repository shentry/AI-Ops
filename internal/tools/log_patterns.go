package tools

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 日志模式归一化：把易变片段替换掉，同一类日志合成一行。
var (
	logUUIDPattern   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	logHexPattern    = regexp.MustCompile(`\b[0-9a-fA-F]{12,}\b`)
	logNumberPattern = regexp.MustCompile(`\d+`)
)

type logPattern struct {
	count       int
	first, last time.Time
	order       int    // 首次出现的顺序，时间相同时保持稳定
	sample      string // 最近一次出现的原文（不含时间戳）
}

// aggregateLogLines 把带 Docker 时间戳的日志按模式聚合：每个模式保留次数、
// 首次/末次出现时间和最近一条样本。按末次出现时间倒序输出 —— 输出预算
// 截断的是尾部，最新的模式必须排在前面，而不是像原始日志那样最新的最先被截掉。
func aggregateLogLines(raw string) string {
	patterns := make(map[string]*logPattern)
	lines := 0
	for line := range strings.Lines(raw) {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines++
		var at time.Time
		message := line
		if stamp, rest, ok := strings.Cut(line, " "); ok {
			if parsed, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
				at, message = parsed.UTC(), rest
			}
		}
		key := logUUIDPattern.ReplaceAllString(message, "<uuid>")
		key = logHexPattern.ReplaceAllString(key, "<hex>")
		key = logNumberPattern.ReplaceAllString(key, "#")
		entry, seen := patterns[key]
		if !seen {
			entry = &logPattern{first: at, order: len(patterns)}
			patterns[key] = entry
		}
		entry.count++
		entry.last, entry.sample = at, message
	}
	if lines == 0 {
		return "lines=0 patterns=0\n"
	}
	sorted := make([]*logPattern, 0, len(patterns))
	for _, entry := range patterns {
		sorted = append(sorted, entry)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].last.Equal(sorted[j].last) {
			return sorted[i].last.After(sorted[j].last)
		}
		return sorted[i].order < sorted[j].order
	})
	var out strings.Builder
	fmt.Fprintf(&out, "lines=%d patterns=%d (most recent first)\n", lines, len(sorted))
	for _, entry := range sorted {
		fmt.Fprintf(&out, "count=%d first=%s last=%s | %s\n", entry.count, logTime(entry.first), logTime(entry.last), entry.sample)
	}
	return out.String()
}

func logTime(at time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	return at.Format(time.RFC3339)
}
