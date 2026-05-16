package main

// pr_integration_test.go — end-to-end test of `iris pr <number>` against a
// real iris.ClientSocket and a real bare git repo on disk (issue #1702).
//
// The test stands up:
//
//   1. A real bare+worktree git repo (created with `git init --bare` + an
//      initial commit on main) so git.PRBranch's caller-supplied stub can
//      return a real branch name and git.CreateWorktree can actually create
//      a directory.
//   2. An in-process iris.ClientSocket whose SpawnSession and DeliverPrompt
//      callbacks are recorders.
//   3. Both halves are driven by runPRAt — which uses prOptions.ResolveBranch
//      to stub the `gh pr view` call, and prOptions.GHLookPath to stub the
//      `gh` PATH check.
//
// What this proves:
//   - The CLI passes the resolved worktree path to the daemon's session_spawn
//     handler — BareRoot propagation is therefore correct (daemon-side, by
//     #1680).
//   - When --prompt is given, prompt_deliver follows the spawn ack with the
//     same session name returned in the spawn frame.
//   - Existing-worktree reuse works (second invocation does not create a
//     duplicate).
//   - A dirty existing worktree refuses without spawning.
//   - A daemon-down dial produces the canonical wording.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
)

// prTestEnv bundles the temp dirs, bare repo, and live daemon socket used by
// the integration tests so each test reads cleanly.
type prTestEnv struct {
	bareRoot   string
	branch     string
	sockPath   string
	spawnCalls *int
	spawnArgs  *[]spawnCall
	deliver    *[]deliverCall
	mu         *sync.Mutex
}

type spawnCall struct {
	worktree string
	role     string
}

type deliverCall struct {
	name string
	text string
}

// newPRTestEnv creates a bare git repo with a single PR-branch commit and
// stands up an in-process iris.ClientSocket on a tempdir socket. Returns the
// env handle and the resolved branch name (which the test stub
// ResolveBranch should return).
func newPRTestEnv(t *testing.T) *prTestEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH — skipping integration test")
	}

	// Short prefix so the AF_UNIX path stays under 108 bytes.
	shortPrefix, err := os.MkdirTemp("", "iris-pr-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })

	bareRoot := filepath.Join(shortPrefix, "myrepo")
	bareDir := filepath.Join(bareRoot, ".bare")
	if err := os.MkdirAll(bareRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll bareRoot: %v", err)
	}
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}

	// Initial commit on main via an orphan worktree, then remove the worktree.
	initDir := filepath.Join(shortPrefix, "init-checkout")
	if out, err := exec.Command("git", "--git-dir", bareDir, "worktree",
		"add", "--orphan", "-b", "main", initDir).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add orphan: %v\n%s", err, out)
	}
	cfg := []string{
		"-C", initDir,
		"-c", "user.email=test@test.com",
		"-c", "user.name=Test",
		"-c", "commit.gpgsign=false",
	}
	if out, err := exec.Command("git", append(cfg,
		"commit", "--allow-empty", "-m", "init")...).CombinedOutput(); err != nil {
		t.Fatalf("git commit init: %v\n%s", err, out)
	}

	// Create the PR's head branch as a local ref so git.CreateWorktree can
	// check it out without needing a remote. (The shim ResolveBranch stub
	// will return this branch name as the resolved head.)
	const branch = "feat/pr-1234"
	if out, err := exec.Command("git", "--git-dir", bareDir, "branch",
		branch, "main").CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", bareDir, "worktree",
		"remove", "--force", initDir).CombinedOutput(); err != nil {
		t.Fatalf("git worktree remove init: %v\n%s", err, out)
	}

	// Stand up the in-process daemon socket.
	dbPath := filepath.Join(shortPrefix, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	sockPath := filepath.Join(shortPrefix, "iris.sock")
	runDir := filepath.Join(shortPrefix, "run")

	mu := &sync.Mutex{}
	spawnCalls := new(int)
	spawnArgs := new([]spawnCall)
	deliver := new([]deliverCall)

	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: sockPath,
		Database: database,
		GetActiveSessions: func() []iris.SessionSnapshot {
			// Return a snapshot containing whatever spawns have happened so
			// far so that the post-spawn `iris prompt` path (which calls
			// sessions_list first) can find the freshly spawned session.
			mu.Lock()
			defer mu.Unlock()
			out := make([]iris.SessionSnapshot, 0, *spawnCalls)
			for _, sc := range *spawnArgs {
				out = append(out, iris.SessionSnapshot{
					Name:       "iris-" + sc.role + "@" + filepath.Base(sc.worktree),
					InstanceID: "test-instance-" + sc.role,
					State:      "active",
					Role:       sc.role,
					Worktree:   sc.worktree,
					StartedAt:  time.Now().UTC().Format(time.RFC3339),
				})
			}
			return out
		},
		SpawnSession: func(ctx context.Context, worktree, role, _parent string, _ map[string]any) (*iris.Supervisor, error) {
			mu.Lock()
			*spawnCalls++
			*spawnArgs = append(*spawnArgs, spawnCall{worktree: worktree, role: role})
			mu.Unlock()
			// Build a minimal supervisor. PIBinaryPath=/bin/true means the
			// child would exit immediately, but we never call Start() —
			// NewSupervisor only binds the per-session harness socket.
			return iris.NewSupervisor(iris.SupervisorConfig{
				SessionName:  "iris-" + role + "@" + filepath.Base(worktree),
				Worktree:     worktree,
				Role:         role,
				BareRoot:     bareRoot,
				PIBinaryPath: "/bin/true",
				RunDir:       runDir,
				Database:     database,
			})
		},
		DeliverPrompt: func(_ context.Context, name, text, _ string, _ []string) error {
			mu.Lock()
			*deliver = append(*deliver, deliverCall{name: name, text: text})
			mu.Unlock()
			return nil
		},
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	srvCtx, srvCancel := context.WithCancel(context.Background())
	t.Cleanup(srvCancel)
	go cs.Serve(srvCtx)

	return &prTestEnv{
		bareRoot:   bareRoot,
		branch:     branch,
		sockPath:   sockPath,
		spawnCalls: spawnCalls,
		spawnArgs:  spawnArgs,
		deliver:    deliver,
		mu:         mu,
	}
}

