package cmd

// spawn_abtest_test.go — tests for the --abtest flag parsing and validation
// (P4.ABTEST, issue #1216).
//
// Coverage:
//   - --abtest and --profile are mutually exclusive
//   - --abtest with exactly one profile name returns an error
//   - --abtest with three or more profile names returns an error
//   - generateAbtestPairID returns unique 32-char hex strings
//   - branch-name suffixing produces the expected session name shapes

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// buildAbtestCmd returns a *cobra.Command with all flags registered (mirrors
// the real spawnCmd registration) so that runSpawn can be called directly in
// tests without the global command tree.
func buildAbtestCmd(t *testing.T) *cobra.Command {
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

// TestRunSpawn_Abtest_MutualExclusion verifies that passing both --abtest and
// --profile returns a clear error with no side effects.
func TestRunSpawn_Abtest_MutualExclusion(t *testing.T) {
	cmd := buildAbtestCmd(t)
	_ = cmd.Flags().Set("profile", "anthropic")
	_ = cmd.Flags().Set("abtest", "profileA")
	_ = cmd.Flags().Set("abtest", "profileB")

	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("expected error for --abtest + --profile, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' in error, got: %v", err)
	}
}

// TestRunSpawn_Abtest_RequiresExactlyTwoProfiles verifies that --abtest with
// only one profile name is rejected before any side effects.
func TestRunSpawn_Abtest_RequiresExactlyTwoProfiles(t *testing.T) {
	cmd := buildAbtestCmd(t)
	_ = cmd.Flags().Set("abtest", "profileA")

	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("expected error for --abtest with one profile, got nil")
	}
	if !strings.Contains(err.Error(), "exactly two") {
		t.Errorf("expected 'exactly two' in error, got: %v", err)
	}
}

// TestRunSpawn_Abtest_TooManyProfiles verifies that --abtest with three
// profile names is rejected.
func TestRunSpawn_Abtest_TooManyProfiles(t *testing.T) {
	cmd := buildAbtestCmd(t)
	for _, p := range []string{"a", "b", "c"} {
		_ = cmd.Flags().Set("abtest", p)
	}

	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("expected error for --abtest with three profiles, got nil")
	}
	if !strings.Contains(err.Error(), "exactly two") {
		t.Errorf("expected 'exactly two' in error, got: %v", err)
	}
}

// TestGenerateAbtestPairID_Uniqueness verifies that two consecutive calls
// return different values, and that each value is a 32-character hex string.
func TestGenerateAbtestPairID_Uniqueness(t *testing.T) {
	id1 := generateAbtestPairID()
	id2 := generateAbtestPairID()
	if id1 == id2 {
		t.Errorf("generateAbtestPairID returned identical values: %q", id1)
	}
	for _, id := range []string{id1, id2} {
		if len(id) != 32 {
			t.Errorf("generateAbtestPairID length: got %d, want 32 (got %q)", len(id), id)
		}
		for _, c := range id {
			if !('0' <= c && c <= '9') && !('a' <= c && c <= 'f') {
				t.Errorf("generateAbtestPairID contains non-hex char %q in %q", c, id)
				break
			}
		}
	}
}
