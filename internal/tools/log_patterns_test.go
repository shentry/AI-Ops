package tools

import (
	"strings"
	"testing"
)

func TestAggregateLogLinesGroupsPatternsMostRecentFirst(t *testing.T) {
	raw := strings.Join([]string{
		"2026-09-22T02:00:01.000000001Z db connect failed: attempt 1 id=0f3a9c2b7d4e11aa",
		"2026-09-22T02:00:02.5Z request 123e4567-e89b-12d3-a456-426614174000 status=500",
		"2026-09-22T02:00:03Z db connect failed: attempt 2 id=77aa9c2b7d4e11bb",
		"2026-09-22T02:00:04Z request 9f0e4567-e89b-12d3-a456-426614174999 status=500",
		"2026-09-22T02:00:05Z db connect failed: attempt 3 id=0000000000000000",
		"",
	}, "\n")
	got := aggregateLogLines(raw)
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 3 || lines[0] != "lines=5 patterns=2 (most recent first)" {
		t.Fatalf("aggregate = %q", got)
	}
	if lines[1] != "count=3 first=2026-09-22T02:00:01Z last=2026-09-22T02:00:05Z | db connect failed: attempt 3 id=0000000000000000" {
		t.Fatalf("latest pattern = %q", lines[1])
	}
	if lines[2] != "count=2 first=2026-09-22T02:00:02Z last=2026-09-22T02:00:04Z | request 9f0e4567-e89b-12d3-a456-426614174999 status=500" {
		t.Fatalf("second pattern = %q", lines[2])
	}
}

// TTY 容器或异常行没有 Docker 时间戳：照样聚合，时间标为 unknown，不编造。
func TestAggregateLogLinesWithoutTimestamps(t *testing.T) {
	got := aggregateLogLines("plain line 1\nplain line 2\n")
	if got != "lines=2 patterns=1 (most recent first)\ncount=2 first=unknown last=unknown | plain line 2\n" {
		t.Fatalf("aggregate = %q", got)
	}
	if got := aggregateLogLines(""); got != "lines=0 patterns=0\n" {
		t.Fatalf("empty aggregate = %q", got)
	}
}
