package sidecar

// Worker-side capture of pr_number on the socket-pipe transport — the path
// every pi session takes. The SSE-path capture in handleMessagePartUpdated
// sees the command and the output on one event; the pipe transport splits
// them across a tool_call frame and a tool_result frame linked by id, so the
// sidecar must remember the call to act on the result.
//
// Tests drive handlePipeFrame with the wire shapes the pi extension emits
// (payload.ToolCall / payload.ToolResult).

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

func pipeToolCall(t *testing.T, id, name, command string) []byte {
	t.Helper()
	frame, err := json.Marshal(map[string]any{
		"type": "tool_call", "id": id, "name": name,
		"args": map[string]string{"command": command},
	})
	if err != nil {
		t.Fatalf("marshal tool_call: %v", err)
	}
	return frame
}

func pipeToolResult(t *testing.T, id string, success bool, output string) []byte {
	t.Helper()
	frame, err := json.Marshal(map[string]any{
		"type": "tool_result", "id": id, "success": success, "output": output,
	})
	if err != nil {
		t.Fatalf("marshal tool_result: %v", err)
	}
	return frame
}

func prNumberFor(t *testing.T, d *db.DB, iid string) *int {
	t.Helper()
	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil {
		t.Fatalf("SpawnOutcomeByInstanceID: %v", err)
	}
	if out == nil {
		return nil
	}
	return out.PRNumber
}

// TestHandlePipeFrame_GhPRCreate_PersistsPRNumber: a tool_call for
// `gh pr create` followed by its successful tool_result writes the PR number
// from the output to spawn_outcome.pr_number.
func TestHandlePipeFrame_GhPRCreate_PersistsPRNumber(t *testing.T) {
	d := openTestDB(t)
	const sess = "prism-test@pipe-pr-create"
	iid := uuid.New().String()
	sc := newWorkerSidecarWithInstance(t, d, sess, iid)
	_ = d.UpsertStatus(sess, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	sc.handlePipeFrame(pipeToolCall(t, "call-1", "bash", "gh pr create --title 'stats: fix aggregates' --body 'Closes #2932'"))
	if got := prNumberFor(t, d, iid); got != nil {
		t.Fatalf("pr_number written on the tool_call frame alone: got %d, want nil until the result arrives", *got)
	}
	sc.handlePipeFrame(pipeToolResult(t, "call-1", true,
		"Creating pull request for fix-stats into main in prismatic-koi/nixos-config\n\nhttps://github.com/prismatic-koi/nixos-config/pull/2933\n"))

	got := prNumberFor(t, d, iid)
	if got == nil {
		t.Fatal("pr_number: nil after a successful gh pr create on the pipe transport")
	}
	if *got != 2933 {
		t.Errorf("pr_number: got %d, want 2933", *got)
	}
	if sc.prCreateCalls.has("call-1") {
		t.Error("tracked call id must be consumed by its result")
	}
}

// TestHandlePipeFrame_GhPRView_DoesNotPersistPRNumber: a `gh pr view` whose
// output carries a /pull/N URL must not write.
func TestHandlePipeFrame_GhPRView_DoesNotPersistPRNumber(t *testing.T) {
	d := openTestDB(t)
	const sess = "prism-test@pipe-pr-view"
	iid := uuid.New().String()
	sc := newWorkerSidecarWithInstance(t, d, sess, iid)
	_ = d.UpsertStatus(sess, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	sc.handlePipeFrame(pipeToolCall(t, "call-2", "bash", "gh pr view 4242 --json url -q .url"))
	sc.handlePipeFrame(pipeToolResult(t, "call-2", true, "https://github.com/owner/repo/pull/4242\n"))

	if got := prNumberFor(t, d, iid); got != nil {
		t.Errorf("pr_number: got %d after gh pr view, want nil", *got)
	}
}

// TestHandlePipeFrame_GhPRCreateFailed_DoesNotPersistPRNumber: a failed
// `gh pr create` consumes the tracked call and writes nothing, even when the
// error text quotes an existing PR URL.
func TestHandlePipeFrame_GhPRCreateFailed_DoesNotPersistPRNumber(t *testing.T) {
	d := openTestDB(t)
	const sess = "prism-test@pipe-pr-create-failed"
	iid := uuid.New().String()
	sc := newWorkerSidecarWithInstance(t, d, sess, iid)
	_ = d.UpsertStatus(sess, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	sc.handlePipeFrame(pipeToolCall(t, "call-3", "bash", "gh pr create --title foo"))
	sc.handlePipeFrame(pipeToolResult(t, "call-3", false,
		"a pull request for branch \"foo\" into branch \"main\" already exists:\nhttps://github.com/owner/repo/pull/99\n"))

	if got := prNumberFor(t, d, iid); got != nil {
		t.Errorf("pr_number: got %d after a failed gh pr create, want nil", *got)
	}
	if sc.prCreateCalls.has("call-3") {
		t.Error("a failed result must still consume the tracked call id")
	}
}

// TestHandlePipeFrame_ToolResultForUntrackedCall_IsIgnored: a result whose
// id was never tracked (a non-bash tool, or a bash command that is not
// `gh pr create`) writes nothing even when its output carries a PR URL.
func TestHandlePipeFrame_ToolResultForUntrackedCall_IsIgnored(t *testing.T) {
	d := openTestDB(t)
	const sess = "prism-test@pipe-untracked"
	iid := uuid.New().String()
	sc := newWorkerSidecarWithInstance(t, d, sess, iid)
	_ = d.UpsertStatus(sess, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	sc.handlePipeFrame(pipeToolCall(t, "call-4", "read", "gh pr create"))
	sc.handlePipeFrame(pipeToolResult(t, "call-4", true, "https://github.com/owner/repo/pull/5\n"))
	sc.handlePipeFrame(pipeToolResult(t, "never-called", true, "https://github.com/owner/repo/pull/6\n"))

	if got := prNumberFor(t, d, iid); got != nil {
		t.Errorf("pr_number: got %d for an untracked call, want nil", *got)
	}
}
