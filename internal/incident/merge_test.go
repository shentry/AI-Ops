package incident

import (
	"strings"
	"testing"
	"time"
)

// 归并规则的单测。这些判定过去埋在 store.AssignIncident 里，
// 只有起真 MySQL 才碰得到；上提到领域层之后用普通单测就能覆盖。

var base = time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)

func validInput() MergeInput {
	return MergeInput{
		GroupKey:    "payments",
		Fingerprint: "fp-1",
		Name:        "HighErrorRate",
		Severity:    3,
		ObservedAt:  base,
	}
}

func TestMergeInputValidate(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*MergeInput)
		window    time.Duration
		minAlerts int
		wantErr   bool
	}{
		{"valid", nil, 15 * time.Minute, 3, false},
		// 身份三要素缺任意一个都不能猜：猜错 group_key 会把不相干的告警缝在一起。
		{"no group key", func(in *MergeInput) { in.GroupKey = "" }, 15 * time.Minute, 3, true},
		{"no fingerprint", func(in *MergeInput) { in.Fingerprint = "" }, 15 * time.Minute, 3, true},
		{"no name", func(in *MergeInput) { in.Name = "" }, 15 * time.Minute, 3, true},
		{"zero observed at", func(in *MergeInput) { in.ObservedAt = time.Time{} }, 15 * time.Minute, 3, true},
		// 窗口必须为正：0 或负数意味着"没有活着的 incident"，会给每条告警建一个新的。
		{"zero window", nil, 0, 3, true},
		{"negative window", nil, -time.Minute, 3, true},
		// minAlerts < 1 会让 candidate 永远升不上去或立刻升级，两种都不是意图。
		{"zero min alerts", nil, 15 * time.Minute, 0, true},
		{"negative min alerts", nil, 15 * time.Minute, -1, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := validInput()
			if test.mutate != nil {
				test.mutate(&in)
			}
			err := in.Validate(test.window, test.minAlerts)
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, test.wantErr)
			}
		})
	}
}

func TestMergeInputCutoffAndTitle(t *testing.T) {
	in := validInput()
	if got, want := in.Title(), "payments: HighErrorRate"; got != want {
		t.Errorf("Title() = %q, want %q", got, want)
	}
	if got, want := in.Cutoff(15*time.Minute), base.Add(-15*time.Minute); !got.Equal(want) {
		t.Errorf("Cutoff() = %v, want %v", got, want)
	}
	// 入参带时区也要按 UTC 算窗口，否则跨时区部署时窗口会偏移。
	in.ObservedAt = base.In(time.FixedZone("CST", 8*3600))
	if got := in.Cutoff(15 * time.Minute); got.Location() != time.UTC {
		t.Errorf("Cutoff() location = %v, want UTC", got.Location())
	}
}

func TestNewCandidate(t *testing.T) {
	// 新建必须是 candidate 且 alerts_count 从 0 起：单条告警不足以下结论，
	// 成员挂载成功后才计数，否则一条孤立告警就能制造一次故障。
	got := NewCandidate(validInput())
	want := State{Status: StatusCandidate, Severity: 3, AlertsCount: 0, LastSeenAt: base}
	if got != want {
		t.Fatalf("NewCandidate() = %#v, want %#v", got, want)
	}
}

func TestMergeCountsOnlyNewMembers(t *testing.T) {
	current := State{Status: StatusFiring, Severity: 3, AlertsCount: 2, LastSeenAt: base}
	in := validInput()

	// 新成员才计数。
	if got := Merge(current, in, true, 3); got.AlertsCount != 3 {
		t.Errorf("Merge(new member) AlertsCount = %d, want 3", got.AlertsCount)
	}
	// 同一指纹反复 firing 不能把 alerts_count 刷高。
	if got := Merge(current, in, false, 3); got.AlertsCount != 2 {
		t.Errorf("Merge(existing member) AlertsCount = %d, want 2", got.AlertsCount)
	}
}

func TestMergeSeverityOnlyRises(t *testing.T) {
	current := State{Status: StatusFiring, Severity: 5, AlertsCount: 1, LastSeenAt: base}

	// 后到的一条 info 不能把 critical 的故障"降级"。
	low := validInput()
	low.Severity = 2
	if got := Merge(current, low, false, 3); got.Severity != 5 {
		t.Errorf("Merge(lower severity) Severity = %d, want 5", got.Severity)
	}

	// 更高级别要吸收进来。
	current.Severity = 3
	high := validInput()
	high.Severity = 5
	if got := Merge(current, high, false, 3); got.Severity != 5 {
		t.Errorf("Merge(higher severity) Severity = %d, want 5", got.Severity)
	}
}

