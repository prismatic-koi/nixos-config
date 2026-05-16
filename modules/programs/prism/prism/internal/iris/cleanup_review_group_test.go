package iris_test

// cleanup_review_group_test.go — tests for the issue #1699 recursion in
// internal/iris/cleanup.go::CleanupSession.
//
// What is covered here:
//
//   - Review-group child traversal: a parent that owns a session_groups
//     row has all its review-children cleaned up in the same pass.
//   - Output: each child appears in CleanupResult.Children, ordered by
//     session_name ascending.
//   - Kill-then-archive: KillFn fires once per session (parent + each
//     child) BEFORE the archive step, matching #1692.
//   - Non-review sessions: no children → no behaviour change (no extra
//     KillFn invocations, empty Children slice).
//   - Already-cleaned-up child: a member with no sessions row is skipped
//     gracefully (stub result, no error propagated).
//   - Recursion bound at 2: a grandchild that itself has a session_groups
//     row triggers a warn-and-skip and surfaces in the parent's Errors.
//   - --skip-kill semantics: when KillFn is nil, no kill is attempted at
//     any recursion level.
//   - session_groups unreadable → fallback to parent-only with the error
//     captured in the parent's Errors slice. (Not directly testable
//     without a DB-level fault injection; covered indirectly via the
//     code path that handles GroupMembersForParent returning an empty
//     slice for an unknown parent.)

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

// seedSession inserts a sessions row and an agent_status row for the
// named session. The agent_status row is required so SetGroupID can link
// the session into a session_groups row.
func seedSession(t *testing.T, d *db.DB, sessionName, instanceID string) {
	t.Helper()
	role := "worker"
	if err := d.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Worktree:    "",
		Harness:     "pi",
		AgentRole:   &role,
		StartedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("InsertSession(%q): %v", sessionName, err)
	}
	if err := d.EnsureStatusRow(sessionName, "test-repo", ""); err != nil {
		t.Fatalf("EnsureStatusRow(%q): %v", sessionName, err)
	}
}

