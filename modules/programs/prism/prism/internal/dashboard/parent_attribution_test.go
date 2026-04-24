package dashboard_test

// Tests for parent-attribution consistency between the dashboard (SortDisplayed
// / BuildDisplayRows) and the flat session list (db.ParentSessionFor).
//
// These tests reproduce the bug described in issue #847: when a coordinator
// running on the @main branch spawns five review sessions named
// @main~review-1-review-*, the dashboard rendered them as children of the last
// depth-1 branch alphabetically before @main rather than as children of @main.
//
// The fix has two parts:
//  1. db.ParentSessionFor() — single named source of truth for parent
//     attribution, used by both views; backed by session_groups.parent_session
//     with a name-heuristic fallback for pre-migration rows.
//  2. SortDisplayed — uses the DB-backed ParentSession field (populated via
//     db.AllGroupParents()) so depth-2 children of @main sort immediately after
//     @main, not after all other depth-1 branches.

import (
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/dashboard"
	"github.com/prismatic-koi/prism/internal/db"
)

// openTestDBForDash opens a temp DB for dashboard tests.
func openTestDBForDash(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// seedSession creates a minimal agent_status row for testing.
func seedSession(t *testing.T, d *db.DB, sessionName, repo, worktree, state string) {
	t.Helper()
	if err := d.UpsertStatus(sessionName, repo, worktree, state, nil, nil); err != nil {
		t.Fatalf("seedSession(%q): %v", sessionName, err)
	}
}

// TestParentSessionFor_DBBacked verifies that db.ParentSessionFor returns the
// session_groups.parent_session value for post-migration sessions (group_id set).
func TestParentSessionFor_DBBacked(t *testing.T) {
	d := openTestDBForDash(t)
	repo := "nixos-config"

	// Seed parent session (coordinator @main).
	parentSession := repo + "@main"
	seedSession(t, d, parentSession, repo, "/worktrees/main", "active")

	// Register a group with the parent.
	groupID, err := d.RegisterGroup(parentSession)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// Seed five review sessions and assign them to the group.
	agents := []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"}
	var reviewSessions []string
	for _, ag := range agents {
		name := repo + "@main~review-1-" + ag
		reviewSessions = append(reviewSessions, name)
		seedSession(t, d, name, repo, "/worktrees/main", "active")
		if err := d.SetGroupID(name, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", name, err)
		}
	}

	// Verify ParentSessionFor returns the correct parent for each review session.
	for _, sess := range reviewSessions {
		got := d.ParentSessionFor(sess)
		if got != parentSession {
			t.Errorf("ParentSessionFor(%q) = %q, want %q", sess, got, parentSession)
		}
	}

	// Verify ParentSessionFor returns "" for the parent session itself.
	got := d.ParentSessionFor(parentSession)
	if got != "" {
		t.Errorf("ParentSessionFor(%q) = %q, want \"\" (no parent for top-level)", parentSession, got)
	}
}

// TestParentSessionFor_NameHeuristicFallback verifies that db.ParentSessionFor
// falls back to the name heuristic when group_id is not set (pre-migration row).
func TestParentSessionFor_NameHeuristicFallback(t *testing.T) {
	d := openTestDBForDash(t)
	repo := "nixos-config"

	// Seed review sessions WITHOUT setting group_id (pre-migration).
	sess := repo + "@feature~review-1-review-goal"
	seedSession(t, d, sess, repo, "/worktrees/feature", "finished")
	// group_id is NOT set — simulates a pre-migration row.

	got := d.ParentSessionFor(sess)
	want := repo + "@feature"
	if got != want {
		t.Errorf("ParentSessionFor(%q) = %q, want %q (name heuristic fallback)", sess, got, want)
	}
}

// TestParentSessionFor_MainParent_NameHeuristic verifies the name heuristic
// specifically for @main-parent sessions (the bug scenario from #847).
func TestParentSessionFor_MainParent_NameHeuristic(t *testing.T) {
	d := openTestDBForDash(t)
	repo := "nixos-config"

	// Seed review sessions without group_id (pre-migration fallback path).
	sess := repo + "@main~review-1-review-code"
	seedSession(t, d, sess, repo, "/worktrees/main", "finished")

	got := d.ParentSessionFor(sess)
	want := repo + "@main"
	if got != want {
		t.Errorf("ParentSessionFor(%q) = %q, want %q", sess, got, want)
	}
}

// TestSortDisplayed_MainParentChildrenSortAfterMain is the regression test for
// issue #847: review sessions spawned from @main must sort immediately after
// @main, not after other depth-1 branches whose names sort after "@d" etc.
//
// Before the fix, nixos-config@main~review-1-review-* would sort after
// nixos-config@design-prism-session-uniformity because the sort key used
// "\x01@main" (the \x01 band) which sorted after "\x01@design" but ALSO after
// all other \x01 band entries, so the review sessions appeared visually nested
// under @design-prism-session-uniformity instead of @main.
func TestSortDisplayed_MainParentChildrenSortAfterMain(t *testing.T) {
	sessions := []dashboard.AgentSession{
		{Name: "nixos-config@ci-pr-gate-workflow", AgentState: "idle"},
		{Name: "nixos-config@design-prism-session-uniformity", AgentState: "finished"},
		{Name: "nixos-config@main", AgentState: "active"},
		{Name: "nixos-config@main~review-1-review-goal", AgentState: "finished"},
		{Name: "nixos-config@main~review-1-review-code", AgentState: "finished"},
		{Name: "nixos-config@main~review-1-review-security", AgentState: "finished"},
		{Name: "nixos-config@main~review-1-review-qa", AgentState: "finished"},
		{Name: "nixos-config@main~review-1-review-context", AgentState: "idle"},
	}

	dashboard.SortDisplayed(sessions)

	// @main must be the FIRST session (it's the top-level session).
	if sessions[0].Name != "nixos-config@main" {
		t.Errorf("position 0: got %q, want nixos-config@main", sessions[0].Name)
	}

	// All five review sessions must appear BEFORE the other depth-1 branches
	// (@ci and @design), since they are children of @main.
	mainIdx := 0
	for i, s := range sessions {
		if s.Name == "nixos-config@main" {
			mainIdx = i
		}
	}

	// Find the range of review sessions.
	firstReviewIdx := -1
	lastReviewIdx := -1
	for i, s := range sessions {
		if dashboard.Depth2ParentBranch(s.Name) == "@main" {
			if firstReviewIdx < 0 {
				firstReviewIdx = i
			}
			lastReviewIdx = i
		}
	}
	if firstReviewIdx < 0 {
		t.Fatal("no review sessions found in sorted slice")
	}

	// Review sessions must immediately follow @main.
	if firstReviewIdx != mainIdx+1 {
		t.Errorf("first review session at position %d, want %d (right after @main at %d)", firstReviewIdx, mainIdx+1, mainIdx)
	}

	// All depth-1 branches must appear AFTER all review sessions.
	for i, s := range sessions {
		branch := dashboard.SessionBranch(s.Name)
		isDep1 := branch != s.Name && branch != "@main" && !dashboard.IsDepth2Session(s.Name)
		if isDep1 && i <= lastReviewIdx {
			t.Errorf("depth-1 session %q at position %d appears before or at last review session (position %d)", s.Name, i, lastReviewIdx)
		}
	}
}

// TestBothViews_ParentChildAgreement is the primary AC test: it exercises both
// views (dashboard SortDisplayed+BuildDisplayRows and db.ParentSessionFor)
// against the same DB state and asserts identical parent-child structure.
//
// Scenario (reproducing issue #847):
//   - Coordinator: nixos-config@main
//   - Worker:      nixos-config@design-prism-session-uniformity
//   - Review sessions spawned by @main coordinator: 5 × @main~review-1-<agent>
//   - One more worker branch @ci-pr-gate-workflow
func TestBothViews_ParentChildAgreement(t *testing.T) {
	d := openTestDBForDash(t)
	repo := "nixos-config"

	// Set up sessions in the DB.
	coordinator := repo + "@main"
	worker1 := repo + "@design-prism-session-uniformity"
	worker2 := repo + "@ci-pr-gate-workflow"
	seedSession(t, d, coordinator, repo, "/wt/main", "active")
	seedSession(t, d, worker1, repo, "/wt/design", "finished")
	seedSession(t, d, worker2, repo, "/wt/ci", "idle")

	// Register a group for the review sessions (parent = coordinator @main).
	groupID, err := d.RegisterGroup(coordinator)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	agents := []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"}
	reviewNames := make([]string, len(agents))
	for i, ag := range agents {
		name := repo + "@main~review-1-" + ag
		reviewNames[i] = name
		seedSession(t, d, name, repo, "/wt/main", "finished")
		if err := d.SetGroupID(name, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", name, err)
		}
	}

	// ── View 1: db.ParentSessionFor (the source-of-truth helper) ────────────
	for _, name := range reviewNames {
		parent := d.ParentSessionFor(name)
		if parent != coordinator {
			t.Errorf("db.ParentSessionFor(%q) = %q, want %q", name, parent, coordinator)
		}
	}

	// ── View 2: dashboard SortDisplayed + BuildDisplayRows ──────────────────
	// Fetch group parents (as FetchSessionsFromDB would) and build AgentSessions.
	groupParents, err := d.AllGroupParents()
	if err != nil {
		t.Fatalf("AllGroupParents: %v", err)
	}

	allStatuses, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}

	var agentSessions []dashboard.AgentSession
	for _, s := range allStatuses {
		agentSessions = append(agentSessions, dashboard.StatusToAgentSession(s, nil, groupParents))
	}

	// Verify that each review session's ParentSession field is correctly set.
	for _, as := range agentSessions {
		if dashboard.IsDepth2Session(as.Name) {
			if as.ParentSession != coordinator {
				t.Errorf("AgentSession(%q).ParentSession = %q, want %q", as.Name, as.ParentSession, coordinator)
			}
		}
	}

	// Sort and build display rows.
	dashboard.SortDisplayed(agentSessions)

	// After sort: @main must be first; review sessions must immediately follow @main.
	if len(agentSessions) == 0 {
		t.Fatal("no sessions after sort")
	}
	if agentSessions[0].Name != coordinator {
		t.Errorf("position 0: got %q, want %q (coordinator should be first)", agentSessions[0].Name, coordinator)
	}

	// All review sessions must appear before the depth-1 branches.
	lastReviewPos := -1
	firstDepth1Pos := -1
	for i, s := range agentSessions {
		if dashboard.IsDepth2Session(s.Name) {
			lastReviewPos = i
		}
		branch := dashboard.SessionBranch(s.Name)
		isDepth1 := branch != s.Name && branch != "@main" && !dashboard.IsDepth2Session(s.Name)
		if isDepth1 && firstDepth1Pos < 0 {
			firstDepth1Pos = i
		}
	}

	if lastReviewPos < 0 {
		t.Fatal("no depth-2 review sessions found after sort")
	}
	if firstDepth1Pos >= 0 && firstDepth1Pos <= lastReviewPos {
		t.Errorf("depth-1 branch appears before review sessions: first depth-1 at position %d, last review at position %d", firstDepth1Pos, lastReviewPos)
	}

	// Build display rows and verify the virtual group row reports @main as parent.
	displayRows, _ := dashboard.BuildDisplayRows(agentSessions, nil, "")

	// The virtual group row should have been created.
	var groupRow *dashboard.AgentSession
	for i := range displayRows {
		if displayRows[i].IsReviewGroup {
			groupRow = &displayRows[i]
			break
		}
	}
	if groupRow == nil {
		t.Fatal("no review group row found in BuildDisplayRows output")
	}

	// The group row's name should be "nixos-config@main~review-1".
	wantGroupName := repo + "@main~review-1"
	if groupRow.Name != wantGroupName {
		t.Errorf("group row Name = %q, want %q", groupRow.Name, wantGroupName)
	}

	// ── Agreement assertion: both views agree on parent attribution ──────────
	// For each review session, db.ParentSessionFor must return the same parent
	// that the session name encodes (i.e. the group row's parent prefix).
	for _, name := range reviewNames {
		dbParent := d.ParentSessionFor(name)
		dashParent := dashboard.Depth2ParentBranch(name)
		// dashParent is "@main"; dbParent is "nixos-config@main".
		// Construct full name from dash parent for comparison.
		fullDashParent := repo + dashParent
		if dbParent != fullDashParent {
			t.Errorf("parent attribution disagrees for %q: db says %q, name-heuristic says %q",
				name, dbParent, fullDashParent)
		}
	}
}

