package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"oncall-agent/internal/approval"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/notify/feishu"
	"oncall-agent/internal/store"
)

// These tests deliberately use another schema, not TEST_MYSQL_DSN: store's
// FIFO/global-queue tests may run concurrently in a different Go package.
// Apply migrations 001–011 to a disposable oncall_api* database first.
func openAPIApprovalMySQL(t *testing.T) *store.DB {
	t.Helper()
	dsn := os.Getenv("TEST_API_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_API_MYSQL_DSN is not set; requires a dedicated migrated oncall_api* MySQL database")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	var database string
	if err := db.Raw("SELECT DATABASE()").Scan(&database).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(database, "oncall_api") {
		t.Fatalf("refusing non-API database %q; use a dedicated oncall_api* schema", database)
	}
	db.DB = db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	return db
}

type apiMySQLPatch struct{ messageID, content string }
type apiMySQLMessageClient struct{ patches chan apiMySQLPatch }

func (c apiMySQLMessageClient) Reply(context.Context, string, string) (notify.Delivery, error) {
	return notify.Delivery{}, fmt.Errorf("unexpected reply in approval-only test")
}
func (c apiMySQLMessageClient) Patch(_ context.Context, id, content string) error {
	c.patches <- apiMySQLPatch{id, content}
	return nil
}

type apiMySQLApproval struct {
	db       *store.DB
	row      store.Approval
	web      *httptest.Server
	business *feishu.CallbackBusiness
	patches  chan apiMySQLPatch
	prefix   string
	session  *http.Cookie
}

