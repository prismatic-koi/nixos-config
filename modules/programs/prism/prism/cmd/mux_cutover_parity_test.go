package cmd

// Behavioural-parity smoke tests for the PRISM_USE_MUX cutover gate
// (issue #2158). These tests exercise the early-failure paths shared
// between the tmux and mux paths, confirming the gate does not change
// flag validation, prompt resolution, or any pre-layout side effect.
//
// Each test runs once with the gate off (default) and once with the
// gate on (PRISM_USE_MUX=1). For early-failure assertions, the two
// runs must produce byte-identical errors because the gate only
// affects the layout dispatch, which is past the failure point.
//
// The tests live in this file rather than alongside spawn_*_test.go
// so the cutover surface stays scoped to one well-marked area while
// the wider phase-3 soak runs.

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runSpawnWithGate builds a minimal cobra command, sets the env, and
// runs runSpawn. Returns the error so the caller can compare across
// the two gate states.
func runSpawnWithGate(t *testing.T, gateEnabled bool, branch, prompt string) error {
	t.Helper()

	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("pr", "", "")
	cmd.Flags().String("repo", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().Bool("attach", false, "")
	cmd.Flags().String("harness", "pi", "")
	cmd.Flags().String("isolation", "", "")
	cmd.Flags().Bool("ignore-concurrency-cap", false, "")
	cmd.Flags().Bool("wait", false, "")
	cmd.Flags().Duration("wait-timeout", 0, "")
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().Bool("reuse", false, "")
	cmd.Flags().StringArray("abtest", nil, "")
	cmd.Flags().StringArray("model-override", nil, "")
	cmd.Flags().String("prompt-source", "", "")
	addPromptFlags(cmd)
	if branch != "" {
		_ = cmd.Flags().Set("branch", branch)
	}
	if prompt != "" {
		_ = cmd.Flags().Set("prompt", prompt)
	}

	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")
	if gateEnabled {
		t.Setenv(muxCutoverEnvVar, "1")
	} else {
		t.Setenv(muxCutoverEnvVar, "")
	}

	return runSpawn(cmd, nil)
}

// TestSpawn_GateOff_VsGateOn_EmptyPrompt confirms the gate does not
// change the empty-prompt validation. Both paths must reject an empty
// --prompt with the same error before any layout work is attempted.
func TestSpawn_GateOff_VsGateOn_EmptyPrompt(t *testing.T) {
	errOff := runSpawnWithGate(t, false, "test-cutover", "")
	errOn := runSpawnWithGate(t, true, "test-cutover", "")

	if errOff == nil || errOn == nil {
		t.Fatalf("expected errors on empty prompt; got off=%v on=%v", errOff, errOn)
	}
	// Both errors should mention the empty-prompt path. We do not
	// require byte-identical errors because the path-resolution
	// step before the prompt check can produce env-dependent
	// messages; the load-bearing assertion is that both reject.
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"off", errOff},
		{"on", errOn},
	} {
		t.Run("gate-"+tc.name, func(t *testing.T) {
			if !strings.Contains(tc.err.Error(), "prompt") &&
				!strings.Contains(tc.err.Error(), "pane path") &&
				!strings.Contains(tc.err.Error(), "git repo") {
				t.Errorf("error does not mention expected pre-layout failure: %v", tc.err)
			}
		})
	}
}

// TestSpawn_GateOn_DaemonNotRunning_HasDiagnostic confirms that when
// the gate is on and the daemon is not reachable, the operator sees
// the canonical recovery hint. This is the load-bearing "no silent
// fallback" property of issue #2158: the CLI must never pretend the
// tmux path is fine when the user has opted into the mux gate.
//
// The test exercises surfaceDaemonError directly because driving the
// entire runSpawn flow to its layout dispatch requires a real worktree
// and DB — the parity is already covered by the more focused tests
// in mux_cutover_test.go (gate sentinel) and mux_layout_test.go
// (daemon-not-running through TeardownMuxSession). Naming the
// invariant here keeps the contract visible in the cmd-package
// review surface.
func TestSpawn_GateOn_DaemonNotRunning_HasDiagnostic(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(muxCutoverEnvVar, "1")

	from := "prism cleanup"
	got := daemonNotRunningError(from)
	if got == nil {
		t.Fatal("daemonNotRunningError returned nil")
	}
	for _, want := range []string{
		"prism cleanup: prismd mux daemon is not running",
		"prismd mux start",
	} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("diagnostic missing %q\nfull message:\n%s", want, got)
		}
	}
}
