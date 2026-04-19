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
//
// TestCleanupYes_DefaultBranch verifies that cleanup of a @main session closes
// the session but preserves the worktree and branch.
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestWorktreePathFromSession_DBFallback verifies that worktreePathFromSession
// falls back to the DB when tmux has no record of the session (dead session).
//
// It seeds a temp DB with a status row containing a real filesystem path, then
// calls worktreePathFromSession with a session name that does not exist in tmux.
// The function should return the DB-stored path when it exists on disk, and ""
// when it does not.
func TestWorktreePathFromSession_DBFallback(t *testing.T) {
	// Create a real directory to act as the worktree path.
	existingPath := t.TempDir()
	nonExistentPath := filepath.Join(t.TempDir(), "gone")

	// Seed a temp DB.
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	sessionDead := "myrepo@dead-branch"
	sessionStale := "myrepo@stale-branch"

	if err := d.UpsertStatus(sessionDead, "myrepo", existingPath, "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus dead: %v", err)
	}
	if err := d.UpsertStatus(sessionStale, "myrepo", nonExistentPath, "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus stale: %v", err)
	}
	d.Close()

	// Point openDB at the temp DB for this test.
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// Neither session exists in tmux (no TMUX env, no server), so the tmux
	// path always returns "" — the function must fall back to the DB.

	t.Run("existing path returned from DB", func(t *testing.T) {
		got := worktreePathFromSession(sessionDead)
		if got != existingPath {
			t.Errorf("got %q, want %q", got, existingPath)
		}
	})

	t.Run("stale path (not on disk) returns empty", func(t *testing.T) {
		got := worktreePathFromSession(sessionStale)
		if got != "" {
			t.Errorf("got %q, want empty string (path does not exist)", got)
		}
	})

	t.Run("unknown session returns empty", func(t *testing.T) {
		got := worktreePathFromSession("myrepo@unknown")
		if got != "" {
			t.Errorf("got %q, want empty string (no DB row)", got)
		}
	})
}

// TestHeadlessCleanup_EmptyWorktreePath verifies that when worktreePath is
// empty, headlessCleanup skips worktree removal, still marks the session as
// ended in the DB, and returns nil.
//
// Runs without tmux, so it exercises only the DB-update path.
func TestHeadlessCleanup_EmptyWorktreePath(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "") // run host-side logic directly, not via proxy
	// Seed a temp DB.
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	session := "myrepo@ghost-branch"
	if err := d.UpsertStatus(session, "myrepo", "", "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	t.Run("empty worktreePath returns nil and marks session ended", func(t *testing.T) {
		err := headlessCleanup(session, "ghost-branch", "", "")
		if err != nil {
			t.Errorf("headlessCleanup returned error %v, want nil", err)
		}

		// Verify the DB row was updated to ended state.
		d2, err := db.Open(dbFile)
		if err != nil {
			t.Fatalf("re-open db: %v", err)
		}
		defer d2.Close()
		status, err := d2.CurrentStatus(session)
		if err != nil {
			t.Fatalf("CurrentStatus: %v", err)
		}
		if status == nil {
			t.Fatal("CurrentStatus returned nil — row missing")
		}
		if status.EndedAt == nil {
			t.Errorf("ended_at is nil — session was not marked as ended")
		}
	})
}

// TestHeadlessCleanup_InvalidWorktreePath verifies AC-2:
// when the worktree path exists on disk but is not a registered git worktree,
// headlessCleanup warns and continues rather than returning an error.
func TestHeadlessCleanup_InvalidWorktreePath(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "") // run host-side logic directly, not via proxy
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH — skipping integration test")
	}

	// Create a real directory that is NOT a git worktree.
	notAWorktree := t.TempDir()

	// Seed a temp DB.
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	session := "myrepo@stale-branch"
	if err := d.UpsertStatus(session, "myrepo", notAWorktree, "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// Use a fake bareRoot (also just a plain directory) so git.RemoveWorktree
	// fails with a "not a git worktree" error.
	fakeBareRoot := t.TempDir()

	err = headlessCleanup(session, "stale-branch", notAWorktree, fakeBareRoot)
	if err != nil {
		t.Errorf("headlessCleanup returned error %v, want nil (should warn and continue)", err)
	}

	// DB should still be marked ended.
	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("CurrentStatus returned nil — row missing")
	}
	if status.EndedAt == nil {
		t.Errorf("ended_at is nil — session was not marked as ended")
	}
}

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
	// Stage an empty commit. Disable signing explicitly: host gitconfig may
	// enable gpgsign globally (via home-manager), which would break this
	// commit in a test env with no signing key.
	cfgArgs := []string{
		"-C", initDir,
		"-c", "user.email=test@test.com",
		"-c", "user.name=Test",
		"-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false",
	}
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
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH — skipping integration test")
	}

	// Redirect XDG_STATE_HOME so the prism binary uses an isolated DB and
	// sidecar state dir instead of the production ~/.local/state/prism/ path.
	// Must be set before newCmdTestServer starts the tmux server.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// Clear PRISM_HOST_API so the spawned binary runs its host-side cleanup
	// path directly rather than proxying to a sidecar that doesn't know
	// about the test's isolated tmux server. This env var is inherited when
	// the test itself runs inside a prism-managed session.
	t.Setenv("PRISM_HOST_API", "")
	// Ignore the host's global gitconfig so the spawned prism binary is not
	// affected by host-level signing config (gpgsign=true without a key
	// available would break any git commit it runs).
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

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