// TestBothViews_WorkerSpawnedReview verifies the correct case: review sessions
// spawned by a WORKER (not coordinator) are attributed to the worker branch.
// Edge-case AC: "review sessions spawned from a worker have consistent parent
// attribution across both views, matching the worker they were spawned from."
func TestBothViews_WorkerSpawnedReview(t *testing.T) {
	d := openTestDBForDash(t)
	repo := "nixos-config"

	coordinator := repo + "@main"
	worker := repo + "@feature-branch"
	seedSession(t, d, coordinator, repo, "/wt/main", "active")
	seedSession(t, d, worker, repo, "/wt/feature", "active")

	// Register group with the WORKER as parent.
	groupID, err := d.RegisterGroup(worker)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	agents := []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"}
	reviewNames := make([]string, len(agents))
	for i, ag := range agents {
		name := repo + "@feature-branch~review-1-" + ag
		reviewNames[i] = name
		seedSession(t, d, name, repo, "/wt/feature", "finished")
		if err := d.SetGroupID(name, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", name, err)
		}
	}

	// db.ParentSessionFor must attribute each review session to the worker.
	for _, name := range reviewNames {
		got := d.ParentSessionFor(name)
		if got != worker {
			t.Errorf("db.ParentSessionFor(%q) = %q, want %q", name, got, worker)
		}
	}

	// Dashboard: review sessions should sort immediately after @feature-branch.
	groupParents, err := d.AllGroupParents()
	if err != nil {
		t.Fatalf("AllGroupParents: %v", err)
	}
	allStatuses, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}
	var agentSessions []dashboard.AgentSession
	for _, s := range allStatuses {
		agentSessions = append(agentSessions, dashboard.StatusToAgentSession(s, nil, groupParents))
	}

	// Verify ParentSession field.
	for _, as := range agentSessions {
		if dashboard.IsDepth2Session(as.Name) {
			if as.ParentSession != worker {
				t.Errorf("AgentSession(%q).ParentSession = %q, want %q", as.Name, as.ParentSession, worker)
			}
		}
	}

	dashboard.SortDisplayed(agentSessions)

	// @main is top-level → appears first.
	// @feature-branch is depth-1 → appears after @main.
	// @feature-branch~review-1-* are depth-2 → must appear right after @feature-branch.
	workerIdx := -1
	firstReviewIdx := -1
	for i, s := range agentSessions {
		if s.Name == worker {
			workerIdx = i
		}
		if dashboard.IsDepth2Session(s.Name) && firstReviewIdx < 0 {
			firstReviewIdx = i
		}
	}
	if workerIdx < 0 {
		t.Fatal("worker session not found in sorted slice")
	}
	if firstReviewIdx < 0 {
		t.Fatal("no depth-2 review sessions found in sorted slice")
	}
	if firstReviewIdx != workerIdx+1 {
		t.Errorf("first review session at position %d, want %d (right after worker at %d)", firstReviewIdx, workerIdx+1, workerIdx)
	}

	// db.ParentSessionFor and the dashboard's ParentSession field must agree.
	for _, as := range agentSessions {
		if !dashboard.IsDepth2Session(as.Name) {
			continue
		}
		dbParent := d.ParentSessionFor(as.Name)
		dashParent := as.ParentSession
		if dbParent != dashParent {
			t.Errorf("parent attribution disagrees for %q: db.ParentSessionFor=%q, AgentSession.ParentSession=%q",
				as.Name, dbParent, dashParent)
		}
	}
}

