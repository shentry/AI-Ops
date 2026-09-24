package store

import (
	"testing"
	"time"

	incidentrule "oncall-agent/internal/incident"
)

// 不需要 MySQL 的纯单测：守住 store 与领域层之间的类型转换。
// 本包其余测试都打真库（见 helpers_test.go），这一条是例外 ——
// 它守的是编译期契约，起库反而测不到。

// TestIncidentInputConvertsToMergeInput 守住 AssignIncident 里的
// incidentrule.MergeInput(input) 这次直接转换。
//
// GroupKey / Fingerprint / Name 三个字段同为 string：谁把它们换个顺序，
// 转换照样编译通过，但字段会静默串位 —— group_key 变成 name，
// 归并就会把不相干的告警缝进同一个 incident。这个测试让那种改动当场失败。
func TestIncidentInputConvertsToMergeInput(t *testing.T) {
	observedAt := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	input := IncidentInput{
		GroupKey:    "group-value",
		Fingerprint: "fingerprint-value",
		Name:        "name-value",
		Severity:    4,
		ObservedAt:  observedAt,
	}

	got := incidentrule.MergeInput(input)

	if got.GroupKey != "group-value" {
		t.Errorf("GroupKey = %q, want %q", got.GroupKey, "group-value")
	}
	if got.Fingerprint != "fingerprint-value" {
		t.Errorf("Fingerprint = %q, want %q", got.Fingerprint, "fingerprint-value")
	}
	if got.Name != "name-value" {
		t.Errorf("Name = %q, want %q", got.Name, "name-value")
	}
	if got.Severity != 4 {
		t.Errorf("Severity = %d, want 4", got.Severity)
	}
	if !got.ObservedAt.Equal(observedAt) {
		t.Errorf("ObservedAt = %v, want %v", got.ObservedAt, observedAt)
	}
}
