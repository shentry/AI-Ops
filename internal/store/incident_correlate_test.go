package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 跑完整 ApplyRawEvent 链路的端到端归并测试：一条 webhook 报文进去，
// 断言 alert / last_alert / incident / incident_alert / agent_run 的最终状态
// 与事务原子性。边界与查询语义的单点测试在 incident_test.go。

func TestApplyRawEventCorrelatesIncidentAtomically(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	seed := t.Name() + time.Now().UTC().Format(time.RFC3339Nano)
	groupKey := "group-" + seed
	createdAt := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	fingerprints := make([]string, 0, 5)
	var rawEventIDs []uint64
	t.Cleanup(func() {
		db.Where("fingerprint IN ?", fingerprints).Delete(&IncidentAlert{})
		db.Where("group_key = ?", groupKey).Delete(&Incident{})
		db.Where("fingerprint IN ?", fingerprints).Delete(&LastAlert{})
		db.Where("fingerprint IN ?", fingerprints).Delete(&Alert{})
		if len(rawEventIDs) > 0 {
			db.Where("id IN ?", rawEventIDs).Delete(&RawEvent{})
		}
		db.Close()
	})

	makeInput := func(name, hashSuffix string, receivedAt time.Time) AlertInput {
		fingerprint := sha256Hex(seed + "-" + name)
		fingerprints = append(fingerprints, fingerprint)
		return AlertInput{
			Fingerprint: fingerprint,
			AlertHash:   md5Hex(seed + "-" + hashSuffix),
			Source:      "alertmanager",
			Name:        name,
			Severity:    3,
			Status:      "firing",
			Labels:      map[string]string{"alertname": name, "service": "payments"},
			Annotations: map[string]string{"summary": "integration"},
			StartsAt:    receivedAt.Add(-time.Minute),
			ReceivedAt:  receivedAt,
		}
	}
	var assignments []IncidentAssignment
	correlate := func(ctx context.Context, tx IncidentTx, result AlertApplyResult) error {
		if result.Input.Status != "firing" {
			return nil
		}
		if result.Dedup == DedupFull {
			if result.Last.IncidentID == nil {
				return nil
			}
			return tx.TouchIncident(ctx, *result.Last.IncidentID, result.Input.ReceivedAt, result.Input.Severity)
		}
		assignment, err := tx.AssignIncident(ctx, IncidentInput{
			GroupKey:    groupKey,
			Fingerprint: result.Input.Fingerprint,
			Name:        result.Input.Name,
			Severity:    result.Input.Severity,
			ObservedAt:  result.Input.ReceivedAt,
		}, 15*time.Minute, 3)
		if err == nil {
			assignments = append(assignments, assignment)
		}
		return err
	}
	apply := func(input AlertInput) []AlertApplyResult {
		event := createTestRawEvent(t, db, ctx, input.ReceivedAt)
		rawEventIDs = append(rawEventIDs, event.ID)
		results, err := db.ApplyRawEvent(ctx, event.ID, []AlertInput{input}, input.ReceivedAt.Add(time.Second), correlate)
		if err != nil {
			t.Fatalf("ApplyRawEvent(%s) error = %v", input.Name, err)
		}
		return results
	}

	first := makeInput("FirstAlert", "first", createdAt)
	second := makeInput("SecondAlert", "second", createdAt.Add(time.Minute))
	second.Severity = 5
	third := makeInput("ThirdAlert", "third", createdAt.Add(2*time.Minute))
	third.Severity = 4
	if got := apply(first); len(got) != 1 || got[0].Dedup != DedupNew {
		t.Fatalf("first result = %#v", got)
	}
	if got := apply(second); len(got) != 1 || got[0].Dedup != DedupNew {
		t.Fatalf("second result = %#v", got)
	}
	if got := apply(third); len(got) != 1 || got[0].Dedup != DedupNew {
		t.Fatalf("third result = %#v", got)
	}
	if len(assignments) != 3 || !assignments[0].Created || assignments[0].Promoted || assignments[1].Promoted || !assignments[2].Promoted {
		t.Fatalf("assignments = %#v", assignments)
	}

	var incident Incident
	if err := db.Where("group_key = ?", groupKey).Order("id ASC").First(&incident).Error; err != nil {
		t.Fatalf("find incident: %v", err)
	}
	if incident.Status != "firing" || incident.AlertsCount != 3 || incident.Severity != 5 || incident.Title != groupKey+": FirstAlert" {
		t.Fatalf("incident = %#v", incident)
	}
	var memberCount int64
	if err := db.Model(&IncidentAlert{}).Where("incident_id = ?", incident.ID).Count(&memberCount).Error; err != nil {
		t.Fatal(err)
	}
	if memberCount != 3 {
		t.Fatalf("incident member count = %d, want 3", memberCount)
	}
	for _, fingerprint := range []string{first.Fingerprint, second.Fingerprint, third.Fingerprint} {
		var last LastAlert
		if err := db.First(&last, "fingerprint = ?", fingerprint).Error; err != nil {
			t.Fatal(err)
		}
		if last.IncidentID == nil || *last.IncidentID != incident.ID {
			t.Fatalf("last alert incident = %#v, want %d", last.IncidentID, incident.ID)
		}
	}

	full := first
	full.ReceivedAt = createdAt.Add(3 * time.Minute)
	if got := apply(full); len(got) != 1 || got[0].Dedup != DedupFull {
		t.Fatalf("full result = %#v", got)
	}
	var refreshed Incident
	if err := db.First(&refreshed, incident.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !refreshed.LastSeenAt.Equal(full.ReceivedAt) || refreshed.AlertsCount != 3 {
		t.Fatalf("full refresh incident = %#v", refreshed)
	}

	partial := first
	partial.AlertHash = md5Hex(seed + "-partial")
	partial.ReceivedAt = createdAt.Add(4 * time.Minute)
	if got := apply(partial); len(got) != 1 || got[0].Dedup != DedupPartial {
		t.Fatalf("partial result = %#v", got)
	}
	if len(assignments) != 4 || assignments[3].Created || assignments[3].Promoted {
		t.Fatalf("partial assignments = %#v", assignments)
	}
	if err := db.Model(&IncidentAlert{}).Where("incident_id = ?", incident.ID).Count(&memberCount).Error; err != nil {
		t.Fatal(err)
	}
	if memberCount != 3 {
		t.Fatalf("partial member count = %d, want 3", memberCount)
	}

	outside := makeInput("OutsideAlert", "outside", createdAt.Add(20*time.Minute))
	if got := apply(outside); len(got) != 1 || got[0].Dedup != DedupNew {
		t.Fatalf("outside result = %#v", got)
	}
	var incidentCount int64
	if err := db.Model(&Incident{}).Where("group_key = ?", groupKey).Count(&incidentCount).Error; err != nil {
		t.Fatal(err)
	}
	if incidentCount != 2 {
		t.Fatalf("incident count after window = %d, want 2", incidentCount)
	}

	rollback := makeInput("RollbackAlert", "rollback", createdAt.Add(21*time.Minute))
	rollbackEvent := createTestRawEvent(t, db, ctx, rollback.ReceivedAt)
	rawEventIDs = append(rawEventIDs, rollbackEvent.ID)
	_, err := db.ApplyRawEvent(ctx, rollbackEvent.ID, []AlertInput{rollback}, rollback.ReceivedAt.Add(time.Second), func(ctx context.Context, tx IncidentTx, result AlertApplyResult) error {
		if _, err := tx.AssignIncident(ctx, IncidentInput{
			GroupKey: groupKey + "-rollback", Fingerprint: result.Input.Fingerprint, Name: result.Input.Name,
			Severity: result.Input.Severity, ObservedAt: result.Input.ReceivedAt,
		}, 15*time.Minute, 1); err != nil {
			return err
		}
		return errors.New("correlation failed")
	})
	if err == nil {
		t.Fatal("ApplyRawEvent() error = nil, want correlation rollback")
	}
	assertRawStatus(t, db, rollbackEvent.ID, "pending")
	var rollbackCount int64
	if err := db.Model(&Alert{}).Where("fingerprint = ?", rollback.Fingerprint).Count(&rollbackCount).Error; err != nil {
		t.Fatal(err)
	}
	if rollbackCount != 0 {
		t.Fatalf("rolled-back alert count = %d, want 0", rollbackCount)
	}
	var rollbackLastCount int64
	if err := db.Model(&LastAlert{}).Where("fingerprint = ?", rollback.Fingerprint).Count(&rollbackLastCount).Error; err != nil {
		t.Fatal(err)
	}
	if rollbackLastCount != 0 {
		t.Fatalf("rolled-back last alert count = %d, want 0", rollbackLastCount)
	}
	var rollbackMemberCount int64
	if err := db.Model(&IncidentAlert{}).Where("fingerprint = ?", rollback.Fingerprint).Count(&rollbackMemberCount).Error; err != nil {
		t.Fatal(err)
	}
	if rollbackMemberCount != 0 {
		t.Fatalf("rolled-back incident member count = %d, want 0", rollbackMemberCount)
	}
	var rollbackIncidentCount int64
	if err := db.Model(&Incident{}).Where("group_key = ?", groupKey+"-rollback").Count(&rollbackIncidentCount).Error; err != nil {
		t.Fatal(err)
	}
	if rollbackIncidentCount != 0 {
		t.Fatalf("rolled-back incident count = %d, want 0", rollbackIncidentCount)
	}
}

func TestIncidentResolvePropagationAndAgentRunQueue(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	seed := t.Name() + time.Now().UTC().Format(time.RFC3339Nano)
	groupKey := "resolve-" + seed
	fingerprints := []string{sha256Hex(seed + "-a"), sha256Hex(seed + "-b")}
	var rawEventIDs []uint64
	var incidentIDs []uint64
	t.Cleanup(func() {
		db.Where("incident_id IN ?", incidentIDs).Delete(&AgentRun{})
		db.Where("fingerprint IN ?", fingerprints).Delete(&IncidentAlert{})
		db.Where("group_key = ?", groupKey).Delete(&Incident{})
		db.Where("fingerprint IN ?", fingerprints).Delete(&LastAlert{})
		db.Where("fingerprint IN ?", fingerprints).Delete(&Alert{})
		if len(rawEventIDs) > 0 {
			db.Where("id IN ?", rawEventIDs).Delete(&RawEvent{})
		}
		db.Close()
	})
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	// 与 worker 的 D05 hook 同构：firing 促发落 agent_run，resolved 传播关单。
	var runs []AgentRun
	var resolvedCount int
	hook := func(ctx context.Context, tx IncidentTx, result AlertApplyResult) error {
		if result.Input.Status != "firing" {
			if result.Input.Status == "resolved" && result.Last.IncidentID != nil {
				closed, err := tx.ResolveIncident(ctx, *result.Last.IncidentID, result.Input.ReceivedAt)
				if err != nil {
					return err
				}
				if closed {
					resolvedCount++
				}
			}
			return nil
		}
		assignment, err := tx.AssignIncident(ctx, IncidentInput{
			GroupKey: groupKey, Fingerprint: result.Input.Fingerprint, Name: result.Input.Name,
			Severity: result.Input.Severity, ObservedAt: result.Input.ReceivedAt,
		}, 15*time.Minute, 1)
		if err != nil {
			return err
		}
		if assignment.Promoted {
			run := AgentRun{IncidentID: assignment.IncidentID, Mode: "full", Status: "pending", StartedAt: result.Input.ReceivedAt}
			if err := tx.EnqueueAgentRun(ctx, &run); err != nil {
				return err
			}
			runs = append(runs, run)
		}
		return nil
	}
	apply := func(input AlertInput) {
		event := createTestRawEvent(t, db, ctx, input.ReceivedAt)
		rawEventIDs = append(rawEventIDs, event.ID)
		if _, err := db.ApplyRawEvent(ctx, event.ID, []AlertInput{input}, input.ReceivedAt.Add(time.Second), hook); err != nil {
			t.Fatalf("ApplyRawEvent(%s) error = %v", input.Name, err)
		}
	}
	firing := func(fingerprint, name, hashSuffix string, receivedAt time.Time) AlertInput {
		return AlertInput{
			Fingerprint: fingerprint, AlertHash: md5Hex(seed + "-" + hashSuffix), Source: "alertmanager",
			Name: name, Severity: 5, Status: "firing", Labels: map[string]string{"alertname": name},
			Annotations: map[string]string{}, StartsAt: receivedAt.Add(-time.Minute), ReceivedAt: receivedAt,
		}
	}
	resolvedInput := func(input AlertInput, receivedAt time.Time) AlertInput {
		input.Status = "resolved"
		input.AlertHash = md5Hex(seed + "-" + input.Name + "-resolved")
		input.ReceivedAt = receivedAt
		return input
	}

	first := firing(fingerprints[0], "ResolveFirst", "a", base)
	second := firing(fingerprints[1], "ResolveSecond", "b", base.Add(time.Minute))
	apply(first)
	apply(second)

	var incident Incident
	if err := db.Where("group_key = ?", groupKey).First(&incident).Error; err != nil {
		t.Fatalf("find incident: %v", err)
	}
	incidentIDs = append(incidentIDs, incident.ID)
	if incident.Status != "firing" || incident.AlertsCount != 2 {
		t.Fatalf("incident = %#v, want firing with 2 members", incident)
	}
	if len(runs) != 1 || runs[0].IncidentID != incident.ID || runs[0].Mode != "full" || runs[0].Status != "pending" {
		t.Fatalf("queued runs = %#v", runs)
	}
	var persistedRuns int64
	if err := db.Model(&AgentRun{}).Where("incident_id = ?", incident.ID).Count(&persistedRuns).Error; err != nil {
		t.Fatal(err)
	}
	if persistedRuns != 1 {
		t.Fatalf("persisted agent runs = %d, want 1", persistedRuns)
	}

	// 只 resolved 一个成员：incident 保持 firing。
	apply(resolvedInput(first, base.Add(2*time.Minute)))
	if err := db.First(&incident, incident.ID).Error; err != nil {
		t.Fatal(err)
	}
	if incident.Status != "firing" || incident.ResolvedAt != nil || resolvedCount != 0 {
		t.Fatalf("partially resolved incident = %#v, closed count = %d", incident, resolvedCount)
	}

	// 全部成员 resolved：incident 关单且 resolved_at 取resolved 告警的接收时间。
	resolvedAt := base.Add(3 * time.Minute)
	apply(resolvedInput(second, resolvedAt))
	if err := db.First(&incident, incident.ID).Error; err != nil {
		t.Fatal(err)
	}
	if incident.Status != "resolved" || incident.ResolvedAt == nil || !incident.ResolvedAt.Equal(resolvedAt) || resolvedCount != 1 {
		t.Fatalf("resolved incident = %#v, closed count = %d", incident, resolvedCount)
	}

	// 重放 resolved：已关单 incident 不重复处理，resolved_at 不被改写。
	apply(resolvedInput(second, base.Add(4*time.Minute)))
	if err := db.First(&incident, incident.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !incident.ResolvedAt.Equal(resolvedAt) || resolvedCount != 1 {
		t.Fatalf("replayed resolve incident = %#v, closed count = %d", incident, resolvedCount)
	}

	// 促发后 hook 失败：incident 和 agent_run 必须一起回滚。
	rollback := firing(sha256Hex(seed+"-c"), "ResolveRollback", "c", base.Add(5*time.Minute))
	fingerprints = append(fingerprints, rollback.Fingerprint)
	rollbackEvent := createTestRawEvent(t, db, ctx, rollback.ReceivedAt)
	rawEventIDs = append(rawEventIDs, rollbackEvent.ID)
	_, err := db.ApplyRawEvent(ctx, rollbackEvent.ID, []AlertInput{rollback}, rollback.ReceivedAt.Add(time.Second), func(ctx context.Context, tx IncidentTx, result AlertApplyResult) error {
		assignment, err := tx.AssignIncident(ctx, IncidentInput{
			GroupKey: groupKey + "-rollback", Fingerprint: result.Input.Fingerprint, Name: result.Input.Name,
			Severity: result.Input.Severity, ObservedAt: result.Input.ReceivedAt,
		}, 15*time.Minute, 1)
		if err != nil {
			return err
		}
		if err := tx.EnqueueAgentRun(ctx, &AgentRun{IncidentID: assignment.IncidentID, Mode: "full", Status: "pending", StartedAt: result.Input.ReceivedAt}); err != nil {
			return err
		}
		return errors.New("post enqueue failure")
	})
	if err == nil {
		t.Fatal("ApplyRawEvent() error = nil, want enqueue rollback")
	}
	var rollbackIncidents []Incident
	if err := db.Where("group_key = ?", groupKey+"-rollback").Find(&rollbackIncidents).Error; err != nil {
		t.Fatal(err)
	}
	if len(rollbackIncidents) != 0 {
		t.Fatalf("rolled-back incidents = %#v, want none", rollbackIncidents)
	}
	var queued int64
	if err := db.Model(&AgentRun{}).Where("incident_id NOT IN ?", incidentIDs).Count(&queued).Error; err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("agent runs after rollback = %d, want 0", queued)
	}
}
