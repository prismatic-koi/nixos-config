package cmd

// review_rebase_sandbox_3335_test.go — #3335: a sandboxed session's
// `prism review --rebase` runs the fetch/rebase/force-push in its own sandbox,
// not on the host. The host is asked to review WITHOUT rebase.

import (
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/sidecar"
)

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

	err := rebaseInSandbox("123")
	if err == nil || !strings.Contains(err.Error(), "not a prism worktree") {
		t.Fatalf("rebaseInSandbox = %v, want a 'not a prism worktree' refusal", err)
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
	rebaseInSandbox = func(prNumber string) error {
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
