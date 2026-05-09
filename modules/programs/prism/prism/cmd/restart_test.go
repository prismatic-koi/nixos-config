// Tests for the success-side summary line emitted by `prism restart`
// (issue #1527).
//
// runRestart itself calls syscall.Exec at the end of the success path, which
// replaces the test process and is therefore not directly unit-testable. We
// instead test the extracted emitRestartSummary helper, which is the same
// call that runRestart makes immediately before syscall.Exec. This is the
// last byte of the parent process's stdout; if it appears in the test's
// captured output, then in production it appears on the user's terminal
// before the re-exec boundary.

package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// TestEmitRestartSummary_WritesExactLine verifies that emitRestartSummary
// writes exactly the documented summary string followed by a newline. Tests
// the byte content rather than just substring presence so any future drift
// (e.g. punctuation change) is caught.
func TestEmitRestartSummary_WritesExactLine(t *testing.T) {
	var buf bytes.Buffer
	emitRestartSummary(&buf)

	got := buf.String()
	want := restartSummaryLine + "\n"
	if got != want {
		t.Errorf("emitRestartSummary output mismatch:\n  got:  %q\n  want: %q", got, want)
	}
}

// TestRestartSummaryLine_NamesKeyActions guards the user-facing wording so a
// future refactor doesn't accidentally drop the per-action breadcrumbs an
// agent or human relies on to confirm "what happened" before the re-exec.
//
// Per AC: the line must name the session-related actions (tmux killed and
// sessions restored). The exact wording is allowed to evolve, but the
// substrings are part of the behavioural contract.
func TestRestartSummaryLine_NamesKeyActions(t *testing.T) {
	for _, want := range []string{
		"prism restart",
		"tmux",
		"sessions",
	} {
		if !strings.Contains(restartSummaryLine, want) {
			t.Errorf("restartSummaryLine %q must contain %q to satisfy issue #1527 AC",
				restartSummaryLine, want)
		}
	}
}
