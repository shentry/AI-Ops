package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Normal tests use an already migrated TEST_MYSQL_DSN. The migration exercise
// below additionally requires an explicit mode and refuses any nonempty schema.
func TestExecutionUpgradeRetirement(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	var before []Approval
	for _, status := range []string{"pending", "approved", "executing", "executed", "failed", "denied", "expired"} {
		before = append(before, upgradeApproval(t, db, status, false))
	}
	for _, status := range []string{"pending", "approved", "executing", "executed", "simulated"} {
		before = append(before, upgradeApproval(t, db, status, true))
	}
	if err := db.CheckExecutionReady(ctx); err == nil || !strings.Contains(err.Error(), "legacy active") {
		t.Fatalf("preflight must reject legacy active approvals: %v", err)
	}
	for _, row := range before {
		assertUpgradeUnchanged(t, db, row)
		assertUpgradeCount(t, db, "incident_event", "approval_id = ?", row.ID, 0)
		assertUpgradeCount(t, db, "incident_problem", "incident_id = ?", row.IncidentID, 0)
	}
	if n, err := db.RetireLegacyApprovals(ctx, time.Time{}); n != 0 || err == nil {
		t.Fatalf("zero retirement time: n=%d err=%v", n, err)
	}
	if n, err := db.RetireLegacyApprovals(ctx, now); n != 3 || err != nil {
		t.Fatalf("retire: n=%d err=%v", n, err)
	}
	for i, row := range before {
		if i < 3 {
			assertUpgradeRetired(t, db, row, now)
		} else {
			assertUpgradeUnchanged(t, db, row)
			assertUpgradeCount(t, db, "incident_event", "approval_id = ?", row.ID, 0)
		}
		assertUpgradeCount(t, db, "verify_task", "approval_id = ?", row.ID, 0)
		assertUpgradeCount(t, db, "fault_cmd_history", "approval_id = ?", row.ID, 0)
	}
	if n, err := db.RetireLegacyApprovals(ctx, now.Add(time.Hour)); n != 0 || err != nil {
		t.Fatalf("repeat retire: n=%d err=%v", n, err)
	}
	for _, row := range before[:3] {
		assertUpgradeRetired(t, db, row, now)
	}
	if err := db.CheckExecutionReady(ctx); err != nil {
		t.Fatalf("preflight after retirement: %v", err)
	}
}