// stubGHFound stubs prOptions.GHLookPath to pretend `gh` is on PATH.
func stubGHFound(string) (string, error) { return "/usr/bin/gh", nil }

// stubGHMissing stubs prOptions.GHLookPath to pretend `gh` is NOT on PATH.
func stubGHMissing(string) (string, error) {
	return "", errors.New(`exec: "gh": executable file not found in $PATH`)
}

// TestPR_HappyPath_NoPrompt drives the full wire path with no --prompt flag:
// branch resolves, worktree is created, daemon receives session_spawn with
// the worktree path, and DeliverPrompt is NEVER invoked.
func TestPR_HappyPath_NoPrompt(t *testing.T) {
	env := newPRTestEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var out bytes.Buffer
	opts := prOptions{
		PRNumber:      "1234",
		BareRoot:      env.bareRoot,
		SockPath:      env.sockPath,
		GHLookPath:    stubGHFound,
		ResolveBranch: func(_, _ string) (string, error) { return env.branch, nil },
	}
	if err := runPRAt(ctx, opts, &out); err != nil {
		t.Fatalf("runPRAt: %v\nstdout: %s", err, out.String())
	}

	env.mu.Lock()
	defer env.mu.Unlock()
	if *env.spawnCalls != 1 {
		t.Errorf("spawn calls = %d, want 1", *env.spawnCalls)
	}
	wantWorktree := filepath.Join(env.bareRoot, env.branch)
	if got := (*env.spawnArgs)[0].worktree; got != wantWorktree {
		t.Errorf("spawn worktree = %q, want %q", got, wantWorktree)
	}
	if got := (*env.spawnArgs)[0].role; got != "worker" {
		t.Errorf("spawn role = %q, want %q", got, "worker")
	}
	if len(*env.deliver) != 0 {
		t.Errorf("DeliverPrompt invoked %d times without --prompt; want 0", len(*env.deliver))
	}
	// Worktree directory should now exist on disk.
	if _, err := os.Stat(wantWorktree); err != nil {
		t.Errorf("expected worktree at %s: %v", wantWorktree, err)
	}
	// Output should include the session UUID and worktree path.
	stdout := out.String()
	if !strings.Contains(stdout, "spawned") {
		t.Errorf("expected 'spawned' in stdout, got %q", stdout)
	}
	if !strings.Contains(stdout, wantWorktree) {
		t.Errorf("expected worktree path %q in stdout, got %q", wantWorktree, stdout)
	}
}

