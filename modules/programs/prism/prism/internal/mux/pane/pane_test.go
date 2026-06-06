// Tests for the pane package — the typed data model the four parallel
// consumer packages (#2152 render, #2153 server, #2155 state, #2156
// persist) all sit on top of. The tests pin the invariants the AC on
// #2151 calls out:
//
//  1. Tree consistency under every operation — no orphaned panes, no
//     sessions outside a repo cluster, no review subsessions outside a
//     parent session.
//  2. Concurrent access produces no race under `go test -race` (see
//     race_test.go for the goroutine fan-out).
//  3. JSON round-trip preserves state and rejects malformed input.
//
// Tests construct a fresh tree per case and never touch global state —
// $HOME, $XDG_STATE_HOME, the on-disk DB. The suite is therefore
// homeless-shelter-clean on every run.
package pane

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// mustAddSession is a tiny test helper — fail the test on AddSession
// error so the happy-path setup in each case stays linear.
func mustAddSession(t *testing.T, tree *SessionTree, s Session) {
	t.Helper()
	if err := tree.AddSession(s); err != nil {
		t.Fatalf("AddSession(%q): %v", s.ID, err)
	}
}

func mustAddPane(t *testing.T, tree *SessionTree, sessionID, paneName string) {
	t.Helper()
	if err := tree.AddPane(sessionID, Pane{Name: paneName}); err != nil {
		t.Fatalf("AddPane(%q, %q): %v", sessionID, paneName, err)
	}
}

// --- Construction & basic accessors ----------------------------------------

// TestNewTreeIsEmpty pins the constructor contract — a fresh tree has
// zero sessions, zero repos, and an empty ActiveSession. The render
// layer (#2152) relies on these zero-valued accessors not panicking
// before the first AddSession lands.
func TestNewTreeIsEmpty(t *testing.T) {
	tree := New()
	if got := tree.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
	if got := tree.Repos(); len(got) != 0 {
		t.Errorf("Repos() = %v, want empty", got)
	}
	if got := tree.Sessions(); len(got) != 0 {
		t.Errorf("Sessions() = %v, want empty", got)
	}
	if got := tree.ActiveSessionID(); got != "" {
		t.Errorf("ActiveSessionID() = %q, want \"\"", got)
	}
	if err := tree.Validate(); err != nil {
		t.Errorf("Validate() on fresh tree: %v", err)
	}
}

// TestAddSessionTopLevel walks the happy path for adding a top-level
// session and verifies the repo cluster is recorded.
func TestAddSessionTopLevel(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{
		ID:          "nixos-config@main",
		Repo:        "nixos-config",
		Branch:      "main",
		Worktree:    "/home/ben/code/nixos-config",
		AgentRole:   "coordinator",
		SidecarAddr: "/run/prism/host.sock",
	})

	if got := tree.Repos(); !reflect.DeepEqual(got, []string{"nixos-config"}) {
		t.Errorf("Repos() = %v, want [nixos-config]", got)
	}
	if got := tree.RepoSessions("nixos-config"); !reflect.DeepEqual(got, []string{"nixos-config@main"}) {
		t.Errorf("RepoSessions = %v, want [nixos-config@main]", got)
	}
	s, ok := tree.Session("nixos-config@main")
	if !ok {
		t.Fatal("Session lookup failed for nixos-config@main")
	}
	if s.Branch != "main" || s.AgentRole != "coordinator" {
		t.Errorf("Session round-trip mismatched: %+v", s)
	}
}

// TestAddSessionRejectsEmptyID and TestAddSessionRejectsDuplicate pin
// the schema validations on AddSession.
func TestAddSessionRejectsEmptyID(t *testing.T) {
	tree := New()
	err := tree.AddSession(Session{Repo: "x"})
	if !errors.Is(err, ErrInvalidSession) {
		t.Errorf("AddSession(empty ID) = %v, want ErrInvalidSession", err)
	}
}

func TestAddSessionRejectsTopLevelWithoutRepo(t *testing.T) {
	tree := New()
	err := tree.AddSession(Session{ID: "lonely"})
	if !errors.Is(err, ErrInvalidSession) {
		t.Errorf("AddSession(top-level, empty Repo) = %v, want ErrInvalidSession", err)
	}
}

func TestAddSessionRejectsDuplicate(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "a", Repo: "r"})
	err := tree.AddSession(Session{ID: "a", Repo: "r"})
	if !errors.Is(err, ErrSessionExists) {
		t.Errorf("AddSession(duplicate) = %v, want ErrSessionExists", err)
	}
}

