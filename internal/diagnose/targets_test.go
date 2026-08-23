package diagnose

import (
	"reflect"
	"testing"

	"oncall-agent/internal/store"
)

func TestKnownTargetsCollectsRunObjectLabels(t *testing.T) {
	target := Target{
		Alerts: []store.Alert{
			{Labels: []byte(`{"alertname":"HighCPU","severity":"critical","container":"sub2api","instance":"10.0.0.1:9100"}`)},
			{Labels: []byte(`{"job":"sub2api","service":"gateway"}`)},
		},
	}
	got := KnownTargets(target)
	want := []string{"10.0.0.1:9100", "gateway", "sub2api"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("KnownTargets() = %v, want %v", got, want)
	}
}

// alertname/severity/team 这类标签不是运行对象：把它们算进来等于
// 让"target 必须来自告警标签"这条护栏形同虚设。
func TestKnownTargetsIgnoresNonObjectLabels(t *testing.T) {
	target := Target{Alerts: []store.Alert{
		{Labels: []byte(`{"alertname":"HighCPU","severity":"critical","team":"payments"}`)},
	}}
	if got := KnownTargets(target); len(got) != 0 {
		t.Fatalf("KnownTargets() = %v, want empty", got)
	}
}

// 标签不是合法 JSON 时不炸、不污染结果 —— 证据层容忍脏数据。
func TestKnownTargetsToleratesBadLabels(t *testing.T) {
	target := Target{Alerts: []store.Alert{
		{Labels: []byte(`not json`)},
		{Labels: []byte(`{"container":"sub2api"}`)},
	}}
	if got := KnownTargets(target); !reflect.DeepEqual(got, []string{"sub2api"}) {
		t.Fatalf("KnownTargets() = %v, want [sub2api]", got)
	}
}

func TestKnownTargetsEmptyWithoutAlerts(t *testing.T) {
	if got := KnownTargets(Target{}); len(got) != 0 {
		t.Fatalf("KnownTargets() = %v, want empty", got)
	}
}