func newAPIApprovalMySQL(t *testing.T) *apiMySQLApproval {
	t.Helper()
	db := openAPIApprovalMySQL(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	parent := store.Incident{
		GroupKey: fmt.Sprintf("api-t3-%s-%d", t.Name(), time.Now().UnixNano()),
		Status:   "firing", Severity: 5, AlertsCount: 1, Title: "isolated approval integration",
		StartedAt: now, LastSeenAt: now,
	}
	if err := db.Create(&parent).Error; err != nil {
		t.Fatal(err)
	}
	fp := fmt.Sprintf("%x", sha256.Sum256([]byte(parent.GroupKey)))
	prefix := fmt.Sprintf("api-t3-%d-", parent.ID)
	// Every cleanup predicate is fixture-scoped, including callback receipts.
	// Register before further inserts so a failed setup also cleans its rows.
	t.Cleanup(func() {
		for _, deletion := range []struct {
			query string
			arg   any
		}{
			{"DELETE FROM integration_event_receipt WHERE event_id LIKE ?", prefix + "%"},
			{"DELETE FROM im_binding WHERE incident_id = ?", parent.ID},
			{"DELETE FROM incident_event WHERE incident_id = ?", parent.ID},
			{"DELETE FROM incident_problem WHERE incident_id = ?", parent.ID},
			{"DELETE FROM approval WHERE incident_id = ?", parent.ID},
			{"DELETE FROM agent_run_step WHERE run_id IN (SELECT id FROM agent_run WHERE incident_id = ?)", parent.ID},
			{"DELETE FROM agent_run WHERE incident_id = ?", parent.ID},
			{"DELETE FROM incident_alert WHERE incident_id = ?", parent.ID},
			{"DELETE FROM last_alert WHERE fingerprint = ?", fp},
			{"DELETE FROM alert WHERE fingerprint = ?", fp},
			{"DELETE FROM incident WHERE id = ?", parent.ID},
		} {
			if err := db.Exec(deletion.query, deletion.arg).Error; err != nil {
				t.Errorf("fixture cleanup %s: %v", deletion.query, err)
			}
		}
	})
	alert := store.Alert{
		Fingerprint: fp, AlertHash: fp[:32], Source: "alertmanager", Name: "Sub2APIDown",
		Status: "firing", Severity: 5, Labels: datatypes.JSON(`{"service":"sub2api","container":"sub2api"}`),
		Annotations: datatypes.JSON(`{}`), StartsAt: now, ReceivedAt: now,
	}
	if err := db.Create(&alert).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&store.LastAlert{Fingerprint: fp, AlertID: alert.ID, AlertHash: alert.AlertHash,
		Status: "firing", Severity: 5, FirstSeen: now, LastSeen: now, IncidentID: &parent.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&store.IncidentAlert{IncidentID: parent.ID, Fingerprint: fp, LinkedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	run, created, err := db.RequestRun(ctx, store.RunRequest{IncidentID: parent.ID, Mode: "full", Trigger: store.RunTriggerAlert, RequestedAt: now})
	if err != nil || !created {
		t.Fatalf("RequestRun: created=%v err=%v", created, err)
	}
	snapshot := apiMySQLJSON(t, incident.ExecutionContext{
		Version: incident.ExecutionContextVersion, Kind: incident.KindPrimary, Service: "sub2api",
		Rule:          incident.RuleRef{ID: "restart", Version: "r1@000000000000", Mode: incident.ModeManual, Alerts: []string{"Sub2APIDown"}},
		ActionVersion: 2, Target: incident.Object{Kind: "container", Name: "sub2api", ID: "c0ffee"}, Revision: "started_at=x", PreState: json.RawMessage(`{}`),
		Members: []string{fp}, FaultAlert: "Sub2APIDown", ExpiresAt: now.Add(30 * time.Minute),
		Verification: incident.VerificationSpec{Checks: []incident.Check{{Kind: incident.CheckHealth, Params: json.RawMessage(`{"base_url":"http://private-target.invalid:8080"}`)}},
			IntervalSeconds: 10, WindowSeconds: 120, TimeoutSeconds: 5, RequiredPasses: 1},
	})
	args := datatypes.JSON(`{"target_kind":"container","target_name":"sub2api"}`)
	hash, err := incident.PlanHash("docker_restart", args, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	// Both actual handlers receive this exact *Service and *store.DB. Only
	// outbound Feishu message delivery is replaced; no executor is started.
	svc := approval.NewService(db)
	draft, err := svc.Prepare(parent.ID, run.ID, approval.Decision{Kind: approval.DecisionApproval,
		ToolName: "docker_restart", Args: json.RawMessage(args), ExecutionContext: snapshot, PlanHash: hash}, "server-authored approval reason")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteRun(ctx, store.RunCompletion{RunID: run.ID, Status: "succeeded", RCA: "supported outage",
		PlanJSON: []byte(`{"action":"docker_restart","confidence":"high"}`), FinishedAt: now, Approval: &draft}); err != nil {
		t.Fatal(err)
	}
	if draft.ID == 0 {
		t.Fatal("CompleteRun did not publish an approval")
	}
	if _, err := db.CreateIMBinding(ctx, store.IMBinding{Provider: "feishu_app", ChatID: "oc_api_test",
		MessageID: prefix + "card", IncidentID: parent.ID, RunID: &run.ID, ApprovalID: &draft.ID,
		MessageKind: "approval", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	patches := make(chan apiMySQLPatch, 4)
	business := feishu.NewCallbackBusiness(feishu.BusinessDependencies{
		Store: db, Approval: svc, Client: apiMySQLMessageClient{patches},
		ChatID: "oc_api_test", OperatorAllowlist: []string{"ou_api_test"},
	})
	auth := testAuth(t)
	web := httptest.NewServer(NewApprovalAPI(svc, auth))
	web.Client().Timeout = 10 * time.Second
	t.Cleanup(web.Close)
	f := &apiMySQLApproval{db: db, row: draft, web: web, business: business, patches: patches, prefix: prefix, session: login(t, auth, testOperatorToken)}
	f.row = f.persisted(t) // MySQL JSON and DATETIME precision, not in-memory draft values.
	return f
}

func apiMySQLJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type apiMySQLHTTPResult struct {
	code int
	body []byte
	err  error
}

func (f *apiMySQLApproval) request(method, action, body, token string) apiMySQLHTTPResult {
	url := fmt.Sprintf("%s/api/v1/approvals/%d", f.web.URL, f.row.ID)
	if action != "" {
		url += "/" + action
	}
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return apiMySQLHTTPResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Operator", "must-not-override-identity")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		// The console path: the operator's session cookie plus the CSRF header.
		req.AddCookie(f.session)
		req.Header.Set(csrfHeader, csrfValue)
	}
	resp, err := f.web.Client().Do(req)
	if err != nil {
		return apiMySQLHTTPResult{err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return apiMySQLHTTPResult{code: resp.StatusCode, body: raw, err: err}
}

func (f *apiMySQLApproval) card(suffix, action, hash string) *callback.CardActionTriggerEvent {
	return &callback.CardActionTriggerEvent{
		EventV2Base: &larkevent.EventV2Base{Header: &larkevent.EventHeader{EventID: f.prefix + suffix, EventType: "card.action.trigger"}},
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_api_test"},
			Action:   &callback.CallBackAction{Value: map[string]any{"action": action, "approval_id": f.row.ID, "plan_hash": hash}},
			Context:  &callback.Context{OpenChatID: "oc_api_test", OpenMessageID: f.prefix + "card"},
		},
	}
}

func (f *apiMySQLApproval) persisted(t *testing.T) store.Approval {
	t.Helper()
	row, err := f.db.GetApproval(context.Background(), f.row.ID)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (f *apiMySQLApproval) decisionEvents(t *testing.T) []store.IncidentEvent {
	t.Helper()
	var events []store.IncidentEvent
	if err := f.db.Where("approval_id = ? AND event_type IN ?", f.row.ID,
		[]string{"approval.approved", "approval.denied"}).Order("id").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	return events
}

func (f *apiMySQLApproval) assertUnchanged(t *testing.T, before store.Approval, events int) {
	t.Helper()
	if after := f.persisted(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("decision/content was rewritten:\nbefore=%+v\nafter=%+v", before, after)
	}
	if got := f.decisionEvents(t); len(got) != events {
		t.Fatalf("durable decision events=%+v, want %d", got, events)
	}
}

func (f *apiMySQLApproval) projection(t *testing.T, status string) {
	t.Helper()
	resp := f.request(http.MethodGet, "", "", "")
	var got ApprovalDTO
	if resp.err != nil || resp.code != http.StatusOK || json.Unmarshal(resp.body, &got) != nil {
		t.Fatalf("GET projection: %+v body=%s", resp, resp.body)
	}
	if got.ID != f.row.ID || got.Status != status || got.PlanHash != f.row.PlanHash || got.ToolName != "docker_restart" ||
		got.Target != "container/sub2api" || got.TargetID != "c0ffee" || got.RuleID != "restart" || got.Mode != incident.ModeManual ||
		got.Reason != f.row.Reason || !got.ExpiresAt.Equal(f.row.ExpiresAt) {
		t.Fatalf("fresh projection lost server snapshot: %+v", got)
	}
	for _, forbidden := range []string{"private-target.invalid", "base_url", "execution_context", "args_json", `"risk"`} {
		if strings.Contains(string(resp.body), forbidden) {
			t.Fatalf("projection leaks %s: %s", forbidden, resp.body)
		}
	}
}

func TestApprovalMySQLWebFeishuRace(t *testing.T) {
	for round := 0; round < 12; round++ {
		t.Run(fmt.Sprintf("round_%02d", round), func(t *testing.T) {
			f := newAPIApprovalMySQL(t)
			f.projection(t, "pending")
			webAction, cardAction := "approve", "deny"
			if round%2 == 1 {
				webAction, cardAction = cardAction, webAction
			}
			// Rendezvous immediately before the real SELECT ... FOR UPDATE.
			// This observes GORM's query boundary, without replacing a query,
			// result, transaction or decision store. It proves BOTH handlers have
			// entered Service.Decide (Feishu's preliminary Get saw pending), so
			// a lucky sequential precheck cannot masquerade as a CAS race.
			entered, release := make(chan struct{}, 2), make(chan struct{})
			if err := f.db.Callback().Query().Before("gorm:query").Register("api_t3_decision_barrier", func(tx *gorm.DB) {
				lock, ok := tx.Statement.Clauses["FOR"].Expression.(clause.Locking)
				if tx.Statement.Table == "approval" && ok && lock.Strength == "UPDATE" {
					entered <- struct{}{}
					<-release
				}
			}); err != nil {
				t.Fatal(err)
			}
			webDone := make(chan apiMySQLHTTPResult, 1)
			type cardResult struct {
				response *callback.CardActionTriggerResponse
				err      error
			}
			cardDone := make(chan cardResult, 1)
			go func() {
				webDone <- f.request(http.MethodPost, webAction, fmt.Sprintf(`{"plan_hash":%q,"reason":"web race reason"}`, f.row.PlanHash), "")
			}()
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				resp, err := f.business.CardAction(ctx, f.card("race", cardAction, f.row.PlanHash))
				cardDone <- cardResult{resp, err}
			}()
			arrivals := 0
			timer := time.NewTimer(5 * time.Second)
		wait:
			for arrivals < 2 {
				select {
				case <-entered:
					arrivals++
				case result := <-cardDone:
					cardDone <- result // Preserve early failure for the diagnostic below.
					break wait
				case <-timer.C:
					break wait
				}
			}
			timer.Stop()
			close(release)
			web, card := <-webDone, <-cardDone
			if err := f.db.Callback().Query().Remove("api_t3_decision_barrier"); err != nil {
				t.Fatal(err)
			}
			if arrivals != 2 || web.err != nil || card.err != nil || card.response == nil || card.response.Toast == nil {
				t.Fatalf("shared decision rendezvous=%d web=%d err=%v body=%s card=%+v err=%v", arrivals, web.code, web.err, web.body, card.response, card.err)
			}
			row := f.persisted(t)
			if row.ToolName != f.row.ToolName || row.PlanHash != f.row.PlanHash || string(row.ArgsJSON) != string(f.row.ArgsJSON) || string(row.ExecutionContext) != string(f.row.ExecutionContext) {
				t.Fatalf("decision rewrote immutable server content: %+v", row)
			}
			status := map[string]string{"approve": "approved", "deny": "denied"}[webAction]
			actor, source, reason := "ops", "web", "web race reason"
			if web.code == http.StatusConflict {
				status, actor, source, reason = "approved", "feishu:ou_api_test", "feishu", ""
				if cardAction == "deny" {
					status = "denied"
				}
				wantToast := map[string]string{"approved": "审批已批准", "denied": "审批已拒绝"}[status]
				if card.response.Toast.Type != "success" || card.response.Toast.Content != wantToast {
					t.Fatalf("winning Feishu response=%+v", card.response.Toast)
				}
				select {
				case patch := <-f.patches:
					if patch.messageID != f.prefix+"card" || !strings.Contains(patch.content, "container/sub2api") ||
						!strings.Contains(patch.content, "**Mode:** manual") || strings.Contains(patch.content, "private-target.invalid") || strings.Contains(patch.content, `"type":"callback"`) {
						t.Fatalf("decision patch lost snapshot/leaked actions: %+v", patch)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("mock outbound patch not received")
				}
			} else if web.code != http.StatusOK || card.response.Toast.Type != "error" {
				t.Fatalf("exactly one winner required: web=%d %s card=%+v", web.code, web.body, card.response.Toast)
			}
			if row.Status != status || row.DecidedBy == nil || *row.DecidedBy != actor || row.DecisionSource == nil || *row.DecisionSource != source ||
				row.DecidedAt == nil || row.DecidedAt.Before(row.CreatedAt) || !row.DecidedAt.Before(row.ExpiresAt) ||
				(reason == "" && row.DecisionReason != nil) || (reason != "" && (row.DecisionReason == nil || *row.DecisionReason != reason)) {
				t.Fatalf("persisted winner metadata=%+v; want %s/%s/%s/%q", row, status, actor, source, reason)
			}
			events := f.decisionEvents(t)
			if len(events) != 1 || events[0].EventType != "approval."+status || events[0].Status != status || events[0].IncidentID != row.IncidentID ||
				events[0].RunID == nil || *events[0].RunID != row.RunID || !events[0].CreatedAt.Equal(*row.DecidedAt) {
				t.Fatalf("durable decision event does not match winner: %+v", events)
			}
			var receipt store.IntegrationEventReceipt
			if err := f.db.First(&receipt, "event_id = ?", f.prefix+"race").Error; err != nil {
				t.Fatal(err)
			}
			wantReceipt := "conflict"
			if source == "feishu" {
				wantReceipt = status
			}
			if receipt.Result != wantReceipt {
				t.Fatalf("durable Feishu receipt=%+v, want %s", receipt, wantReceipt)
			}
			// A new callback event ID bypasses receipt deduplication: the row
			// lock/state check, not redelivery dedupe, must preserve the winner.
			if retry := f.request(http.MethodPost, webAction, fmt.Sprintf(`{"plan_hash":%q,"reason":"rewrite attempt"}`, row.PlanHash), ""); retry.err != nil || retry.code != http.StatusConflict {
				t.Fatalf("Web replay=%+v", retry)
			}
			if resp, err := f.business.CardAction(context.Background(), f.card("fresh-replay", cardAction, row.PlanHash)); err != nil || resp == nil || resp.Toast == nil || resp.Toast.Content != "该审批已处理" {
				t.Fatalf("fresh callback replay=%+v err=%v", resp, err)
			}
			f.assertUnchanged(t, row, 1)
			f.projection(t, status)
			t.Logf("approval=%d both decision transactions entered; winner=%s/%s; immutable metadata; decision_events=1", row.ID, source, status)
		})
	}
}

func TestApprovalMySQLChangedContentFailsClosed(t *testing.T) {
	for _, change := range []string{"target", "mode", "verification_check"} {
		for _, rehash := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rehash_%t", change, rehash), func(t *testing.T) {
				f := newAPIApprovalMySQL(t)
				snapshot, err := incident.ParseExecutionContext(f.row.ExecutionContext)
				if err != nil {
					t.Fatal(err)
				}
				args := f.row.ArgsJSON
				switch change {
				case "target":
					args = datatypes.JSON(`{"target_kind":"container","target_name":"other-sub2api"}`)
					snapshot.Target.Name = "other-sub2api" // Keep shape valid: reject hash drift, not a malformed target.
				case "mode":
					snapshot.Rule.Mode = incident.ModeAuto
				case "verification_check":
					snapshot.Verification.Checks[0].Params = json.RawMessage(`{"base_url":"http://other-private-target.invalid:8080"}`)
				}
				content := apiMySQLJSON(t, snapshot)
				changedHash, err := incident.PlanHash(f.row.ToolName, args, content)
				if err != nil || changedHash == f.row.PlanHash {
					t.Fatalf("changed content must have a different valid hash: %s %v", changedHash, err)
				}
				updates := map[string]any{"args_json": args, "execution_context": datatypes.JSON(content)}
				if rehash {
					updates["plan_hash"] = changedHash
				}
				// Fault injection ONLY into this fixture, never a supported edit
				// path. Test both corrupt content with the stored hash untouched,
				// and an old client reference after a different consistent snapshot.
				if result := f.db.Model(&store.Approval{}).Where("id = ?", f.row.ID).Updates(updates); result.Error != nil || result.RowsAffected != 1 {
					t.Fatalf("inject snapshot change: rows=%d err=%v", result.RowsAffected, result.Error)
				}
				before := f.persisted(t)
				for _, action := range []string{"approve", "deny"} {
					resp := f.request(http.MethodPost, action, fmt.Sprintf(`{"plan_hash":%q,"reason":"stale reference"}`, f.row.PlanHash), "")
					if resp.err != nil || resp.code != http.StatusConflict {
						t.Fatalf("old Web hash accepted: %+v body=%s", resp, resp.body)
					}
					card, err := f.business.CardAction(context.Background(), f.card(action, action, f.row.PlanHash))
					if err != nil || card == nil || card.Toast == nil || card.Toast.Type != "error" {
						t.Fatalf("expected Feishu hash-conflict feedback: %+v err=%v", card, err)
					}
					f.assertUnchanged(t, before, 0)
				}
				if !rehash {
					// Neither the old stored hash nor the new computed hash may
					// authorize inconsistent immutable content.
					resp := f.request(http.MethodPost, "approve", fmt.Sprintf(`{"plan_hash":%q,"reason":"computed hash"}`, changedHash), "")
					if resp.err != nil || resp.code != http.StatusConflict {
						t.Fatalf("inconsistent stored hash accepted: %+v", resp)
					}
					projection := f.request(http.MethodGet, "", "", "")
					var dto ApprovalDTO
					if projection.err != nil || projection.code != http.StatusOK || json.Unmarshal(projection.body, &dto) != nil || dto.Mode != "" || dto.Target != "" {
						t.Fatalf("corrupt snapshot projected as actionable: %+v body=%s", projection, projection.body)
					}
					f.assertUnchanged(t, before, 0)
				}
			})
		}
	}
}

func TestApprovalMySQLRequiredHashAndCanonicalProjection(t *testing.T) {
	f := newAPIApprovalMySQL(t)
	for _, token := range []string{"", testOperatorToken} {
		for _, action := range []string{"approve", "deny"} {
			for _, body := range []string{`{"reason":"no hash"}`, `{"plan_hash":"","reason":"empty hash"}`, `{"plan_hash":"   ","reason":"blank hash"}`} {
				resp := f.request(http.MethodPost, action, body, token)
				if resp.err != nil || resp.code != http.StatusBadRequest {
					t.Fatalf("missing Web/automation hash accepted: %+v", resp)
				}
				f.assertUnchanged(t, f.row, 0)
			}
		}
	}
	// MySQL normalizes JSON on storage. Explicitly reverse object-key order
	// and change whitespace, then exercise the original reference end-to-end.
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(f.row.ExecutionContext, &snapshot); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(snapshot))
	for key := range snapshot {
		keys = append(keys, key)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	var reorderedText strings.Builder
	reorderedText.WriteString("{\n")
	for i, key := range keys {
		if i > 0 {
			reorderedText.WriteString(",\n  ")
		}
		fmt.Fprintf(&reorderedText, "%q : %s", key, snapshot[key])
	}
	reorderedText.WriteString("\n}")
	reordered := []byte(reorderedText.String())
	args := []byte("{\n \"target_name\":\"sub2api\", \"target_kind\":\"container\"\n}")
	hash, err := incident.PlanHash(f.row.ToolName, args, reordered)
	if err != nil || hash != f.row.PlanHash {
		t.Fatalf("key/whitespace changed immutable hash: %s %v", hash, err)
	}
	if err := f.db.Model(&store.Approval{}).Where("id = ?", f.row.ID).Updates(map[string]any{
		"args_json": datatypes.JSON(args), "execution_context": datatypes.JSON(reordered),
	}).Error; err != nil {
		t.Fatal(err)
	}
	f.assertUnchanged(t, f.row, 0)
	f.projection(t, "pending")
	resp := f.request(http.MethodPost, "approve", fmt.Sprintf(`{"plan_hash":%q,"reason":"canonical snapshot"}`, hash), "")
	if resp.err != nil || resp.code != http.StatusOK {
		t.Fatalf("canonical snapshot rejected: %+v body=%s", resp, resp.body)
	}
	row := f.persisted(t)
	if row.Status != "approved" || row.DecidedBy == nil || *row.DecidedBy != "ops" || row.DecisionSource == nil || *row.DecisionSource != "web" ||
		row.DecisionReason == nil || *row.DecisionReason != "canonical snapshot" || row.DecidedAt == nil || len(f.decisionEvents(t)) != 1 {
		t.Fatalf("canonical decision not durable: %+v", row)
	}
	f.projection(t, "approved")
}
