package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestReviewWait_FallbackReplacesRefusedSession covers issue #3112: a session
// that a fallback session replaced does not fail the round.
func TestReviewWait_FallbackReplacesRefusedSession(t *testing.T) {
	refused := "nixos-config@x~review-1-review-security"
	fallback := refused + "-fallback-1"
	members := map[string]db.GroupMemberResult{
		refused:  {SessionName: refused, State: "error", RunError: "refused", RunErrorRawStopReason: "refusal"},
		fallback: {SessionName: fallback, State: "finished", LastMessage: `{"text":"<verdict>PASS</verdict>"}`},
	}
	all := []db.Status{{SessionName: refused, State: "error"}, {SessionName: fallback, State: "finished"}}

	var err error
	out := captureStdout(t, func() {
		err = emitReviewWaitTerminalAgg("42", "g1", all, members, true)
	})
	if err != nil {
		t.Fatalf("emitReviewWaitTerminalAgg: %v", err)
	}
	var got reviewWaitJSON
	if jErr := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); jErr != nil {
		t.Fatalf("output is not JSON: %v\n%s", jErr, out)
	}
	if got.Verdict != "PASS" || len(got.Agents) != 1 || got.Agents[0].Session != fallback {
		t.Errorf("got verdict %q agents %+v, want PASS with only the fallback session", got.Verdict, got.Agents)
	}
}
