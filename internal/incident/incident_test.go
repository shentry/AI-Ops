package incident

import (
	"testing"
	"time"
)

func TestSeverityName(t *testing.T) {
	tests := []struct {
		severity int
		want     string
	}{
		{5, "critical"},
		{4, "high"},
		{3, "warning"},
		{2, "info"},
		{1, "low"},
		// 未知级别回退 warning，与 ingest.Severity 的缺省一致。
		{0, "warning"},
		{99, "warning"},
		{-1, "warning"},
	}
	for _, test := range tests {
		if got := SeverityName(test.severity); got != test.want {
			t.Errorf("SeverityName(%d) = %q, want %q", test.severity, got, test.want)
		}
	}
}

func TestRouteMode(t *testing.T) {
	route := map[string]string{
		"critical": "full",
		"high":     "full",
		"warning":  "light",
		"info":     "skip",
		"low":      "skip",
	}
	tests := []struct {
		name     string
		severity int
		route    map[string]string
		want     string
	}{
		{"critical full", 5, route, ModeFull},
		{"high full", 4, route, ModeFull},
		{"warning light", 3, route, ModeLight},
		{"info skip", 2, route, ModeSkip},
		{"low skip", 1, route, ModeSkip},
		// 配置缺条目回退 light：full 浪费预算、skip 漏诊断。
		{"missing entry falls back", 5, map[string]string{"low": "skip"}, ModeLight},
		{"nil route falls back", 5, nil, ModeLight},
		// 配置里的非法值同样回退，不原样透传给 agent_run.mode。
		{"invalid value falls back", 5, map[string]string{"critical": "turbo"}, ModeLight},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := RouteMode(test.severity, test.route); got != test.want {
				t.Fatalf("RouteMode(%d) = %q, want %q", test.severity, got, test.want)
			}
		})
	}
}

func TestNewQueueRun(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	pending := NewQueueRun(42, ModeFull, now)
	if pending.IncidentID != 42 || pending.Mode != ModeFull || pending.Status != "pending" || pending.Finished {
		t.Fatalf("NewQueueRun(full) = %#v", pending)
	}
	if !pending.StartedAt.Equal(now) {
		t.Fatalf("NewQueueRun() StartedAt = %v, want %v", pending.StartedAt, now)
	}

	// skip 直接落 succeeded：留一行可统计，但不占诊断队列。
	skipped := NewQueueRun(42, ModeSkip, now)
	if skipped.Status != "succeeded" || !skipped.Finished {
		t.Fatalf("NewQueueRun(skip) = %#v", skipped)
	}

	// 入参带时区的时间要归一到 UTC，否则落库时间基准会漂。
	shanghai := time.FixedZone("CST", 8*3600)
	local := NewQueueRun(42, ModeFull, now.In(shanghai))
	if local.StartedAt.Location() != time.UTC || !local.StartedAt.Equal(now) {
		t.Fatalf("NewQueueRun() StartedAt = %v, want UTC %v", local.StartedAt, now)
	}
}

func TestIsOpen(t *testing.T) {
	for _, status := range []string{StatusCandidate, StatusFiring} {
		if !IsOpen(status) {
			t.Errorf("IsOpen(%q) = false, want true", status)
		}
	}
	// acknowledged 是人工接管，resolved 是已关单：两者都不该被自动路径改写。
	for _, status := range []string{StatusAcknowledged, StatusResolved, "", "unknown"} {
		if IsOpen(status) {
			t.Errorf("IsOpen(%q) = true, want false", status)
		}
	}
}
