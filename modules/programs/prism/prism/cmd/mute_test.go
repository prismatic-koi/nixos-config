package cmd

// Tests for `prism mute`.
//
// The command is intentionally hidden; these tests exercise the RunE surface
// directly without going through `prism --help`. The discoverability AC is
// asserted separately in TestMuteCmd_IsHidden.

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// resetMuteFlags clears the on/off flags between sub-tests so flag state from
// one case does not bleed into the next. cobra.Command flags are package-level
// singletons, so explicit reset is required.
func resetMuteFlags(t *testing.T) {
	t.Helper()
	_ = muteCmd.Flags().Set("on", "false")
	_ = muteCmd.Flags().Set("off", "false")
}

// runMuteHelper invokes runMute against a freshly-constructed cobra command
// tree so the mutual-exclusion validation in cobra.MarkFlagsMutuallyExclusive
// actually fires. It mirrors how the real binary parses argv.
func runMuteHelper(t *testing.T, argv ...string) (string, error) {
	t.Helper()
	resetMuteFlags(t)

	// Build a throwaway parent so cobra Execute() walks the flag set the
	// same way as the real CLI. We can't use rootCmd directly because it
	// registers many side-effecting commands; spinning up a tiny clone is
	// cheaper and keeps the test focused.
	parent := &cobra.Command{Use: "prism"}
	// Reuse the configured muteCmd so its flag registrations (mutually
	// exclusive group, hidden bit, etc.) are exercised verbatim.
	parent.AddCommand(muteCmd)
	parent.SetArgs(append([]string{"mute"}, argv...))

	out := captureStdout(t, func() {
		_ = parent.Execute() // capture error via ExecuteC below
	})
	// Re-run via ExecuteC to retrieve the error directly. cobra's Execute
	// already printed any usage; ExecuteC short-circuits validation we have
	// already proved above and returns the error from RunE.
	parent.SetArgs(append([]string{"mute"}, argv...))
	resetMuteFlags(t)
	parent.SetOut(nil)
	parent.SetErr(nil)
	_, err := parent.ExecuteC()
	return out, err
}

// TestMute_NoArgs_TogglesCurrentSession asserts that running `prism mute`
// in a pane with PRISM_SESSION_NAME=foo toggles foo's muted flag and prints
// the expected stdout line.
func TestMute_NoArgs_TogglesCurrentSession(t *testing.T) {
	d := openStatsTestDB(t)

	const session = "prism-test@mute-noargs"
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	t.Setenv("PRISM_SESSION_NAME", session)
	resetMuteFlags(t)

	out := captureStdout(t, func() {
		if err := runMute(muteCmd, nil); err != nil {
			t.Fatalf("runMute: %v", err)
		}
	})
	if want := "muted: " + session; !strings.Contains(out, want) {
		t.Errorf("expected stdout to contain %q, got %q", want, out)
	}

	muted, ok, err := d.IsMuted(session)
	if err != nil || !ok {
		t.Fatalf("IsMuted: %v ok=%v", err, ok)
	}
	if !muted {
		t.Errorf("expected session muted after first toggle")
	}

	// Second toggle flips it back.
	out2 := captureStdout(t, func() {
		if err := runMute(muteCmd, nil); err != nil {
			t.Fatalf("runMute (second): %v", err)
		}
	})
	if want := "unmuted: " + session; !strings.Contains(out2, want) {
		t.Errorf("expected stdout to contain %q, got %q", want, out2)
	}
	muted2, _, _ := d.IsMuted(session)
	if muted2 {
		t.Errorf("expected session unmuted after second toggle")
	}
}

// TestMute_PositionalTargetsNamedSession asserts that `prism mute bar`
// toggles bar's muted flag even when the calling pane's PRISM_SESSION_NAME is
// set to a different session.
func TestMute_PositionalTargetsNamedSession(t *testing.T) {
	d := openStatsTestDB(t)
	const target = "prism-test@positional-target"
	const caller = "prism-test@positional-caller"
	for _, s := range []string{target, caller} {
		if err := d.UpsertStatus(s, "prism-test", "/tmp/w", "active", nil, nil); err != nil {
			t.Fatalf("UpsertStatus %q: %v", s, err)
		}
	}
	t.Setenv("PRISM_SESSION_NAME", caller)
	resetMuteFlags(t)

	captureStdout(t, func() {
		if err := runMute(muteCmd, []string{target}); err != nil {
			t.Fatalf("runMute: %v", err)
		}
	})

	mutedTarget, _, _ := d.IsMuted(target)
	if !mutedTarget {
		t.Errorf("positional target was not muted")
	}
	mutedCaller, _, _ := d.IsMuted(caller)
	if mutedCaller {
		t.Errorf("caller session should not have been touched by positional invocation")
	}
}