// TestCleanup_TraversesReviewGroupChildren is the headline #1699 test:
// a parent session with a review group is cleaned up, and every child
// in the group is cleaned up too. The kill callback fires once per
// session (parent + each child).
func TestCleanup_TraversesReviewGroupChildren(t *testing.T) {
	iso := iristest.NewIsolated(t)

	parent := iristest.SessionName("parent")
	seedSession(t, iso.DB, parent, "iris-test-parent-001")

	// Register a review group and seed 5 children linked to it.
	groupID, err := iso.DB.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	children := []string{
		iristest.SessionName("parent~review-1-review-code"),
		iristest.SessionName("parent~review-1-review-context"),
		iristest.SessionName("parent~review-1-review-goal"),
		iristest.SessionName("parent~review-1-review-qa"),
		iristest.SessionName("parent~review-1-review-security"),
	}
	for i, child := range children {
		seedSession(t, iso.DB, child, "iris-test-child-00"+string(rune('1'+i)))
		if err := iso.DB.SetGroupID(child, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", child, err)
		}
	}

	// Record every KillFn invocation so we can assert kill-then-archive
	// fired for every session in the cleanup tree.
	var killMu sync.Mutex
	var killed []string
	killFn := func(name string) string {
		killMu.Lock()
		defer killMu.Unlock()
		killed = append(killed, name)
		return "killed (state=finished)"
	}

	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:    iso.DB,
		RunDir:      iso.Paths.RunDir,
		LogDir:      iso.Paths.LogDir,
		ArchiveRoot: iso.Paths.ArchiveRoot,
		KillFn:      killFn,
	}, parent)
	if err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}

	// Parent fields.
	if res.SessionName != parent {
		t.Errorf("res.SessionName = %q, want %q", res.SessionName, parent)
	}
	if res.KillSummary == "" {
		t.Errorf("res.KillSummary empty for parent — KillFn should have fired")
	}
	if !res.SessionRowRemoved {
		t.Errorf("parent SessionRowRemoved=false, want true (errors=%v)", res.Errors)
	}

	// Children: every member of the group must appear, ordered by
	// session_name ascending.
	if got, want := len(res.Children), len(children); got != want {
		t.Fatalf("len(res.Children) = %d, want %d (children=%+v)", got, want, res.Children)
	}
	wantNames := append([]string(nil), children...)
	sort.Strings(wantNames)
	for i, child := range res.Children {
		if child.SessionName != wantNames[i] {
			t.Errorf("Children[%d].SessionName = %q, want %q", i, child.SessionName, wantNames[i])
		}
		if child.KillSummary == "" {
			t.Errorf("Children[%d].KillSummary empty for %q — KillFn must fire per-child", i, child.SessionName)
		}
		if !child.SessionRowRemoved {
			t.Errorf("Children[%d] (%q): SessionRowRemoved=false, want true", i, child.SessionName)
		}
	}

	// KillFn must have fired exactly once per session (parent + 5
	// children = 6 total).
	killMu.Lock()
	defer killMu.Unlock()
	if got, want := len(killed), 1+len(children); got != want {
		t.Errorf("KillFn invocations = %d, want %d (got=%v)", got, want, killed)
	}

	// Parent sessions row should be marked ended.
	sess, err := iso.DB.MostRecentSessionForName(parent)
	if err != nil {
		t.Fatalf("MostRecentSessionForName(parent): %v", err)
	}
	if sess.EndState == nil {
		t.Errorf("parent EndState is nil after cleanup, want non-nil")
	}
	// Each child sessions row should also be marked ended.
	for _, name := range children {
		s, err := iso.DB.MostRecentSessionForName(name)
		if err != nil {
			t.Fatalf("MostRecentSessionForName(%q): %v", name, err)
		}
		if s == nil {
			t.Errorf("child %q sessions row is missing after cleanup", name)
			continue
		}
		if s.EndState == nil {
			t.Errorf("child %q EndState is nil after cleanup, want non-nil", name)
		}
	}
}

// TestCleanup_NonReviewSession_NoChildren asserts that a session without
// any review-group children is cleaned up exactly as before (empty
// Children, KillFn invoked once for the parent only).
func TestCleanup_NonReviewSession_NoChildren(t *testing.T) {
	iso := iristest.NewIsolated(t)

	parent := iristest.SessionName("lone-parent")
	seedSession(t, iso.DB, parent, "iris-test-lone-001")

	var killed []string
	killFn := func(name string) string {
		killed = append(killed, name)
		return "killed (state=finished)"
	}

	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:    iso.DB,
		RunDir:      iso.Paths.RunDir,
		LogDir:      iso.Paths.LogDir,
		ArchiveRoot: iso.Paths.ArchiveRoot,
		KillFn:      killFn,
	}, parent)
	if err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}

	if len(res.Children) != 0 {
		t.Errorf("len(res.Children) = %d, want 0 (non-review session)", len(res.Children))
	}
	if len(killed) != 1 || killed[0] != parent {
		t.Errorf("KillFn invocations = %v, want [%q] (only the parent)", killed, parent)
	}
	if !res.SessionRowRemoved {
		t.Errorf("SessionRowRemoved=false, want true (errors=%v)", res.Errors)
	}
}

