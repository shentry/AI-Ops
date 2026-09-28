package metrics

import (
	"strings"
	"testing"
)

// A counter that first appears at 1 hides that increment from increase();
// every declared counter must be exported as 0 before its first event.
func TestCountersExportedBeforeFirstIncrement(t *testing.T) {
	var out strings.Builder
	Write(&out)
	for _, line := range []string{"ai_opus_verify_failed 0\n", "ai_opus_notification_failed 0\n", "ai_opus_agent_run_succeeded 0\n"} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("missing %q in:\n%s", line, out.String())
		}
	}
}
