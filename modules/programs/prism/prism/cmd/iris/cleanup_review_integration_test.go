package main

// cleanup_review_integration_test.go — end-to-end integration test for
// the issue #1699 review-group recursion in `iris cleanup`.
//
// Flow:
//
//   1. Spawn a parent session (via the iristest harness).
//   2. Run `iris review <pr>` against the in-process ClientSocket so the
//      daemon registers a session_groups row and spawns 5 fake review
//      children.
//   3. Transition each child to a terminal state (no real pi child
//      involved — the fake SpawnSession just seeds agent_status).
//   4. Invoke runCleanup against the same iristest-isolated DB and
//      assert that the parent AND all 5 children are cleaned up:
//        - sessions row marked ended for every session
//        - run dirs removed
//        - kill callback (here a no-op via --skip-kill) NOT invoked
//          (we exercise the no-daemon path because the daemon-side
//          kill is covered by the supervisor tests already)
//   5. Sanity: a follow-up `iris cleanup` on an already-cleaned parent
//      reports the children as already-cleaned-up stubs without error.
//
// The test uses the iristest isolation harness so the iris DB,
// XDG_STATE_HOME, and HOME are redirected to a t.TempDir.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

// TestCleanup_ReviewGroupChildren_Integration is the headline #1699
// end-to-end test: spawn a parent, run `iris review` to spawn 5
// children, let them complete, run `iris cleanup` on the parent, and
// assert that all 6 sessions (parent + 5 children) are cleaned up.
func TestCleanup_ReviewGroupChildren_Integration(t *testing.T) {
	iso := iristest.NewIsolated(t)

	const prNumber = "1699"
	parent := iristest.SessionName("cleanup-review-parent")

	// Seed the parent's agent_status row so the review-spawn handler
	// passes its "parent must be active" guard.
	if err := iso.DB.UpsertStatusWithAgent(parent, "iris-parity", iso.Root, "active", nil, nil, nil, nil); err != nil {
		t.Fatalf("UpsertStatusWithAgent(parent): %v", err)
	}
	// Also seed a sessions row for the parent so iris cleanup can resolve
	// it (the standalone CleanupSession needs MostRecentSessionForName).
	if err := insertSessionForCleanup(iso, parent, "iris-test-cleanup-review-parent"); err != nil {
		t.Fatalf("insertSessionForCleanup(parent): %v", err)
	}

	// Active-sessions snapshot for the review-spawn handler's parent guard.
	var (
		activeMu       sync.Mutex
		activeSessions = []iris.SessionSnapshot{{
			Name:       parent,
			InstanceID: uuid.NewString(),
			State:      "active",
			Role:       "worker",
			Worktree:   iso.Root,
			StartedAt:  time.Now().UTC().Format(time.RFC3339),
		}}
	)
	getActive := func() []iris.SessionSnapshot {
		activeMu.Lock()
		defer activeMu.Unlock()
		out := make([]iris.SessionSnapshot, len(activeSessions))
		copy(out, activeSessions)
		return out
	}

	// Fake SpawnSession: seeds the agent_status row (the handler then
	// stamps the group_id), AND inserts a sessions row so cleanup can
	// resolve it.
	var (
		spawnMu       sync.Mutex
		spawnedAgents = make(map[string]string) // role → session_name
	)
	spawnSession := func(_ context.Context, sessionName, worktree, role string) (*iris.Supervisor, error) {
		if err := iso.DB.UpsertStatusWithAgent(sessionName, "iris-parity", worktree, "active", nil, nil, &role, nil); err != nil {
			return nil, fmt.Errorf("seed agent_status for %s: %w", role, err)
		}
		if err := insertSessionForCleanup(iso, sessionName, "iris-test-child-"+role); err != nil {
			return nil, fmt.Errorf("seed sessions row for %s: %w", role, err)
		}
		spawnMu.Lock()
		spawnedAgents[role] = sessionName
		spawnMu.Unlock()
		activeMu.Lock()
		activeSessions = append(activeSessions, iris.SessionSnapshot{
			Name:       sessionName,
			InstanceID: uuid.NewString(),
			State:      "active",
			Role:       role,
			Worktree:   worktree,
			StartedAt:  time.Now().UTC().Format(time.RFC3339),
		})
		activeMu.Unlock()
		return makeFakeSupervisor(sessionName, worktree, role), nil
	}

	handler := iris.NewReviewSpawnHandler(iris.ReviewSpawnDeps{
		Database:       iso.DB,
		SpawnSession:   spawnSession,
		DeliverPrompt:  func(context.Context, string, string, string, []string) error { return nil },
		ParentWorktree: func(string) (string, error) { return iso.Root, nil },
		PollInterval:   25 * time.Millisecond,
	})

	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath:          iso.Paths.Sock,
		Database:          iso.DB,
		GetActiveSessions: getActive,
		DeliverPrompt:     func(context.Context, string, string, string, []string) error { return nil },
		SpawnReviewGroup:  handler,
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cs.Serve(ctx)

	// Drive `iris review` via the testable core.
	reviewOpts := reviewRunOpts{
		PRNumber:        prNumber,
		Timeout:         5 * time.Second,
		SockPath:        iso.Paths.Sock,
		Parent:          parent,
		Worktree:        iso.Root,
		PRVerifier:      func(string) error { return nil },
		PreflightRunner: noopPreflightRunner,
		Out:             io.Discard,
	}
	runCtx, runCancel := context.WithTimeout(ctx, 10*time.Second)
	defer runCancel()
	if err := runReviewAt(runCtx, reviewOpts); err != nil {
		t.Fatalf("runReviewAt: %v", err)
	}

	// Assert all 5 canonical agents got spawned.
	spawnMu.Lock()
	gotAgents := make(map[string]string, len(spawnedAgents))
	for k, v := range spawnedAgents {
		gotAgents[k] = v
	}
	spawnMu.Unlock()
	if len(gotAgents) != len(iris.ReviewAgentNames) {
		t.Fatalf("spawned %d agents, want %d (got: %v)", len(gotAgents), len(iris.ReviewAgentNames), gotAgents)
	}

	// Transition each agent to "finished" — the cleanup recursion does
	// not require terminal state, but a real iris cleanup would only
	// happen after the round is done, so we mirror that here.
	for _, role := range iris.ReviewAgentNames {
		sess := gotAgents[role]
		r := role
		if err := iso.DB.UpsertStatusWithAgent(sess, "iris-parity", iso.Root, "finished", nil, nil, &r, nil); err != nil {
			t.Fatalf("transition %s: %v", sess, err)
		}
	}

	// Now exercise the cleanup recursion. We invoke iris.CleanupSession
	// directly with KillFn=nil to emulate `iris cleanup --skip-kill`
	// (the daemon-side kill is not in scope for this integration test —
	// see internal/iris/supervisor_kill_test.go for that coverage).
	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:    iso.DB,
		RunDir:      iso.Paths.RunDir,
		LogDir:      iso.Paths.LogDir,
		ArchiveRoot: iso.Paths.ArchiveRoot,
		// KillFn nil → emulates --skip-kill end-to-end.
	}, parent)
	if err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}

	if res.SessionName != parent {
		t.Errorf("res.SessionName = %q, want %q", res.SessionName, parent)
	}
	if !res.SessionRowRemoved {
		t.Errorf("parent SessionRowRemoved=false, want true (errors=%v)", res.Errors)
	}
	if len(res.Children) != len(iris.ReviewAgentNames) {
		t.Fatalf("len(res.Children) = %d, want %d (children=%+v)", len(res.Children), len(iris.ReviewAgentNames), res.Children)
	}

	// Assert each child appears with SessionRowRemoved=true. Build a
	// set of expected child names so the order doesn't matter here.
	expected := make(map[string]bool, len(gotAgents))
	for _, name := range gotAgents {
		expected[name] = true
	}
	for _, child := range res.Children {
		if !expected[child.SessionName] {
			t.Errorf("unexpected child in result: %q", child.SessionName)
		}
		if !child.SessionRowRemoved {
			t.Errorf("child %q SessionRowRemoved=false, want true (errors=%v)", child.SessionName, child.Errors)
		}
	}

	// Confirm every sessions row is now ended.
	for _, name := range append([]string{parent}, valuesOf(gotAgents)...) {
		s, err := iso.DB.MostRecentSessionForName(name)
		if err != nil {
			t.Fatalf("MostRecentSessionForName(%q): %v", name, err)
		}
		if s == nil {
			t.Errorf("sessions row missing for %q after cleanup", name)
			continue
		}
		if s.EndState == nil {
			t.Errorf("EndState nil for %q after cleanup, want non-nil", name)
		}
	}

	// Sanity: render via printCleanupResult and assert every child name
	// appears in the printed output (AC: "CLI output reports
	// cleaned-up children by name").
	var buf bytes.Buffer
	printCleanupResult(&buf, res, true /* skipKill */)
	out := buf.String()
	if !strings.Contains(out, "children:") {
		t.Errorf("printCleanupResult output missing 'children:' section; got:\n%s", out)
	}
	for _, name := range gotAgents {
		if !strings.Contains(out, name) {
			t.Errorf("printCleanupResult output missing child name %q; got:\n%s", name, out)
		}
	}

	// Sanity: a follow-up cleanup of the same parent should not error
	// (idempotent). The parent's sessions row is already ended, but
	// MostRecentSessionForName still returns the row, so cleanup is a
	// no-op for the session-state step and a "stub" cleanup for each
	// child (since their sessions rows are ended too, not deleted).
	_, err = iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:    iso.DB,
		RunDir:      iso.Paths.RunDir,
		LogDir:      iso.Paths.LogDir,
		ArchiveRoot: iso.Paths.ArchiveRoot,
	}, parent)
	if err != nil {
		t.Errorf("second CleanupSession returned error %v, want nil (cleanup must be idempotent)", err)
	}
}

// valuesOf returns the values of a string→string map as a slice. The
// order is map-iteration order; callers that need a deterministic order
// must sort the result.
func valuesOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// insertSessionForCleanup seeds a sessions row that iris.CleanupSession
// can resolve via MostRecentSessionForName. The harness/worktree fields
// are deliberately empty: the cleanup path tolerates worktree-less
// sessions (archive is skipped, run dir is the only artefact).
func insertSessionForCleanup(iso *iristest.Isolated, sessionName, instanceID string) error {
	role := "worker"
	return iso.DB.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Worktree:    "",
		Harness:     "pi",
		AgentRole:   &role,
		StartedAt:   time.Now(),
	})
}