// TestPR_HappyPath_WithPrompt asserts that --prompt is delivered to the
// freshly spawned session after the spawn ack.
func TestPR_HappyPath_WithPrompt(t *testing.T) {
	env := newPRTestEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const promptBody = "review this PR"
	var out bytes.Buffer
	opts := prOptions{
		PRNumber:      "1234",
		BareRoot:      env.bareRoot,
		SockPath:      env.sockPath,
		Prompt:        promptBody,
		HasPrompt:     true,
		GHLookPath:    stubGHFound,
		ResolveBranch: func(_, _ string) (string, error) { return env.branch, nil },
	}
	if err := runPRAt(ctx, opts, &out); err != nil {
		t.Fatalf("runPRAt: %v\nstdout: %s", err, out.String())
	}

	env.mu.Lock()
	defer env.mu.Unlock()
	if *env.spawnCalls != 1 {
		t.Errorf("spawn calls = %d, want 1", *env.spawnCalls)
	}
	if len(*env.deliver) != 1 {
		t.Fatalf("DeliverPrompt invoked %d times, want 1; spawn calls=%d", len(*env.deliver), *env.spawnCalls)
	}
	if got := (*env.deliver)[0].text; got != promptBody {
		t.Errorf("delivered text = %q, want %q", got, promptBody)
	}
	wantSessionName := "iris-worker@" + env.branch
	// Branch name has a slash so basename is the trailing component.
	wantSessionName = "iris-worker@" + filepath.Base(env.branch)
	if got := (*env.deliver)[0].name; got != wantSessionName {
		t.Errorf("delivered name = %q, want %q", got, wantSessionName)
	}
	if !strings.Contains(out.String(), "prompt delivered") {
		t.Errorf("expected 'prompt delivered' in stdout, got %q", out.String())
	}
}

