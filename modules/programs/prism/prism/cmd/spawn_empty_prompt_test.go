package cmd

// Tests for the keybind carve-out of the layer-1+2 empty-prompt guard
// (issue #2012). The tmux Prefix+a keybind invokes `prism spawn --attach`
// with no --prompt — the operator types the initial prompt to the live
// agent after the popup attaches. The keybind discriminator is the
// PRISM_SPAWN_PATH environment variable; when that is set and no prompt
// was supplied, runSpawn must skip the empty-prompt rejection so the
// popup does not flash-close with an unreadable error.
//
// Without PRISM_SPAWN_PATH set, the existing emptyPromptError must still
// fire — protecting the shell-invocation path and proving the relaxation
// is gated on the discriminator only.

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// buildSpawnCmdForEmptyPromptTest mirrors buildAbtestCmd but keeps the test
// helper local to this file so it does not couple to abtest-only fixtures.
func buildSpawnCmdForEmptyPromptTest(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("pr", "", "")
	cmd.Flags().String("repo", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().StringArray("abtest", nil, "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().Bool("attach", false, "")
	cmd.Flags().String("harness", "pi", "")
	cmd.Flags().StringArray("model-override", nil, "")
	cmd.Flags().String("isolation", "", "")
	cmd.Flags().Bool("ignore-concurrency-cap", false, "")
	cmd.Flags().String("prompt-source", "", "")
	addPromptFlags(cmd)
	return cmd
}

// TestRunSpawn_KeybindCarveOut_EmptyPromptAccepted verifies that runSpawn
// with PRISM_SPAWN_PATH set and no prompt does NOT return emptyPromptError.
//
// Because the test environment has no real git repo at PRISM_SPAWN_PATH and
// no live tmux server is available, runSpawn will still fail downstream of
// the layer-1+2 guard — typically on resolveBareRoot ("not inside a git
// repo") or another pre-flight check. The assertion is therefore negative:
// the error must NOT contain the empty-prompt message. That is sufficient
// evidence that the layer-1+2 guard let the call through, which is the
// exact behaviour the issue #2012 fix promises.
func TestRunSpawn_KeybindCarveOut_EmptyPromptAccepted(t *testing.T) {
	cmd := buildSpawnCmdForEmptyPromptTest(t)
	// No --prompt / --prompt-file flags set — promptText resolves to "".

	t.Setenv("PRISM_HOST_API", "")
	// PRISM_SPAWN_PATH is the keybind discriminator. Set it to a non-git
	// temp dir so resolveBareRoot returns "not inside a git repo" without
	// hitting the live tmux pane path (#1180 isolation pattern).
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")
	withNoopTmux(t)

	err := runSpawn(cmd, nil)
	if err == nil {
		// runSpawn must fail later (no git repo), but it must not error
		// on the empty-prompt guard. A nil error here would mean the
		// test environment accidentally bootstrapped a session — fail loudly.
		t.Fatal("runSpawn returned nil — expected a downstream failure (no git repo) past the empty-prompt guard")
	}
	if strings.Contains(err.Error(), "a prompt is required") ||
		strings.Contains(err.Error(), "empty string — supply a non-empty prompt") ||
		strings.Contains(err.Error(), "empty stdin — supply a non-empty prompt") ||
		strings.Contains(err.Error(), "file is empty — supply a non-empty prompt") {
		t.Errorf("runSpawn returned empty-prompt error despite PRISM_SPAWN_PATH being set: %v", err)
	}
}

// TestRunSpawn_NoKeybind_EmptyPromptStillRejected verifies that runSpawn
// with PRISM_SPAWN_PATH unset (i.e. invoked from a normal shell) and no
// prompt still returns the existing emptyPromptError shape. This guards
// the existing layer-1+2 behaviour for the non-keybind path.
func TestRunSpawn_NoKeybind_EmptyPromptStillRejected(t *testing.T) {
	cmd := buildSpawnCmdForEmptyPromptTest(t)
	// No --prompt flags set.

	t.Setenv("PRISM_HOST_API", "")
	// IMPORTANT: PRISM_SPAWN_PATH explicitly unset (not just empty in the
	// caller's environment — t.Setenv guarantees the env var is "" for
	// the duration of the test, which os.Getenv treats identically to
	// unset for the fromKeybind check).
	t.Setenv("PRISM_SPAWN_PATH", "")
	t.Setenv("PRISM_BARE_ROOT", "")
	withNoopTmux(t)

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("runSpawn with empty prompt and no PRISM_SPAWN_PATH: expected non-nil error, got nil")
	}
	// The default branch of emptyPromptError fires when no prompt flag was
	// set at all: "a prompt is required — supply --prompt <text>, --prompt -
	// (stdin), or --prompt-file <path>".
	if !strings.Contains(err.Error(), "a prompt is required") {
		t.Errorf("error %q does not contain 'a prompt is required'", err.Error())
	}
}

// TestRunSpawn_KeybindCarveOut_DoesNotSwallowSuppliedPrompt verifies the
// AC edge-case: when PRISM_SPAWN_PATH is set AND a non-empty --prompt is
// also supplied, the carve-out does not swallow the prompt. We assert this
// by checking the error is NOT an empty-prompt error (proving the guard
// is bypassed cleanly without erasing the prompt the caller supplied).
//
// The downstream failure here is the same as the empty-prompt-accepted
// test: "not inside a git repo" from resolveBareRoot. We only care that
// the layer-1+2 guard does not misclassify a supplied prompt as empty.
func TestRunSpawn_KeybindCarveOut_DoesNotSwallowSuppliedPrompt(t *testing.T) {
	cmd := buildSpawnCmdForEmptyPromptTest(t)
	_ = cmd.Flags().Set("prompt", "hello from the keybind path")

	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")
	withNoopTmux(t)

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("runSpawn returned nil — expected a downstream failure (no git repo)")
	}
	if strings.Contains(err.Error(), "a prompt is required") ||
		strings.Contains(err.Error(), "empty string — supply a non-empty prompt") {
		t.Errorf("runSpawn misclassified a supplied prompt as empty: %v", err)
	}
}
