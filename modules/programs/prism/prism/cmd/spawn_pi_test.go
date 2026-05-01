package cmd

// spawn_pi_test.go — tests for the PI harness integration (P2.SPAWN, #1212).
//
// Coverage:
//   - --harness pi on Darwin returns a clear error pointing at #1213
//   - --harness pi on Linux passes the harness validation step (the spawn
//     fails later for an unrelated reason since the test does not prepare a
//     real bwrap environment, but the failure is NOT a harness validation
//     error).

import (
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestRunSpawn_HarnessPI_DarwinReturnsClearError verifies the edge-case AC:
//
//	`--harness pi` on Darwin fails with a clear "not yet supported, see
//	 #1213" message until P2.DARWIN lands.
//
// On Linux this branch is unreachable; the test skips so that CI on the
// supported platform continues to exercise the rest of the suite. The string
// match asserts that the error mentions both the OS and the tracking issue
// so that an operator on a Mac sees the right pointer immediately.
func TestRunSpawn_HarnessPI_DarwinReturnsClearError(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Darwin-specific: --harness pi guard is conditioned on runtime.GOOS != linux")
	}

	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("pr", "", "")
	cmd.Flags().String("repo", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().Bool("attach", false, "")
	cmd.Flags().String("harness", "opencode", "")
	cmd.Flags().StringArray("model-override", nil, "")
	addPromptFlags(cmd)
	_ = cmd.Flags().Set("harness", "pi")

	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("runSpawn with --harness pi on Darwin: expected non-nil error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not yet supported") {
		t.Errorf("error %q does not mention 'not yet supported'", msg)
	}
	if !strings.Contains(msg, "#1213") {
		t.Errorf("error %q does not reference the P2.DARWIN tracking issue (#1213)", msg)
	}
}

// TestRunSpawn_HarnessPI_LinuxPassesValidation verifies the positive path:
// the PI harness is registered and validation passes. The test does not
// prepare a real bwrap environment so runSpawn fails further down; we assert
// the failure is NOT a harness-validation error to prove validation passed.
func TestRunSpawn_HarnessPI_LinuxPassesValidation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only: PI harness only supported on Linux today")
	}

	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("pr", "", "")
	cmd.Flags().String("repo", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().Bool("attach", false, "")
	cmd.Flags().String("harness", "opencode", "")
	cmd.Flags().StringArray("model-override", nil, "")
	addPromptFlags(cmd)
	_ = cmd.Flags().Set("harness", "pi")

	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")
	withNoopTmux(t)

	err := runSpawn(cmd, nil)
	// We expect a non-nil error (no git repo), but it must not be a harness
	// validation or Darwin-guard error.
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "unknown harness") {
			t.Errorf("runSpawn with --harness pi returned harness validation error: %v", err)
		}
		if strings.Contains(msg, "not yet supported") {
			t.Errorf("runSpawn with --harness pi on Linux hit Darwin guard: %v", err)
		}
	}
}
