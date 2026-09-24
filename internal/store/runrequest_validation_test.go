package store

import (
	"context"
	"testing"
	"time"
)

func TestRunRequestValidationUsesUnifiedAdmission(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC().Truncate(time.Millisecond)
	parent := insertTestIncident(t, db, now, "invalid-admission")
	for _, tc := range []struct {
		name   string
		mutate func(*RunRequest)
	}{
		{"missing incident", func(r *RunRequest) { r.IncidentID = 0 }},
		{"missing mode", func(r *RunRequest) { r.Mode = "" }},
		{"invalid mode", func(r *RunRequest) { r.Mode = "arbitrary" }},
		{"missing time", func(r *RunRequest) { r.RequestedAt = time.Time{} }},
		{"invalid trigger", func(r *RunRequest) { r.Trigger = "arbitrary" }},
		{"manual skip", func(r *RunRequest) { r.Mode = "skip" }},
		{"retry without parent", func(r *RunRequest) { r.Trigger = RunTriggerRetry }},
		{"manual with retry parent", func(r *RunRequest) { id := uint64(1); r.RetryOf = &id }},
		{"zero retry parent", func(r *RunRequest) { id := uint64(0); r.Trigger, r.RetryOf = RunTriggerRetry, &id }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := RunRequest{IncidentID: parent.ID, Mode: "full", Trigger: RunTriggerManual, RequestedAt: now}
			tc.mutate(&request)
			if _, created, err := db.RequestRun(context.Background(), request); err == nil || created {
				t.Fatalf("invalid request admitted: %+v %v", request, err)
			}
		})
	}
	var runs int64
	if err := db.Model(&AgentRun{}).Where("incident_id = ?", parent.ID).Count(&runs).Error; err != nil || runs != 0 {
		t.Fatalf("invalid requests wrote %d runs: %v", runs, err)
	}
}
