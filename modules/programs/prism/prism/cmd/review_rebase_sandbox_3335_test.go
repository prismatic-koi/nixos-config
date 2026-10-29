package cmd

// review_rebase_sandbox_3335_test.go — #3335: a sandboxed session's
// `prism review --rebase` runs the fetch/rebase/force-push in its own sandbox,
// not on the host. The host is asked to review WITHOUT rebase.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config/configtest"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/sidecar"
)

// cmdGitOut runs git in cwd and returns its trimmed stdout, failing on error.
func cmdGitOut(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// writeTestFile writes content to path, failing on error.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newCmdPrismLayout builds a minimal prism bare+worktree layout for cmd tests:
// an origin bare repo, a project root with a `feat` worktree one commit ahead
// of main, and origin/main advanced by one commit so `feat` is behind (so a
// rebase would do real work). It returns the origin path, the project root,
// and the worker worktree.
func newCmdPrismLayout(t *testing.T) (origin, root, worker string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")

	tmp := t.TempDir()
	origin = filepath.Join(tmp, "origin.git")
	runGitOrFail(t, "", "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(tmp, "seed")
	runGitOrFail(t, "", "clone", "-q", origin, seed)
	writeTestFile(t, filepath.Join(seed, "README.md"), "init\n")
	runGitOrFail(t, seed, "add", ".")
	runGitOrFail(t, seed, "commit", "-q", "-m", "init")
	runGitOrFail(t, seed, "push", "-q", "origin", "main")

	root = filepath.Join(tmp, "proj")
	configtest.AllowProjectRoots(t, root)
	if err := git.CloneWorktree(origin, root, func(string) {}); err != nil {
		t.Fatalf("CloneWorktree: %v", err)
	}
	created, err := git.CreateWorktree(root, "feat")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	worker = created.Path
	writeTestFile(t, filepath.Join(worker, "feat.txt"), "feat\n")
	runGitOrFail(t, worker, "add", ".")
	runGitOrFail(t, worker, "commit", "-q", "-m", "feat")
	runGitOrFail(t, worker, "push", "-q", "-u", "origin", "feat")

	// Advance origin/main so feat is behind.
	writeTestFile(t, filepath.Join(seed, "main.txt"), "main\n")
	runGitOrFail(t, seed, "add", ".")
	runGitOrFail(t, seed, "commit", "-q", "-m", "main advances")
	runGitOrFail(t, seed, "push", "-q", "origin", "main")
	return origin, root, worker
}

// TestRebaseInSandbox_RefusesNonWorktree checks that the in-sandbox rebase
// refuses a PRISM_SPAWN_PATH that is not a prism worktree (a plain clone),
// before it runs any rebase — the same worktree gate the host pre-flight uses
// (#3323, #3335). This covers the rebaseInSandbox glue's refusal path.
func TestRebaseInSandbox_RefusesNonWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	runGitOrFail(t, "", "init", "-q", "--bare", origin)
	plain := filepath.Join(tmp, "plain")
	runGitOrFail(t, "", "clone", "-q", origin, plain)
	t.Setenv("PRISM_SPAWN_PATH", plain)

	err := rebaseInSandbox("123", nil)
	if err == nil || !strings.Contains(err.Error(), "not a prism worktree") {
		t.Fatalf("rebaseInSandbox = %v, want a 'not a prism worktree' refusal", err)
	}
}

// TestRunReview_HostDirectRebaseRefusesSandboxedSession checks that the
// host-direct path refuses --rebase when the parent session is sandboxed
// (#3335). A bwrap/sandbox-exec session also has a host `term` window whose
// shell carries no PRISM_HOST_API, so `prism review --rebase` typed there
// reaches the host-direct branch; running git rebase on the host in the
// worker's worktree would reopen the success-path race. The refusal must fire
// before any host git runs.
//
// Non-vacuous: drop the isolation-mode check and the host-direct path runs on
// to EnsurePrismWorktree/forge/Preflight, so the error no longer names the
// host --rebase refusal.
func TestRunReview_HostDirectRebaseRefusesSandboxedSession(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_SPAWN_PATH", "")

	d := openReviewTestDB(t)
	const session = "testrepo@feat"
	t.Setenv("PRISM_SESSION_NAME", session)
	// A sandboxed (bwrap) session row. The worktree path need not be a real
	// prism worktree: the isolation refusal fires before EnsurePrismWorktree
	// and any git call.
	if err := d.UpsertStatus(session, "testrepo", t.TempDir(), "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetIsolationMode(session, "bwrap"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}

	if err := reviewCmd.Flags().Set("rebase", "true"); err != nil {
		t.Fatalf("set --rebase: %v", err)
	}
	t.Cleanup(func() { _ = reviewCmd.Flags().Set("rebase", "false") })

	err := runReview(reviewCmd, []string{"123"})
	if err == nil || !strings.Contains(err.Error(), "not allowed on the host") {
		t.Fatalf("runReview = %v, want a host --rebase refusal for a sandboxed session", err)
	}
	if !strings.Contains(err.Error(), "bwrap") {
		t.Errorf("refusal should name the bwrap isolation mode: %v", err)
	}
}

// TestRunReview_HostDirectRebaseAllowedForHostSession checks the other side of
// the #3335 host-direct gate: a genuine host-mode session is NOT refused by
// the isolation check — it proceeds past it (here to the prism-worktree check,
// which rejects the non-worktree temp dir). Without this, an inverted or
// widened check would silently refuse host-mode --rebase and nothing would
// fail.
func TestRunReview_HostDirectRebaseAllowedForHostSession(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_SPAWN_PATH", "")

	d := openReviewTestDB(t)
	const session = "testrepo@feat"
	t.Setenv("PRISM_SESSION_NAME", session)
	if err := d.UpsertStatus(session, "testrepo", t.TempDir(), "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetIsolationMode(session, "host"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}

	if err := reviewCmd.Flags().Set("rebase", "true"); err != nil {
		t.Fatalf("set --rebase: %v", err)
	}
	t.Cleanup(func() { _ = reviewCmd.Flags().Set("rebase", "false") })

	err := runReview(reviewCmd, []string{"123"})
	if err == nil || strings.Contains(err.Error(), "not allowed on the host") {
		t.Fatalf("host-mode --rebase must not be refused by the isolation gate; got %v", err)
	}
	if !strings.Contains(err.Error(), "not a prism worktree") {
		t.Errorf("host-mode --rebase should proceed past the isolation gate to the worktree check; got %v", err)
	}
}

// TestRunReview_SandboxedRebaseProgressQuietUnderWaitJSON checks that the
// in-sandbox rebase progress lines are dropped under --wait --json (so stdout
// stays JSON-only) and stream otherwise. The stub records the progress
// callback the container branch passes and calls it.
func TestRunReview_SandboxedRebaseProgressQuietUnderWaitJSON(t *testing.T) {
	openReviewTestDB(t)
	t.Setenv("PRISM_SESSION_NAME", "testrepo@feat")
	t.Setenv("PRISM_SPAWN_PATH", "")
	// A non-dialable socket is fine: rebaseInSandbox returns a sentinel error
	// before proxyReviewAsync is called.
	t.Setenv("PRISM_HOST_API", "unix:///nonexistent-3335.sock")

	sentinel := errors.New("sentinel: stop after progress")
	orig := rebaseInSandbox
	rebaseInSandbox = func(_ string, onProgress func(string)) error {
		onProgress("preflight: rebasing onto origin/main …")
		return sentinel
	}
	t.Cleanup(func() { rebaseInSandbox = orig })
	t.Cleanup(func() {
		_ = reviewCmd.Flags().Set("rebase", "false")
		_ = reviewCmd.Flags().Set("wait", "false")
		_ = reviewCmd.Flags().Set("json", "false")
	})

	run := func(wait, jsonOut bool) string {
		if err := reviewCmd.Flags().Set("rebase", "true"); err != nil {
			t.Fatalf("set --rebase: %v", err)
		}
		if err := reviewCmd.Flags().Set("wait", strconv.FormatBool(wait)); err != nil {
			t.Fatalf("set --wait: %v", err)
		}
		if err := reviewCmd.Flags().Set("json", strconv.FormatBool(jsonOut)); err != nil {
			t.Fatalf("set --json: %v", err)
		}
		return captureStdout(t, func() {
			if err := runReview(reviewCmd, []string{"123"}); !errors.Is(err, sentinel) {
				t.Fatalf("runReview = %v, want the sentinel error from the stub", err)
			}
		})
	}

	if out := run(true, true); strings.Contains(out, "rebasing onto") {
		t.Errorf("--wait --json: rebase progress must not reach stdout, got %q", out)
	}
	if out := run(false, false); !strings.Contains(out, "rebasing onto") {
		t.Errorf("without --wait --json: rebase progress should stream to stdout, got %q", out)
	}
}

// TestRebaseInSandbox_RefusesNonOpenPRBeforeRebase checks that rebaseInSandbox
// runs review.CheckPRState before review.Preflight, so a sandboxed --rebase
// does not rebase and force-push onto a non-OPEN PR (#2057 ordering, raised in
// review of #3335). For a merged PR the push would recreate the branch GitHub
// deleted on merge.
//
// Non-vacuous: drop the CheckPRState call in rebaseInSandbox and the merged
// PR falls through to Preflight, which rebases the (behind) worker branch and
// moves its HEAD — so the error is no longer a merged-PR refusal and HEAD
// changes.
func TestRebaseInSandbox_RefusesNonOpenPRBeforeRebase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	_, _, worker := newCmdPrismLayout(t)
	t.Setenv("PRISM_SPAWN_PATH", worker)
	t.Chdir(worker)

	// Stub gh: report the PR as MERGED so CheckPRState refuses.
	stubDir := t.TempDir()
	writeTestFile(t, filepath.Join(stubDir, "gh"),
		"#!/bin/sh\necho '{\"state\":\"MERGED\",\"mergedAt\":\"2020-01-01T00:00:00Z\"}'\nexit 0\n")
	if err := os.Chmod(filepath.Join(stubDir, "gh"), 0o755); err != nil {
		t.Fatalf("chmod gh: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	headBefore := cmdGitOut(t, worker, "rev-parse", "HEAD")

	err := rebaseInSandbox("123", nil)
	if err == nil || !strings.Contains(err.Error(), "merged") {
		t.Fatalf("rebaseInSandbox = %v, want a merged-PR refusal before any rebase", err)
	}
	if head := cmdGitOut(t, worker, "rev-parse", "HEAD"); head != headBefore {
		t.Errorf("worker HEAD moved from %q to %q; CheckPRState must refuse before the rebase", headBefore, head)
	}
}

// TestRunReview_SandboxedRebaseRunsInSandboxNotHost drives runReview in
// container mode (PRISM_HOST_API set) with --rebase and asserts two things:
//
//  1. the in-sandbox rebase (rebaseInSandbox) runs, with the PR number; and
//  2. the request proxied to the host carries rebase=false — the host must not
//     rebase for a sandboxed session (#3335).
//
// Non-vacuous: dropping the `if rebaseFlag { rebaseInSandbox(...) }` call makes
// (1) fail; restoring the old `rebaseFlag` argument to proxyReviewAsync makes
// (2) fail.
func TestRunReview_SandboxedRebaseRunsInSandboxNotHost(t *testing.T) {
	// A worker session, resolvable by rejectIfCoordinator's heuristic (not
	// "@main"), with an isolated test DB.
	openReviewTestDB(t)
	t.Setenv("PRISM_SESSION_NAME", "testrepo@feat")
	t.Setenv("PRISM_SPAWN_PATH", "")

	// Mock host sidecar: capture the proxied request body, then ack + pass.
	bodyCh := make(chan string, 1)
	srv := newMockTCPServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case bodyCh <- string(b):
		default:
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Review in progress — PR #123, round 1 (group: 5103f218-aaaa-bbbb-cccc-2448dac6ab26)\n"))
		_, _ = w.Write([]byte(sidecar.ReviewSentinelPassed + "\n"))
	})
	t.Setenv("PRISM_HOST_API", srv.apiURL())

	// Stub the in-sandbox rebase so the test needs no git layout; record it.
	called := 0
	gotPR := ""
	orig := rebaseInSandbox
	rebaseInSandbox = func(prNumber string, _ func(string)) error {
		called++
		gotPR = prNumber
		return nil
	}
	t.Cleanup(func() { rebaseInSandbox = orig })

	if err := reviewCmd.Flags().Set("rebase", "true"); err != nil {
		t.Fatalf("set --rebase: %v", err)
	}
	t.Cleanup(func() { _ = reviewCmd.Flags().Set("rebase", "false") })

	_ = captureStdout(t, func() {
		if err := runReview(reviewCmd, []string{"123"}); err != nil {
			t.Fatalf("runReview: %v", err)
		}
	})

	if called != 1 {
		t.Fatalf("rebaseInSandbox called %d time(s), want 1 (the rebase must run in the sandbox)", called)
	}
	if gotPR != "123" {
		t.Errorf("rebaseInSandbox pr = %q, want \"123\"", gotPR)
	}

	select {
	case body := <-bodyCh:
		var req map[string]any
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal proxied body %q: %v", body, err)
		}
		if v, ok := req["rebase"]; ok && v == true {
			t.Errorf("proxied request carries rebase=true; the host must not rebase for a sandboxed session (#3335): %q", body)
		}
	default:
		t.Fatal("mock host sidecar received no /review request")
	}
}
