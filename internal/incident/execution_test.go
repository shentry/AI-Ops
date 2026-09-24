package incident_test

import (
	"strings"
	"testing"

	"oncall-agent/internal/incident"
)

const testExecutionContext = `{"safety_level":"L2","dry_run":false,"verification":{"kind":"sub2api_http_health","target_name":"sub2api","base_url":"http://127.0.0.1:8080","member_fingerprints":["fp-1"],"interval_seconds":10,"window_seconds":120,"timeout_seconds":5}}`

func TestExecutionSnapshotBindsModeAndVerification(t *testing.T) {
	args := []byte(`{"target_kind":"container","target_name":"sub2api"}`)
	hash, err := incident.PlanHash("docker_restart", args, []byte(testExecutionContext))
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := incident.PlanHash("docker_restart", []byte(`{ "target_name": "sub2api", "target_kind": "container" }`), []byte(testExecutionContext))
	if err != nil || ordered != hash {
		t.Fatalf("equivalent JSON changed hash: %q, %v", ordered, err)
	}
	for _, changed := range []string{
		strings.Replace(testExecutionContext, `"dry_run":false`, `"dry_run":true`, 1),
		strings.Replace(testExecutionContext, "127.0.0.1:8080", "127.0.0.1:8081", 1),
		strings.Replace(testExecutionContext, "fp-1", "fp-2", 1),
	} {
		other, err := incident.PlanHash("docker_restart", args, []byte(changed))
		if err != nil || other == hash {
			t.Fatalf("changed approval content must have a different hash: %q, %v", other, err)
		}
	}
}

func TestExecutionSnapshotRejectsIncompleteOrUnsupportedActions(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`,
		strings.Replace(testExecutionContext, `"dry_run":false,`, "", 1),
		strings.Replace(testExecutionContext, `"L2"`, `"L4"`, 1),
		strings.Replace(testExecutionContext, `"timeout_seconds":5`, `"timeout_seconds":10`, 1),
		strings.Replace(testExecutionContext, `http://127.0.0.1:8080`, `http://user:secret@localhost`, 1),
		testExecutionContext + `{}`,
	} {
		if _, err := incident.ParseExecutionContext([]byte(raw)); err == nil {
			t.Fatalf("invalid snapshot accepted: %s", raw)
		}
	}
	if _, err := incident.PlanHash("shell", []byte(`{"target_kind":"container","target_name":"sub2api"}`), []byte(testExecutionContext)); err == nil {
		t.Fatal("unsupported action accepted")
	}
	if _, err := incident.PlanHash("docker_restart", []byte(`{"target_kind":"container","target_name":"other"}`), []byte(testExecutionContext)); err == nil {
		t.Fatal("target not bound to verification accepted")
	}
}

func TestExecutionSnapshotRejectsConfigAndMemberDrift(t *testing.T) {
	snapshot, err := incident.ParseExecutionContext([]byte(testExecutionContext))
	if err != nil {
		t.Fatal(err)
	}
	binding := incident.ExecutionBinding{Container: "sub2api", BaseURL: "http://127.0.0.1:8080/", AllowedContainers: []string{"sub2api"}, SafetyLevel: "L2"}
	if err := snapshot.ValidateBinding(binding, true); err != nil {
		t.Fatal(err)
	}
	binding.DryRun = true
	if err := snapshot.ValidateBinding(binding, true); err == nil {
		t.Fatal("real action allowed after dry-run enabled")
	}
	if err := snapshot.ValidateBinding(binding, false); err != nil {
		t.Fatal("dry-run switch must not prevent read-only verification")
	}
	binding.BaseURL = "http://127.0.0.1:8081"
	if err := snapshot.ValidateBinding(binding, false); err == nil {
		t.Fatal("verification silently redirected to another host")
	}
	members := []incident.ExecutionMember{{Fingerprint: "fp-1", Name: "Sub2APIDown", Status: "firing", Container: "sub2api", Service: "sub2api"}}
	if err := snapshot.ValidateMembers(members, true); err != nil {
		t.Fatal(err)
	}
	members[0].Status = "resolved"
	if err := snapshot.ValidateMembers(members, true); err == nil {
		t.Fatal("resolved incident allowed to execute")
	}
	if err := snapshot.ValidateMembers(members, false); err != nil {
		t.Fatal("resolved notification should support verification")
	}
	members = append(members, incident.ExecutionMember{Fingerprint: "fp-2", Name: "Sub2APISlow", Status: "firing", Container: "sub2api", Service: "sub2api"})
	if err := snapshot.ValidateMembers(members, false); err == nil {
		t.Fatal("mixed failure accepted")
	}
}