// TestAddSessionReviewSubsession exercises the parent-child case the
// §3.1 hierarchy is built around.
func TestAddSessionReviewSubsession(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "repo@feature", Repo: "repo"})
	mustAddSession(t, tree, Session{
		ID:        "repo@feature~review-1-review-code",
		ParentID:  "repo@feature",
		AgentRole: "review-code",
	})

	// Repo on the child should have been backfilled from the parent.
	child, ok := tree.Session("repo@feature~review-1-review-code")
	if !ok {
		t.Fatal("child session not found")
	}
	if child.Repo != "repo" {
		t.Errorf("child.Repo = %q, want %q (backfilled from parent)", child.Repo, "repo")
	}
	if !child.IsReview() {
		t.Error("IsReview() = false on child with ParentID set")
	}

	// Tree-order iteration places the child directly after its parent.
	ids := sessionIDs(tree.Sessions())
	want := []string{"repo@feature", "repo@feature~review-1-review-code"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("Sessions() iteration order = %v, want %v", ids, want)
	}

	// Children accessor returns the child.
	if got := tree.Children("repo@feature"); !reflect.DeepEqual(got, []string{"repo@feature~review-1-review-code"}) {
		t.Errorf("Children = %v, want one entry", got)
	}
}

// TestAddSessionRejectsMissingParent and TestAddSessionRejectsNestedReview
// pin the §3.1 invariants: reviews must have a parent, and reviews cannot
// themselves have children.
func TestAddSessionRejectsMissingParent(t *testing.T) {
	tree := New()
	err := tree.AddSession(Session{
		ID:       "orphan~review-1-review-code",
		ParentID: "does-not-exist",
	})
	if !errors.Is(err, ErrParentNotFound) {
		t.Errorf("AddSession(orphan review) = %v, want ErrParentNotFound", err)
	}
}

func TestAddSessionRejectsNestedReview(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "repo@feature", Repo: "repo"})
	mustAddSession(t, tree, Session{ID: "repo@feature~review-1-review-code", ParentID: "repo@feature"})
	err := tree.AddSession(Session{
		ID:       "repo@feature~review-1-review-code~oops",
		ParentID: "repo@feature~review-1-review-code",
	})
	if !errors.Is(err, ErrParentIsReview) {
		t.Errorf("AddSession(nested review) = %v, want ErrParentIsReview", err)
	}
}

// TestAddSessionReviewRepoMustMatchParent — if the caller sets Repo on a
// review subsession, it must match the parent's Repo. Avoids silent
// inconsistency in callers that build the struct from two unrelated
// sources.
func TestAddSessionReviewRepoMustMatchParent(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "a@x", Repo: "a"})
	err := tree.AddSession(Session{
		ID:       "a@x~review-1-r",
		Repo:     "b", // wrong
		ParentID: "a@x",
	})
	if !errors.Is(err, ErrInvalidSession) {
		t.Errorf("AddSession(mismatched repo) = %v, want ErrInvalidSession", err)
	}
}

// --- RemoveSession ----------------------------------------------------------

// TestRemoveSessionCascadesReviews pins the §3.1 invariant in reverse:
// removing a top-level session drops every one of its review subs.
// Without this, the next snapshot would carry orphan children.
func TestRemoveSessionCascadesReviews(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "repo@feature", Repo: "repo"})
	mustAddSession(t, tree, Session{ID: "repo@feature~review-1-code", ParentID: "repo@feature"})
	mustAddSession(t, tree, Session{ID: "repo@feature~review-1-goal", ParentID: "repo@feature"})

	if err := tree.RemoveSession("repo@feature"); err != nil {
		t.Fatalf("RemoveSession: %v", err)
	}

	if tree.HasSession("repo@feature") {
		t.Error("top-level session still present after remove")
	}
	if tree.HasSession("repo@feature~review-1-code") {
		t.Error("review subsession should have been cascade-removed")
	}
	if tree.HasSession("repo@feature~review-1-goal") {
		t.Error("review subsession should have been cascade-removed")
	}
	if err := tree.Validate(); err != nil {
		t.Errorf("Validate after cascade: %v", err)
	}
}

