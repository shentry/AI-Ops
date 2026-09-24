package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// incident 归并的边界语义：时间窗、group key 比较、只读查询。

func TestIncidentWindowBoundary(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	seed := t.Name() + time.Now().UTC().Format(time.RFC3339Nano)
	boundaryGroup := "boundary-" + seed
	outsideGroup := "outside-" + seed
	fingerprints := []string{sha256Hex(seed + "-boundary-first"), sha256Hex(seed + "-boundary-edge"), sha256Hex(seed + "-outside-first"), sha256Hex(seed + "-outside-late")}
	var rawEventIDs []uint64
	t.Cleanup(func() {
		db.Where("fingerprint IN ?", fingerprints).Delete(&IncidentAlert{})
		db.Where("group_key IN ?", []string{boundaryGroup, outsideGroup}).Delete(&Incident{})
		db.Where("fingerprint IN ?", fingerprints).Delete(&LastAlert{})
		db.Where("fingerprint IN ?", fingerprints).Delete(&Alert{})
		if len(rawEventIDs) > 0 {
			db.Where("id IN ?", rawEventIDs).Delete(&RawEvent{})
		}
		db.Close()
	})
	base := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	assign := func(groupKey, fingerprint, name string, observedAt time.Time) IncidentAssignment {
		input := AlertInput{
			Fingerprint: fingerprint,
			AlertHash:   md5Hex(seed + "-" + name),
			Source:      "alertmanager",
			Name:        name,
			Severity:    1,
			Status:      "firing",
			Labels:      map[string]string{"alertname": name},
			Annotations: map[string]string{},
			StartsAt:    observedAt.Add(-time.Minute),
			ReceivedAt:  observedAt,
		}
		event := createTestRawEvent(t, db, ctx, observedAt)
		rawEventIDs = append(rawEventIDs, event.ID)
		var assignment IncidentAssignment
		_, err := db.ApplyRawEvent(ctx, event.ID, []AlertInput{input}, observedAt.Add(time.Second), func(ctx context.Context, tx IncidentTx, result AlertApplyResult) error {
			var err error
			assignment, err = tx.AssignIncident(ctx, IncidentInput{
				GroupKey: groupKey, Fingerprint: result.Input.Fingerprint, Name: result.Input.Name,
				Severity: result.Input.Severity, ObservedAt: result.Input.ReceivedAt,
			}, 15*time.Minute, 1)
			return err
		})
		if err != nil {
			t.Fatalf("assign %s: %v", name, err)
		}
		return assignment
	}

	first := assign(boundaryGroup, fingerprints[0], "BoundaryFirst", base)
	edge := assign(boundaryGroup, fingerprints[1], "BoundaryEdge", base.Add(15*time.Minute))
	if !first.Created || edge.Created || edge.IncidentID != first.IncidentID {
		t.Fatalf("window edge assignments: first=%#v edge=%#v", first, edge)
	}
	outsideFirst := assign(outsideGroup, fingerprints[2], "OutsideFirst", base)
	outsideLate := assign(outsideGroup, fingerprints[3], "OutsideLate", base.Add(15*time.Minute+time.Millisecond))
	if !outsideFirst.Created || !outsideLate.Created || outsideLate.IncidentID == outsideFirst.IncidentID {
		t.Fatalf("window outside assignments: first=%#v late=%#v", outsideFirst, outsideLate)
	}
}

func TestIncidentGroupKeyUsesExactComparison(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	seed := t.Name() + time.Now().UTC().Format(time.RFC3339Nano)
	groups := []string{"Payments-" + seed, "payments-" + seed, "payments- " + seed}
	fingerprints := []string{sha256Hex(seed + "-upper"), sha256Hex(seed + "-lower"), sha256Hex(seed + "-space")}
	var rawEventIDs []uint64
	t.Cleanup(func() {
		db.Where("fingerprint IN ?", fingerprints).Delete(&IncidentAlert{})
		db.Where("group_key IN ?", groups).Delete(&Incident{})
		db.Where("fingerprint IN ?", fingerprints).Delete(&LastAlert{})
		db.Where("fingerprint IN ?", fingerprints).Delete(&Alert{})
		if len(rawEventIDs) > 0 {
			db.Where("id IN ?", rawEventIDs).Delete(&RawEvent{})
		}
		db.Close()
	})
	assign := func(groupKey, fingerprint, name string) IncidentAssignment {
		observedAt := time.Date(2026, 8, 18, 13, 0, 0, 0, time.UTC)
		input := AlertInput{
			Fingerprint: fingerprint, AlertHash: md5Hex(seed + name), Source: "alertmanager",
			Name: name, Severity: 3, Status: "firing", Labels: map[string]string{"alertname": name},
			Annotations: map[string]string{}, StartsAt: observedAt.Add(-time.Minute), ReceivedAt: observedAt,
		}
		event := createTestRawEvent(t, db, ctx, observedAt)
		rawEventIDs = append(rawEventIDs, event.ID)
		var assignment IncidentAssignment
		_, err := db.ApplyRawEvent(ctx, event.ID, []AlertInput{input}, observedAt.Add(time.Second), func(ctx context.Context, tx IncidentTx, result AlertApplyResult) error {
			var err error
			assignment, err = tx.AssignIncident(ctx, IncidentInput{
				GroupKey: groupKey, Fingerprint: result.Input.Fingerprint, Name: result.Input.Name,
				Severity: result.Input.Severity, ObservedAt: result.Input.ReceivedAt,
			}, 15*time.Minute, 1)
			return err
		})
		if err != nil {
			t.Fatalf("assign %s: %v", groupKey, err)
		}
		return assignment
	}

	upper := assign(groups[0], fingerprints[0], "UpperAlert")
	lower := assign(groups[1], fingerprints[1], "LowerAlert")
	spaced := assign(groups[2], fingerprints[2], "SpacedAlert")
	if !upper.Created || !lower.Created || !spaced.Created || upper.IncidentID == lower.IncidentID || lower.IncidentID == spaced.IncidentID {
		t.Fatalf("exact group key assignments: upper=%#v lower=%#v spaced=%#v", upper, lower, spaced)
	}
}

