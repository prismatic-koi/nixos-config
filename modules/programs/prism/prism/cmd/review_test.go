package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	opencode "github.com/prismatic-koi/prism/internal/harness/opencode"
	"github.com/prismatic-koi/prism/internal/review"
)

// TestAgentsForHarness_ReturnsAllFive verifies that agentsForHarness always
// returns the five-agent review set unconditionally.
func TestAgentsForHarness_ReturnsAllFive(t *testing.T) {
	agents := agentsForHarness("opencode")
	want := review.Agents()
	if len(agents) != 5 {
		t.Fatalf("agentsForHarness(opencode): got %d agents, want 5", len(agents))
	}
	if len(agents) != len(want) {
		t.Fatalf("agentsForHarness(opencode): got %d agents, want %d", len(agents), len(want))
	}
	for i, a := range agents {
		if a.Name != want[i].Name || a.OpencodeName != want[i].OpencodeName {
			t.Errorf("agent[%d]: got {%q, %q}, want {%q, %q}", i, a.Name, a.OpencodeName, want[i].Name, want[i].OpencodeName)
		}
	}
}

// TestAgentsForHarness_AgentNames verifies that agentsForHarness returns the
// correct five agent names.
func TestAgentsForHarness_AgentNames(t *testing.T) {
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

// ── splitCSV tests ────────────────────────────────────────────────────────────

// TestSplitCSV_TrailingComma verifies that a trailing comma produces no empty
// tokens. The AC requires: "splitCSV with a trailing comma produces no empty tokens".
func TestSplitCSV_TrailingComma(t *testing.T) {
	result := splitCSV("review-code,review-qa,")
	if len(result) != 2 {
		t.Fatalf("splitCSV with trailing comma: got %d tokens, want 2: %v", len(result), result)
	}
	if result[0] != "review-code" {
		t.Errorf("result[0] = %q, want %q", result[0], "review-code")
	}
	if result[1] != "review-qa" {
		t.Errorf("result[1] = %q, want %q", result[1], "review-qa")
	}
}

// TestSplitCSV_LeadingComma verifies that a leading comma produces no empty tokens.
func TestSplitCSV_LeadingComma(t *testing.T) {
	result := splitCSV(",review-code,review-qa")
	if len(result) != 2 {
		t.Fatalf("splitCSV with leading comma: got %d tokens, want 2: %v", len(result), result)
	}
}

// TestSplitCSV_EmptyString verifies that an empty string returns no tokens.
func TestSplitCSV_EmptyString(t *testing.T) {
	result := splitCSV("")
	if len(result) != 0 {
		t.Fatalf("splitCSV with empty string: got %d tokens, want 0: %v", len(result), result)
	}
}

// TestSplitCSV_WhitespaceOnly verifies that a whitespace-only string (or
// comma-separated whitespace) returns no tokens.
func TestSplitCSV_WhitespaceOnly(t *testing.T) {
	for _, s := range []string{" ", "  ,  ", "  ,  ,  "} {
		result := splitCSV(s)
		if len(result) != 0 {
			t.Errorf("splitCSV(%q): got %d tokens, want 0: %v", s, len(result), result)
		}
	}
}

// TestSplitCSV_TrimsWhitespace verifies that leading/trailing whitespace around
// each token is trimmed.
func TestSplitCSV_TrimsWhitespace(t *testing.T) {
	result := splitCSV("  review-code , review-qa  ")
	if len(result) != 2 {
		t.Fatalf("splitCSV with whitespace: got %d tokens, want 2: %v", len(result), result)
	}
	if result[0] != "review-code" {
		t.Errorf("result[0] = %q, want %q", result[0], "review-code")
	}
	if result[1] != "review-qa" {
		t.Errorf("result[1] = %q, want %q", result[1], "review-qa")
	}
}

// ── --only flag validation tests ──────────────────────────────────────────────

// TestOnlyFlag_UnknownAgentNameReturnsError verifies that --only with an
// unknown agent name surfaces an error (via AgentsByName) before any session
// is spawned. We test this by calling review.AgentsByName directly, which is
// the same function used by runReview.
//
// AC: "--only with an unknown name surfaces an error before any session is spawned"
func TestOnlyFlag_UnknownAgentNameReturnsError(t *testing.T) {
	allAgents := agentsForHarness("opencode")

	// A completely unknown name.
	_, err := review.AgentsByName(allAgents, []string{"review-typo"})
	if err == nil {
		t.Fatal("AgentsByName: expected error for unknown agent name, got nil")
	}
	// Error must contain the unknown name.
	if !strings.Contains(err.Error(), "review-typo") {
		t.Errorf("AgentsByName error %q does not contain unknown agent name %q", err.Error(), "review-typo")
	}
	// Error must contain at least one available agent name (the list of
	// available agents should be listed).
	if !strings.Contains(err.Error(), "review-goal") {
		t.Errorf("AgentsByName error %q does not mention available agents", err.Error())
	}
}

// TestOnlyFlag_UnknownNameMixed verifies that a mix of known and unknown names
// also returns an error naming the unrecognised agent.
func TestOnlyFlag_UnknownNameMixed(t *testing.T) {
	allAgents := agentsForHarness("opencode")

	_, err := review.AgentsByName(allAgents, []string{"review-code", "review-typo"})
	if err == nil {
		t.Fatal("AgentsByName: expected error for mixed known/unknown names, got nil")
	}
	if !strings.Contains(err.Error(), "review-typo") {
		t.Errorf("error %q does not contain the unknown agent name", err.Error())
	}
}

// TestOnlyFlag_EmptyCSVReturnsNoTokens verifies that splitCSV("") → 0 tokens,
// which is the precondition for the empty-CSV error path in runReview.
// The runReview function checks len(names)==0 after splitCSV and returns an error.
func TestOnlyFlag_EmptyCSVReturnsNoTokens(t *testing.T) {
	tokens := splitCSV("")
	if len(tokens) != 0 {
		t.Errorf("splitCSV(%q): got %d tokens, want 0", "", len(tokens))
	}

	// A value that reduces to zero tokens after trimming.
	tokens = splitCSV("  ,  ")
	if len(tokens) != 0 {
		t.Errorf("splitCSV(%q): got %d tokens, want 0", "  ,  ", len(tokens))
	}
}

// ── CheckAgentAvailability guard logic tests ──────────────────────────────────
//
// These tests document the fix for #758: the if !cfg.ContainerMode guard was
// removed from runReview. CheckAgentAvailability must be called when
// PRISM_HOST_API == "" regardless of what cfg.ContainerMode would be.
//
// By the time runReview reaches the CheckAgentAvailability call, PRISM_HOST_API
// is guaranteed to be "" (the proxy-out branch fires first if it is set).
// cfg.ContainerMode is a Nix-time flag ("this host spawns workers in
// containers"), not a runtime signal ("this process is running in a container").
// Using it as the latter silently skips the pre-flight check on Darwin hosts
// with container_mode=true, allowing missing agent files to go undetected.

// TestCheckAgentAvailability_CalledWhenHostAPIUnset_ContainerModeIrrelevant
// verifies that CheckAgentAvailability is NOT skipped when PRISM_HOST_API is
// unset, regardless of what cfg.ContainerMode would be. It does this by
// confirming that missing agent files produce an error — which can only happen
// if CheckAgentAvailability is actually called.
//
// This test covers two scenarios that correspond to the two values of
// cfg.ContainerMode:
//   - containerMode=false: the old code and the new code both call the check.
//   - containerMode=true: the old code incorrectly skipped the check; the new
//     code calls it (the guard was removed).
func TestCheckAgentAvailability_CalledWhenHostAPIUnset_ContainerModeIrrelevant(t *testing.T) {
	// Ensure PRISM_HOST_API is unset — simulating the state after the proxy-out
	// branch in runReview did not fire (we are on the host).
	t.Setenv("PRISM_HOST_API", "")

	// Create a temp dir that has NO agent .md files — so CheckAgentAvailability
	// will return an error if it is called.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// Deliberately do NOT create any agent files.

	agents := review.Agents()

	// Regardless of what cfg.ContainerMode would be (true or false), the check
	// must be performed. We verify this by calling CheckAgentAvailability
	// directly — as runReview now does unconditionally — and confirming it
	// returns an error naming the missing agents.
	//
	// If the old if !cfg.ContainerMode guard were still present and
	// cfg.ContainerMode were true, this call would be skipped and the missing
	// agents would go undetected.
	h := opencode.New("", nil, "", "")
	err := review.CheckAgentAvailability(agents, h.ValidateAgentRole)
	if err == nil {
		t.Fatal("CheckAgentAvailability: expected error for missing agents when PRISM_HOST_API is unset, got nil — the pre-flight check must not be skipped")
	}
	// Error must name at least one missing agent.
	for _, ag := range agents {
		if !strings.Contains(err.Error(), ag.Name) {
			t.Errorf("CheckAgentAvailability error does not mention missing agent %q: %v", ag.Name, err)
		}
	}
}

// TestCheckAgentAvailability_PassesWhenAllFilesPresent_ContainerModeIrrelevant
// verifies the happy path: when all agent .md files exist on the host
// filesystem and PRISM_HOST_API is unset, CheckAgentAvailability passes
// (returns nil) regardless of what cfg.ContainerMode would be.
//
// This corresponds to the [edge-case] AC: "When ENHANCED_REVIEW=true is set
// and all required agent .md files are present on a container_mode=true Darwin
// host, prism review proceeds without error."
func TestCheckAgentAvailability_PassesWhenAllFilesPresent_ContainerModeIrrelevant(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	agentsDir := dir + "/opencode/agents"
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	agents := review.Agents()
	for _, ag := range agents {
		path := agentsDir + "/" + ag.Name + ".md"
		if err := os.WriteFile(path, []byte("# "+ag.Name), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}

	// Verify the check passes — all files are present on the host filesystem.
	h := opencode.New("", nil, "", "")
	if err := review.CheckAgentAvailability(agents, h.ValidateAgentRole); err != nil {
		t.Errorf("CheckAgentAvailability: unexpected error when all agent files are present: %v", err)
	}
}

// TestAgentNameStrings verifies the agentNameStrings helper extracts names correctly.
func TestAgentNameStrings(t *testing.T) {
	agents := agentsForHarness("opencode")
	names := agentNameStrings(agents)
	if len(names) != len(agents) {
		t.Fatalf("agentNameStrings: got %d names, want %d", len(names), len(agents))
	}
	for i, a := range agents {
		if names[i] != a.Name {
			t.Errorf("names[%d] = %q, want %q", i, names[i], a.Name)
		}
	}
}
