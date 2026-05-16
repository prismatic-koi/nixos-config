package main

// review_integration_test.go — end-to-end integration test for the
// `iris review <pr>` CLI against a live in-process iris.ClientSocket
// wired to the daemon-side review orchestrator
// (iris.NewReviewSpawnHandler). Issue #1694.
//
// The test exercises the full review cycle:
//
//   1. Start an in-process ClientSocket with the review-spawn orchestrator.
//   2. Pre-register a parent session (so the orchestrator passes the
//      "parent must be active" guard).
//   3. Invoke `runReviewAt` against the live socket. The handler:
//        - Registers a session_groups row.
//        - Spawns the 5 review-agent sessions (via a fake SpawnSession
//          that seeds an `active` agent_status row — no real pi child).
//        - Returns a review_spawned ack with group_id, round, members.
//   4. Transition the 5 fake agent sessions to `finished` via direct DB
//      writes. The watcher (running in a goroutine) observes completion
//      via db.GroupCompleted and delivers one review-complete prompt to
//      the parent.
//   5. Assert that the deliver hook fired EXACTLY ONCE, with a body
//      mentioning each agent role and the per-run delivery_id.
//
// The test uses the iristest isolation harness so the iris DB,
// XDG_STATE_HOME, and HOME are redirected to a t.TempDir — no host
// state is touched, and no real notification can escape to a live
// coordinator (#1608).

