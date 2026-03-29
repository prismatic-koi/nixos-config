// Package cmd integration tests for prism cleanup.
//
// TestCleanupYes verifies the headless (--yes) path of cleanupCmd:
//
//	prism cleanup --yes --session <project@branch>
//
// The test sets up a minimal bare git repository so that the binary can satisfy
// all its pre-flight checks (session contains "@", worktree path findable,
// bareRoot found, branch != default). It then asserts that:
//   - clients attached to the target session are redirected to scratchpad
//   - clients attached to other sessions are unaffected
//   - the target session is removed
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// setupMinimalBareRepo creates a minimal bare+worktree git repository layout
// under baseDir with the following structure:
//
//	baseDir/myrepo/          ← bareRoot (contains .bare/)
//	baseDir/myrepo/.bare/    ← bare git repo
//	baseDir/myrepo/main/     ← default branch worktree
//	baseDir/myrepo/feature/  ← feature branch worktree (the one we'll clean up)
//
// Returns (bareRoot, worktreePath, branchName).
func setupMinimalBareRepo(t *testing.T) (bareRoot, worktreePath, branchName string) {
	t.Helper()

	baseDir := t.TempDir()
	bareRoot = filepath.Join(baseDir, "myrepo")
	bareDir := filepath.Join(bareRoot, ".bare")
	branchName = "feature"

	if err := os.MkdirAll(bareRoot, 0o755); err != nil {
		t.Fatalf("mkdir bareRoot: %v", err)
	}

	// Initialize a bare repo.
	if out, err := exec.Command("git", "init", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}

	// Suppress "hint:" noise in tests.
	_ = exec.Command("git", "--git-dir", bareDir, "config", "advice.detachedHead", "false").Run()

	// Create an initial commit so the repo has a HEAD.
	// We'll use a temp checkout worktree to do the initial commit.
	initDir := filepath.Join(baseDir, "init-checkout")
	if out, err := exec.Command("git", "--git-dir", bareDir, "worktree",
		"add", "--orphan", "-b", "main", initDir).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add (orphan): %v\n%s", err, out)
	}
	// Stage an empty commit.
	cfgArgs := []string{"-C", initDir, "-c", "user.email=test@test.com", "-c", "user.name=Test"}
	if out, err := exec.Command("git", append(cfgArgs,
		"commit", "--allow-empty", "-m", "init")...).CombinedOutput(); err != nil {
		t.Fatalf("git commit (init): %v\n%s", err, out)
	}

	// Remove the temp checkout worktree — we only needed it for the initial commit.
	if out, err := exec.Command("git", "--git-dir", bareDir, "worktree",
		"remove", "--force", initDir).CombinedOutput(); err != nil {
		t.Fatalf("git worktree remove init-checkout: %v\n%s", err, out)
	}

	// Create the main worktree at bareRoot/main.
	mainDir := filepath.Join(bareRoot, "main")
	if out, err := exec.Command("git", "--git-dir", bareDir, "worktree",
		"add", mainDir, "main").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add main: %v\n%s", err, out)
	}

	// Create the feature branch and worktree at bareRoot/feature.
	worktreePath = filepath.Join(bareRoot, branchName)
	if out, err := exec.Command("git", "--git-dir", bareDir, "worktree",
		"add", "-b", branchName, worktreePath, "main").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add feature: %v\n%s", err, out)
	}

	return bareRoot, worktreePath, branchName
}

// TestCleanupYes_RedirectsClientsAndKillsSession is the end-to-end test for
// the non-interactive cleanup path.
//
// Layout:
//   - session "myrepo@feature"  ← the target; has an "agent" window at worktreePath
//   - session "other"           ← a bystander session
//   - clientTarget attached to "myrepo@feature"
//   - clientOther attached to "other"
//   - The binary is invoked from a pane in "other" so that TMUX is set to the
//     test server's socket and the binary can use tmux commands correctly.
func TestCleanupYes_RedirectsClientsAndKillsSession(t *testing.T) {
	// Uses withCmdServer which mutates TmuxBin — must not be parallel.

	prismBin := buildPrismBinary(t)

	_, worktreePath, branchName := setupMinimalBareRepo(t)

	s := newCmdTestServer(t)
	withCmdServer(t, s)

	targetSession := "myrepo@" + branchName // "myrepo@feature"

	// Create the target session with its first window (index 0) rooted at
	// worktreePath. We'll rename it "agent" so that worktreePathFromSession
	// can find the worktree path from the window list.
	if err := s.run("new-session", "-ds", targetSession, "-c", worktreePath); err != nil {
		t.Fatalf("new-session %q: %v", targetSession, err)
	}
	if err := s.run("rename-window", "-t", targetSession+":0", "agent"); err != nil {
		t.Fatalf("rename-window agent: %v", err)
	}

	// Create the "other" bystander session, used both as the bystander client's
	// home and as the pane from which we invoke the binary.
	s.newSession("other")

	// Attach a client to the target session and one to "other".
	clientTarget := s.attachClientToSession(t, targetSession)
	clientOther := s.attachClientToSession(t, "other")

	// Invoke `prism cleanup --yes --session <targetSession>` as a new-window
	// command on "other".  Using new-window avoids the PTY echo buffer
	// deadlock that send-keys causes with long command strings.  The
	// new-window's TMUX env points to the test socket so the binary can use
	// tmux commands against the right server.
	cleanupArgs := fmt.Sprintf("%s cleanup --yes --session %s", prismBin, targetSession)
	runInNewWindow(t, s, "other", "/tmp", cleanupArgs)

	// Poll until the target session disappears (cleanup completed).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !s.hasSession(targetSession) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if s.hasSession(targetSession) {
		t.Fatalf("session %q still exists after cleanup (timed out)", targetSession)
	}

	// Poll until clientTarget lands on "scratchpad".  headlessCleanup calls
	// SwitchClient before KillSession; we wait for the session to disappear
	// above, but the switch-client may not yet be committed in tmux's state at
	// that instant, so we poll rather than read once.
	var gotTarget string
	targetDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(targetDeadline) {
		if sess, err := s.clientSession(clientTarget); err == nil {
			gotTarget = sess
			if sess == "scratchpad" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Only assert if we got at least one successful read: a persistent error
	// (e.g. client briefly unreachable after session kill) is not evidence of
	// a wrong switch and should not cause a spurious failure.
	if gotTarget != "" && gotTarget != "scratchpad" {
		t.Errorf("clientTarget session = %q, want %q — client was not redirected to scratchpad",
			gotTarget, "scratchpad")
	}
	if gotTarget == "" {
		t.Errorf("clientTarget: could not confirm session after cleanup (all clientSession calls failed)")
	}

	// The bystander client should still be on "other".  Poll briefly for
	// stability rather than reading once.  Guard: only assert if at least one
	// successful read was obtained.
	var gotOther string
	otherDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(otherDeadline) {
		if sess, err := s.clientSession(clientOther); err == nil {
			gotOther = sess
		}
		time.Sleep(50 * time.Millisecond)
	}
	if gotOther != "" && gotOther != "other" {
		t.Errorf("clientOther session = %q, want %q — unrelated client was incorrectly moved",
			gotOther, "other")
	}
}