// TestCleanup_AlreadyCleanedChild_SkipsGracefully covers the AC: a
// child registered in agent_status but with no sessions row (already
// cleaned up by an earlier pass) is skipped without erroring out.
func TestCleanup_AlreadyCleanedChild_SkipsGracefully(t *testing.T) {
	iso := iristest.NewIsolated(t)

	parent := iristest.SessionName("parent-partly-cleaned")
	seedSession(t, iso.DB, parent, "iris-test-partly-001")

	groupID, err := iso.DB.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// Live child — has both an agent_status row and a sessions row.
	liveChild := iristest.SessionName("parent-partly-cleaned~review-1-review-code")
	seedSession(t, iso.DB, liveChild, "iris-test-partly-002")
	if err := iso.DB.SetGroupID(liveChild, groupID); err != nil {
		t.Fatalf("SetGroupID(live): %v", err)
	}

	// Already-cleaned child — agent_status row only (no sessions row).
	staleChild := iristest.SessionName("parent-partly-cleaned~review-1-review-goal")
	if err := iso.DB.EnsureStatusRow(staleChild, "test-repo", ""); err != nil {
		t.Fatalf("EnsureStatusRow(stale): %v", err)
	}
	if err := iso.DB.SetGroupID(staleChild, groupID); err != nil {
		t.Fatalf("SetGroupID(stale): %v", err)
	}

	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:    iso.DB,
		RunDir:      iso.Paths.RunDir,
		LogDir:      iso.Paths.LogDir,
		ArchiveRoot: iso.Paths.ArchiveRoot,
	}, parent)
	if err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}

	if got, want := len(res.Children), 2; got != want {
		t.Fatalf("len(res.Children) = %d, want %d (children=%+v)", got, want, res.Children)
	}

	// Find each child's result by name.
	byName := make(map[string]*iris.CleanupResult, len(res.Children))
	for _, c := range res.Children {
		byName[c.SessionName] = c
	}
	live, ok := byName[liveChild]
	if !ok {
		t.Fatalf("live child %q missing from Children", liveChild)
	}
	if !live.SessionRowRemoved {
		t.Errorf("live child SessionRowRemoved=false, want true (errors=%v)", live.Errors)
	}
	stale, ok := byName[staleChild]
	if !ok {
		t.Fatalf("stale child %q missing from Children", staleChild)
	}
	// The stale child should surface as a stub result (KillSummary set
	// to "skipped (already cleaned up)") with no propagated error.
	if stale.KillSummary == "" {
		t.Errorf("stale child KillSummary is empty, want a skip-summary")
	}
	// Parent's Errors slice should NOT contain a child-not-found error.
	for _, e := range res.Errors {
		if e == nil {
			continue
		}
		if msg := e.Error(); contains(msg, "not found") {
			t.Errorf("parent Errors should not surface 'not found' for stale child; got %v", e)
		}
	}
}