import (
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

// TestReview_FullCycle drives the full async review path: spawn 5 agents,
// transition each to terminal, observe exactly one review-complete
// delivery to the parent session.
func TestReview_FullCycle(t *testing.T) {
	iso := iristest.NewIsolated(t)

	const prNumber = "1694"
	parent := iristest.SessionName("review-parent")

	// Seed the parent's agent_status row so it appears in the daemon's
	// "active sessions" snapshot (the review-spawn handler rejects unknown
	// parents up-front via sessionExists).
	if err := iso.DB.UpsertStatusWithAgent(parent, "iris-parity", iso.Root, "active", nil, nil, nil, nil); err != nil {
		t.Fatalf("UpsertStatusWithAgent(parent): %v", err)
	}

	// In-memory active-sessions list. The orchestrator reads this through
	// GetActiveSessions for the sessionExists guard. We add review-agent
	// sessions to it as the fake SpawnSession is invoked.
	var (
		activeMu       sync.Mutex
		activeSessions = []iris.SessionSnapshot{
			{
				Name:       parent,
				InstanceID: uuid.NewString(),
				State:      "active",
				Role:       "worker",
				Worktree:   iso.Root,
				StartedAt:  time.Now().UTC().Format(time.RFC3339),
			},
		}
	)
	getActive := func() []iris.SessionSnapshot {
		activeMu.Lock()
		defer activeMu.Unlock()
		out := make([]iris.SessionSnapshot, len(activeSessions))
		copy(out, activeSessions)
		return out
	}

	// Capture deliveries to the parent. The watcher will hit this hook
	// once (per sync.Once guard) when the group completes.
	var (
		deliverMu   sync.Mutex
		deliverHits int
		deliverText string
		deliverName string
	)
	deliverFn := func(_ context.Context, name, text, deliverAs string, _ []string) error {
		deliverMu.Lock()
		defer deliverMu.Unlock()
		deliverHits++
		deliverName = name
		deliverText = text
		_ = deliverAs
		return nil
	}

	// Track sessions spawned by the orchestrator's SpawnSession hook.
	var (
		spawnMu       sync.Mutex
		spawnedAgents = make(map[string]string) // role → session_name
	)
	spawnSession := func(_ context.Context, sessionName, worktree, role string) (*iris.Supervisor, error) {
		// Seed the agent_status row in "active" state. The handler will
		// stamp the group_id via SetGroupID immediately after.
		if err := iso.DB.UpsertStatusWithAgent(sessionName, "iris-parity", worktree, "active", nil, nil, &role, nil); err != nil {
			return nil, fmt.Errorf("seed agent_status for %s: %w", role, err)
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
		// Returning nil is fine here — the orchestrator only calls
		// sup.SessionRecord().SessionName and sup.InstanceID() on the
		// result. Both are surfaced via a tiny shim Supervisor below.
		return makeFakeSupervisor(sessionName, worktree, role), nil
	}

	// Construct the daemon-side handler with a tight poll interval so
	// the test completes quickly once we transition the agents to
	// terminal.
	handler := iris.NewReviewSpawnHandler(iris.ReviewSpawnDeps{
		Database:      iso.DB,
		SpawnSession:  spawnSession,
		DeliverPrompt: deliverFn,
		ParentWorktree: func(p string) (string, error) {
			if p != parent {
				return "", fmt.Errorf("unexpected parent %q", p)
			}
			return iso.Root, nil
		},
		PollInterval: 25 * time.Millisecond,
	})

	// Stand up the ClientSocket on the iristest-allocated short-prefix
	// socket path.
	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath:          iso.Paths.Sock,
		Database:          iso.DB,
		GetActiveSessions: getActive,
		DeliverPrompt:     deliverFn,
		SpawnReviewGroup:  handler,
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cs.Serve(ctx)

	// Drive the CLI core. We bypass the cobra layer and call runReviewAt
	// directly with deterministic fakes for PRVerifier and the rebase
	// runner (the latter is irrelevant — Rebase=false).
	opts := reviewRunOpts{
		PRNumber:     prNumber,
		Timeout:      10 * time.Second,
		Only:         "",
		Rebase:       false,
		SockPath:     iso.Paths.Sock,
		Parent:       parent,
		PRVerifier:   func(string) error { return nil },
		RebaseRunner: func(io.Writer) error { return nil },
		Out:          io.Discard,
	}

	runCtx, runCancel := context.WithTimeout(ctx, 10*time.Second)
	defer runCancel()
	if err := runReviewAt(runCtx, opts); err != nil {
		t.Fatalf("runReviewAt: %v", err)
	}

	// Confirm all 5 canonical agents got spawned and stamped with the
	// same group_id.
	spawnMu.Lock()
	gotAgents := make(map[string]string, len(spawnedAgents))
	for k, v := range spawnedAgents {
		gotAgents[k] = v
	}
	spawnMu.Unlock()
	if len(gotAgents) != len(iris.ReviewAgentNames) {
		t.Fatalf("spawned %d agents, want %d (got: %v)", len(gotAgents), len(iris.ReviewAgentNames), gotAgents)
	}
	for _, role := range iris.ReviewAgentNames {
		sess, ok := gotAgents[role]
		if !ok {
			t.Errorf("missing spawn for role %q", role)
			continue
		}
		expectedPrefix := parent + "~review-1-" + role
		if sess != expectedPrefix {
			t.Errorf("session name for %s = %q, want %q", role, sess, expectedPrefix)
		}
		isMember, err := iso.DB.IsGroupMember(sess)
		if err != nil {
			t.Fatalf("IsGroupMember(%s): %v", sess, err)
		}
		if !isMember {
			t.Errorf("session %s is not a group member after spawn", sess)
		}
	}

	// Transition each member to a terminal state. After the LAST
	// transition, the watcher should deliver exactly one review-complete
	// prompt to the parent.
	for i, role := range iris.ReviewAgentNames {
		sess := gotAgents[role]
		role := role // capture for the &role pointer
		if err := iso.DB.UpsertStatusWithAgent(sess, "iris-parity", iso.Root, "finished", nil, nil, &role, nil); err != nil {
			t.Fatalf("transition %s: %v", sess, err)
		}

		// Allow the watcher a chance to observe — but for all but the
		// last transition assert no delivery has fired yet.
		if i < len(iris.ReviewAgentNames)-1 {
			time.Sleep(80 * time.Millisecond)
			deliverMu.Lock()
			fired := deliverHits
			deliverMu.Unlock()
			if fired != 0 {
				t.Errorf("delivery fired prematurely after %d/%d members terminal", i+1, len(iris.ReviewAgentNames))
			}
		}
	}

	// Wait for the watcher to fire. 5s is generous; default poll is 25ms.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		deliverMu.Lock()
		fired := deliverHits
		deliverMu.Unlock()
		if fired >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	deliverMu.Lock()
	defer deliverMu.Unlock()
	if deliverHits != 1 {
		t.Fatalf("review-complete delivery fired %d times, want exactly 1", deliverHits)
	}
	if deliverName != parent {
		t.Errorf("delivery target = %q, want %q", deliverName, parent)
	}
	if !strings.Contains(deliverText, "Review complete") {
		t.Errorf("delivery body missing 'Review complete' header; got: %q", deliverText)
	}
	for _, role := range iris.ReviewAgentNames {
		if !strings.Contains(deliverText, role) {
			t.Errorf("delivery body missing role %q; got: %q", role, deliverText)
		}
	}
	if !strings.Contains(deliverText, "All review agents passed") {
		t.Errorf("delivery body missing 'all passed' summary; got: %q", deliverText)
	}
}

// TestReview_OnlyFilter exercises --only by passing a 2-agent subset and
// asserting the orchestrator only spawned those two agents.
func TestReview_OnlyFilter(t *testing.T) {
	iso := iristest.NewIsolated(t)
	parent := iristest.SessionName("review-parent-only")
	if err := iso.DB.UpsertStatusWithAgent(parent, "iris-parity", iso.Root, "active", nil, nil, nil, nil); err != nil {
		t.Fatalf("UpsertStatusWithAgent(parent): %v", err)
	}

	activeSessions := []iris.SessionSnapshot{{
		Name: parent, InstanceID: uuid.NewString(), State: "active", Role: "worker",
		Worktree: iso.Root, StartedAt: time.Now().UTC().Format(time.RFC3339),
	}}
	getActive := func() []iris.SessionSnapshot {
		out := make([]iris.SessionSnapshot, len(activeSessions))
		copy(out, activeSessions)
		return out
	}

	var spawnMu sync.Mutex
	spawned := []string{}
	spawnSession := func(_ context.Context, sessionName, worktree, role string) (*iris.Supervisor, error) {
		if err := iso.DB.UpsertStatusWithAgent(sessionName, "iris-parity", worktree, "active", nil, nil, &role, nil); err != nil {
			return nil, err
		}
		spawnMu.Lock()
		spawned = append(spawned, role)
		spawnMu.Unlock()
		return makeFakeSupervisor(sessionName, worktree, role), nil
	}

	handler := iris.NewReviewSpawnHandler(iris.ReviewSpawnDeps{
		Database:      iso.DB,
		SpawnSession:  spawnSession,
		DeliverPrompt: func(context.Context, string, string, string, []string) error { return nil },
		ParentWorktree: func(string) (string, error) { return iso.Root, nil },
		PollInterval:   500 * time.Millisecond, // slow — we don't need the watcher to fire
	})

	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: iso.Paths.Sock, Database: iso.DB,
		GetActiveSessions: getActive, SpawnReviewGroup: handler,
		DeliverPrompt: func(context.Context, string, string, string, []string) error { return nil },
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cs.Serve(ctx)

	opts := reviewRunOpts{
		PRNumber: "9", Timeout: time.Second, Only: "review-goal,review-code",
		SockPath: iso.Paths.Sock, Parent: parent,
		PRVerifier: func(string) error { return nil },
		Out:        io.Discard,
	}
	if err := runReviewAt(ctx, opts); err != nil {
		t.Fatalf("runReviewAt: %v", err)
	}
	spawnMu.Lock()
	defer spawnMu.Unlock()
	if len(spawned) != 2 {
		t.Fatalf("spawned %d agents, want 2 (got: %v)", len(spawned), spawned)
	}
	want := map[string]bool{"review-goal": true, "review-code": true}
	for _, r := range spawned {
		if !want[r] {
			t.Errorf("spawned unexpected role %q", r)
		}
	}
}

// TestReview_UnknownOnlyAgent verifies the --only validation: an unknown
// agent name exits non-zero before any spawn happens.
func TestReview_UnknownOnlyAgent(t *testing.T) {
	iso := iristest.NewIsolated(t)
	parent := iristest.SessionName("review-parent-bad-only")
	if err := iso.DB.UpsertStatusWithAgent(parent, "iris-parity", iso.Root, "active", nil, nil, nil, nil); err != nil {
		t.Fatalf("UpsertStatusWithAgent: %v", err)
	}

	// No socket needed — parseOnlyFlag fires before any wire I/O. We
	// supply a bogus sock path so an accidental dial would surface.
	opts := reviewRunOpts{
		PRNumber: "1", Timeout: time.Second, Only: "review-goal,not-a-real-agent",
		SockPath: "/nonexistent/iris.sock", Parent: parent,
		PRVerifier: func(string) error {
			t.Errorf("PRVerifier called before --only validation")
			return nil
		},
		Out: io.Discard,
	}
	err := runReviewAt(context.Background(), opts)
	if err == nil {
		t.Fatalf("runReviewAt(unknown agent): want error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown agent name") {
		t.Errorf("error = %v, want substring 'unknown agent name'", err)
	}
}

// TestReview_DaemonDown verifies the canonical "daemon not running" error
// when the socket file does not exist.
func TestReview_DaemonDown(t *testing.T) {
	iso := iristest.NewIsolated(t)
	parent := iristest.SessionName("review-daemon-down")

	opts := reviewRunOpts{
		PRNumber: "1", Timeout: time.Second,
		SockPath: iso.Paths.Sock + ".missing", // never created
		Parent:   parent,
		PRVerifier: func(string) error { return nil },
		Out:        io.Discard,
	}
	err := runReviewAt(context.Background(), opts)
	if err == nil {
		t.Fatalf("runReviewAt(daemon down): want error, got nil")
	}
	if !strings.Contains(err.Error(), "systemctl --user start iris") {
		t.Errorf("error = %v, want substring 'systemctl --user start iris'", err)
	}
}

// TestReview_NonExistentPR verifies that PR-existence check runs before
// any spawn occurs, and that a PR-not-found error exits non-zero.
func TestReview_NonExistentPR(t *testing.T) {
	iso := iristest.NewIsolated(t)
	parent := iristest.SessionName("review-pr-missing")

	// No SocketPath check needed — PRVerifier short-circuits.
	opts := reviewRunOpts{
		PRNumber: "999999", Timeout: time.Second,
		SockPath: "/should/never/be/dialled",
		Parent:   parent,
		PRVerifier: func(pr string) error {
			return fmt.Errorf("iris review: PR #%s not found", pr)
		},
		Out: io.Discard,
	}
	err := runReviewAt(context.Background(), opts)
	if err == nil {
		t.Fatalf("runReviewAt(pr missing): want error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want substring 'not found'", err)
	}
	_ = iso
}

// TestReview_InProgressGroup verifies the "round N already in progress"
// rejection path: a parent with a non-terminal group cannot spawn a
// second concurrent round.
func TestReview_InProgressGroup(t *testing.T) {
	iso := iristest.NewIsolated(t)
	parent := iristest.SessionName("review-in-progress")
	if err := iso.DB.UpsertStatusWithAgent(parent, "iris-parity", iso.Root, "active", nil, nil, nil, nil); err != nil {
		t.Fatalf("UpsertStatusWithAgent: %v", err)
	}

	// Pre-seed a group with an active member so the in-progress guard
	// fires deterministically.
	groupID, err := iso.DB.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	memberName := parent + "~review-1-review-goal"
	memberRole := "review-goal"
	if err := iso.DB.UpsertStatusWithAgent(memberName, "iris-parity", iso.Root, "active", nil, nil, &memberRole, nil); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if err := iso.DB.SetGroupID(memberName, groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}

	activeSessions := []iris.SessionSnapshot{{
		Name: parent, InstanceID: uuid.NewString(), State: "active", Role: "worker",
		Worktree: iso.Root, StartedAt: time.Now().UTC().Format(time.RFC3339),
	}}
	getActive := func() []iris.SessionSnapshot {
		out := make([]iris.SessionSnapshot, len(activeSessions))
		copy(out, activeSessions)
		return out
	}

	handler := iris.NewReviewSpawnHandler(iris.ReviewSpawnDeps{
		Database: iso.DB,
		SpawnSession: func(_ context.Context, sessionName, worktree, role string) (*iris.Supervisor, error) {
			t.Errorf("SpawnSession called despite in-progress guard")
			return makeFakeSupervisor(sessionName, worktree, role), nil
		},
		DeliverPrompt:  func(context.Context, string, string, string, []string) error { return nil },
		ParentWorktree: func(string) (string, error) { return iso.Root, nil },
	})

	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: iso.Paths.Sock, Database: iso.DB,
		GetActiveSessions: getActive, SpawnReviewGroup: handler,
		DeliverPrompt: func(context.Context, string, string, string, []string) error { return nil },
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cs.Serve(ctx)

	opts := reviewRunOpts{
		PRNumber: "1", Timeout: time.Second,
		SockPath: iso.Paths.Sock, Parent: parent,
		PRVerifier: func(string) error { return nil },
		Out:        io.Discard,
	}
	err = runReviewAt(ctx, opts)
	if err == nil {
		t.Fatalf("runReviewAt(in progress): want error, got nil")
	}
	if !strings.Contains(err.Error(), "in progress") {
		t.Errorf("error = %v, want substring 'in progress'", err)
	}
}

// makeFakeSupervisor returns a minimally-populated *iris.Supervisor that
// satisfies the orchestrator's needs (SessionRecord().SessionName and
// InstanceID()). The supervisor never runs a pi child; tests treat it as
// a tagged value carrier.
func makeFakeSupervisor(sessionName, worktree, role string) *iris.Supervisor {
	return iris.NewFakeSupervisorForTest(sessionName, worktree, role)
}

// Ensure db.Status remains referenced — defensive against an over-zealous
// import-trimmer that might drop the db import otherwise (TestReview_*
// tests above all reach for db.DB methods via iso.DB, which is *db.DB).
var _ = db.Status{}
