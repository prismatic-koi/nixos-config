package parity_test

// cleanup_test.go — §10.3 checklist item: "cleanup (kill a session and its
// artefacts)".
//
// D-10 AC (functional, cleanup):
//
//   A test cleans up a session and asserts the worktree, branch, tmux
//   session (if one was created), DB row, run-directory artefacts, and
//   per-session tmpdir are all removed.
//
// Mechanics:
//
//   - Build a temporary git bare+worktree layout so iris.CleanupSession's
//     `git worktree remove` path can run end-to-end.
//   - Seed the iris session row, the per-session run dir (with a sentinel
//     file inside its tmp/ subdir), and call CleanupSession with
//     RemoveWorktree=true.
//   - Assert: run dir gone, worktree gone, branch gone (via `git branch
//     --list`), DB row marked ended.
//
// Tmux: iris does not create tmux sessions. The AC's "tmux session (if
// one was created)" clause is vacuously satisfied; we still record that
// fact explicitly so a future change that does introduce tmux sessions
// doesn't silently miss the assertion.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

func TestParityCleanup_AllArtefactsRemoved(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	iso := iristest.NewIsolated(t)

	// Build a real bare+worktree layout.
	bareRoot := filepath.Join(iso.Root, "bare-layout")
	if err := os.MkdirAll(bareRoot, 0o755); err != nil {
		t.Fatalf("mkdir bareRoot: %v", err)
	}
	mustRun(t, bareRoot, "git", "init", "--bare", ".bare")
	// Make a "main" worktree first so a feature worktree can branch off.
	// Use an absolute --git-dir so the worktree path written into
	// refs/worktrees/ is the absolute one and `git --git-dir <abs>` can
	// resolve it later (matching how prism's deriveBareRoot works).
	absGitDir := filepath.Join(bareRoot, ".bare")
	mainDir := filepath.Join(bareRoot, "main")
	mustRun(t, bareRoot, "git", "--git-dir", absGitDir, "worktree", "add", "-B", "main", mainDir)
	// Seed an initial commit so feature branches have somewhere to root.
	mustRun(t, mainDir, "git", "commit", "--allow-empty", "-m", "init")

	featBranch := "parity-cleanup-feat"
	featDir := filepath.Join(bareRoot, featBranch)
	mustRun(t, bareRoot, "git", "--git-dir", absGitDir, "worktree", "add", "-b", featBranch, featDir, "main")

	sessionName := iristest.SessionName("cleanup")
	instanceID := "iris-test-cleanup-001"
	role := "worker"
	if err := iso.DB.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Worktree:    featDir,
		Harness:     "pi",
		AgentRole:   &role,
		StartedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := iso.DB.IrisUpdateSessionState(instanceID, string(iris.StateActive)); err != nil {
		t.Fatalf("IrisUpdateSessionState: %v", err)
	}

	// Seed the per-session run dir (harness socket placeholder + tmp/sentinel).
	sessionRunDir := filepath.Join(iso.Paths.RunDir, instanceID)
	if err := os.MkdirAll(filepath.Join(sessionRunDir, "tmp"), 0o700); err != nil {
		t.Fatalf("mkdir run/tmp: %v", err)
	}
	sentinel := filepath.Join(sessionRunDir, "tmp", "sentinel.log")
	if err := os.WriteFile(sentinel, []byte("from a tool subprocess"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:       iso.DB,
		RunDir:         iso.Paths.RunDir,
		ArchiveRoot:    iso.Paths.ArchiveRoot,
		PIAgentDir:     iso.PIAgentDir,
		RemoveWorktree: true,
	}, sessionName)
	if err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}

	// Worktree gone.
	if _, err := os.Stat(featDir); !os.IsNotExist(err) {
		t.Errorf("worktree %q still exists after cleanup (err=%v)", featDir, err)
	}
	if !res.WorktreeRemoved {
		t.Errorf("WorktreeRemoved = false, want true (errors=%v)", res.Errors)
	}

	// Branch gone. Use --git-dir <bareRoot>/.bare because bareRoot is the
	// prism wrapper directory, not the bare git repo itself.
	gitDir := filepath.Join(bareRoot, ".bare")
	out, err := exec.Command("git", "--git-dir", gitDir, "branch", "--list", featBranch).CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("branch %q still exists after cleanup: %q", featBranch, string(out))
	}
	if !res.BranchRemoved {
		t.Errorf("BranchRemoved = false, want true (errors=%v)", res.Errors)
	}

	// Per-session run directory + tmpdir gone.
	if _, err := os.Stat(sessionRunDir); !os.IsNotExist(err) {
		t.Errorf("run dir %q still exists after cleanup (err=%v)", sessionRunDir, err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Errorf("per-session tmpdir sentinel %q still exists after cleanup", sentinel)
	}
	if !res.RunDirRemoved {
		t.Errorf("RunDirRemoved = false, want true (errors=%v)", res.Errors)
	}

	// DB row: end_state set to "finished".
	sess, err := iso.DB.SessionByInstanceID(instanceID)
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if sess == nil {
		t.Fatalf("session row missing for %s", instanceID)
	}
	if sess.EndState == nil || *sess.EndState != "finished" {
		t.Errorf("end_state = %v, want \"finished\"", sess.EndState)
	}
	if !res.SessionRowRemoved {
		t.Errorf("SessionRowRemoved = false, want true (errors=%v)", res.Errors)
	}

	// Tmux: iris never created one. We assert no `iris-*` tmux session
	// exists for this test by NOT calling tmux at all — the parity AC says
	// "(if one was created)", so the absent-tmux-session case satisfies it.
	// A future change adding tmux integration to iris must add an
	// explicit tmux teardown assertion here.
}