// TestMute_OnIsIdempotent asserts the AC: `prism mute --on foo` twice is a
// no-op on the second call and emits no stdout. The exit code is 0 either way.
func TestMute_OnIsIdempotent(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "prism-test@idempotent-on"
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	resetMuteFlags(t)
	_ = muteCmd.Flags().Set("on", "true")

	out1 := captureStdout(t, func() {
		if err := runMute(muteCmd, []string{session}); err != nil {
			t.Fatalf("runMute --on (first): %v", err)
		}
	})
	if !strings.Contains(out1, "muted: "+session) {
		t.Errorf("first --on must print muted line, got %q", out1)
	}

	// Second --on: no stdout, exit 0.
	resetMuteFlags(t)
	_ = muteCmd.Flags().Set("on", "true")
	out2 := captureStdout(t, func() {
		if err := runMute(muteCmd, []string{session}); err != nil {
			t.Fatalf("runMute --on (second): %v", err)
		}
	})
	if strings.TrimSpace(out2) != "" {
		t.Errorf("second --on must be silent, got %q", out2)
	}
	muted, _, _ := d.IsMuted(session)
	if !muted {
		t.Errorf("session should remain muted after second --on")
	}
}

// TestMute_OffIsIdempotent mirrors the --on idempotence AC for --off.
func TestMute_OffIsIdempotent(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "prism-test@idempotent-off"
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Pre-mute the session so --off has work to do.
	if _, err := d.SetMuted(session, true); err != nil {
		t.Fatalf("seed SetMuted: %v", err)
	}

	resetMuteFlags(t)
	_ = muteCmd.Flags().Set("off", "true")
	out1 := captureStdout(t, func() {
		if err := runMute(muteCmd, []string{session}); err != nil {
			t.Fatalf("runMute --off (first): %v", err)
		}
	})
	if !strings.Contains(out1, "unmuted: "+session) {
		t.Errorf("first --off must print unmuted line, got %q", out1)
	}

	resetMuteFlags(t)
	_ = muteCmd.Flags().Set("off", "true")
	out2 := captureStdout(t, func() {
		if err := runMute(muteCmd, []string{session}); err != nil {
			t.Fatalf("runMute --off (second): %v", err)
		}
	})
	if strings.TrimSpace(out2) != "" {
		t.Errorf("second --off must be silent, got %q", out2)
	}
}

// TestMute_OnOff_MutuallyExclusive asserts the AC: `prism mute --on --off`
// exits non-zero with a cobra-enforced mutual-exclusion error.
func TestMute_OnOff_MutuallyExclusive(t *testing.T) {
	// Use a fresh sub-tree so cobra's mutual-exclusion check actually runs.
	parent := &cobra.Command{Use: "prism"}
	parent.AddCommand(muteCmd)
	parent.SetArgs([]string{"mute", "--on", "--off", "irrelevant"})
	resetMuteFlags(t)
	// Silence stderr/usage prints so the test output stays clean.
	parent.SetErr(devNullWriter{})
	parent.SetOut(devNullWriter{})

	_, err := parent.ExecuteC()
	resetMuteFlags(t)
	if err == nil {
		t.Fatal("expected error for --on --off, got nil")
	}
	// cobra phrases this as "if any flags in the group [on off] are set none
	// of the others can be". Accept either the human phrasing or the cobra
	// canonical wording so the test does not break on a cobra upgrade.
	msg := err.Error()
	if !strings.Contains(msg, "mutually exclusive") && !strings.Contains(msg, "[on off]") {
		t.Errorf("error must signal mutual exclusion, got %q", err)
	}
}

// TestMute_NoArgs_MissingEnv_Errors asserts the AC: `prism mute` with no args
// and PRISM_SESSION_NAME unset exits non-zero with a clear error.
func TestMute_NoArgs_MissingEnv_Errors(t *testing.T) {
	openStatsTestDB(t) // wires up testDBPath even though we'll error before opening
	t.Setenv("PRISM_SESSION_NAME", "")
	resetMuteFlags(t)

	err := runMute(muteCmd, nil)
	if err == nil {
		t.Fatal("expected error for no-args + missing PRISM_SESSION_NAME, got nil")
	}
	if !strings.Contains(err.Error(), "no current session") {
		t.Errorf("error must mention 'no current session', got %q", err)
	}
}

// TestMute_UnknownSession_Errors asserts the AC: `prism mute does-not-exist`
// exits non-zero with a clear "session not found" error AND does not insert a
// new agent_status row for the missing session.
func TestMute_UnknownSession_Errors(t *testing.T) {
	d := openStatsTestDB(t)
	resetMuteFlags(t)

	err := runMute(muteCmd, []string{"does-not-exist"})
	if err == nil {
		t.Fatal("expected error for unknown session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error must mention 'not found', got %q", err)
	}

	// Confirm no phantom row was inserted.
	st, lookupErr := d.CurrentStatus("does-not-exist")
	if lookupErr != nil {
		t.Fatalf("CurrentStatus: %v", lookupErr)
	}
	if st != nil {
		t.Errorf("phantom row inserted for missing session: %+v", st)
	}
}

// TestMuteCmd_IsHidden asserts the discoverability AC: the cobra command
// carries Hidden=true so it does not appear in `--help` or completion output.
func TestMuteCmd_IsHidden(t *testing.T) {
	if !muteCmd.Hidden {
		t.Error("muteCmd.Hidden must be true so prism mute does not appear in --help or completion")
	}
}

// devNullWriter discards writes so tests do not pollute stderr/stdout.
type devNullWriter struct{}

func (devNullWriter) Write(p []byte) (int, error) { return len(p), nil }