// TestRemoveSessionLastInRepoDropsRepo verifies the §3.1 "header counts
// non-review sessions across all repos" invariant by way of the
// underlying RepoOrder bookkeeping — an empty repo is removed from the
// display order entirely.
func TestRemoveSessionLastInRepoDropsRepo(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "lonely@main", Repo: "lonely"})
	mustAddSession(t, tree, Session{ID: "keeper@main", Repo: "keeper"})

	if err := tree.RemoveSession("lonely@main"); err != nil {
		t.Fatalf("RemoveSession: %v", err)
	}
	if got := tree.Repos(); !reflect.DeepEqual(got, []string{"keeper"}) {
		t.Errorf("Repos() = %v, want [keeper]", got)
	}
	if got := tree.RepoSessions("lonely"); got != nil {
		t.Errorf("RepoSessions(lonely) = %v, want nil after last session removed", got)
	}
}

// TestRemoveSessionPreservesRepoOrderOfSiblings — removing one session
// from a multi-session repo keeps the remaining ones in their original
// insertion order (no reshuffle, no swap-and-pop).
func TestRemoveSessionPreservesRepoOrderOfSiblings(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "r@a", Repo: "r"})
	mustAddSession(t, tree, Session{ID: "r@b", Repo: "r"})
	mustAddSession(t, tree, Session{ID: "r@c", Repo: "r"})
	if err := tree.RemoveSession("r@b"); err != nil {
		t.Fatal(err)
	}
	if got := tree.RepoSessions("r"); !reflect.DeepEqual(got, []string{"r@a", "r@c"}) {
		t.Errorf("after remove middle: RepoSessions(r) = %v, want [r@a r@c]", got)
	}
}

func TestRemoveSessionUnknownID(t *testing.T) {
	tree := New()
	err := tree.RemoveSession("nope")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("RemoveSession(unknown) = %v, want ErrSessionNotFound", err)
	}
}

// TestRemoveSessionClearsActiveSession ensures the tree-level focus
// pointer never references a session that has been removed (UI would
// dereference a stale ID).
func TestRemoveSessionClearsActiveSession(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "r@x", Repo: "r"})
	if err := tree.ActivateSession("r@x"); err != nil {
		t.Fatal(err)
	}
	if err := tree.RemoveSession("r@x"); err != nil {
		t.Fatal(err)
	}
	if got := tree.ActiveSessionID(); got != "" {
		t.Errorf("ActiveSessionID after remove = %q, want empty", got)
	}
}

// TestRemoveSessionCascadeClearsActiveReview — if the focused session
// happens to be a review subsession whose parent is being cascade-removed,
// ActiveSession must clear, not point at the now-deleted review.
func TestRemoveSessionCascadeClearsActiveReview(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "r@x", Repo: "r"})
	mustAddSession(t, tree, Session{ID: "r@x~review-1-c", ParentID: "r@x"})
	if err := tree.ActivateSession("r@x~review-1-c"); err != nil {
		t.Fatal(err)
	}
	if err := tree.RemoveSession("r@x"); err != nil {
		t.Fatal(err)
	}
	if got := tree.ActiveSessionID(); got != "" {
		t.Errorf("ActiveSessionID after cascade = %q, want empty", got)
	}
}

// --- Pane operations --------------------------------------------------------

// TestAddPaneAutoActivates pins the convenience behaviour: the first
// pane added to a session becomes ActivePane automatically, so the
// renderer does not have to special-case "session has panes but
// nothing is active".
func TestAddPaneAutoActivates(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	mustAddPane(t, tree, "s", "agent")
	got, ok := tree.ActivePaneName("s")
	if !ok || got != "agent" {
		t.Errorf("ActivePaneName = (%q, %v), want (agent, true)", got, ok)
	}
	mustAddPane(t, tree, "s", "term")
	// Adding a second pane does NOT change the active pane.
	got, ok = tree.ActivePaneName("s")
	if !ok || got != "agent" {
		t.Errorf("ActivePaneName after second AddPane = (%q, %v), want unchanged", got, ok)
	}
}

func TestAddPaneRejectsDuplicate(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	mustAddPane(t, tree, "s", "agent")
	err := tree.AddPane("s", Pane{Name: "agent"})
	if !errors.Is(err, ErrPaneExists) {
		t.Errorf("AddPane(duplicate) = %v, want ErrPaneExists", err)
	}
}

func TestAddPaneRejectsEmptyName(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	err := tree.AddPane("s", Pane{})
	if !errors.Is(err, ErrInvalidSession) {
		t.Errorf("AddPane(empty name) = %v, want ErrInvalidSession", err)
	}
}

func TestAddPaneRejectsUnknownSession(t *testing.T) {
	tree := New()
	err := tree.AddPane("missing", Pane{Name: "agent"})
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("AddPane(missing session) = %v, want ErrSessionNotFound", err)
	}
}