// TestParityCleanup_CoordinatorWorktreeIsProtected verifies the coordinator
// invariant: cleanup refuses to remove a worktree named "main". This is a
// safety check that mirrors prism's behaviour.
func TestParityCleanup_CoordinatorWorktreeIsProtected(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	iso := iristest.NewIsolated(t)
	bareRoot := filepath.Join(iso.Root, "bare-coord")
	if err := os.MkdirAll(bareRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustRun(t, bareRoot, "git", "init", "--bare", ".bare")
	absGitDir := filepath.Join(bareRoot, ".bare")
	mainDir := filepath.Join(bareRoot, "main")
	mustRun(t, bareRoot, "git", "--git-dir", absGitDir, "worktree", "add", "-B", "main", mainDir)
	mustRun(t, mainDir, "git", "commit", "--allow-empty", "-m", "init")

	sessionName := iristest.SessionName("coord-protect")
	role := "coordinator"
	if err := iso.DB.InsertSession(db.Session{
		InstanceID:  "iris-test-coord-protect-001",
		SessionName: sessionName,
		Worktree:    mainDir,
		Harness:     "pi",
		AgentRole:   &role,
		StartedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:       iso.DB,
		RunDir:         iso.Paths.RunDir,
		ArchiveRoot:    iso.Paths.ArchiveRoot,
		PIAgentDir:     iso.PIAgentDir,
		RemoveWorktree: true,
	}, sessionName)
	if err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}
	if res.WorktreeRemoved {
		t.Errorf("WorktreeRemoved = true on coordinator main worktree; cleanup must protect it")
	}
	if _, err := os.Stat(mainDir); err != nil {
		t.Errorf("main worktree no longer exists at %q (err=%v) — coordinator invariant violated", mainDir, err)
	}
	if len(res.Errors) == 0 {
		t.Errorf("expected an error explaining the coordinator-worktree refusal")
	}
}

func mustRun(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=iris-parity",
		"GIT_AUTHOR_EMAIL=iris@parity.test",
		"GIT_COMMITTER_NAME=iris-parity",
		"GIT_COMMITTER_EMAIL=iris@parity.test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v in %q: %v: %s", name, args, dir, err, string(out))
	}
}
