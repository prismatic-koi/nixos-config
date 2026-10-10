package cmd

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// Issue #3088: `prism review --wait` must show the run_error reason, as the
// review-complete report does.
func TestEmitReviewWaitTerminalAgg_RunErrorShowsStopReason(t *testing.T) {
	const sess = "nixos-config@feature~review-1-review-security"
	const reason = `the model ended its turn with no text and no tool call (stop reason "error", provider stop reason "refusal")`
	members := map[string]db.GroupMemberResult{
		sess: {SessionName: sess, State: "error", RunError: reason},
	}
	out := captureStdout(t, func() {
		_ = emitReviewWaitTerminalAgg("42", "g1", []db.Status{{SessionName: sess, State: "error"}}, members, false)
	})
	if !strings.Contains(out, "review-security") || !strings.Contains(out, `stop reason "error"`) {
		t.Errorf("output does not name the agent and the stop reason:\n%s", out)
	}
}