// TestRemovePaneShiftsActiveToSlot is the load-bearing "focus follows
// the slot" invariant. Removing the active pane should snap focus to
// whatever pane now occupies that slice index — matches tmux's
// kill-pane behaviour and is what the renderer ends up coding against.
func TestRemovePaneShiftsActiveToSlot(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	mustAddPane(t, tree, "s", "agent")
	mustAddPane(t, tree, "s", "term")
	mustAddPane(t, tree, "s", "edit")

	// Activate middle pane and remove it. The slot is taken by "edit".
	if err := tree.ActivatePane("s", "term"); err != nil {
		t.Fatal(err)
	}
	if err := tree.RemovePane("s", "term"); err != nil {
		t.Fatal(err)
	}
	got, _ := tree.ActivePaneName("s")
	if got != "edit" {
		t.Errorf("after removing middle active pane, ActivePane = %q, want edit", got)
	}
}

// TestRemovePaneFromTailFallsBack — when the active pane is the last
// one in the slice and gets removed, focus falls back to the new last.
func TestRemovePaneFromTailFallsBack(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	mustAddPane(t, tree, "s", "agent")
	mustAddPane(t, tree, "s", "term")
	if err := tree.ActivatePane("s", "term"); err != nil {
		t.Fatal(err)
	}
	if err := tree.RemovePane("s", "term"); err != nil {
		t.Fatal(err)
	}
	got, _ := tree.ActivePaneName("s")
	if got != "agent" {
		t.Errorf("after removing tail active pane, ActivePane = %q, want agent", got)
	}
}

// TestRemovePaneLeavesActiveAloneIfNotActive — removing a non-active
// pane must NOT shift focus.
func TestRemovePaneLeavesActiveAloneIfNotActive(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	mustAddPane(t, tree, "s", "agent")
	mustAddPane(t, tree, "s", "term")
	mustAddPane(t, tree, "s", "edit")
	if err := tree.RemovePane("s", "edit"); err != nil {
		t.Fatal(err)
	}
	got, _ := tree.ActivePaneName("s")
	if got != "agent" {
		t.Errorf("removing non-active pane shifted focus: %q, want agent", got)
	}
}

// TestRemovePaneEmptiesSession — removing the only pane clears
// ActivePane back to "".
func TestRemovePaneEmptiesSession(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	mustAddPane(t, tree, "s", "agent")
	if err := tree.RemovePane("s", "agent"); err != nil {
		t.Fatal(err)
	}
	got, ok := tree.ActivePaneName("s")
	if !ok || got != "" {
		t.Errorf("after removing last pane, ActivePaneName = (%q, %v), want (\"\", true)", got, ok)
	}
}

func TestRemovePaneUnknown(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	if err := tree.RemovePane("s", "missing"); !errors.Is(err, ErrPaneNotFound) {
		t.Errorf("RemovePane(missing) = %v, want ErrPaneNotFound", err)
	}
	if err := tree.RemovePane("nope", "agent"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("RemovePane(missing session) = %v, want ErrSessionNotFound", err)
	}
}

// TestActivatePane pins the lookup path: activate a known pane,
// reject an unknown one.
func TestActivatePane(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	mustAddPane(t, tree, "s", "agent")
	mustAddPane(t, tree, "s", "term")
	if err := tree.ActivatePane("s", "term"); err != nil {
		t.Fatal(err)
	}
	got, _ := tree.ActivePaneName("s")
	if got != "term" {
		t.Errorf("ActivePane = %q, want term", got)
	}
	if err := tree.ActivatePane("s", "missing"); !errors.Is(err, ErrPaneNotFound) {
		t.Errorf("ActivatePane(missing) = %v, want ErrPaneNotFound", err)
	}
	if err := tree.ActivatePane("nope", "agent"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("ActivatePane(missing session) = %v, want ErrSessionNotFound", err)
	}
}

// TestActivateSession pins the tree-level focus pointer.
func TestActivateSession(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	if err := tree.ActivateSession("s"); err != nil {
		t.Fatal(err)
	}
	if got := tree.ActiveSessionID(); got != "s" {
		t.Errorf("ActiveSessionID = %q, want s", got)
	}

	// Empty string clears.
	if err := tree.ActivateSession(""); err != nil {
		t.Fatalf("ActivateSession(empty) = %v, want nil", err)
	}
	if got := tree.ActiveSessionID(); got != "" {
		t.Errorf("ActiveSessionID after clear = %q, want empty", got)
	}

	// Unknown ID returns an error and does not change state.
	if err := tree.ActivateSession("ghost"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("ActivateSession(ghost) = %v, want ErrSessionNotFound", err)
	}
	if got := tree.ActiveSessionID(); got != "" {
		t.Errorf("ActiveSessionID unexpectedly set to %q after failed Activate", got)
	}
}

