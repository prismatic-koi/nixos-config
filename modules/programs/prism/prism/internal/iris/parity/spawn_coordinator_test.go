package parity_test

// spawn_coordinator_test.go — §10.3 checklist item: "Spawn coordinator session".
//
// D-10 AC (functional, spawn coordinator):
//
//   A test spawns a coordinator session via iris on a branch named `main`
//   and asserts (a) the resolved agent name is `coordinator`, (b) a `bash`
//   tool call invoking `prism merge --help` is permitted (coordinator's
//   bash allow list), and (c) a worker session spawned on a feature branch
//   resolves to agent `worker` and the same `bash` call invoking
//   `prism merge --help` is denied by the bash allow list.
//
// Mechanics:
//
//   - The default-agent rule lives in iris.ResolveAgent (mirrors prism's
//     session.DefaultAgent). It returns "coordinator" when the worktree's
//     parent has a .bare marker and the basename is "main".
//
//   - The bash allow/deny list lives in iris.CheckBashPermission. The
//     iris bash dispatcher calls it before any subprocess starts. The
//     parity contract is the role-keyed decision, exercised by sending a
//     `prism merge --help` tool_exec from the fake extension and asserting
//     the resulting tool_exec_result.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

// makeBareWorktreeLayout creates a prism-style bare+worktree layout:
//
//	<root>/.bare
//	<root>/main/
//	<root>/feat-branch/
//
// Returns the root path. Caller's t.TempDir() ownership handles cleanup.
func makeBareWorktreeLayout(t *testing.T, root, featBranch string) (mainDir, featDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".bare"), []byte("gitdir"), 0o644); err != nil {
		t.Fatalf("makeBareWorktreeLayout: write .bare: %v", err)
	}
	mainDir = filepath.Join(root, "main")
	featDir = filepath.Join(root, featBranch)
	for _, d := range []string{mainDir, featDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("makeBareWorktreeLayout: mkdir %q: %v", d, err)
		}
	}
	return mainDir, featDir
}

// TestParitySpawnCoordinator_DefaultAgentAndBashPerm covers (a), (b), (c).
func TestParitySpawnCoordinator_DefaultAgentAndBashPerm(t *testing.T) {
	iso := iristest.NewIsolated(t)

	// Create a prism-style bare+worktree layout under iso.Root so the
	// default-agent heuristic resolves correctly.
	layoutRoot := filepath.Join(iso.Root, "bare-layout")
	if err := os.MkdirAll(layoutRoot, 0o755); err != nil {
		t.Fatalf("mkdir layoutRoot: %v", err)
	}
	mainDir, featDir := makeBareWorktreeLayout(t, layoutRoot, "feat-spawn-coordinator")

	// (a) Resolved agent name for the `main` worktree is "coordinator".
	gotCoord := iris.ResolveAgent(mainDir, "")
	if gotCoord != "coordinator" {
		t.Errorf("ResolveAgent(%q, \"\") = %q, want %q", mainDir, gotCoord, "coordinator")
	}
	gotWorker := iris.ResolveAgent(featDir, "")
	if gotWorker != "worker" {
		t.Errorf("ResolveAgent(%q, \"\") = %q, want %q", featDir, gotWorker, "worker")
	}

	// Spawn a coordinator session against the `main` worktree.
	coord := newFakeSession(t, iso, fakeSessionOptions{
		Role:        "coordinator",
		Worktree:    mainDir,
		SessionName: iristest.SessionName("coord-main"),
	})
	if got := coord.HelloAck["session_role"]; got != "coordinator" {
		t.Errorf("coord hello_ack.session_role = %v, want %q", got, "coordinator")
	}

	// Spawn a worker session against the feature worktree.
	worker := newFakeSession(t, iso, fakeSessionOptions{
		Role:        "worker",
		Worktree:    featDir,
		SessionName: iristest.SessionName("worker-feat"),
	})
	if got := worker.HelloAck["session_role"]; got != "worker" {
		t.Errorf("worker hello_ack.session_role = %v, want %q", got, "worker")
	}

	// (b) `prism merge --help` is permitted for the coordinator.
	coordResult, _ := coord.runDispatchedToolExec(t, map[string]any{
		"type": "tool_exec",
		"id":   "coord-prism-merge-help",
		"name": "bash",
		"args": map[string]any{
			// The actual command does not exist on the test runner, but the
			// permission check runs BEFORE the subprocess starts. We expect
			// the dispatcher to attempt execution (success=false from
			// exec-not-found is fine); the assertion is that the iris
			// permission gate did NOT block it. We detect a permission
			// block by the iris-specific error string emitted by
			// iris.CheckBashPermission.
			"command": "prism merge --help",
		},
	})
	output, _ := coordResult["output"].(string)
	if strings.Contains(output, "bash permission denied") {
		t.Errorf("coordinator: prism merge --help was denied by bash permission gate (output=%q) — coordinator must be allowed", output)
	}

	// (c) `prism merge --help` is denied for the worker.
	workerResult, _ := worker.runDispatchedToolExec(t, map[string]any{
		"type": "tool_exec",
		"id":   "worker-prism-merge-help",
		"name": "bash",
		"args": map[string]any{
			"command": "prism merge --help",
		},
	})
	if got := workerResult["success"]; got != false {
		t.Errorf("worker: tool_exec_result.success = %v, want false (prism merge must be denied)", got)
	}
	if got := workerResult["is_error"]; got != true {
		t.Errorf("worker: tool_exec_result.is_error = %v, want true", got)
	}
	workerOutput, _ := workerResult["output"].(string)
	if !strings.Contains(workerOutput, "permission denied") {
		t.Errorf("worker: tool_exec_result.output = %q, want it to mention permission denial", workerOutput)
	}
	if !strings.Contains(workerOutput, "prism merge") {
		t.Errorf("worker: tool_exec_result.output = %q, want it to name the blocked command", workerOutput)
	}

	// Double-check the pure-Go decision function so a future refactor that
	// moves the bash gate elsewhere still asserts the contract directly.
	if allowed, _ := iris.CheckBashPermission("coordinator", "prism merge --help"); !allowed {
		t.Error("CheckBashPermission(coordinator, prism merge --help) returned not-allowed; AC requires allowed")
	}
	if allowed, reason := iris.CheckBashPermission("worker", "prism merge --help"); allowed {
		t.Error("CheckBashPermission(worker, prism merge --help) returned allowed; AC requires denied")
	} else if !strings.Contains(reason, "prism merge") {
		t.Errorf("CheckBashPermission(worker, ...) reason = %q, want it to mention the blocked command", reason)
	}
}

// TestParitySpawnCoordinator_NonGitedBashIsAllowed asserts that bash
// commands unrelated to the role-keyed allow list run normally for both
// roles. This is the negative case ensuring the permission gate is not
// over-broad: any other command must be permitted regardless of role.
func TestParitySpawnCoordinator_NonGitedBashIsAllowed(t *testing.T) {
	iso := iristest.NewIsolated(t)
	worker := newFakeSession(t, iso, fakeSessionOptions{Role: "worker"})
	result, _ := worker.runDispatchedToolExec(t, map[string]any{
		"type": "tool_exec",
		"id":   "worker-echo",
		"name": "bash",
		"args": map[string]any{"command": "echo ok"},
	})
	if got := result["success"]; got != true {
		t.Errorf("worker echo success = %v, want true (output=%v)", got, result["output"])
	}
}