// TestCleanupYes_DefaultBranch verifies that the headless (--yes) cleanup of a
// default-branch session (e.g. myrepo@main) closes the session but preserves
// the worktree directory and git branch.
//
// Layout:
//   - session "myrepo@main"  ← the target; has an "agent" window at mainWorktreePath
//   - session "other"        ← a bystander session
//   - clientTarget attached to "myrepo@main"
//   - clientOther attached to "other"
func TestCleanupYes_DefaultBranch(t *testing.T) {
	// Uses withCmdServer which mutates TmuxBin — must not be parallel.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH — skipping integration test")
	}

	// Redirect XDG_STATE_HOME so the prism binary uses an isolated DB and
	// sidecar state dir instead of the production ~/.local/state/prism/ path.
	// Must be set before newCmdTestServer starts the tmux server.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// Clear PRISM_HOST_API so the spawned binary runs its host-side cleanup
	// path directly rather than proxying to a sidecar that doesn't know
	// about the test's isolated tmux server. This env var is inherited when
	// the test itself runs inside a prism-managed session.
	t.Setenv("PRISM_HOST_API", "")
	// Ignore the host's global gitconfig so the spawned prism binary is not
	// affected by host-level signing config (gpgsign=true without a key
	// available would break any git commit it runs).
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	prismBin := buildPrismBinary(t)

	bareRoot, _, _ := setupMinimalBareRepo(t)
	// The default branch worktree is at bareRoot/main.
	mainWorktreePath := filepath.Join(bareRoot, "main")

	s := newCmdTestServer(t)
	withCmdServer(t, s)

	targetSession := "myrepo@main"

	// Create the target session rooted at the main worktree.
	if err := s.run("new-session", "-ds", targetSession, "-c", mainWorktreePath); err != nil {
		t.Fatalf("new-session %q: %v", targetSession, err)
	}
	if err := s.run("rename-window", "-t", targetSession+":0", "agent"); err != nil {
		t.Fatalf("rename-window agent: %v", err)
	}

	// Create the bystander session.
	s.newSession("other")

	// Attach clients.
	clientTarget := s.attachClientToSession(t, targetSession)
	clientOther := s.attachClientToSession(t, "other")

	// Invoke cleanup on the default-branch session.
	cleanupArgs := fmt.Sprintf("%s cleanup --yes --session %s", prismBin, targetSession)
	runInNewWindow(t, s, "other", "/tmp", cleanupArgs)

	// Poll until the target session disappears.
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

	// The worktree directory must still exist.
	if _, err := os.Stat(mainWorktreePath); err != nil {
		t.Errorf("worktree directory %q was removed — expected it to be preserved: %v",
			mainWorktreePath, err)
	}

	// The git branch "main" must still exist.
	bareDir := filepath.Join(bareRoot, ".bare")
	if err := exec.Command("git", "--git-dir", bareDir, "rev-parse", "--verify",
		"refs/heads/main").Run(); err != nil {
		t.Errorf("branch 'main' no longer exists — expected it to be preserved: %v", err)
	}

	// Poll until clientTarget lands on "scratchpad".
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
	if gotTarget != "" && gotTarget != "scratchpad" {
		t.Errorf("clientTarget session = %q, want %q — client was not redirected to scratchpad",
			gotTarget, "scratchpad")
	}
	if gotTarget == "" {
		t.Errorf("clientTarget: could not confirm session after cleanup (all clientSession calls failed)")
	}

	// The bystander client should still be on "other".
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

// ── non-worktree session tests ─────────────────────────────────────────────────

// TestHeadlessCloseSession_NonWorktree_MarksEnded verifies that
// headlessCloseSession marks the DB row as ended for a non-worktree session
// (no "@" in name, e.g. "obsidian").
//
// Runs without tmux, so it exercises only the DB-update path.
func TestHeadlessCloseSession_NonWorktree_MarksEnded(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "") // run host-side logic directly, not via proxy
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sessionName := fmt.Sprintf("prism-test-headless-%d", time.Now().UnixNano())
	if err := d.UpsertStatus(sessionName, "", "", "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	if err := headlessCloseSession(sessionName); err != nil {
		t.Fatalf("headlessCloseSession returned error %v, want nil", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	status, err := d2.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("CurrentStatus returned nil — row missing")
	}
	if status.EndedAt == nil {
		t.Errorf("ended_at is nil — session was not marked as ended")
	}
}