// TestNextPrevPaneCycles drives the cycling invariants: forward wraps
// at the end, backward wraps at the start, single-pane is a no-op,
// empty session reports ErrNoPanes.
func TestNextPrevPaneCycles(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	mustAddPane(t, tree, "s", "agent")
	mustAddPane(t, tree, "s", "term")
	mustAddPane(t, tree, "s", "edit")

	// Forward through three panes and wrap.
	want := []string{"term", "edit", "agent", "term"}
	for i, w := range want {
		got, err := tree.NextPane("s")
		if err != nil {
			t.Fatalf("NextPane #%d: %v", i, err)
		}
		if got != w {
			t.Errorf("NextPane #%d = %q, want %q", i, got, w)
		}
	}

	// PrevPane reverses.
	got, err := tree.PrevPane("s")
	if err != nil {
		t.Fatal(err)
	}
	if got != "agent" {
		t.Errorf("PrevPane = %q, want agent", got)
	}

	// Wrap backward across the start.
	got, err = tree.PrevPane("s")
	if err != nil {
		t.Fatal(err)
	}
	if got != "edit" {
		t.Errorf("PrevPane wrap = %q, want edit", got)
	}
}

func TestNextPaneEmptySession(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	if _, err := tree.NextPane("s"); !errors.Is(err, ErrNoPanes) {
		t.Errorf("NextPane(empty) = %v, want ErrNoPanes", err)
	}
	if _, err := tree.PrevPane("s"); !errors.Is(err, ErrNoPanes) {
		t.Errorf("PrevPane(empty) = %v, want ErrNoPanes", err)
	}
}

func TestNextPaneSinglePaneIsIdempotent(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	mustAddPane(t, tree, "s", "agent")
	for i := 0; i < 3; i++ {
		got, err := tree.NextPane("s")
		if err != nil || got != "agent" {
			t.Fatalf("NextPane #%d = (%q, %v), want (agent, nil)", i, got, err)
		}
	}
}

// TestNextPaneSeedsFromFirstWhenActiveEmpty — if for some reason
// ActivePane is empty but panes exist (e.g. caller built the session
// from JSON without setting ActivePane), NextPane should drop into the
// first pane rather than report an error.
func TestNextPaneSeedsFromFirstWhenActiveEmpty(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{
		ID:    "s",
		Repo:  "r",
		Panes: []Pane{{Name: "agent"}, {Name: "term"}},
	})
	// AddSession auto-activates the first pane. Manually clear via a
	// direct manipulation through the lock so we can exercise the seed
	// branch.
	tree.mu.Lock()
	tree.state.Sessions["s"].ActivePane = ""
	tree.mu.Unlock()

	got, err := tree.NextPane("s")
	if err != nil {
		t.Fatal(err)
	}
	if got != "agent" {
		t.Errorf("NextPane(seed) = %q, want agent", got)
	}
}

// --- Session value passed into AddSession with pre-existing panes ----------

// TestAddSessionWithPreloadedPanes verifies that a Session carrying
// panes at AddSession time is accepted, panes are deep-copied (mutating
// the caller's slice after AddSession does not affect the tree), and the
// first pane is auto-activated if ActivePane was left empty.
func TestAddSessionWithPreloadedPanes(t *testing.T) {
	tree := New()
	panes := []Pane{{Name: "agent"}, {Name: "term"}}
	mustAddSession(t, tree, Session{ID: "s", Repo: "r", Panes: panes})

	// Mutate the caller's slice; the tree must not observe.
	panes[0].Name = "tampered"

	got, _ := tree.Session("s")
	if got.Panes[0].Name != "agent" {
		t.Errorf("AddSession kept a shared reference: pane 0 = %q, want agent", got.Panes[0].Name)
	}
	if got.ActivePane != "agent" {
		t.Errorf("AddSession did not auto-activate first pane: ActivePane = %q", got.ActivePane)
	}
}

// TestAddSessionRejectsDuplicatePaneInPreload and
// TestAddSessionRejectsActivePaneNotInSlice pin the schema checks on the
// preload path.
func TestAddSessionRejectsDuplicatePaneInPreload(t *testing.T) {
	tree := New()
	err := tree.AddSession(Session{
		ID: "s", Repo: "r",
		Panes: []Pane{{Name: "agent"}, {Name: "agent"}},
	})
	if !errors.Is(err, ErrInvalidSession) {
		t.Errorf("AddSession(duplicate pane) = %v, want ErrInvalidSession", err)
	}
}

