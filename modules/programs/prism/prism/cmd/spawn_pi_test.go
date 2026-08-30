package cmd

// spawn_pi_test.go — tests for the PI harness spawn path.
//
// Coverage:
//   - --harness pi on Darwin passes harness validation
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

// TestRunSpawn_HarnessPI_DarwinPassesValidation verifies that --harness pi on
// Darwin passes harness validation. The test does not prepare a real
// sandbox-exec environment so runSpawn fails further down; we assert the
// failure is NOT a harness-validation error and NOT a Darwin "not yet
// supported" error, proving validation passed and no such guard fires.
func TestRunSpawn_HarnessPI_DarwinPassesValidation(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Darwin-specific: verifies Darwin guard has been removed")
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
	cmd.Flags().String("harness", "pi", "")
	cmd.Flags().StringArray("model-override", nil, "")
	addPromptFlags(cmd)
	_ = cmd.Flags().Set("harness", "pi")

	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")

	err := runSpawn(cmd, nil)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "unknown harness") {
			t.Errorf("runSpawn with --harness pi on Darwin returned harness validation error: %v", err)
		}
		if strings.Contains(msg, "not yet supported") {
			t.Errorf("runSpawn with --harness pi on Darwin returned the old Darwin guard error — guard should have been removed by P2.DARWIN (#1213): %v", err)
		}
		if strings.Contains(msg, "#1213") {
			t.Errorf("runSpawn with --harness pi on Darwin returned old #1213 pointer — guard should have been removed: %v", err)
		}
	}
}

// TestRunSpawn_HarnessPI_LinuxPassesValidation verifies the positive path:
// the PI harness is registered and validation passes. The test does not
// prepare a real bwrap environment so runSpawn fails further down; we assert
// the failure is NOT a harness-validation error to prove validation passed.
func TestRunSpawn_HarnessPI_LinuxPassesValidation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only: bwrap is only supported on Linux")
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
	cmd.Flags().String("harness", "pi", "")
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