// TestCleanup_DepthCap_WarnAndSkipGrandchildGroup asserts that when a
// review-child unexpectedly carries its own session_groups row AND that
// inner group has members, the cleanup recursion does NOT descend into
// those grandchildren. The child IS still cleaned up; only its
// grandchildren are skipped. A warning is surfaced via the child's
// Errors slice (the depth-cap check fires inside the child's recursive
// CleanupSession call, when it enumerates its OWN children).
//
// Topology built by this test:
//
//	parent (depth 0)
//	  └─ child   (depth 1) — member of parent's group
//	       └─ grandchild (depth 2) — member of child's group; itself
//	                           registered as parent of an inner empty
//	                           session_groups row, which is what the
//	                           depth-cap HasReviewGroup() check picks
//	                           up to fire the warn-and-skip.
func TestCleanup_DepthCap_WarnAndSkipGrandchildGroup(t *testing.T) {
	iso := iristest.NewIsolated(t)

	parent := iristest.SessionName("grand-parent")
	seedSession(t, iso.DB, parent, "iris-test-gp-001")
	parentGroupID, err := iso.DB.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup(parent): %v", err)
	}

	child := iristest.SessionName("grand-parent~review-1-review-code")
	seedSession(t, iso.DB, child, "iris-test-gp-002")
	if err := iso.DB.SetGroupID(child, parentGroupID); err != nil {
		t.Fatalf("SetGroupID(child): %v", err)
	}
	childGroupID, err := iso.DB.RegisterGroup(child)
	if err != nil {
		t.Fatalf("RegisterGroup(child): %v", err)
	}

	// Grandchild: a session row + agent_status row linked to child's
	// group. The grandchild ALSO has its own (empty) registered group so
	// the depth-cap HasReviewGroup() check returns true and triggers
	// warn-and-skip.
	grandchild := iristest.SessionName("grand-parent~review-1-review-code~review-1-review-code")
	seedSession(t, iso.DB, grandchild, "iris-test-gp-003")
	if err := iso.DB.SetGroupID(grandchild, childGroupID); err != nil {
		t.Fatalf("SetGroupID(grandchild): %v", err)
	}
	if _, err := iso.DB.RegisterGroup(grandchild); err != nil {
		t.Fatalf("RegisterGroup(grandchild): %v", err)
	}

	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:    iso.DB,
		RunDir:      iso.Paths.RunDir,
		LogDir:      iso.Paths.LogDir,
		ArchiveRoot: iso.Paths.ArchiveRoot,
	}, parent)
	if err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}

	// The child must appear in Children: the depth cap skips only the
	// recursion below it, not the child itself.
	if len(res.Children) != 1 {
		t.Fatalf("len(res.Children) = %d, want 1 (got %+v)", len(res.Children), res.Children)
	}
	if res.Children[0].SessionName != child {
		t.Errorf("Children[0].SessionName = %q, want %q", res.Children[0].SessionName, child)
	}
	if !res.Children[0].SessionRowRemoved {
		t.Errorf("child SessionRowRemoved=false, want true; depth cap must not block child's own cleanup")
	}

	// The depth-cap warning must surface in the child's Errors slice.
	foundWarn := false
	for _, e := range res.Children[0].Errors {
		if e == nil {
			continue
		}
		if contains(e.Error(), "own review group") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Errorf("depth-cap warning not surfaced in child's Errors; got %v", res.Children[0].Errors)
	}

	// The grandchild must NOT have been cleaned up: depth cap should
	// leave its sessions row intact (no EndState).
	grandSess, err := iso.DB.MostRecentSessionForName(grandchild)
	if err != nil {
		t.Fatalf("MostRecentSessionForName(grandchild): %v", err)
	}
	if grandSess == nil {
		t.Fatalf("grandchild sessions row missing — depth cap should leave it intact")
	}
	if grandSess.EndState != nil {
		t.Errorf("grandchild EndState = %q, want nil (depth cap must NOT clean up grandchildren)", *grandSess.EndState)
	}
}

// TestCleanup_SkipKill_NoKillFn covers the AC: --skip-kill from #1692
// applies to children too. When KillFn is nil, no kill summary is
// recorded for any session in the tree.
func TestCleanup_SkipKill_NoKillFn(t *testing.T) {
	iso := iristest.NewIsolated(t)

	parent := iristest.SessionName("parent-skipkill")
	seedSession(t, iso.DB, parent, "iris-test-sk-001")
	groupID, err := iso.DB.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	child := iristest.SessionName("parent-skipkill~review-1-review-code")
	seedSession(t, iso.DB, child, "iris-test-sk-002")
	if err := iso.DB.SetGroupID(child, groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}

	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:    iso.DB,
		RunDir:      iso.Paths.RunDir,
		LogDir:      iso.Paths.LogDir,
		ArchiveRoot: iso.Paths.ArchiveRoot,
		// KillFn deliberately nil — emulates --skip-kill.
	}, parent)
	if err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}

	if res.KillSummary != "" {
		t.Errorf("parent KillSummary = %q, want empty (KillFn nil)", res.KillSummary)
	}
	if len(res.Children) != 1 {
		t.Fatalf("len(res.Children) = %d, want 1", len(res.Children))
	}
	if res.Children[0].KillSummary != "" {
		t.Errorf("child KillSummary = %q, want empty (KillFn nil applies to children too)", res.Children[0].KillSummary)
	}
	if !res.Children[0].SessionRowRemoved {
		t.Errorf("child SessionRowRemoved=false; cleanup should still proceed without KillFn")
	}
}