func TestIncidentQueryMethods(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	seed := t.Name() + time.Now().UTC().Format(time.RFC3339Nano)
	groupKey := "query-" + seed
	fingerprint := sha256Hex(seed + "-member")
	var rawEventIDs []uint64
	t.Cleanup(func() {
		db.Where("fingerprint = ?", fingerprint).Delete(&IncidentAlert{})
		db.Where("group_key = ?", groupKey).Delete(&Incident{})
		db.Where("fingerprint = ?", fingerprint).Delete(&LastAlert{})
		db.Where("fingerprint = ?", fingerprint).Delete(&Alert{})
		if len(rawEventIDs) > 0 {
			db.Where("id IN ?", rawEventIDs).Delete(&RawEvent{})
		}
		db.Close()
	})
	base := time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC)
	input := AlertInput{
		Fingerprint: fingerprint, AlertHash: md5Hex(seed), Source: "alertmanager",
		Name: "QueryAlert", Severity: 4, Status: "firing", Labels: map[string]string{"alertname": "QueryAlert"},
		Annotations: map[string]string{}, StartsAt: base.Add(-time.Minute), ReceivedAt: base,
	}
	event := createTestRawEvent(t, db, ctx, base)
	rawEventIDs = append(rawEventIDs, event.ID)
	var incidentID uint64
	_, err := db.ApplyRawEvent(ctx, event.ID, []AlertInput{input}, base.Add(time.Second), func(ctx context.Context, tx IncidentTx, result AlertApplyResult) error {
		assignment, err := tx.AssignIncident(ctx, IncidentInput{
			GroupKey: groupKey, Fingerprint: result.Input.Fingerprint, Name: result.Input.Name,
			Severity: result.Input.Severity, ObservedAt: result.Input.ReceivedAt,
		}, 15*time.Minute, 1)
		incidentID = assignment.IncidentID
		return err
	})
	if err != nil {
		t.Fatalf("ApplyRawEvent() error = %v", err)
	}

	// 列表：status 过滤命中与不命中；倒序稳定（本组只有一条，直接断言内容）。
	firing, err := db.ListIncidents(ctx, "firing")
	if err != nil {
		t.Fatalf("ListIncidents(firing) error = %v", err)
	}
	found := false
	for _, item := range firing {
		if item.ID == incidentID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListIncidents(firing) missing incident %d", incidentID)
	}
	resolved, err := db.ListIncidents(ctx, "resolved")
	if err != nil {
		t.Fatalf("ListIncidents(resolved) error = %v", err)
	}
	for _, item := range resolved {
		if item.ID == incidentID {
			t.Fatalf("ListIncidents(resolved) unexpectedly contains incident %d", incidentID)
		}
	}
	all, err := db.ListIncidents(ctx, "")
	if err != nil || len(all) < len(firing) {
		t.Fatalf("ListIncidents(all) error = %v, len = %d, firing len = %d", err, len(all), len(firing))
	}

	// 详情：命中与 404 哨兵。
	got, err := db.GetIncident(ctx, incidentID)
	if err != nil {
		t.Fatalf("GetIncident() error = %v", err)
	}
	if got.GroupKey != groupKey || got.Severity != 4 {
		t.Fatalf("GetIncident() = %#v", got)
	}
	if _, err := db.GetIncident(ctx, 1<<62); !errors.Is(err, ErrIncidentNotFound) {
		t.Fatalf("GetIncident(missing) error = %v, want ErrIncidentNotFound", err)
	}

	// 成员视图：带快照状态和告警名。
	members, err := db.ListIncidentMembers(ctx, incidentID)
	if err != nil {
		t.Fatalf("ListIncidentMembers() error = %v", err)
	}
	if len(members) != 1 || members[0].Fingerprint != fingerprint || members[0].Name != "QueryAlert" || members[0].Status != "firing" || members[0].Severity != 4 {
		t.Fatalf("ListIncidentMembers() = %#v", members)
	}

	// 手动重诊：落 pending run；缺字段拒绝。
	run, _, err := db.RequestRun(ctx, RunRequest{IncidentID: incidentID, Mode: "light", Trigger: RunTriggerManual, RequestedAt: base})
	if err != nil {
		t.Fatalf("RequestRun() error = %v", err)
	}
	defer db.Where("id = ?", run.ID).Delete(&AgentRun{})
	if run.ID == 0 || run.RetryOf != nil || run.Status != "pending" {
		t.Fatalf("RequestRun() = %#v", run)
	}
	if _, _, err := db.RequestRun(ctx, RunRequest{Mode: "light", Trigger: RunTriggerManual, RequestedAt: base}); err == nil {
		t.Fatal("RequestRun() error = nil, want validation failure")
	}
}