// TestHeadlessCloseSession_NonWorktree_NoDB verifies that headlessCloseSession
// exits 0 even when no DB row exists for the session (never recorded).
func TestHeadlessCloseSession_NonWorktree_NoDB(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "") // run host-side logic directly, not via proxy
	// Point openDB at an empty temp DB (no row for the session).
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	sessionName := fmt.Sprintf("prism-test-headless-%d", time.Now().UnixNano())
	if err := headlessCloseSession(sessionName); err != nil {
		t.Errorf("headlessCloseSession returned error %v, want nil (no DB row)", err)
	}
}

// TestCleanupYes_NonWorktreeSession verifies the end-to-end headless (--yes)
// cleanup path for a non-worktree session (no "@" in name, e.g. "obsidian").
//
// Layout:
//   - session "obsidian"  ← the target (no "@" in name)
//   - session "other"     ← a bystander session
//   - clientTarget attached to "obsidian"
//   - clientOther attached to "other"
//
// After cleanup:
//   - "obsidian" must no longer exist in tmux.
//   - clientTarget must be redirected to "scratchpad".
//   - clientOther must remain on "other".
//   - No git commands are invoked (absence is inferred from the absence of a
//     bare repo — if git ops were called they would fail and surface as errors).
func TestCleanupYes_NonWorktreeSession(t *testing.T) {
	// Uses withCmdServer which mutates TmuxBin — must not be parallel.

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// Ignore the host's global gitconfig so the spawned prism binary is not
	// affected by host-level signing config (gpgsign=true without a key
	// available would break any git commit it runs).
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	// Clear PRISM_HOST_API so the spawned binary runs its host-side cleanup
	// path directly rather than proxying to a sidecar that doesn't know
	// about the test's isolated tmux server. This env var is inherited when
	// the test itself runs inside a prism-managed session.
	t.Setenv("PRISM_HOST_API", "")

	prismBin := buildPrismBinary(t)

	s := newCmdTestServer(t)
	withCmdServer(t, s)

	targetSession := "obsidian"

	// Create the target session.
	if err := s.run("new-session", "-ds", targetSession, "-c", "/tmp"); err != nil {
		t.Fatalf("new-session %q: %v", targetSession, err)
	}

	// Create the bystander session.
	s.newSession("other")

	// Attach clients.
	clientTarget := s.attachClientToSession(t, targetSession)
	clientOther := s.attachClientToSession(t, "other")

	// Invoke `prism cleanup --yes --session obsidian`.
	cleanupArgs := fmt.Sprintf("%s cleanup --yes --session %s", prismBin, targetSession)
	runInNewWindow(t, s, "other", "/tmp", cleanupArgs)

	// Poll until the target session disappears.
	nwDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(nwDeadline) {
		if !s.hasSession(targetSession) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if s.hasSession(targetSession) {
		t.Fatalf("session %q still exists after cleanup (timed out)", targetSession)
	}

	// Poll until clientTarget lands on "scratchpad".
	var gotTargetNW string
	targetDeadlineNW := time.Now().Add(5 * time.Second)
	for time.Now().Before(targetDeadlineNW) {
		if sess, err := s.clientSession(clientTarget); err == nil {
			gotTargetNW = sess
			if sess == "scratchpad" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if gotTargetNW != "" && gotTargetNW != "scratchpad" {
		t.Errorf("clientTarget session = %q, want %q — client was not redirected to scratchpad",
			gotTargetNW, "scratchpad")
	}
	if gotTargetNW == "" {
		t.Errorf("clientTarget: could not confirm session after cleanup (all clientSession calls failed)")
	}

	// The bystander client should still be on "other".
	var gotOtherNW string
	otherDeadlineNW := time.Now().Add(2 * time.Second)
	for time.Now().Before(otherDeadlineNW) {
		if sess, err := s.clientSession(clientOther); err == nil {
			gotOtherNW = sess
		}
		time.Sleep(50 * time.Millisecond)
	}
	if gotOtherNW != "" && gotOtherNW != "other" {
		t.Errorf("clientOther session = %q, want %q — unrelated client was incorrectly moved",
			gotOtherNW, "other")
	}
}

// TestHeadlessCloseSession_AlreadyDeadTmux_MarksEnded verifies that
// headlessCloseSession marks the DB row as ended even when the tmux session
// no longer exists (already killed or never started). This calls
// headlessCloseSession directly — it does not exercise cleanupCmd routing.
func TestHeadlessCloseSession_AlreadyDeadTmux_MarksEnded(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "") // run host-side logic directly, not via proxy
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sessionName := fmt.Sprintf("prism-test-headless-%d", time.Now().UnixNano())
	if err := d.UpsertStatus(sessionName, "", "", "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// headlessCloseSession is what the cleanup command routes to for non-@
	// sessions with --yes. Calling it directly with no tmux server exercises the
	// "session already dead" edge case: KillSession will fail silently and the
	// DB update must still succeed.
	if err := headlessCloseSession(sessionName); err != nil {
		t.Fatalf("headlessCloseSession returned error %v, want nil", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	status, err := d2.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("CurrentStatus returned nil — row missing")
	}
	if status.EndedAt == nil {
		t.Errorf("ended_at is nil — session was not marked as ended despite tmux session being absent")
	}
}
