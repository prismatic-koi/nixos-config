package cmd

// agent_run_errnot_aworktree_guard_test.go — wiring guard for the
// ErrNotAWorktree handling at both agent-run call sites (issue #2551).
//
// PR #2550 fixed the pane-death regression (#2549) by adding
// git.ErrNotAWorktree and checking it with errors.Is at two call sites:
//
//   - cmd/agent_run.go
//   - cmd/agent_run_sandbox_exec_darwin.go
//
// All three tests added by that PR sit at the internal/git level and test the
// sentinel itself. None test the wiring: deleting the errors.Is branch from
// either call site leaves every other test green while the #2549 regression
// returns (dead pane for a normal clone).
//
// The Darwin call site is the more exposed of the two because it is not built
// or exercised on the Linux CI path.
//
// This guard reads the source of both dispatch files and asserts they handle
// git.ErrNotAWorktree by setting worktreeGitDir to empty string. Reading
// source in a test follows the precedent set by
// internal/db/schema-version-guard_test.go (issue #1869) and
// agent_env_roles_guard_test.go (issue #2533). The sandbox-exec file is
// Darwin-only, so it is read as text rather than compiled — that keeps the
// guard effective on a Linux CI runner.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAgentRunHandlesErrNotAWorktree asserts that both the bwrap dispatch
// (agent_run.go) and the sandbox-exec dispatch (agent_run_sandbox_exec_darwin.go)
// properly handle git.ErrNotAWorktree by setting worktreeGitDir to empty
// string, and that neither bypasses the error check via comments or cosmetic
// changes.
func TestAgentRunHandlesErrNotAWorktree(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: could not determine source file location")
	}
	dir := filepath.Dir(thisFile)

	for _, name := range []string{"agent_run.go", "agent_run_sandbox_exec_darwin.go"} {
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("ReadFile %q: %v", name, err)
			}
			body := string(src)

			// The error check must be present as active code (not commented out).
			// Pattern: if err != nil && !errors.Is(err, git.ErrNotAWorktree)
			const errCheckPattern = "if err != nil && !errors.Is(err, git.ErrNotAWorktree)"
			if !hasActiveCodeLine(body, errCheckPattern) {
				t.Errorf("%s must check git.ErrNotAWorktree with %q to handle normal clones (issue #2549, #2550)", name, errCheckPattern)
			}

			// The recovery path must be present as active code.
			// Pattern: if errors.Is(err, git.ErrNotAWorktree)
			const recoveryPattern = "if errors.Is(err, git.ErrNotAWorktree)"
			if !hasActiveCodeLine(body, recoveryPattern) {
				t.Errorf("%s must handle git.ErrNotAWorktree with %q to set worktreeGitDir empty (issue #2549, #2550)", name, recoveryPattern)
			}

			// The recovery path must assign worktreeGitDir to empty string as active code.
			const recoveryAssign = `worktreeGitDir = ""`
			if !hasActiveCodeLine(body, recoveryAssign) {
				t.Errorf("%s must set %q in the recovery path for ErrNotAWorktree (issue #2549, #2550)", name, recoveryAssign)
			}
		})
	}
}

// hasActiveCodeLine checks if a line containing pattern appears in body as
// real code, not just in comments. A line is considered "active code" if the
// pattern is not preceded by "//" on that line. This allows the guard to be
// insensitive to comment edits and reformatting (issue #2551, AC: edge-case).
func hasActiveCodeLine(body string, pattern string) bool {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		// Skip lines that are pure comments at the start
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		// Check if pattern is on this line before any comment marker
		if strings.Contains(line, pattern) {
			// Ensure the pattern is not commented out (pattern comes before //)
			commentIdx := strings.Index(line, "//")
			patternIdx := strings.Index(line, pattern)
			// patternIdx < commentIdx means pattern appears before the comment marker
			if commentIdx == -1 || patternIdx < commentIdx {
				return true
			}
		}
	}
	return false
}
