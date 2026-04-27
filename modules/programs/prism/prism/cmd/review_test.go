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

// ── resolveParentIsolationMode tests ──────────────────────────────────────────
//
// These tests cover the DB lookup added by issue #1034: prism review must use
// the parent session's recorded isolation_mode, not the machine-level default
// from cfg.EffectiveIsolationMode(). On navi the machine default is "podman",
// but worker sessions run as "bwrap"; using the wrong mode causes agent-run to
// reject the spawned review agents.

// TestResolveParentIsolationMode_Bwrap verifies that a session recorded as
// "bwrap" in the DB returns "bwrap" from resolveParentIsolationMode.
func TestResolveParentIsolationMode_Bwrap(t *testing.T) {
	d := openReviewTestDB(t)

	const session = "nixos-config@fix-review-isolation-mode"
	if err := d.UpsertStatus(session, "nixos-config", "/worktree/fix", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetIsolationMode(session, "bwrap"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}

	got := resolveParentIsolationMode(session)
	if got != "bwrap" {
		t.Errorf("resolveParentIsolationMode = %q, want %q", got, "bwrap")
	}
}

// TestResolveParentIsolationMode_Podman verifies that a session recorded as
// "podman" in the DB returns "podman" from resolveParentIsolationMode.
func TestResolveParentIsolationMode_Podman(t *testing.T) {
	d := openReviewTestDB(t)

	const session = "nixos-config@podman-worker"
	if err := d.UpsertStatus(session, "nixos-config", "/worktree/podman", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetIsolationMode(session, "podman"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}

	got := resolveParentIsolationMode(session)
	if got != "podman" {
		t.Errorf("resolveParentIsolationMode = %q, want %q", got, "podman")
	}
}

// TestResolveParentIsolationMode_PreV10HostModeTrue verifies that a pre-v10
// back-compat row with isolation_mode="" but host_mode=true returns "host" from
// resolveParentIsolationMode via status.EffectiveIsolationMode(). This is the
// back-compat behaviour established in PR #882 / restore.go.
func TestResolveParentIsolationMode_PreV10HostModeTrue(t *testing.T) {
	d := openReviewTestDB(t)

	const session = "nixos-config@legacy-host-session"
	// UpsertStatus leaves isolation_mode NULL. SetHostMode sets host_mode=1.
	if err := d.UpsertStatus(session, "nixos-config", "/worktree/legacy", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetHostMode(session, true); err != nil {
		t.Fatalf("SetHostMode: %v", err)
	}

	got := resolveParentIsolationMode(session)
	if got != "host" {
		t.Errorf("resolveParentIsolationMode for host_mode=true, isolation_mode=NULL = %q, want %q", got, "host")
	}
}

// TestResolveParentIsolationMode_PreV10HostModeFalse verifies that a pre-v10
// back-compat row with isolation_mode="" and host_mode=false returns "podman"
// (the EffectiveIsolationMode default for legacy container sessions).
func TestResolveParentIsolationMode_PreV10HostModeFalse(t *testing.T) {
	d := openReviewTestDB(t)

	const session = "nixos-config@legacy-podman-session"
	// UpsertStatus leaves both isolation_mode NULL and host_mode=0.
	if err := d.UpsertStatus(session, "nixos-config", "/worktree/legacy", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	got := resolveParentIsolationMode(session)
	if got != "podman" {
		t.Errorf("resolveParentIsolationMode for host_mode=false, isolation_mode=NULL = %q, want %q", got, "podman")
	}
}

// TestResolveParentIsolationMode_EmptyWhenSessionMissing verifies that when the
// parent session has no DB row at all, resolveParentIsolationMode returns ""
// (triggering the cfg.EffectiveIsolationMode() fallback).
func TestResolveParentIsolationMode_EmptyWhenSessionMissing(t *testing.T) {
	openReviewTestDB(t) // empty DB

	got := resolveParentIsolationMode("nixos-config@nonexistent")
	if got != "" {
		t.Errorf("resolveParentIsolationMode for missing session = %q, want %q", got, "")
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
// removed from runReview. CheckAgentAvailability is now called unconditionally
// when PRISM_HOST_API == "". By the time runReview reaches the
// CheckAgentAvailability call, PRISM_HOST_API is guaranteed to be "" (the
// proxy-out branch fires first if it is set).

// TestCheckAgentAvailability_CalledWhenHostAPIUnset verifies that
// CheckAgentAvailability is NOT skipped when PRISM_HOST_API is unset. It does
// this by confirming that missing agent files produce an error — which can only
// happen if CheckAgentAvailability is actually called.
func TestCheckAgentAvailability_CalledWhenHostAPIUnset(t *testing.T) {
	// Ensure PRISM_HOST_API is unset — simulating the state after the proxy-out
	// branch in runReview did not fire (we are on the host).
	t.Setenv("PRISM_HOST_API", "")

	// Create a temp dir that has NO agent .md files — so CheckAgentAvailability
	// will return an error if it is called.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// Deliberately do NOT create any agent files.

	agents := review.Agents()

	// The check must be performed unconditionally. We verify this by calling
	// CheckAgentAvailability directly — as runReview now does — and confirming
	// it returns an error naming the missing agents.
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

// TestCheckAgentAvailability_PassesWhenAllFilesPresent verifies the happy path:
// when all agent .md files exist on the host filesystem and PRISM_HOST_API is
// unset, CheckAgentAvailability passes (returns nil).
func TestCheckAgentAvailability_PassesWhenAllFilesPresent(t *testing.T) {
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

// ── rejectIfCoordinator tests ─────────────────────────────────────────────────
//
// These tests cover the coordinator guard added by issue #846.
// All tests set PRISM_SESSION_NAME (and TMUX="" to avoid tmux look-ups) to
// control which session is detected, and set testDBPath to an isolated temp DB.

// TestRejectIfCoordinator_BlocksCoordinatorSession verifies that a session
// whose root_agent_name is "coordinator" in the DB causes rejectIfCoordinator
// to return a non-nil error. This is the primary functional AC: a coordinator
// session running prism review must be rejected before any sessions spawn.
func TestRejectIfCoordinator_BlocksCoordinatorSession(t *testing.T) {
	d := openReviewTestDB(t)

	const coordSession = "nixos-config@main"
	if err := d.UpsertStatusSeedRootAgentName(coordSession, "nixos-config", "/worktree/main", "idle", nil, nil, "coordinator"); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}

	// Make review.LookupParentSession return the coordinator session.
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "") // ensure no tmux fallback

	err := rejectIfCoordinator()
	if err == nil {
		t.Fatal("rejectIfCoordinator: expected error for coordinator session, got nil — coordinator must be blocked")
	}
	if !strings.Contains(err.Error(), "worker sessions only") {
		t.Errorf("rejectIfCoordinator error %q does not mention 'worker sessions only'", err.Error())
	}
	if !strings.Contains(err.Error(), "prism pr") {
		t.Errorf("rejectIfCoordinator error %q does not mention 'prism pr'", err.Error())
	}
}

// TestRejectIfCoordinator_AllowsWorkerSession verifies that a session whose
// root_agent_name is "worker" in the DB is NOT blocked by rejectIfCoordinator.
// This is the regression AC: worker sessions must continue to work as before.
func TestRejectIfCoordinator_AllowsWorkerSession(t *testing.T) {
	d := openReviewTestDB(t)

	const workerSession = "nixos-config@feature-branch"
	if err := d.UpsertStatusSeedRootAgentName(workerSession, "nixos-config", "/worktree/feature-branch", "idle", nil, nil, "worker"); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}

	t.Setenv("PRISM_SESSION_NAME", workerSession)
	t.Setenv("TMUX", "")

	if err := rejectIfCoordinator(); err != nil {
		t.Errorf("rejectIfCoordinator: unexpected error for worker session: %v", err)
	}
}

// TestRejectIfCoordinator_NullRootAgentName_MainFallback verifies that a
// pre-migration row with NULL root_agent_name and a @main session name causes
// rejectIfCoordinator to fall back to the name-suffix heuristic and return an
// error (the @main heuristic identifies it as a coordinator).
func TestRejectIfCoordinator_NullRootAgentName_MainFallback(t *testing.T) {
	d := openReviewTestDB(t)

	// UpsertStatus leaves root_agent_name NULL (pre-migration path).
	const coordSession = "nixos-config@main"
	if err := d.UpsertStatus(coordSession, "nixos-config", "/worktree/main", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	// The heuristic should identify nixos-config@main as a coordinator.
	err := rejectIfCoordinator()
	if err == nil {
		t.Fatal("rejectIfCoordinator: expected error for @main session with NULL root_agent_name, got nil")
	}
	if !strings.Contains(err.Error(), "worker sessions only") {
		t.Errorf("rejectIfCoordinator error %q does not mention 'worker sessions only'", err.Error())
	}
}

// TestRejectIfCoordinator_NullRootAgentName_WorkerFallback verifies that a
// pre-migration row with NULL root_agent_name and a non-@main session name
// does NOT cause rejectIfCoordinator to block (heuristic: not @main → worker).
func TestRejectIfCoordinator_NullRootAgentName_WorkerFallback(t *testing.T) {
	d := openReviewTestDB(t)

	const workerSession = "nixos-config@feature-branch"
	if err := d.UpsertStatus(workerSession, "nixos-config", "/worktree/feature-branch", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	t.Setenv("PRISM_SESSION_NAME", workerSession)
	t.Setenv("TMUX", "")

	if err := rejectIfCoordinator(); err != nil {
		t.Errorf("rejectIfCoordinator: unexpected error for non-@main session with NULL root_agent_name: %v", err)
	}
}

// TestRejectIfCoordinator_NoSession_NoError verifies that when no session can
// be determined (PRISM_SESSION_NAME unset, TMUX unset), rejectIfCoordinator
// returns nil — ad-hoc (non-tmux) invocations must not be blocked.
func TestRejectIfCoordinator_NoSession_NoError(t *testing.T) {
	openReviewTestDB(t) // use isolated DB even though we don't query it

	t.Setenv("PRISM_SESSION_NAME", "")
	t.Setenv("TMUX", "")

	if err := rejectIfCoordinator(); err != nil {
		t.Errorf("rejectIfCoordinator: unexpected error when no session can be determined: %v", err)
	}
}

// TestRejectIfCoordinator_NameHeuristicMainBlocked verifies that a @main
// session blocks even when the DB has no row for it (no migration yet).
// This covers the pure heuristic path for unregistered sessions.
func TestRejectIfCoordinator_NameHeuristicMainBlocked(t *testing.T) {
	openReviewTestDB(t) // empty DB — no rows

	t.Setenv("PRISM_SESSION_NAME", "nixos-config@main")
	t.Setenv("TMUX", "")

	err := rejectIfCoordinator()
	if err == nil {
		t.Fatal("rejectIfCoordinator: expected error for @main session with no DB row, got nil")
	}
}
