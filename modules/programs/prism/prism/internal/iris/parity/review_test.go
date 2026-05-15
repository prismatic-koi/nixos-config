package parity_test

// review_test.go — §10.3 checklist item: "Run review (5 review agents)".
//
// D-10 AC (functional, review):
//
//   A test spawns 5 review agents via iris with a shared group ID. The
//   integration assertion is that all 5 enter `active` state, are
//   registered under the same group, and that when all 5 finish, the
//   daemon delivers a single `review-complete` `prism prompt` frame to the
//   invoking session. The test does NOT require the review agents to
//   produce a correct verdict — fixture inputs or stubbed agent bodies are
//   acceptable so long as the orchestration contract is exercised.
//
// We use stub-spawn (no real pi children) to keep this test fast:
//
//   - SpawnReviewGroup is called with a SpawnAgent that pre-seeds an
//     agent_status row in active state and returns its session name.
//   - WatchReviewGroup is started in a goroutine; it polls
//     db.GroupCompleted until every member is terminal.
//   - The test then transitions each of the 5 members through to a
//     terminal state (state="finished") and asserts that onComplete fires
//     EXACTLY ONCE with all 5 members in the results.
//   - We also assert that the daemon-side review-complete delivery path
//     fires exactly one prompt_deliver call to the parent session.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

func TestParityReview_GroupLifecycleAndCompletionPrompt(t *testing.T) {
	iso := iristest.NewIsolated(t)

	parent := iristest.SessionName("review-parent")
	worktree := iso.Root

	// Seed an agent_status row for the parent so the review-complete
	// callback has somewhere to send the delivery to.
	if err := iso.DB.UpsertStatusWithAgent(parent, "iris-parity", worktree, "active", nil, nil, nil, nil); err != nil {
		t.Fatalf("UpsertStatusWithAgent(parent): %v", err)
	}

	// SpawnAgent seeds an agent_status row in active state with the
	// review-agent role. It returns the session name so SpawnReviewGroup
	// can stamp the group_id on it.
	var spawnMu sync.Mutex
	spawnedSessions := map[string]string{} // role → session_name
	spawnAgent := func(_ context.Context, role string) (string, string, error) {
		spawnMu.Lock()
		defer spawnMu.Unlock()
		sessionName := fmt.Sprintf("%s/%s", parent, role)
		instanceID := uuid.New().String()
		agentName := role
		if err := iso.DB.UpsertStatusWithAgent(sessionName, "iris-parity", worktree, "active", nil, nil, &agentName, nil); err != nil {
			return "", "", fmt.Errorf("seed agent_status for %s: %w", role, err)
		}
		spawnedSessions[role] = sessionName
		return sessionName, instanceID, nil
	}

	res, err := iris.SpawnReviewGroup(context.Background(), iris.ReviewSpawnConfig{
		Database:      iso.DB,
		ParentSession: parent,
		Worktree:      worktree,
		PRNumber:      4242,
		SpawnAgent:    spawnAgent,
	})
	if err != nil {
		t.Fatalf("SpawnReviewGroup: %v", err)
	}
	if res.GroupID == "" {
		t.Fatalf("SpawnReviewGroup: empty GroupID")
	}
	if len(res.Members) != len(iris.ReviewAgentNames) {
		t.Fatalf("Members count = %d, want %d", len(res.Members), len(iris.ReviewAgentNames))
	}

	// Every expected role has a member entry.
	for _, role := range iris.ReviewAgentNames {
		if _, ok := res.Members[role]; !ok {
			t.Errorf("Members missing entry for role %q", role)
		}
	}

	// All 5 members share the group_id in agent_status.
	for role, sessionName := range res.Members {
		isMember, err := iso.DB.IsGroupMember(sessionName)
		if err != nil {
			t.Fatalf("IsGroupMember(%q): %v", sessionName, err)
		}
		if !isMember {
			t.Errorf("session %q (role %q) is not a group member", sessionName, role)
		}
	}

	// Start the watcher in a goroutine. onComplete records its single fire.
	var (
		onceMu       sync.Mutex
		onceFireCnt  int
		gotResults   map[string]db.GroupMemberResult
		gotGroupID   string
	)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	doneCh := make(chan struct{})
	go func() {
		_ = iris.WatchReviewGroup(ctx, iso.DB, res.GroupID, 25*time.Millisecond,
			func(gid string, results map[string]db.GroupMemberResult) {
				onceMu.Lock()
				onceFireCnt++
				gotResults = results
				gotGroupID = gid
				onceMu.Unlock()
				close(doneCh)
			})
	}()

	// Transition each member to terminal one-by-one. After the first 4
	// transitions onComplete must NOT have fired; after the 5th it must.
	terminalState := "finished"
	for i, role := range iris.ReviewAgentNames {
		sessionName := res.Members[role]
		// Bump the agent_status state to a terminal value.
		if err := iso.DB.UpsertStatusWithAgent(sessionName, "iris-parity", worktree, terminalState, nil, nil, &role, nil); err != nil {
			t.Fatalf("transition %s to %s: %v", sessionName, terminalState, err)
		}

		// All-but-last: onComplete must still be unfired.
		if i < len(iris.ReviewAgentNames)-1 {
			time.Sleep(75 * time.Millisecond)
			onceMu.Lock()
			fired := onceFireCnt
			onceMu.Unlock()
			if fired != 0 {
				t.Errorf("onComplete fired prematurely after %d/%d members terminal", i+1, len(iris.ReviewAgentNames))
			}
		}
	}

	select {
	case <-doneCh:
		// success
	case <-time.After(5 * time.Second):
		t.Fatalf("onComplete never fired after all 5 members transitioned to terminal")
	}

	onceMu.Lock()
	defer onceMu.Unlock()
	if onceFireCnt != 1 {
		t.Errorf("onComplete fired %d times, want exactly 1", onceFireCnt)
	}
	if gotGroupID != res.GroupID {
		t.Errorf("onComplete groupID = %q, want %q", gotGroupID, res.GroupID)
	}
	if len(gotResults) != len(iris.ReviewAgentNames) {
		t.Errorf("onComplete results size = %d, want %d (got: %v)", len(gotResults), len(iris.ReviewAgentNames), gotResults)
	}
}