// TestAllGroupParents_BatchQuery verifies that db.AllGroupParents returns all
// registered groups in a single batch query.
func TestAllGroupParents_BatchQuery(t *testing.T) {
	d := openTestDBForDash(t)
	repo := "nixos-config"

	parent1 := repo + "@main"
	parent2 := repo + "@feature"
	seedSession(t, d, parent1, repo, "/wt/main", "active")
	seedSession(t, d, parent2, repo, "/wt/feature", "active")

	gid1, err := d.RegisterGroup(parent1)
	if err != nil {
		t.Fatalf("RegisterGroup(%q): %v", parent1, err)
	}
	gid2, err := d.RegisterGroup(parent2)
	if err != nil {
		t.Fatalf("RegisterGroup(%q): %v", parent2, err)
	}

	got, err := d.AllGroupParents()
	if err != nil {
		t.Fatalf("AllGroupParents: %v", err)
	}

	if got[gid1] != parent1 {
		t.Errorf("AllGroupParents[%q] = %q, want %q", gid1, got[gid1], parent1)
	}
	if got[gid2] != parent2 {
		t.Errorf("AllGroupParents[%q] = %q, want %q", gid2, got[gid2], parent2)
	}
}

// TestStatusToAgentSession_PopulatesParentSession verifies that StatusToAgentSession
// correctly populates the ParentSession field from the groupParents map.
func TestStatusToAgentSession_PopulatesParentSession(t *testing.T) {
	parentSession := "nixos-config@main"
	groupID := "test-group-id"

	// Simulate a db.Status for a review session with a group_id.
	gid := groupID
	status := db.Status{
		SessionName: "nixos-config@main~review-1-review-goal",
		Repo:        "nixos-config",
		Worktree:    "/wt/main",
		State:       "finished",
		GroupID:     &gid,
	}

	groupParents := map[string]string{
		groupID: parentSession,
	}

	as := dashboard.StatusToAgentSession(status, nil, groupParents)
	if as.ParentSession != parentSession {
		t.Errorf("ParentSession = %q, want %q", as.ParentSession, parentSession)
	}
}

