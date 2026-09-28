package diagnose

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"oncall-agent/internal/incident"
	"oncall-agent/internal/sub2api"
	"oncall-agent/internal/tools"
)

type bindingAccountReader struct {
	fakeAccounts
	reads int
}

func (r *bindingAccountReader) Account(ctx context.Context, id int64) (sub2api.Account, error) {
	r.reads++
	return r.fakeAccounts.Account(ctx, id)
}

func (r *bindingAccountReader) Availability(ctx context.Context) (sub2api.Availability, error) {
	r.reads++
	return r.fakeAccounts.Availability(ctx)
}

func TestVerificationBindingMismatchNeverReadsCurrentService(t *testing.T) {
	for _, phase := range []string{"verify", "watch"} {
		t.Run(phase, func(t *testing.T) {
			for name, mutate := range map[string]func(*incident.ExecutionBinding){
				"unchanged":                     func(*incident.ExecutionBinding) {},
				"missing binding":               func(b *incident.ExecutionBinding) { *b = incident.ExecutionBinding{} },
				"service changed":               func(b *incident.ExecutionBinding) { b.Service = "replacement" },
				"configuration release changed": func(b *incident.ExecutionBinding) { b.RulesVersion = "r1@111111111111" },
				"action removed":                func(b *incident.ExecutionBinding) { delete(b.Actions, tools.ActionDockerRestart) },
				"action version changed":        func(b *incident.ExecutionBinding) { b.Actions[tools.ActionDockerRestart]++ },
				"rule removed":                  func(b *incident.ExecutionBinding) { delete(b.Rules, "restart") },
				"auto authorization revoked": func(b *incident.ExecutionBinding) {
					rule := b.Rules["restart"]
					rule.Mode = incident.ModeManual
					b.Rules["restart"] = rule
				},
			} {
				t.Run(name, func(t *testing.T) {
					var requests atomic.Int32
					// One server answers both /health and the business probe, which needs real model output.
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						requests.Add(1)
						_, _ = w.Write([]byte(`{"content":[{"text":"ok"}]}`))
					}))
					defer server.Close()
					fixture, db, now, notifier := newVerificationFixture(t, server.URL)
					refreeze(t, db, server.URL, func(s *incident.ExecutionContext) {
						s.Verification.Checks = []incident.Check{
							checkOf(incident.CheckAccount, tools.AccountCheck{AccountID: 7, Schedulable: false}),
							checkOf(incident.CheckGroupAvailable, tools.GroupAvailableCheck{GroupIDs: []int64{2}, Min: 1}),
							checkOf(incident.CheckProbe, struct{}{}),
							checkOf(incident.CheckHealth, tools.HealthCheck{BaseURL: server.URL}),
						}
					})
					accounts := &bindingAccountReader{fakeAccounts: fakeAccounts{availability: sub2api.Availability{Enabled: true, Groups: map[string]sub2api.GroupAvailability{"2": {GroupID: 2, AvailableCount: 2}}}}}
					probe, err := sub2api.NewProbe(server.URL, "/v1/messages", "test-key", "test-model", time.Second)
					if err != nil {
						t.Fatal(err)
					}
					binding := fixture.binding
					mutate(&binding)
					worker := NewVerificationWorker(db, NewVerifier(tools.NewRegistry(), accounts, probe), 3600, notifier, fixture.logger, binding)
					worker.now = fixture.now
					db.task.Phase = phase
					last := now.Add(-time.Second)
					db.task.LastCheckedAt = &last
					if err := worker.RunOnce(context.Background()); err != nil {
						t.Fatal(err)
					}
					if len(db.completions) != 1 {
						t.Fatalf("completions=%d", len(db.completions))
					}
					completion := db.completions[0]
					if name == "unchanged" {
						if accounts.reads != 2 || requests.Load() != 2 || completion.Observation != "healthy" {
							t.Fatalf("valid binding did not exercise all checks: account reads=%d HTTP requests=%d completion=%+v", accounts.reads, requests.Load(), completion)
						}
						return
					}
					if accounts.reads != 0 || requests.Load() != 0 {
						t.Fatalf("revoked snapshot accessed current service: account reads=%d HTTP requests=%d", accounts.reads, requests.Load())
					}
					if completion.Status != "inconclusive" || completion.Observation != "unavailable" || completion.Detail == "" || completion.Memory != nil || completion.Retry || completion.DemoteFingerprint != "" || len(notifier.calls) != 0 {
						t.Fatalf("binding mismatch produced recovery effects: completion=%+v notifications=%v", completion, notifier.calls)
					}
					if err := worker.RunOnce(context.Background()); err != nil {
						t.Fatal(err)
					}
					if len(db.completions) != 1 || accounts.reads != 0 || requests.Load() != 0 {
						t.Fatal("terminal mismatch was probed again")
					}
				})
			}
		})
	}
}