func TestMergeLastSeenOnlyAdvances(t *testing.T) {
	current := State{Status: StatusFiring, Severity: 3, AlertsCount: 1, LastSeenAt: base}

	// 重放积压时告警可能乱序到达，last_seen_at 不能被一条旧告警拉回去。
	old := validInput()
	old.ObservedAt = base.Add(-time.Hour)
	if got := Merge(current, old, false, 3); !got.LastSeenAt.Equal(base) {
		t.Errorf("Merge(older) LastSeenAt = %v, want %v", got.LastSeenAt, base)
	}

	newer := validInput()
	newer.ObservedAt = base.Add(time.Minute)
	if got := Merge(current, newer, false, 3); !got.LastSeenAt.Equal(base.Add(time.Minute)) {
		t.Errorf("Merge(newer) LastSeenAt = %v, want %v", got.LastSeenAt, base.Add(time.Minute))
	}
}

func TestMergePromotesAtThreshold(t *testing.T) {
	in := validInput()

	// 差一个成员：还不升级。
	below := State{Status: StatusCandidate, Severity: 3, AlertsCount: 1, LastSeenAt: base}
	after := Merge(below, in, true, 3)
	if after.Status != StatusCandidate {
		t.Errorf("Merge() Status = %q, want candidate (2/3 members)", after.Status)
	}
	if Promoted(below, after) {
		t.Error("Promoted() = true below threshold, want false")
	}

	// 恰好够 minAlerts 的那一刻升级，且 Promoted 为真。
	atThreshold := State{Status: StatusCandidate, Severity: 3, AlertsCount: 2, LastSeenAt: base}
	after = Merge(atThreshold, in, true, 3)
	if after.Status != StatusFiring {
		t.Errorf("Merge() Status = %q, want firing (3/3 members)", after.Status)
	}
	if !Promoted(atThreshold, after) {
		t.Error("Promoted() = false at threshold, want true")
	}

	// 已经 firing 的再挂成员不重复促发 —— worker 靠 Promoted 只做一次促发动作。
	firing := State{Status: StatusFiring, Severity: 3, AlertsCount: 5, LastSeenAt: base}
	after = Merge(firing, in, true, 3)
	if Promoted(firing, after) {
		t.Error("Promoted() = true for already-firing incident, want false")
	}
}

func TestMergeRejectsInvalidThreshold(t *testing.T) {
	current := State{Status: StatusCandidate, Severity: 3, AlertsCount: 1, LastSeenAt: base}
	in := validInput()
	for _, minAlerts := range []int{0, -1} {
		if got := Merge(current, in, true, minAlerts); got != current {
			t.Errorf("Merge(minAlerts=%d) = %#v, want unchanged state %#v", minAlerts, got, current)
		}
	}
}

func TestPromotedRequiresTransition(t *testing.T) {
	// 判据是状态跃迁，不是"现在是 firing"。
	tests := []struct {
		name   string
		before string
		after  string
		want   bool
	}{
		{"candidate to firing", StatusCandidate, StatusFiring, true},
		{"already firing", StatusFiring, StatusFiring, false},
		{"still candidate", StatusCandidate, StatusCandidate, false},
		{"resolved to firing", StatusResolved, StatusFiring, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Promoted(State{Status: test.before}, State{Status: test.after})
			if got != test.want {
				t.Fatalf("Promoted(%q → %q) = %v, want %v", test.before, test.after, got, test.want)
			}
		})
	}
}

