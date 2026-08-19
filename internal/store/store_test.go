package store

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"
)

func TestCreateRawEventRejectsInvalidJSON(t *testing.T) {
	db := &DB{}
	_, err := db.CreateRawEvent(context.Background(), "alertmanager", []byte(`{"version":`), time.Now())
	if !errors.Is(err, ErrInvalidRawEvent) {
		t.Fatalf("CreateRawEvent() error = %v, want ErrInvalidRawEvent", err)
	}
}

func TestApplyRawEventDedupAndAtomicStatus(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	seed := t.Name() + time.Now().UTC().Format(time.RFC3339Nano)
	fingerprint := sha256Hex(seed)
	rollbackFingerprint := sha256Hex(seed + "-rollback")
	createdAt := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	generatorURL := "http://127.0.0.1:9090/graph?g0.expr=vector(1)"
	var rawEventIDs []uint64
	t.Cleanup(func() {
		db.Where("fingerprint IN ?", []string{fingerprint, rollbackFingerprint}).Delete(&LastAlert{})
		db.Where("fingerprint IN ?", []string{fingerprint, rollbackFingerprint}).Delete(&Alert{})
		if len(rawEventIDs) > 0 {
			db.Where("id IN ?", rawEventIDs).Delete(&RawEvent{})
		}
		db.Close()
	})

	newInput := AlertInput{
		Fingerprint:  fingerprint,
		AlertHash:    md5Hex(seed + "-firing"),
		Source:       "alertmanager",
		Name:         "StoreIntegration",
		Severity:     5,
		Status:       "firing",
		Labels:       map[string]string{"alertname": "StoreIntegration", "instance": seed},
		Annotations:  map[string]string{"summary": "integration"},
		GeneratorURL: generatorURL,
		StartsAt:     createdAt.Add(-time.Minute),
		ReceivedAt:   createdAt,
	}

	first := createTestRawEvent(t, db, ctx, createdAt)
	rawEventIDs = append(rawEventIDs, first.ID)
	results, err := db.ApplyRawEvent(ctx, first.ID, []AlertInput{newInput}, createdAt.Add(time.Second), nil)
	if err != nil {
		t.Fatalf("ApplyRawEvent(new) error = %v", err)
	}
	if len(results) != 1 || results[0].Dedup != DedupNew {
		t.Fatalf("ApplyRawEvent(new) results = %#v", results)
	}
	assertRawStatus(t, db, first.ID, "processed")

	second := createTestRawEvent(t, db, ctx, createdAt.Add(time.Minute))
	rawEventIDs = append(rawEventIDs, second.ID)
	fullInput := newInput
	fullInput.ReceivedAt = second.CreatedAt
	results, err = db.ApplyRawEvent(ctx, second.ID, []AlertInput{fullInput}, second.CreatedAt.Add(time.Second), nil)
	if err != nil {
		t.Fatalf("ApplyRawEvent(full) error = %v", err)
	}
	if len(results) != 1 || results[0].Dedup != DedupFull {
		t.Fatalf("ApplyRawEvent(full) results = %#v", results)
	}

	third := createTestRawEvent(t, db, ctx, createdAt.Add(2*time.Minute))
	rawEventIDs = append(rawEventIDs, third.ID)
	partialInput := fullInput
	partialInput.AlertHash = md5Hex(seed + "-resolved")
	partialInput.Status = "resolved"
	partialInput.ReceivedAt = third.CreatedAt
	results, err = db.ApplyRawEvent(ctx, third.ID, []AlertInput{partialInput}, third.CreatedAt.Add(time.Second), nil)
	if err != nil {
		t.Fatalf("ApplyRawEvent(partial) error = %v", err)
	}
	if len(results) != 1 || results[0].Dedup != DedupPartial {
		t.Fatalf("ApplyRawEvent(partial) results = %#v", results)
	}

	var alertCount int64
	if err := db.Model(&Alert{}).Where("fingerprint = ?", fingerprint).Count(&alertCount).Error; err != nil {
		t.Fatal(err)
	}
	if alertCount != 2 {
		t.Fatalf("alert count = %d, want 2", alertCount)
	}
	var last LastAlert
	if err := db.First(&last, "fingerprint = ?", fingerprint).Error; err != nil {
		t.Fatal(err)
	}
	if last.Status != "resolved" || last.FiringCount != 2 || !last.LastSeen.Equal(third.CreatedAt) {
		t.Fatalf("last alert = %#v", last)
	}
	var latest Alert
	if err := db.Where("fingerprint = ?", fingerprint).Order("id DESC").First(&latest).Error; err != nil {
		t.Fatal(err)
	}
	if latest.GeneratorURL != generatorURL {
		t.Fatalf("generator URL = %q", latest.GeneratorURL)
	}

	rollbackRaw := createTestRawEvent(t, db, ctx, createdAt.Add(3*time.Minute))
	rawEventIDs = append(rawEventIDs, rollbackRaw.ID)
	valid := newInput
	valid.Fingerprint = rollbackFingerprint
	valid.AlertHash = md5Hex(seed + "-rollback-valid")
	invalid := valid
	invalid.Name = ""
	if _, err := db.ApplyRawEvent(ctx, rollbackRaw.ID, []AlertInput{valid, invalid}, time.Now().UTC(), nil); err == nil {
		t.Fatal("ApplyRawEvent() error = nil, want transaction failure")
	}
	assertRawStatus(t, db, rollbackRaw.ID, "pending")
	if err := db.Model(&Alert{}).Where("fingerprint = ?", rollbackFingerprint).Count(&alertCount).Error; err != nil {
		t.Fatal(err)
	}
	if alertCount != 0 {
		t.Fatalf("rolled-back alert count = %d, want 0", alertCount)
	}
}

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
			if err := tx.EnqueueAgentRun(ctx, run); err != nil {
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
		if err := tx.EnqueueAgentRun(ctx, AgentRun{IncidentID: assignment.IncidentID, Mode: "full", Status: "pending", StartedAt: result.Input.ReceivedAt}); err != nil {
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
	run, err := db.CreateAgentRun(ctx, AgentRun{IncidentID: incidentID, Mode: "light", Status: "pending", StartedAt: base})
	if err != nil {
		t.Fatalf("CreateAgentRun() error = %v", err)
	}
	defer db.Where("id = ?", run.ID).Delete(&AgentRun{})
	if run.ID == 0 || run.RetryOf != nil || run.Status != "pending" {
		t.Fatalf("CreateAgentRun() = %#v", run)
	}
	if _, err := db.CreateAgentRun(ctx, AgentRun{Mode: "light", Status: "pending", StartedAt: base}); err == nil {
		t.Fatal("CreateAgentRun() error = nil, want validation failure")
	}
}

func TestCompleteAgentRun(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	run, err := db.CreateAgentRun(ctx, AgentRun{IncidentID: 1, Mode: "full", Status: "pending", StartedAt: now})
	if err != nil {
		t.Fatalf("CreateAgentRun() error = %v", err)
	}
	t.Cleanup(func() {
		db.Where("id = ?", run.ID).Delete(&AgentRun{})
		db.Close()
	})

	// 正常结论：RCA、Plan、token、终态一起落库。
	if err := db.CompleteAgentRun(ctx, run.ID, "root cause", []byte(`{"action":"none"}`), 120, 45, "succeeded", now.Add(time.Minute)); err != nil {
		t.Fatalf("CompleteAgentRun() error = %v", err)
	}
	var got AgentRun
	if err := db.First(&got, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || got.TokensIn != 120 || got.TokensOut != 45 || got.RCAText == nil || *got.RCAText != "root cause" || got.PlanJSON == nil || got.FinishedAt == nil {
		t.Fatalf("agent run = %#v", got)
	}

	// 终态不可覆盖：审计记录不能被改写。
	if err := db.CompleteAgentRun(ctx, run.ID, "rewrite", nil, 0, 0, "failed", now.Add(2*time.Minute)); err == nil {
		t.Fatal("CompleteAgentRun(terminal) error = nil, want refusal")
	}
	if err := db.First(&got, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || got.TokensIn != 120 {
		t.Fatalf("terminal run was rewritten: %#v", got)
	}

	// 非法终态与缺 id 直接拒绝。
	if err := db.CompleteAgentRun(ctx, run.ID, "", nil, 0, 0, "running", now); err == nil {
		t.Fatal("CompleteAgentRun(running) error = nil, want refusal")
	}
	if err := db.CompleteAgentRun(ctx, 0, "", nil, 0, 0, "succeeded", now); err == nil {
		t.Fatal("CompleteAgentRun(id=0) error = nil, want refusal")
	}
}

func TestAgentRunQueueMethods(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 13, 0, 0, 0, time.UTC)
	var runIDs []uint64
	t.Cleanup(func() {
		if len(runIDs) > 0 {
			db.Where("run_id IN ?", runIDs).Delete(&AgentRunStep{})
			db.Where("id IN ?", runIDs).Delete(&AgentRun{})
		}
		db.Close()
	})

	first, err := db.CreateAgentRun(ctx, AgentRun{IncidentID: 1, Mode: "full", Status: "pending", StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.CreateAgentRun(ctx, AgentRun{IncidentID: 2, Mode: "light", Status: "pending", StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	runIDs = append(runIDs, first.ID, second.ID)

	// 队首是 id 最小的 pending。
	next, found, err := db.NextPendingAgentRun(ctx)
	if err != nil || !found || next.ID != first.ID {
		t.Fatalf("NextPendingAgentRun() = %v, %v, %v", next.ID, found, err)
	}

	// 认领是原子的：第二次认领同一行失败。
	// 认领时间取 10 分钟前，让后面的超时回补测试能把这条当 stale。
	claimed, err := db.ClaimAgentRun(ctx, first.ID, now.Add(-10*time.Minute))
	if err != nil || !claimed {
		t.Fatalf("ClaimAgentRun() = %v, %v", claimed, err)
	}
	again, err := db.ClaimAgentRun(ctx, first.ID, now)
	if err != nil || again {
		t.Fatalf("re-claim = %v, %v, want false", again, err)
	}
	// 队首前进到第二条。
	next, found, err = db.NextPendingAgentRun(ctx)
	if err != nil || !found || next.ID != second.ID {
		t.Fatalf("NextPendingAgentRun() after claim = %v, %v", next.ID, found)
	}

	// 超时 running 回补 pending；未超时的不动。
	recent, err := db.CreateAgentRun(ctx, AgentRun{IncidentID: 3, Mode: "full", Status: "running", StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	runIDs = append(runIDs, recent.ID)
	requeued, err := db.RequeueStaleAgentRuns(ctx, now.Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if requeued == 0 {
		t.Fatal("RequeueStaleAgentRuns() = 0, want >= 1")
	}
	var firstRun AgentRun
	if err := db.First(&firstRun, first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if firstRun.Status != "pending" {
		t.Fatalf("stale run status = %q, want pending after requeue", firstRun.Status)
	}
	var recentRun AgentRun
	if err := db.First(&recentRun, recent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if recentRun.Status != "running" {
		t.Fatalf("recent running run was requeued: %q", recentRun.Status)
	}

	// step 落库与校验。
	finishedAt := now.Add(time.Second)
	if err := db.AppendRunStep(ctx, AgentRunStep{RunID: first.ID, Seq: 1, Kind: "evidence", Name: "collect", StartedAt: now, FinishedAt: &finishedAt}); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendRunStep(ctx, AgentRunStep{RunID: 0, Seq: 1, Kind: "evidence", Name: "x", StartedAt: now}); err == nil {
		t.Fatal("AppendRunStep(runID=0) error = nil, want refusal")
	}
}

func openIntegrationDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is not set")
	}
	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return db
}

func createTestRawEvent(t *testing.T, db *DB, ctx context.Context, createdAt time.Time) RawEvent {
	t.Helper()
	event, err := db.CreateRawEvent(ctx, "alertmanager", []byte(`{"version":"4","alerts":[]}`), createdAt)
	if err != nil {
		t.Fatalf("CreateRawEvent() error = %v", err)
	}
	return event
}

func assertRawStatus(t *testing.T, db *DB, id uint64, want string) {
	t.Helper()
	var event RawEvent
	if err := db.First(&event, id).Error; err != nil {
		t.Fatal(err)
	}
	if event.Status != want {
		t.Fatalf("raw event %d status = %q, want %q", id, event.Status, want)
	}
}

func sha256Hex(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func md5Hex(value string) string {
	digest := md5.Sum([]byte(value))
	return hex.EncodeToString(digest[:])
}

func TestApprovalLifecycle(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 14, 0, 0, 0, time.UTC)
	var ids []uint64
	t.Cleanup(func() {
		if len(ids) > 0 {
			db.Where("id IN ?", ids).Delete(&Approval{})
		}
		db.Close()
	})

	created, err := db.CreateApproval(ctx, Approval{
		IncidentID: 1, RunID: 2, ToolName: "resize_pool", ArgsJSON: []byte(`{"target_name":"sub2api"}`),
		Reason: "L3", PlanHash: "hash-1", ExpiresAt: now.Add(30 * time.Minute), CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, created.ID)
	if created.Status != "pending" {
		t.Fatalf("status = %q", created.Status)
	}

	// 批准成功；重复批准 409；过期窗口外不能决策。
	if err := db.DecideApproval(ctx, created.ID, "approved", "ops", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.DecideApproval(ctx, created.ID, "denied", "ops", now.Add(2*time.Minute)); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("re-decide error = %v, want conflict", err)
	}
	if err := db.DecideApproval(ctx, 1<<62, "approved", "ops", now); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("missing error = %v, want not found", err)
	}

	// 过期单不能被批准。
	expired, err := db.CreateApproval(ctx, Approval{
		IncidentID: 1, RunID: 2, ToolName: "x", ArgsJSON: []byte(`{}`),
		Reason: "r", PlanHash: "h", ExpiresAt: now.Add(time.Minute), CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, expired.ID)
	if err := db.DecideApproval(ctx, expired.ID, "approved", "ops", now.Add(2*time.Minute)); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("expired approve error = %v, want conflict", err)
	}

	// 主动过期 sweep。
	swept, err := db.ExpireApprovals(ctx, now.Add(2*time.Minute))
	if err != nil || swept == 0 {
		t.Fatalf("ExpireApprovals() = %d, %v", swept, err)
	}
	got, err := db.GetApproval(ctx, expired.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "expired" {
		t.Fatalf("status = %q, want expired", got.Status)
	}
	// 已批准的不会被 sweep。
	got, _ = db.GetApproval(ctx, created.ID)
	if got.Status != "approved" {
		t.Fatalf("approved status = %q", got.Status)
	}

	// 列表过滤。
	approvals, err := db.ListApprovals(ctx, "expired")
	if err != nil {
		t.Fatal(err)
	}
	foundExpired := false
	for _, a := range approvals {
		if a.ID == expired.ID {
			foundExpired = true
		}
		if a.ID == created.ID {
			t.Fatal("approved approval leaked into expired filter")
		}
	}
	if !foundExpired {
		t.Fatal("expired approval missing from list")
	}
}

func TestFaultMemoryCRUD(t *testing.T) {
	db := openIntegrationDB(t)
	ctx := context.Background()
	fp := "testfp" + md5Hex(t.Name())[:6]
	t.Cleanup(func() {
		db.Where("fingerprint = ?", fp).Delete(&FaultMemory{})
		db.Where("fingerprint = ?", fp).Delete(&FaultCmdHistory{})
		db.Close()
	})

	now := time.Date(2026, 8, 19, 15, 0, 0, 0, time.UTC)
	if _, err := db.GetFaultMemory(ctx, fp); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("missing lookup = %v", err)
	}
	entry := FaultMemory{
		Fingerprint: fp, GroupKey: "payments", AlertName: "HighCPU",
		RCAText: "容器退出", PlanJSON: []byte(`{"action":"docker_restart"}`),
		Confidence: "high", FirstSeen: now, LastSuccess: now, TTLSeconds: 3600,
	}
	if err := db.UpsertFaultMemory(ctx, entry); err != nil {
		t.Fatal(err)
	}
	// Upsert 覆盖同指纹。
	entry.RCAText = "新 RCA"
	if err := db.UpsertFaultMemory(ctx, entry); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetFaultMemory(ctx, fp)
	if err != nil {
		t.Fatal(err)
	}
	if got.RCAText != "新 RCA" || got.Confidence != "high" {
		t.Fatalf("got = %+v", got)
	}
	// 命中计数。
	if err := db.TouchFaultMemory(ctx, fp, now); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetFaultMemory(ctx, fp)
	if got.Hits != 1 || got.LastUsed == nil {
		t.Fatalf("after touch = %+v", got)
	}
	// 降级。
	if err := db.DemoteFaultMemory(ctx, fp, now); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetFaultMemory(ctx, fp)
	if got.Confidence != "low" {
		t.Fatalf("after demote confidence = %q", got.Confidence)
	}
	// 命令历史倒序限量。
	for i := range 7 {
		if err := db.InsertFaultCmdHistory(ctx, FaultCmdHistory{
			Fingerprint: fp, ToolName: "docker_restart", ArgsJSON: []byte(`{}`),
			ResultBrief: "ok", CreatedAt: now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	cmds, err := db.ListCmdHistory(ctx, fp, 5)
	if err != nil || len(cmds) != 5 {
		t.Fatalf("ListCmdHistory = %d, %v", len(cmds), err)
	}
}
