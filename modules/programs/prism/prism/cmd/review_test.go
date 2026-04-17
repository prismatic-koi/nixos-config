package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

// TestAgentsForHarness_EnvUnset verifies that when ENHANCED_REVIEW is not set,
// agentsForHarness returns the single default agent set.
func TestAgentsForHarness_EnvUnset(t *testing.T) {
	os.Unsetenv("ENHANCED_REVIEW")

	agents := agentsForHarness("opencode")
	want := review.DefaultAgents()
	if len(agents) != len(want) {
		t.Fatalf("agentsForHarness(opencode) with ENHANCED_REVIEW unset: got %d agents, want %d", len(agents), len(want))
	}
	for i, a := range agents {
		if a.Name != want[i].Name || a.OpencodeName != want[i].OpencodeName {
			t.Errorf("agent[%d]: got {%q, %q}, want {%q, %q}", i, a.Name, a.OpencodeName, want[i].Name, want[i].OpencodeName)
		}
	}
}

// TestAgentsForHarness_EnvTrue verifies that when ENHANCED_REVIEW=true,
// agentsForHarness returns the five-agent enhanced set.
func TestAgentsForHarness_EnvTrue(t *testing.T) {
	t.Setenv("ENHANCED_REVIEW", "true")

	agents := agentsForHarness("opencode")
	want := review.EnhancedAgents()
	if len(agents) != 5 {
		t.Fatalf("agentsForHarness(opencode) with ENHANCED_REVIEW=true: got %d agents, want 5", len(agents))
	}
	if len(agents) != len(want) {
		t.Fatalf("agentsForHarness(opencode) with ENHANCED_REVIEW=true: got %d agents, want %d", len(agents), len(want))
	}
	for i, a := range agents {
		if a.Name != want[i].Name || a.OpencodeName != want[i].OpencodeName {
			t.Errorf("agent[%d]: got {%q, %q}, want {%q, %q}", i, a.Name, a.OpencodeName, want[i].Name, want[i].OpencodeName)
		}
	}
}

// TestAgentsForHarness_EnvFalse verifies that ENHANCED_REVIEW=false (or any
// non-"true" value) returns the default single-agent set.
func TestAgentsForHarness_EnvFalse(t *testing.T) {
	for _, val := range []string{"false", "0", "yes", "TRUE", "1"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("ENHANCED_REVIEW", val)

			agents := agentsForHarness("opencode")
			want := review.DefaultAgents()
			if len(agents) != len(want) {
				t.Fatalf("agentsForHarness(opencode) with ENHANCED_REVIEW=%q: got %d agents, want %d", val, len(agents), len(want))
			}
			if agents[0].Name != "review" {
				t.Errorf("agentsForHarness(opencode) with ENHANCED_REVIEW=%q: got agent name %q, want %q", val, agents[0].Name, "review")
			}
		})
	}
}

// TestAgentsForHarness_EnhancedAgentNames verifies that the enhanced agent names
// match the expected opencode agent identifiers.
func TestAgentsForHarness_EnhancedAgentNames(t *testing.T) {
	t.Setenv("ENHANCED_REVIEW", "true")

	agents := agentsForHarness("opencode")
	expectedNames := []string{
		"review-goal",
		"review-code",
		"review-security",
		"review-qa",
		"review-context",
	}
	for i, name := range expectedNames {
		if i >= len(agents) {
			t.Fatalf("not enough agents: got %d, want at least %d", len(agents), i+1)
		}
		if agents[i].Name != name {
			t.Errorf("agents[%d].Name = %q, want %q", i, agents[i].Name, name)
		}
		if agents[i].OpencodeName != name {
			t.Errorf("agents[%d].OpencodeName = %q, want %q", i, agents[i].OpencodeName, name)
		}
	}
}

// ── resolveReviewWorktree tests ────────────────────────────────────────────────
//
// These tests cover the three scenarios mandated by #751:
//
//  1. Happy path: session present in DB with a real host-side worktree path →
//     resolveReviewWorktree returns that path (not "/workspace").
//
//  2. Missing DB row: session not found → error containing the session name.
//
//  3. Empty worktree: session found but Worktree == "" → error containing the
//     session name.
//
// All tests set testDBPath so that openDB() (via SetTestDBPath) uses an
// isolated temp DB, never the real prism.db.

// openReviewTestDB opens a temp SQLite DB for review tests, registers cleanup,
// and points SetTestDBPath at it so resolveReviewWorktree uses it.
func openReviewTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	SetTestDBPath(path)
	t.Cleanup(func() { SetTestDBPath("") })
	return d
}

// TestResolveReviewWorktree_ContainerMode_UsesDBPath verifies that when
// cfg.ContainerMode is true on the host and PRISM_HOST_API is unset,
// resolveReviewWorktree returns the host-side worktree stored in the DB, not
// "/workspace" (the container-internal fallback that was the root of bug #751).
//
// The test seeds a session with a host-side path, then calls
// resolveReviewWorktree and asserts the returned path matches the DB value.
func TestResolveReviewWorktree_ContainerMode_UsesDBPath(t *testing.T) {
	// Ensure we are exercising the host-side code path (PRISM_HOST_API unset).
	t.Setenv("PRISM_HOST_API", "")
	// Ensure PRISM_SPAWN_PATH is unset — the old code read this on the
	// container-mode branch, which must NOT be used.
	t.Setenv("PRISM_SPAWN_PATH", "")

	d := openReviewTestDB(t)

	const session = "nixos-config@fix-worktree-leak"
	hostWorktree := "/Users/bensherman/code/nixos-config/fix-worktree-leak"

	if err := d.UpsertStatus(session, "nixos-config", hostWorktree, "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	got, err := resolveReviewWorktree(session)
	if err != nil {
		t.Fatalf("resolveReviewWorktree: unexpected error: %v", err)
	}
	if got != hostWorktree {
		t.Errorf("resolveReviewWorktree = %q, want %q", got, hostWorktree)
	}
	// Explicit guard: the old broken fallback must not be returned.
	if got == "/workspace" {
		t.Errorf("resolveReviewWorktree returned /workspace — this is the container-internal path, not the host path")
	}
}

// TestResolveReviewWorktree_SessionNotInDB verifies that when the parent
// session has no row in the DB, resolveReviewWorktree returns a descriptive
// error containing the session name.
func TestResolveReviewWorktree_SessionNotInDB(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")

	// Open an empty DB — no rows seeded.
	openReviewTestDB(t)

	const session = "nixos-config@missing-session"
	_, err := resolveReviewWorktree(session)
	if err == nil {
		t.Fatal("resolveReviewWorktree: expected error for missing session, got nil")
	}
	if !strings.Contains(err.Error(), session) {
		t.Errorf("error %q does not contain session name %q", err.Error(), session)
	}
}

// TestResolveReviewWorktree_EmptyWorktree verifies that when the DB row for
// the parent session exists but has an empty Worktree field,
// resolveReviewWorktree returns a descriptive error containing the session
// name and does not proceed with an empty path.
func TestResolveReviewWorktree_EmptyWorktree(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")

	d := openReviewTestDB(t)

	const session = "nixos-config@empty-worktree"
	// Seed a row with an empty worktree. UpsertStatus accepts "" for worktree.
	if err := d.UpsertStatus(session, "nixos-config", "", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	_, err := resolveReviewWorktree(session)
	if err == nil {
		t.Fatal("resolveReviewWorktree: expected error for empty worktree, got nil")
	}
	if !strings.Contains(err.Error(), session) {
		t.Errorf("error %q does not contain session name %q", err.Error(), session)
	}
}