func TestHeartbeat(t *testing.T) {
	current := State{Status: StatusFiring, Severity: 3, LastSeenAt: base}

	// 续命：时间前进、severity 只升不降。
	next, changed := Heartbeat(current, base.Add(time.Minute), 5)
	if !changed || !next.LastSeenAt.Equal(base.Add(time.Minute)) || next.Severity != 5 {
		t.Fatalf("Heartbeat() = %#v, changed = %v", next, changed)
	}

	// 旧时间不回退，低级别不降级。
	next, changed = Heartbeat(current, base.Add(-time.Hour), 1)
	if !changed || !next.LastSeenAt.Equal(base) || next.Severity != 3 {
		t.Fatalf("Heartbeat(stale) = %#v, changed = %v", next, changed)
	}

	// 非 open 状态不动：changed=false 让调用方跳过写库。
	for _, status := range []string{StatusAcknowledged, StatusResolved} {
		closed := State{Status: status, Severity: 3, LastSeenAt: base}
		if next, changed := Heartbeat(closed, base.Add(time.Hour), 5); changed || next != closed {
			t.Errorf("Heartbeat(%q) = %#v, changed = %v, want unchanged", status, next, changed)
		}
	}
}

func TestCanResolve(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		unresolved int64
		want       bool
	}{
		// resolve_on=ALL：全部成员 resolved 才关单。
		{"firing all resolved", StatusFiring, 0, true},
		{"candidate all resolved", StatusCandidate, 0, true},
		{"one member still firing", StatusFiring, 1, false},
		// 重放 resolved 不能把 resolved_at 改来改去。
		{"already resolved", StatusResolved, 0, false},
		// 人工接管的不该被自动路径关单。
		{"acknowledged", StatusAcknowledged, 0, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CanResolve(test.status, test.unresolved); got != test.want {
				t.Fatalf("CanResolve(%q, %d) = %v, want %v", test.status, test.unresolved, got, test.want)
			}
		})
	}
}

func TestNormalizeRetryReason(t *testing.T) {
	// 空原因给确定的默认值：事后回放时"没写原因"和"原因是空串"要能区分。
	for _, empty := range []string{"", "   ", "\n\t "} {
		if got := NormalizeRetryReason(empty); got != "verification failed" {
			t.Errorf("NormalizeRetryReason(%q) = %q, want default", empty, got)
		}
	}
	if got := NormalizeRetryReason("  probe failed  "); got != "probe failed" {
		t.Errorf("NormalizeRetryReason() = %q, want trimmed", got)
	}

	// 超长原因按 rune 截断，并保留截断标记。
	long := strings.Repeat("失", MaxRetryReasonRunes+100)
	got := NormalizeRetryReason(long)
	if len([]rune(got)) != MaxRetryReasonRunes {
		t.Fatalf("NormalizeRetryReason() rune len = %d, want %d", len([]rune(got)), MaxRetryReasonRunes)
	}
	if !strings.HasSuffix(got, "…[truncated]") {
		t.Errorf("NormalizeRetryReason() = %q, want truncation marker", got[len(got)-40:])
	}
}

func TestRetrySummaries(t *testing.T) {
	if got, want := RetryQueuedSummary("probe failed"), "retry run queued: probe failed"; got != want {
		t.Errorf("RetryQueuedSummary() = %q, want %q", got, want)
	}
	if got, want := RetryScheduledSummary("probe failed"), "retry scheduled: probe failed"; got != want {
		t.Errorf("RetryScheduledSummary() = %q, want %q", got, want)
	}

	// 摘要列比 payload 窄，长原因要在这一层再收一次。
	long := strings.Repeat("x", MaxRetryReasonRunes)
	for _, summary := range []string{RetryQueuedSummary(long), RetryScheduledSummary(long)} {
		if len([]rune(summary)) != MaxRetrySummaryRunes {
			t.Errorf("summary rune len = %d, want %d", len([]rune(summary)), MaxRetrySummaryRunes)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	// 不在多字节字符中间切开。
	got := TruncateRunes(strings.Repeat("中", 30), 20)
	if len([]rune(got)) != 20 {
		t.Fatalf("TruncateRunes() rune len = %d, want 20", len([]rune(got)))
	}
	if strings.ContainsRune(got, '�') {
		t.Error("TruncateRunes() split a multi-byte rune")
	}

	// 不超限时原样返回。
	if got := TruncateRunes("short", 20); got != "short" {
		t.Errorf("TruncateRunes(short) = %q, want unchanged", got)
	}
	// 标记本身放不下时退化为硬截，不返回比上限更长的串。
	if got := TruncateRunes("abcdefghij", 3); len([]rune(got)) != 3 {
		t.Errorf("TruncateRunes(tiny max) = %q, want 3 runes", got)
	}
	if got := TruncateRunes("abc", 0); got != "" {
		t.Errorf("TruncateRunes(max=0) = %q, want empty", got)
	}
}