func TestExecutionUpgradeRollback(t *testing.T) {
	for _, table := range []string{"incident_event", "incident_problem"} {
		t.Run(table, func(t *testing.T) {
			db := openIntegrationDB(t)
			t.Cleanup(func() { db.Close() })
			// An earlier approval commits, but every change to the failing row rolls back.
			first := upgradeApproval(t, db, "pending", false)
			row := upgradeApproval(t, db, "executing", false)
			trigger := fmt.Sprintf("upgrade_fail_%s_%d", table, row.ID)
			stmt := fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON %s FOR EACH ROW
				BEGIN IF NEW.incident_id = %d THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'upgrade audit injection'; END IF; END`, trigger, table, row.IncidentID)
			if err := db.Exec(stmt).Error; err != nil {
				t.Fatal(err)
			}
			defer db.Exec("DROP TRIGGER IF EXISTS " + trigger)
			now := time.Now().UTC().Truncate(time.Millisecond)
			n, err := db.RetireLegacyApprovals(context.Background(), now)
			if n != 1 || err == nil || !strings.Contains(err.Error(), "upgrade audit injection") {
				t.Fatalf("rollback: n=%d err=%v", n, err)
			}
			assertUpgradeRetired(t, db, first, now)
			assertUpgradeUnchanged(t, db, row)
			assertUpgradeCount(t, db, "incident_event", "approval_id = ?", row.ID, 0)
			assertUpgradeCount(t, db, "incident_problem", "incident_id = ?", row.IncidentID, 0)
			if err := db.Exec("DROP TRIGGER " + trigger).Error; err != nil {
				t.Fatal(err)
			}
			if n, err := db.RetireLegacyApprovals(context.Background(), now); n != 1 || err != nil {
				t.Fatalf("resume: n=%d err=%v", n, err)
			}
			assertUpgradeRetired(t, db, row, now)
		})
	}
}

func TestExecutionUpgradeCommitFailureCount(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	first := upgradeApproval(t, db, "pending", false)
	row := upgradeApproval(t, db, "executing", false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel after the last successful INSERT, not during a statement. All writes
	// happen in real MySQL; database/sql must reject COMMIT for a canceled context.
	callback := "test:upgrade_cancel_before_commit"
	if err := db.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "incident_problem" {
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Create().Remove(callback)
	now := time.Now().UTC().Truncate(time.Millisecond)
	n, err := db.RetireLegacyApprovals(ctx, now)
	if n != 1 || (!errors.Is(err, context.Canceled) && !errors.Is(err, sql.ErrTxDone)) {
		t.Fatalf("commit failure counted uncommitted row: n=%d err=%v", n, err)
	}
	assertUpgradeRetired(t, db, first, now)
	assertUpgradeUnchanged(t, db, row)
	assertUpgradeCount(t, db, "incident_event", "approval_id = ?", row.ID, 0)
	assertUpgradeCount(t, db, "incident_problem", "incident_id = ?", row.IncidentID, 0)
}

func TestExecutionUpgradeConcurrentRetirement(t *testing.T) {
	db := openIntegrationDB(t)
	t.Cleanup(func() { db.Close() })
	row := upgradeApproval(t, db, "executing", false)
	now := time.Now().UTC().Truncate(time.Millisecond)
	// Force both commands to see the same candidate before either locks it.
	var scanned sync.WaitGroup
	scanned.Add(2)
	callback := "test:upgrade_candidate_barrier"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]Approval); ok {
			scanned.Done()
			scanned.Wait()
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Query().Remove(callback)
	start := make(chan struct{})
	counts := make(chan int, 2)
	errors := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			n, err := db.RetireLegacyApprovals(context.Background(), now)
			counts <- n
			errors <- err
		}()
	}
	close(start)
	wg.Wait()
	for i := 0; i < 2; i++ {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if n := <-counts + <-counts; n != 1 {
		t.Fatalf("concurrent committed count = %d, want 1", n)
	}
	assertUpgradeRetired(t, db, row, now)
}

func TestExecutionUpgradeMigrations(t *testing.T) {
	mode := os.Getenv("TEST_EXECUTION_UPGRADE_MODE")
	if mode == "" {
		t.Skip("explicit empty/legacy mode required; run tests/migrations/execution-upgrade.sh")
	}
	if mode != "empty" && mode != "legacy" {
		t.Fatalf("unknown migration exercise mode %q", mode)
	}
	if os.Getenv("TEST_MYSQL_DSN") == "" {
		t.Fatal("TEST_MYSQL_DSN required for explicit migration exercise")
	}
	cfg, err := mysqlDriver.ParseDSN(os.Getenv("TEST_MYSQL_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MultiStatements = true
	db, err := Open(cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var count int64
	if err := db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("migration exercise requires a NEW EMPTY disposable database; refusing to modify existing schema")
	}
	t.Logf("migration exercise mode=%s database=%s", mode, cfg.DBName)
	ctx := context.Background()
	if err := db.CheckExecutionReady(ctx); err == nil {
		t.Fatal("empty database passed preflight")
	}
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) != 15 {
		t.Fatalf("expected migrations 001–015: %v %v", files, err)
	}
	var history []Approval
	for i, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(string(content)).Error; err != nil {
			t.Fatalf("apply %s: %v", path, err)
		}
		t.Logf("applied %s", filepath.Base(path))
		if i == 7 {
			if mode == "legacy" {
				for _, status := range []string{"pending", "approved", "executing", "executed", "failed", "denied", "expired"} {
					history = append(history, upgradeApproval(t, db, status, false))
				}
			}
			upgradeCommand(t, false, "-check")
			upgradeCommand(t, false) // No flag cannot mutate, even with a valid DSN.
			upgradeCommand(t, false, "-apply", "-check")
			for _, row := range history {
				assertUpgradeUnchanged(t, db, row)
			}
			want := 0
			if mode == "legacy" {
				want = 3
			}
			out := upgradeCommand(t, true, "-apply")
			if !strings.Contains(out, fmt.Sprintf("retired %d legacy", want)) {
				t.Fatalf("unexpected command result: %s", out)
			}
			out = upgradeCommand(t, true, "-apply")
			if !strings.Contains(out, "retired 0 legacy") {
				t.Fatalf("repeat command changed rows: %s", out)
			}
		}
		if i >= 7 && i < len(files)-1 {
			if err := db.CheckExecutionReady(ctx); err == nil {
				t.Fatalf("schema through %s passed incomplete preflight", path)
			} else {
				t.Logf("incomplete schema correctly refused: %v", err)
			}
		}
	}
	upgradeCommand(t, true, "-check")
	for i, row := range history {
		if i < 3 {
			var event IncidentEvent
			if err := db.Where("approval_id = ?", row.ID).First(&event).Error; err != nil {
				t.Fatal(err)
			}
			assertUpgradeRetired(t, db, row, event.CreatedAt)
			t.Logf("legacy approval=%d %s→%s event=%s; original hash/context retained", row.ID, row.Status, event.Status, event.EventType)
		} else {
			assertUpgradeUnchanged(t, db, row)
		}
		assertUpgradeCount(t, db, "verify_task", "approval_id = ?", row.ID, 0)
		assertUpgradeCount(t, db, "fault_cmd_history", "approval_id = ?", row.ID, 0)
	}
	// This also exercises forgotten pre-009 retirement after the schema is already
	// upgraded: -check refuses without mutation and -apply touches only SQL NULL.
	legacy := upgradeApproval(t, db, "approved", false)
	modern := upgradeApproval(t, db, "approved", true)
	upgradeCommand(t, false, "-check")
	assertUpgradeUnchanged(t, db, legacy)
	assertUpgradeUnchanged(t, db, modern)
	if out := upgradeCommand(t, true, "-apply"); !strings.Contains(out, "retired 1 legacy") {
		t.Fatalf("post-009 retirement: %s", out)
	}
	assertUpgradeUnchanged(t, db, modern)
	upgradeCommand(t, true, "-check")
	t.Log("PASS: sequential schema, explicit retirement, terminal history, no verification/action backfill, unsafe startup refusal and idempotence")
}

func upgradeCommand(t *testing.T, success bool, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"run", "./cmd/retire-approvals"}, args...)...)
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), "MYSQL_DSN="+os.Getenv("TEST_MYSQL_DSN"))
	out, err := cmd.CombinedOutput()
	t.Logf("retire-approvals %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	if (err == nil) != success {
		t.Fatalf("command success=%v, expected %v: %v\n%s", err == nil, success, err, out)
	}
	return string(out)
}

func upgradeApproval(t *testing.T, db *DB, status string, modern bool) Approval {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	incident := insertTestIncident(t, db, now, status)
	run := AgentRun{IncidentID: incident.ID, Mode: "full", Status: "succeeded", StartedAt: now, FinishedAt: &now}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	actor, reason, source := "legacy-operator", "original decision", "web"
	result := datatypes.JSON(`{"original":"must survive unless executing"}`)
	row := Approval{IncidentID: incident.ID, RunID: run.ID, ToolName: "docker_restart", ArgsJSON: datatypes.JSON(`{"target_name":"sub2api"}`), Reason: "original reason", PlanHash: strings.Repeat("a", 64), Status: status, ExpiresAt: now.Add(time.Hour), DecidedBy: &actor, DecidedAt: &now, DecisionReason: &reason, DecisionSource: &source, ResultJSON: &result, CreatedAt: now}
	// Works before 009 and 014 too: columns those migrations add are omitted.
	query := db.Omit("ExecutionContext", "Verification", "Service", "RuleID", "ParentApprovalID", "OperationID", "OperationStartedAt")
	if modern {
		row.ExecutionContext = datatypes.JSON(`{"version":2,"safety_level":"L2","dry_run":true,"verification":{"kind":"sub2api_http_health","target_name":"sub2api","base_url":"http://127.0.0.1:8080","member_fingerprints":["fixture"],"interval_seconds":10,"window_seconds":120,"timeout_seconds":5,"required_passes":1}}`)
		query = db.Omit("Verification", "Service", "RuleID", "ParentApprovalID", "OperationID", "OperationStartedAt")
	}
	if err := query.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Where("id = ?", row.ID).Delete(&Approval{})
		db.Where("id = ?", run.ID).Delete(&AgentRun{})
	})
	if err := db.First(&row, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func assertUpgradeUnchanged(t *testing.T, db *DB, before Approval) {
	t.Helper()
	var after Approval
	if err := db.First(&after, before.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("approval %d unexpectedly changed:\nbefore=%+v\nafter=%+v", before.ID, before, after)
	}
}

func assertUpgradeRetired(t *testing.T, db *DB, before Approval, now time.Time) {
	t.Helper()
	var after Approval
	if err := db.First(&after, before.ID).Error; err != nil {
		t.Fatal(err)
	}
	wantStatus, wantType := "expired", "approval.expired"
	if before.Status == "executing" {
		wantStatus, wantType = "failed", "execution.failed"
		var result struct {
			ManualCheck bool   `json:"manual_check"`
			Error       string `json:"error"`
		}
		if after.ResultJSON == nil || json.Unmarshal(*after.ResultJSON, &result) != nil || !result.ManualCheck || !strings.Contains(result.Error, "outcome unknown") {
			t.Fatalf("missing unknown-outcome result: %s", after.ResultJSON)
		}
		var problem IncidentProblem
		if err := db.Where("incident_id = ? AND code = 'manual_check'", before.IncidentID).First(&problem).Error; err != nil {
			t.Fatal(err)
		}
		if problem.Status != "open" || problem.Severity != "critical" || problem.RunID == nil || *problem.RunID != before.RunID || !problem.LastSeenAt.Equal(now) || !strings.Contains(problem.Summary, "outcome unknown") {
			t.Fatalf("incorrect manual problem: %+v", problem)
		}
		assertUpgradeCount(t, db, "incident_problem", "incident_id = ?", before.IncidentID, 1)
		after.ResultJSON = before.ResultJSON
	} else {
		assertUpgradeCount(t, db, "incident_problem", "incident_id = ?", before.IncidentID, 0)
	}
	if after.Status != wantStatus {
		t.Fatalf("approval %d status=%s want %s", before.ID, after.Status, wantStatus)
	}
	after.Status = before.Status
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("retirement overwrote immutable history for approval %d:\nbefore=%+v\nafter=%+v", before.ID, before, after)
	}
	var events []IncidentEvent
	if err := db.Where("approval_id = ?", before.ID).Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventType != wantType || events[0].Status != wantStatus || events[0].RunID == nil || *events[0].RunID != before.RunID || !events[0].CreatedAt.Equal(now) || !strings.Contains(events[0].Summary, "upgrade") {
		t.Fatalf("incorrect retirement audit: %+v", events)
	}
}

func assertUpgradeCount(t *testing.T, db *DB, table, where string, id uint64, want int64) {
	t.Helper()
	var count int64
	if err := db.Table(table).Where(where, id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("%s count for %s %d = %d, want %d", table, where, id, count, want)
	}
}
