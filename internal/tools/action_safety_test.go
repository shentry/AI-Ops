package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/store"
	"oncall-agent/internal/sub2api"
)

func TestRollbackReceiptMatchesChangeContract(t *testing.T) {
	engine, docker := newFakeEngine(t)
	oldRef := "weishaw/sub2api@sha256:" + strings.Repeat("a", 64)
	goodRef := "weishaw/sub2api@sha256:" + strings.Repeat("b", 64)
	engine.digests = map[string][]string{"sha256:old": {oldRef}, "sha256:new": {goodRef}}
	verified := time.Now().UTC().Add(-time.Hour)
	releases := []Release{{ID: "r2", ImageRef: oldRef, Migration: "none"}, {ID: "r1", ImageRef: goodRef, Migration: "none", VerifiedAt: &verified}}
	service := releaseScript(t, engine, `echo "$1" > ran`)
	engine.deployed = filepath.Join(service.Release.WorkDir, "ran")
	action := NewRollbackAction(docker, func(context.Context) ([]Release, error) { return releases, nil }, service)
	prepared, err := action.Prepare(context.Background(), PrepareRequest{Target: incident.Object{Kind: "service", Name: "sub2api"}, Params: json.RawMessage(`{"release_id":"r1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := action.Execute(context.Background(), operation(prepared))
	if err != nil || !receipt.Written || receipt.Change == nil {
		t.Fatalf("rollback = %+v, %v", receipt, err)
	}
	if receipt.Change.Before != oldRef || receipt.Change.After != goodRef {
		t.Fatalf("change must retain complete image references: %+v", receipt.Change)
	}
	if dsn := os.Getenv("TEST_MYSQL_DSN"); dsn != "" {
		db, err := store.Open(dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		now, change := time.Now().UTC(), receipt.Change
		row, _, err := db.RecordChange(context.Background(), store.ChangeEvent{Env: "test", Service: "receipt-test", ChangeType: change.Type, ReleaseID: &change.ReleaseID, BeforeRef: &change.Before, ImageRef: &change.After, DBMigration: "none", Actor: "test", Source: "test", IdempotencyKey: now.String(), OccurredAt: now, CreatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Delete(&row)
	}
}

func TestRollbackRefusesSkippedMigrationsAndNewRelease(t *testing.T) {
	engine, docker := newFakeEngine(t)
	verified := time.Now().UTC().Add(-time.Hour)
	releases := []Release{
		{ID: "r3", ImageRef: "weishaw/sub2api@sha256:bad", Migration: "none"},
		{ID: "r2", ImageRef: "weishaw/sub2api@sha256:middle", Migration: "incompatible", VerifiedAt: &verified},
		{ID: "r1", ImageRef: "weishaw/sub2api@sha256:good", Migration: "none", VerifiedAt: &verified},
	}
	service := releaseScript(t, engine, `echo "$1" > ran`)
	action := NewRollbackAction(docker, func(context.Context) ([]Release, error) { return releases, nil }, service)
	req := PrepareRequest{Target: incident.Object{Kind: "service", Name: "sub2api"}, Params: json.RawMessage(`{"release_id":"r1"}`)}
	if _, err := action.Prepare(context.Background(), req); err == nil {
		t.Fatal("rollback skipped a release with an incompatible migration")
	}
	req.Params = json.RawMessage(`{"release_id":"r2"}`)
	prepared, err := action.Prepare(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// A deployment may use the same digest but still change configuration/schema.
	releases[0].ID = "r4"
	receipt, err := action.Execute(context.Background(), operation(prepared))
	if err != nil || receipt.Written {
		t.Fatalf("new release was not refused: %+v %v", receipt, err)
	}
	if _, err := os.Stat(filepath.Join(service.Release.WorkDir, "ran")); !os.IsNotExist(err) {
		t.Fatal("deploy command ran after the release record changed")
	}
}

func TestQuarantineRevalidatesCapacityAndStateBeforeWriting(t *testing.T) {
	for _, change := range []string{"capacity", "groups", "temporary", "status", "rate_limit"} {
		t.Run(change, func(t *testing.T) {
			admin := &fakeAdmin{account: sub2api.Account{Status: "active", Schedulable: true, GroupIDs: []int64{2}}, availability: sub2api.Availability{Enabled: true, Groups: map[string]sub2api.GroupAvailability{"2": {GroupID: 2, AvailableCount: 2}}, Accounts: map[string]sub2api.AccountAvailability{"7": {AccountID: 7, IsAvailable: true}}}}
			action := NewUpstreamActions(admin)[0]
			prepared, err := action.Prepare(context.Background(), PrepareRequest{Target: incident.Object{Kind: "upstream_account", Name: "7"}, Rule: config.RuleConfig{MinAvailableAccounts: 1}})
			if err != nil {
				t.Fatal(err)
			}
			future := time.Now().Add(time.Hour)
			switch change {
			case "capacity":
				admin.availability.Groups["2"] = sub2api.GroupAvailability{GroupID: 2, AvailableCount: 1}
			case "groups":
				admin.account.GroupIDs = []int64{3}
			case "temporary":
				admin.account.TempUnschedulableUntil = &future
			case "status":
				admin.account.Status = "disabled"
			case "rate_limit":
				admin.account.RateLimitResetAt = &future
			}
			receipt, err := action.Execute(context.Background(), operation(prepared))
			if err != nil || receipt.Written || len(admin.writes) != 0 {
				t.Fatalf("changed prerequisites accepted: %+v writes=%v err=%v", receipt, admin.writes, err)
			}
		})
	}
}

func TestRestartRefusesTargetRemovedFromConfiguration(t *testing.T) {
	engine, docker := newFakeEngine(t)
	service := testService()
	prepared, err := NewRestartAction(docker, service).Prepare(context.Background(), PrepareRequest{Target: incident.Object{Kind: "container", Name: "sub2api"}})
	if err != nil {
		t.Fatal(err)
	}
	service.Container = "replacement-service"
	receipt, err := NewRestartAction(docker, service).Execute(context.Background(), operation(prepared))
	if err != nil || receipt.Written || engine.restarts != 0 {
		t.Fatalf("removed target executed: %+v %v", receipt, err)
	}
}