func TestAddSessionRejectsActivePaneNotInSlice(t *testing.T) {
	tree := New()
	err := tree.AddSession(Session{
		ID: "s", Repo: "r",
		Panes:      []Pane{{Name: "agent"}},
		ActivePane: "term",
	})
	if !errors.Is(err, ErrInvalidSession) {
		t.Errorf("AddSession(bad ActivePane) = %v, want ErrInvalidSession", err)
	}
}

// --- Snapshot isolation -----------------------------------------------------

// TestSessionAccessorReturnsDeepCopy pins the "callers never get a
// pointer into our state" contract. Mutating the returned Session's
// Panes slice must not mutate the tree.
func TestSessionAccessorReturnsDeepCopy(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	mustAddPane(t, tree, "s", "agent")

	got, _ := tree.Session("s")
	got.Panes[0].Name = "tampered"

	again, _ := tree.Session("s")
	if again.Panes[0].Name != "agent" {
		t.Errorf("Session returned aliased slice: pane 0 = %q", again.Panes[0].Name)
	}
}

// --- JSON round-trip --------------------------------------------------------

// TestJSONRoundTrip is the smoke test for the persist layer in #2156 —
// a complex tree marshals, unmarshals, and compares equal under the
// accessor surface. Equality is by accessor output (Repos, Sessions in
// order, ActiveSession, ActivePane per session) — internal map iteration
// order is intentionally not part of the contract.
func TestJSONRoundTrip(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{
		ID: "nixos-config@main", Repo: "nixos-config", Branch: "main",
		Worktree: "/home/ben/code/nixos-config", AgentRole: "coordinator",
		SidecarAddr: "/run/prism/host.sock",
	})
	mustAddSession(t, tree, Session{
		ID: "nixos-config@2141-mux-spike", Repo: "nixos-config",
		Branch: "2141-mux-spike", AgentRole: "worker",
	})
	mustAddSession(t, tree, Session{
		ID:       "nixos-config@2141-mux-spike~review-1-review-code",
		ParentID: "nixos-config@2141-mux-spike", AgentRole: "review-code",
	})
	mustAddSession(t, tree, Session{
		ID: "home-ops@main", Repo: "home-ops", AgentRole: "coordinator",
	})
	mustAddPane(t, tree, "nixos-config@main", "agent")
	mustAddPane(t, tree, "nixos-config@main", "term")
	mustAddPane(t, tree, "nixos-config@main", "edit")
	if err := tree.ActivatePane("nixos-config@main", "term"); err != nil {
		t.Fatal(err)
	}
	if err := tree.ActivateSession("nixos-config@2141-mux-spike"); err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(tree)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	restored := New()
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got, want := restored.Repos(), tree.Repos(); !reflect.DeepEqual(got, want) {
		t.Errorf("Repos mismatch: got %v want %v", got, want)
	}
	if got, want := sessionIDs(restored.Sessions()), sessionIDs(tree.Sessions()); !reflect.DeepEqual(got, want) {
		t.Errorf("Sessions order mismatch:\n got  %v\n want %v", got, want)
	}
	if got, want := restored.ActiveSessionID(), tree.ActiveSessionID(); got != want {
		t.Errorf("ActiveSessionID mismatch: got %q want %q", got, want)
	}
	gotPane, _ := restored.ActivePaneName("nixos-config@main")
	wantPane, _ := tree.ActivePaneName("nixos-config@main")
	if gotPane != wantPane {
		t.Errorf("ActivePane on nixos-config@main mismatch: got %q want %q", gotPane, wantPane)
	}
	if err := restored.Validate(); err != nil {
		t.Errorf("Validate after round-trip: %v", err)
	}
}