// TestStatusToAgentSession_FallsBackToNameHeuristic verifies that when
// group_id is nil (pre-migration row), the name heuristic is used.
func TestStatusToAgentSession_FallsBackToNameHeuristic(t *testing.T) {
	status := db.Status{
		SessionName: "nixos-config@feature~review-1-review-goal",
		Repo:        "nixos-config",
		Worktree:    "/wt/feature",
		State:       "finished",
		GroupID:     nil, // pre-migration: no group_id
	}

	as := dashboard.StatusToAgentSession(status, nil, nil)
	want := "nixos-config@feature"
	if as.ParentSession != want {
		t.Errorf("ParentSession = %q, want %q (name heuristic fallback)", as.ParentSession, want)
	}
}

// TestSortDisplayed_WithDBBackedParent verifies that when AgentSession.ParentSession
// is set from the DB (post-migration), SortDisplayed correctly uses it for sort
// ordering. This ensures the DB-backed parent wins over any name-derived parent.
func TestSortDisplayed_WithDBBackedParent(t *testing.T) {
	// Simulate the exact scenario from issue #847: coordinator at @main,
	// sessions named @main~review-*, DB records parent as @main.
	sessions := []dashboard.AgentSession{
		{Name: "nixos-config@main", AgentState: "active"},
		{Name: "nixos-config@ci-pr-gate-workflow", AgentState: "idle"},
		{Name: "nixos-config@design-prism-session-uniformity", AgentState: "finished"},
		{
			Name:          "nixos-config@main~review-1-review-goal",
			AgentState:    "finished",
			ParentSession: "nixos-config@main", // DB-backed
		},
		{
			Name:          "nixos-config@main~review-1-review-code",
			AgentState:    "finished",
			ParentSession: "nixos-config@main",
		},
		{
			Name:          "nixos-config@main~review-1-review-security",
			AgentState:    "finished",
			ParentSession: "nixos-config@main",
		},
		{
			Name:          "nixos-config@main~review-1-review-qa",
			AgentState:    "finished",
			ParentSession: "nixos-config@main",
		},
		{
			Name:          "nixos-config@main~review-1-review-context",
			AgentState:    "idle",
			ParentSession: "nixos-config@main",
		},
	}

	dashboard.SortDisplayed(sessions)

	// Position 0: @main.
	if sessions[0].Name != "nixos-config@main" {
		t.Errorf("position 0: got %q, want nixos-config@main", sessions[0].Name)
	}

	// Positions 1-5: review sessions (children of @main, immediately after it).
	for i := 1; i <= 5; i++ {
		if !dashboard.IsDepth2Session(sessions[i].Name) {
			t.Errorf("position %d: got %q, want a depth-2 review session (child of @main)", i, sessions[i].Name)
		}
	}

	// Positions 6-7: the depth-1 branches (@ci and @design) appear AFTER reviews.
	for i := 6; i < len(sessions); i++ {
		if dashboard.IsDepth2Session(sessions[i].Name) {
			t.Errorf("position %d: got depth-2 session %q, want a depth-1 branch (all reviews should be before depth-1 children)", i, sessions[i].Name)
		}
	}
}


