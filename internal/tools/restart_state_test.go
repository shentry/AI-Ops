package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"oncall-agent/internal/incident"
)

type restartStateTransport func(*http.Request) (*http.Response, error)

func (f restartStateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRestartCannotUndoManualStopAfterPreparation(t *testing.T) {
	for _, policy := range []string{"no", "always", "unless-stopped"} {
		t.Run(policy, func(t *testing.T) {
			for _, stopped := range []bool{false, true} {
				name := "still running"
				if stopped {
					name = "manually stopped"
				}
				t.Run(name, func(t *testing.T) {
					engine, docker := newFakeEngine(t)
					manualStop := false
					// Reuse the Engine fixture, changing only process state and restart
					// policy in inspect responses. A manual stop retains ID/StartedAt.
					docker.httpClient.Transport = restartStateTransport(func(r *http.Request) (*http.Response, error) {
						recorder := httptest.NewRecorder()
						engine.serve(recorder, r)
						response := recorder.Result()
						if r.Method == http.MethodGet && r.URL.Path == "/containers/sub2api/json" {
							var body map[string]any
							if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
								t.Fatal(err)
							}
							response.Body.Close()
							body["HostConfig"].(map[string]any)["RestartPolicy"].(map[string]any)["Name"] = policy
							if manualStop {
								state := body["State"].(map[string]any)
								state["Status"], state["Running"] = "exited", false
							}
							raw, err := json.Marshal(body)
							if err != nil {
								t.Fatal(err)
							}
							response.Body = io.NopCloser(strings.NewReader(string(raw)))
						}
						return response, nil
					})
					action := NewRestartAction(docker, testService())
					ctx := context.Background()
					prepared, err := action.Prepare(ctx, PrepareRequest{Target: incident.Object{Kind: "container", Name: "sub2api"}})
					if err != nil {
						t.Fatal(err)
					}
					manualStop = stopped
					live, err := docker.Inspect(ctx, "sub2api")
					if err != nil {
						t.Fatal(err)
					}
					if live.ID != prepared.Target.ID || startedRevision(live.StartedAt) != prepared.Revision || live.Running == stopped {
						t.Fatalf("fixture changed more than process state: prepared=%+v live=%+v", prepared, live)
					}
					receipt, err := action.Execute(ctx, operation(prepared))
					if err != nil {
						t.Fatal(err)
					}
					if stopped {
						if receipt.Written || engine.restarts != 0 || receipt.Detail == "" {
							t.Fatalf("manual stop undone: receipt=%+v restarts=%d", receipt, engine.restarts)
						}
					} else if !receipt.Written || engine.restarts != 1 {
						t.Fatalf("unchanged running container was not restarted: receipt=%+v restarts=%d", receipt, engine.restarts)
					}
				})
			}
		})
	}
}