// TestPR_WorktreeReuse asserts that a second invocation against the same PR
// reuses the existing worktree (no duplicate spawn-side worktree path
// mismatch, no error).
func TestPR_WorktreeReuse(t *testing.T) {
	env := newPRTestEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	opts := prOptions{
		PRNumber:      "1234",
		BareRoot:      env.bareRoot,
		SockPath:      env.sockPath,
		GHLookPath:    stubGHFound,
		ResolveBranch: func(_, _ string) (string, error) { return env.branch, nil },
	}
	var out1 bytes.Buffer
	if err := runPRAt(ctx, opts, &out1); err != nil {
		t.Fatalf("first runPRAt: %v", err)
	}
	var out2 bytes.Buffer
	if err := runPRAt(ctx, opts, &out2); err != nil {
		t.Fatalf("second runPRAt: %v\nstdout=%s", err, out2.String())
	}
	if !strings.Contains(out2.String(), "reusing existing worktree") {
		t.Errorf("expected reuse message in second-call stdout, got %q", out2.String())
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if *env.spawnCalls != 2 {
		t.Errorf("spawn calls = %d, want 2", *env.spawnCalls)
	}
	// Both spawns must target the same worktree path.
	if (*env.spawnArgs)[0].worktree != (*env.spawnArgs)[1].worktree {
		t.Errorf("spawn worktrees differ: %q vs %q",
			(*env.spawnArgs)[0].worktree, (*env.spawnArgs)[1].worktree)
	}
}

// TestPR_DirtyWorktreeRefused asserts that a dirty existing worktree is
// refused before any spawn frame is sent.
func TestPR_DirtyWorktreeRefused(t *testing.T) {
	env := newPRTestEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// First call creates the worktree cleanly.
	opts := prOptions{
		PRNumber:      "1234",
		BareRoot:      env.bareRoot,
		SockPath:      env.sockPath,
		GHLookPath:    stubGHFound,
		ResolveBranch: func(_, _ string) (string, error) { return env.branch, nil },
	}
	var out1 bytes.Buffer
	if err := runPRAt(ctx, opts, &out1); err != nil {
		t.Fatalf("first runPRAt: %v", err)
	}
	worktree := filepath.Join(env.bareRoot, env.branch)

	// Dirty the worktree: add a tracked file then modify it after committing.
	// Easier: just write an untracked file — git diff --numstat HEAD reports
	// untracked? It does NOT. We need a modification to a tracked file or a
	// staged add. Use `git add` of a new file to make it appear in
	// `--cached`.
	junkPath := filepath.Join(worktree, "junk.txt")
	if err := os.WriteFile(junkPath, []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write junk.txt: %v", err)
	}
	if out, err := exec.Command("git",
		"-C", worktree,
		"-c", "user.email=test@test.com",
		"-c", "user.name=Test",
		"add", "junk.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	// Reset spawn counter so we can assert that no NEW spawn happened.
	env.mu.Lock()
	startSpawns := *env.spawnCalls
	startDelivers := len(*env.deliver)
	env.mu.Unlock()

	var out2 bytes.Buffer
	err := runPRAt(ctx, opts, &out2)
	if err == nil {
		t.Fatalf("runPRAt: want dirty-worktree error, got nil")
	}
	if !strings.Contains(err.Error(), "dirty") {
		t.Errorf("error missing 'dirty' wording: %q", err.Error())
	}

	env.mu.Lock()
	defer env.mu.Unlock()
	if *env.spawnCalls != startSpawns {
		t.Errorf("spawn calls = %d, want unchanged from %d", *env.spawnCalls, startSpawns)
	}
	if len(*env.deliver) != startDelivers {
		t.Errorf("deliver calls = %d, want unchanged from %d", len(*env.deliver), startDelivers)
	}
}

// TestPR_GHMissing asserts that a missing `gh` CLI produces the documented
// error without dialling the daemon.
func TestPR_GHMissing(t *testing.T) {
	env := newPRTestEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opts := prOptions{
		PRNumber:      "1234",
		BareRoot:      env.bareRoot,
		SockPath:      env.sockPath,
		GHLookPath:    stubGHMissing,
		ResolveBranch: func(_, _ string) (string, error) { return env.branch, nil },
	}
	var out bytes.Buffer
	err := runPRAt(ctx, opts, &out)
	if err == nil {
		t.Fatalf("runPRAt: want gh-missing error, got nil")
	}
	if !strings.Contains(err.Error(), "gh") {
		t.Errorf("error missing 'gh' wording: %q", err.Error())
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if *env.spawnCalls != 0 {
		t.Errorf("spawn calls = %d, want 0 (gh missing should short-circuit)", *env.spawnCalls)
	}
}

// TestPR_NoSuchPR asserts that a branch-resolver error (e.g. gh saying the PR
// doesn't exist) is surfaced cleanly and no worktree is created.
func TestPR_NoSuchPR(t *testing.T) {
	env := newPRTestEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opts := prOptions{
		PRNumber:   "9999",
		BareRoot:   env.bareRoot,
		SockPath:   env.sockPath,
		GHLookPath: stubGHFound,
		ResolveBranch: func(_, _ string) (string, error) {
			return "", errors.New("iris pr: could not resolve PR #9999: no pull requests found for branch")
		},
	}
	var out bytes.Buffer
	err := runPRAt(ctx, opts, &out)
	if err == nil {
		t.Fatalf("runPRAt: want no-such-PR error, got nil")
	}
	if !strings.Contains(err.Error(), "9999") {
		t.Errorf("error missing PR number: %q", err.Error())
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if *env.spawnCalls != 0 {
		t.Errorf("spawn calls = %d, want 0 (resolver error should short-circuit)", *env.spawnCalls)
	}
}

// TestPR_DaemonNotRunning asserts that pointing at a non-existent socket path
// surfaces the canonical "daemon not running … systemctl --user start iris"
// wording.
func TestPR_DaemonNotRunning(t *testing.T) {
	env := newPRTestEnv(t)
	// Replace sockPath with a non-existent path so the dial fails.
	missingSock := filepath.Join(filepath.Dir(env.sockPath), "no-such-iris.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opts := prOptions{
		PRNumber:      "1234",
		BareRoot:      env.bareRoot,
		SockPath:      missingSock,
		GHLookPath:    stubGHFound,
		ResolveBranch: func(_, _ string) (string, error) { return env.branch, nil },
	}
	var out bytes.Buffer
	err := runPRAt(ctx, opts, &out)
	if err == nil {
		t.Fatalf("runPRAt: want daemon-not-running error, got nil")
	}
	if !strings.Contains(err.Error(), "iris daemon not running") {
		t.Errorf("error missing 'iris daemon not running' wording: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "systemctl --user start iris") {
		t.Errorf("error missing systemctl hint: %q", err.Error())
	}
}