// TestJSONRoundTripEmpty verifies the zero-state survives the round
// trip — important because persist will marshal an empty tree on first
// startup before any session lands.
func TestJSONRoundTripEmpty(t *testing.T) {
	tree := New()
	data, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	restored := New()
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.Len() != 0 {
		t.Errorf("Len() = %d, want 0", restored.Len())
	}
	if err := restored.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestUnmarshalRejectsOrphanReview pins the §3.1 invariant on the
// load path. A snapshot whose ChildOrder names a child whose ParentID
// doesn't match must be rejected — silent acceptance here would let
// tampered persisted state leak into the live tree.
func TestUnmarshalRejectsOrphanReview(t *testing.T) {
	// Construct a deliberately inconsistent JSON: a child whose
	// ParentID names a session that isn't actually a parent (the
	// ChildOrder entry is missing).
	bad := `{
		"sessions": {
			"a@x":         {"id":"a@x","repo":"a","panes":[],"active_pane":""},
			"a@x~review-1":{"id":"a@x~review-1","parent_id":"a@x"}
		},
		"repo_order":     ["a"],
		"session_order":  {"a":["a@x"]},
		"child_order":    {},
		"active_session": ""
	}`
	tree := New()
	err := json.Unmarshal([]byte(bad), tree)
	if !errors.Is(err, ErrInconsistent) {
		t.Errorf("Unmarshal(orphan review) = %v, want ErrInconsistent", err)
	}
}

func TestUnmarshalRejectsMissingRepoInOrder(t *testing.T) {
	bad := `{
		"sessions": {"r@x":{"id":"r@x","repo":"r"}},
		"repo_order": ["r","ghost"],
		"session_order": {"r":["r@x"]},
		"child_order": {},
		"active_session": ""
	}`
	tree := New()
	err := json.Unmarshal([]byte(bad), tree)
	if !errors.Is(err, ErrInconsistent) {
		t.Errorf("Unmarshal(ghost repo) = %v, want ErrInconsistent", err)
	}
}

func TestUnmarshalRejectsActiveSessionUnknown(t *testing.T) {
	bad := `{
		"sessions": {"r@x":{"id":"r@x","repo":"r"}},
		"repo_order": ["r"],
		"session_order": {"r":["r@x"]},
		"child_order": {},
		"active_session": "ghost"
	}`
	tree := New()
	err := json.Unmarshal([]byte(bad), tree)
	if !errors.Is(err, ErrInconsistent) {
		t.Errorf("Unmarshal(ghost active) = %v, want ErrInconsistent", err)
	}
}

func TestUnmarshalRejectsTopLevelWithoutRepo(t *testing.T) {
	bad := `{
		"sessions": {"x":{"id":"x"}},
		"repo_order": [""],
		"session_order": {"":["x"]},
		"child_order": {}
	}`
	tree := New()
	err := json.Unmarshal([]byte(bad), tree)
	if !errors.Is(err, ErrInconsistent) {
		t.Errorf("Unmarshal(top-level without repo) = %v, want ErrInconsistent", err)
	}
}

func TestUnmarshalRejectsDuplicatePane(t *testing.T) {
	bad := `{
		"sessions": {"r@x":{"id":"r@x","repo":"r","panes":[{"name":"a"},{"name":"a"}]}},
		"repo_order": ["r"],
		"session_order": {"r":["r@x"]},
		"child_order": {}
	}`
	tree := New()
	err := json.Unmarshal([]byte(bad), tree)
	if !errors.Is(err, ErrInconsistent) {
		t.Errorf("Unmarshal(duplicate pane) = %v, want ErrInconsistent", err)
	}
}

func TestUnmarshalRejectsActivePaneNotInSlice(t *testing.T) {
	bad := `{
		"sessions": {"r@x":{"id":"r@x","repo":"r","panes":[{"name":"a"}],"active_pane":"ghost"}},
		"repo_order": ["r"],
		"session_order": {"r":["r@x"]},
		"child_order": {}
	}`
	tree := New()
	err := json.Unmarshal([]byte(bad), tree)
	if !errors.Is(err, ErrInconsistent) {
		t.Errorf("Unmarshal(bad active pane) = %v, want ErrInconsistent", err)
	}
}

// TestUnmarshalRejectsReviewParentingReview pins the §3.1 two-level
// invariant on the load path.
func TestUnmarshalRejectsReviewParentingReview(t *testing.T) {
	bad := `{
		"sessions": {
			"a@x":              {"id":"a@x","repo":"a"},
			"a@x~r1":            {"id":"a@x~r1","parent_id":"a@x","repo":"a"},
			"a@x~r1~r2":         {"id":"a@x~r1~r2","parent_id":"a@x~r1","repo":"a"}
		},
		"repo_order": ["a"],
		"session_order": {"a":["a@x"]},
		"child_order": {"a@x":["a@x~r1"],"a@x~r1":["a@x~r1~r2"]}
	}`
	tree := New()
	err := json.Unmarshal([]byte(bad), tree)
	if !errors.Is(err, ErrInconsistent) {
		t.Errorf("Unmarshal(nested review) = %v, want ErrInconsistent", err)
	}
}

func TestUnmarshalRejectsOrphanedSession(t *testing.T) {
	// A session not referenced by either SessionOrder or ChildOrder.
	bad := `{
		"sessions": {
			"r@x":  {"id":"r@x","repo":"r"},
			"r@y":  {"id":"r@y","repo":"r"}
		},
		"repo_order": ["r"],
		"session_order": {"r":["r@x"]},
		"child_order": {}
	}`
	tree := New()
	err := json.Unmarshal([]byte(bad), tree)
	if !errors.Is(err, ErrInconsistent) {
		t.Errorf("Unmarshal(orphaned session) = %v, want ErrInconsistent", err)
	}
}

func TestUnmarshalRebuildsNilMaps(t *testing.T) {
	// Minimal JSON: just an empty active_session string. The
	// constructor should reinitialise the nil maps so subsequent
	// AddSession calls work without panic.
	tree := New()
	if err := json.Unmarshal([]byte(`{}`), tree); err != nil {
		t.Fatal(err)
	}
	mustAddSession(t, tree, Session{ID: "s", Repo: "r"})
	if tree.Len() != 1 {
		t.Errorf("after Unmarshal({}) + AddSession, Len = %d, want 1", tree.Len())
	}
}

// --- Validate after a sequence of operations -------------------------------

// TestValidateAfterMixedSequence builds up the canonical §3.1 example
// from docs (`nixos-config` with a mux-spike session, several review
// subs, plus a home-ops cluster) through the public API and then asserts
// every invariant holds.
func TestValidateAfterMixedSequence(t *testing.T) {
	tree := New()
	mustAddSession(t, tree, Session{ID: "nixos-config@main", Repo: "nixos-config"})
	mustAddSession(t, tree, Session{ID: "nixos-config@2141-mux-spike", Repo: "nixos-config"})
	for _, agent := range []string{"code", "goal", "qa", "security", "context"} {
		mustAddSession(t, tree, Session{
			ID:       "nixos-config@2141-mux-spike~review-1-review-" + agent,
			ParentID: "nixos-config@2141-mux-spike",
		})
	}
	mustAddSession(t, tree, Session{ID: "home-ops@main", Repo: "home-ops"})
	mustAddSession(t, tree, Session{ID: "home-ops@plex-image-bump", Repo: "home-ops"})

	mustAddPane(t, tree, "nixos-config@main", "agent")
	mustAddPane(t, tree, "nixos-config@main", "term")
	mustAddPane(t, tree, "nixos-config@2141-mux-spike", "agent")

	if err := tree.ActivateSession("nixos-config@2141-mux-spike"); err != nil {
		t.Fatal(err)
	}
	if err := tree.Validate(); err != nil {
		t.Fatalf("Validate after mixed sequence: %v", err)
	}

	// Remove the mux-spike session: cascade-removes all five review
	// subs. Validate again — still consistent.
	if err := tree.RemoveSession("nixos-config@2141-mux-spike"); err != nil {
		t.Fatal(err)
	}
	if err := tree.Validate(); err != nil {
		t.Fatalf("Validate after cascade: %v", err)
	}
	if tree.ActiveSessionID() != "" {
		t.Errorf("ActiveSessionID should clear when active session removed")
	}
	if !tree.HasSession("nixos-config@main") || !tree.HasSession("home-ops@plex-image-bump") {
		t.Errorf("unrelated sessions should survive cascade")
	}
	for _, agent := range []string{"code", "goal", "qa", "security", "context"} {
		id := "nixos-config@2141-mux-spike~review-1-review-" + agent
		if tree.HasSession(id) {
			t.Errorf("review subsession %q should have been cascade-removed", id)
		}
	}
}

// --- Error message ergonomics ---------------------------------------------

// TestErrorsCarryContext spot-checks that errors include the offending
// ID — server callers (#2153) surface these to the CLI client, and a
// bare "session not found" is much less useful than "session not found:
// nixos-config@feature".
func TestErrorsCarryContext(t *testing.T) {
	tree := New()
	err := tree.RemoveSession("nixos-config@feature")
	if err == nil || !strings.Contains(err.Error(), "nixos-config@feature") {
		t.Errorf("error %q should mention the missing ID", err)
	}
}

// --- helpers ---------------------------------------------------------------

func sessionIDs(sessions []Session) []string {
	out := make([]string, len(sessions))
	for i, s := range sessions {
		out[i] = s.ID
	}
	return out
}
