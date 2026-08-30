// Unit tests for `prism close`.
//
// The tests fall into three groups:
//
//  1. decideClose — pure decision-tree tests that swap prProbe for a stub.
//  2. probePRStateExec — exercises the probe against the ghExecOutput exec
//     seam with canned outputs, so the JSON-parse and
//     state-reduction branches run in-process with no subprocess, no $PATH
//     dependence, and no network.
//  3. runCloseCmd — flag validation (mutually-exclusive force flags, --json
//     requires --yes, unknown --session) and the integration with the
//     existing cleanup-soft / cleanup-hard paths.
//
// Integration tests that drive tmux clients (the popup-style end-to-end) live
// alongside the cleanup integration tests further down; this file covers the
// command-internal logic without spinning up a tmux server.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

// withPRProbeStub swaps prProbe for the duration of the test.
func withPRProbeStub(t *testing.T, fn func(workdir, branch string) (string, error)) {
	t.Helper()
	orig := prProbe
	prProbe = fn
	t.Cleanup(func() { prProbe = orig })
}

// TestDecideClose_ForceFlagsWin confirms --keep-worktree and --remove-worktree
// short-circuit before the gh probe is ever consulted.
func TestDecideClose_ForceFlagsWin(t *testing.T) {
	// Probe should never run when a force flag is set — fail loudly if it does.
	withPRProbeStub(t, func(workdir, branch string) (string, error) {
		t.Errorf("prProbe should not be called when a force flag is set; got branch=%q", branch)
		return "", fmt.Errorf("unreachable")
	})

	t.Run("--keep-worktree forces soft close", func(t *testing.T) {
		soft, hard := decideClose("myrepo@feature", true, false)
		if !soft || hard {
			t.Errorf("decideClose(--keep-worktree) = (soft=%v, hard=%v), want (true, false)", soft, hard)
		}
	})

	t.Run("--remove-worktree forces hard cleanup", func(t *testing.T) {
		soft, hard := decideClose("myrepo@feature", false, true)
		if soft || !hard {
			t.Errorf("decideClose(--remove-worktree) = (soft=%v, hard=%v), want (false, true)", soft, hard)
		}
	})
}

// TestDecideClose_NonWorktreeSession verifies the non-"@" path soft-closes
// without consulting gh or the DB.
func TestDecideClose_NonWorktreeSession(t *testing.T) {
	withPRProbeStub(t, func(workdir, branch string) (string, error) {
		t.Errorf("prProbe should not be called for a non-worktree session")
		return "", fmt.Errorf("unreachable")
	})
	soft, hard := decideClose("obsidian", false, false)
	if !soft || hard {
		t.Errorf("decideClose(non-worktree) = (soft=%v, hard=%v), want (true, false)", soft, hard)
	}
}

// TestDecideClose_CoordinatorSession verifies a coordinator session
// (root_agent_name == "coordinator") soft-closes without consulting gh. This
// is the key behaviour for the @main coordinator carve-out.
func TestDecideClose_CoordinatorSession(t *testing.T) {
	// Set up DB with a coordinator row.
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Coordinator on a non-@main branch — root_agent_name must be the
	// deciding factor, not the branch heuristic.
	if err := d.UpsertStatusSeedRootAgentName(
		"myrepo@some-coord-branch", "myrepo", "/code/myrepo", "active",
		nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed coordinator: %v", err)
	}
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	withPRProbeStub(t, func(workdir, branch string) (string, error) {
		t.Errorf("prProbe should not be called for a coordinator session; got branch=%q", branch)
		return "", fmt.Errorf("unreachable")
	})

	soft, hard := decideClose("myrepo@some-coord-branch", false, false)
	if !soft || hard {
		t.Errorf("decideClose(coordinator) = (soft=%v, hard=%v), want (true, false)", soft, hard)
	}
}

// TestDecideClose_WorkerWithOpenPR verifies an OPEN PR routes to soft close.
func TestDecideClose_WorkerWithOpenPR(t *testing.T) {
	// Empty DB — no coordinator rows; decideClose must consult gh.
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, _ := db.Open(dbFile)
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	withPRProbeStub(t, func(workdir, branch string) (string, error) {
		if branch != "feature-foo" {
			t.Errorf("probe called with branch %q, want feature-foo", branch)
		}
		return "OPEN", nil
	})

	soft, hard := decideClose("myrepo@feature-foo", false, false)
	if !soft || hard {
		t.Errorf("decideClose(open PR) = (soft=%v, hard=%v), want (true, false)", soft, hard)
	}
}

// TestDecideClose_WorkerWithMergedOrClosedPR verifies MERGED and CLOSED both
// route to hard cleanup.
func TestDecideClose_WorkerWithMergedOrClosedPR(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, _ := db.Open(dbFile)
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	for _, state := range []string{"MERGED", "CLOSED", "merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			withPRProbeStub(t, func(workdir, branch string) (string, error) {
				return state, nil
			})
			soft, hard := decideClose("myrepo@some-merged-branch", false, false)
			if soft || !hard {
				t.Errorf("decideClose(state=%s) = (soft=%v, hard=%v), want (false, true)", state, soft, hard)
			}
		})
	}
}

// TestDecideClose_WorkerWithNoPR verifies "no PR found" routes to hard cleanup.
func TestDecideClose_WorkerWithNoPR(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, _ := db.Open(dbFile)
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	withPRProbeStub(t, func(workdir, branch string) (string, error) {
		return "", nil // no PR
	})

	soft, hard := decideClose("myrepo@orphan-branch", false, false)
	if soft || !hard {
		t.Errorf("decideClose(no PR) = (soft=%v, hard=%v), want (false, true)", soft, hard)
	}
}

// TestDecideClose_WorkerProbeError verifies any probe error fails safe to
// soft close. This is load-bearing: "fail safe to soft close" means
// destructive paths never fire when gh is unavailable.
func TestDecideClose_WorkerProbeError(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, _ := db.Open(dbFile)
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	cases := []struct {
		name string
		err  error
	}{
		{"gh missing", fmt.Errorf("exec: \"gh\": executable file not found in $PATH")},
		{"network failure", fmt.Errorf("dial: connection refused")},
		{"unauthenticated", fmt.Errorf("HTTP 401")},
		{"timeout", fmt.Errorf("context deadline exceeded")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withPRProbeStub(t, func(workdir, branch string) (string, error) {
				return "", tc.err
			})
			soft, hard := decideClose("myrepo@some-branch", false, false)
			if !soft || hard {
				t.Errorf("decideClose(probe err=%v) = (soft=%v, hard=%v), want (true, false)",
					tc.err, soft, hard)
			}
		})
	}
}

// TestDecideClose_WorkerUnknownState verifies an unknown gh state value
// (e.g. a future state name) fails safe to soft close.
func TestDecideClose_WorkerUnknownState(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, _ := db.Open(dbFile)
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	withPRProbeStub(t, func(workdir, branch string) (string, error) {
		return "DRAFT_NEW_STATE", nil
	})

	soft, hard := decideClose("myrepo@some-branch", false, false)
	if !soft || hard {
		t.Errorf("decideClose(unknown state) = (soft=%v, hard=%v), want (true, false)", soft, hard)
	}
}

// ── probePRStateExec ─────────────────────────────────────────────────────────
//
// These tests inject the gh exec seam (ghExecOutput) with canned outputs so
// the probe's argv construction, JSON parsing, and state reduction are
// exercised in-process: no subprocess, no $PATH lookup, no network, and no
// dependence on a gh binary existing in the environment. A PATH-injected
// fake-gh fixture is environment-fragile — in some worker sandboxes the probe
// reaches the real gh and fails at its 5s network timeout.

// withGhExecStub swaps the ghExecOutput exec seam for the duration of the
// test.
func withGhExecStub(t *testing.T, fn func(ctx context.Context, workdir string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := ghExecOutput
	ghExecOutput = fn
	t.Cleanup(func() { ghExecOutput = orig })
}

// cannedGhOutput stubs the gh exec seam to return the given stdout with a
// zero exit.
func cannedGhOutput(t *testing.T, stdout string) {
	t.Helper()
	withGhExecStub(t, func(ctx context.Context, workdir string, args ...string) ([]byte, error) {
		return []byte(stdout), nil
	})
}

// withGhProbeTimeout shrinks the probe's context deadline for the duration of
// the test, so timeout behaviour is covered without the production 5-second
// wall-clock wait.
func withGhProbeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := ghProbeTimeout
	ghProbeTimeout = d
	t.Cleanup(func() { ghProbeTimeout = orig })
}

// TestProbePRStateExec_OpenPR verifies the happy path: a single OPEN PR.
func TestProbePRStateExec_OpenPR(t *testing.T) {
	cannedGhOutput(t, `[{"number":42,"state":"OPEN"}]`)

	state, err := probePRStateExec("", "feature-foo")
	if err != nil {
		t.Fatalf("probePRStateExec: %v", err)
	}
	if state != "OPEN" {
		t.Errorf("state = %q, want OPEN", state)
	}
}

// TestProbePRStateExec_MergedPR verifies a single MERGED PR returns MERGED.
func TestProbePRStateExec_MergedPR(t *testing.T) {
	cannedGhOutput(t, `[{"number":42,"state":"MERGED"}]`)

	state, err := probePRStateExec("", "feature-merged")
	if err != nil {
		t.Fatalf("probePRStateExec: %v", err)
	}
	if state != "MERGED" {
		t.Errorf("state = %q, want MERGED", state)
	}
}

// TestProbePRStateExec_NoPR verifies an empty result returns ("", nil), which
// the caller treats as "no PR found → hard cleanup".
func TestProbePRStateExec_NoPR(t *testing.T) {
	cannedGhOutput(t, `[]`)

	state, err := probePRStateExec("", "orphan-branch")
	if err != nil {
		t.Fatalf("probePRStateExec: %v", err)
	}
	if state != "" {
		t.Errorf("state = %q, want \"\" (no PR)", state)
	}
}

// TestProbePRStateExec_MultiPROpenWins verifies that when multiple PRs exist
// for the same head branch and any is OPEN, the probe returns OPEN.
func TestProbePRStateExec_MultiPROpenWins(t *testing.T) {
	// Two PRs: one MERGED, one OPEN. OPEN must win regardless of order.
	cannedGhOutput(t, `[{"number":1,"state":"MERGED"},{"number":2,"state":"OPEN"}]`)

	state, err := probePRStateExec("", "branch-with-multiple-prs")
	if err != nil {
		t.Fatalf("probePRStateExec: %v", err)
	}
	if state != "OPEN" {
		t.Errorf("state = %q, want OPEN (any-OPEN-wins multi-PR contract)", state)
	}
}

// TestProbePRStateExec_GhExitsNonZero verifies an error from the gh runner
// (e.g. a non-zero exit from an unauthenticated gh) propagates, so
// decideClose can fail-safe to soft close.
func TestProbePRStateExec_GhExitsNonZero(t *testing.T) {
	withGhExecStub(t, func(ctx context.Context, workdir string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1: gh: not authenticated")
	})

	state, err := probePRStateExec("", "some-branch")
	if err == nil {
		t.Errorf("expected non-nil error from non-zero gh exit; got state=%q", state)
	}
}

// TestProbePRStateExec_MalformedJSON verifies that gh stdout which fails to
// parse propagates as an error (caller fails safe to soft close).
func TestProbePRStateExec_MalformedJSON(t *testing.T) {
	cannedGhOutput(t, `{not json`)

	state, err := probePRStateExec("", "some-branch")
	if err == nil {
		t.Errorf("expected non-nil error for malformed gh output; got state=%q", state)
	}
}

// TestProbePRStateExec_GhMissing verifies the production runner
// (ghExecOutputReal, reached through the default seam) returns an error when
// gh is not on $PATH. PATH is set to an empty tempdir to guarantee no gh on
// PATH for this test, decoupled from the test runner's environment. The
// exec.LookPath failure is immediate — no subprocess is spawned and no
// network is touched.
func TestProbePRStateExec_GhMissing(t *testing.T) {
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)

	state, err := probePRStateExec("", "some-branch")
	if err == nil {
		t.Errorf("expected non-nil error when gh missing; got state=%q", state)
	}
}

// TestProbePRStateExec_EmptyBranch verifies the early-return on an empty
// branch (defensive guard) — the gh runner must never be invoked.
func TestProbePRStateExec_EmptyBranch(t *testing.T) {
	withGhExecStub(t, func(ctx context.Context, workdir string, args ...string) ([]byte, error) {
		t.Error("gh runner should not be invoked for an empty branch")
		return nil, fmt.Errorf("unreachable")
	})
	if state, err := probePRStateExec("", ""); err == nil {
		t.Errorf("expected error for empty branch; got state=%q", state)
	}
}

// TestProbePRStateExec_PassesArgv verifies the exact argv shape handed to the
// gh runner: `pr list --head <branch> --state all --json state,number
// --limit 10`, plus workdir passthrough. This is a regression guard:
// multi-PR support requires `gh pr list --head`, not `gh pr view`.
func TestProbePRStateExec_PassesArgv(t *testing.T) {
	var gotWorkdir string
	var gotArgs []string
	withGhExecStub(t, func(ctx context.Context, workdir string, args ...string) ([]byte, error) {
		gotWorkdir = workdir
		gotArgs = args
		return []byte(`[]`), nil
	})

	if _, err := probePRStateExec("/some/workdir", "my-branch"); err != nil {
		t.Fatalf("probePRStateExec: %v", err)
	}

	if gotWorkdir != "/some/workdir" {
		t.Errorf("workdir = %q, want /some/workdir", gotWorkdir)
	}
	wantArgs := []string{"pr", "list", "--head", "my-branch", "--state", "all", "--json", "state,number", "--limit", "10"}
	if !slices.Equal(gotArgs, wantArgs) {
		t.Errorf("argv = %q, want %q", gotArgs, wantArgs)
	}
}

// TestProbePRStateExec_TimeoutFailsSafe verifies the probe applies the
// ghProbeTimeout deadline to the context it hands the gh runner and
// propagates the cancellation error — the input decideClose relies on for its
// fail-safe-to-soft-close path. The deadline is shrunk to 50ms via the
// ghProbeTimeout seam so real cancellation behaviour is asserted without the
// production 5-second wall-clock wait.
func TestProbePRStateExec_TimeoutFailsSafe(t *testing.T) {
	withGhProbeTimeout(t, 50*time.Millisecond)
	withGhExecStub(t, func(ctx context.Context, workdir string, args ...string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("probe context has no deadline — ghProbeTimeout not applied")
		}
		// Simulate a hung gh that only returns when the context is cancelled.
		// The bounded escape hatch keeps the failure mode crisp (a named
		// error within 3s, not a suite hang) if the deadline is ever dropped.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
			return nil, fmt.Errorf("probe context was never cancelled")
		}
	})

	start := time.Now()
	state, err := probePRStateExec("", "stuck-branch")
	elapsed := time.Since(start)

	if err == nil {
		t.Errorf("expected error from hung probe; got state=%q", state)
	}
	if elapsed > 2*time.Second {
		t.Errorf("probe returned after %v, want ~50ms (deadline not enforced)", elapsed)
	}
}

// TestProbePRStateExec_HangingSubprocessKilledAtDeadline exercises the
// production runner (ghExecOutputReal) end to end against a hanging fake gh,
// proving exec.CommandContext actually kills the subprocess at the deadline
// and cmd.Output returns promptly. The deadline is shrunk to 250ms so there
// is no 5-second wall-clock wait, and $PATH is set to ONLY the fake's dir so
// the real gh is unreachable in every environment — if the fake cannot be
// executed at all, exec fails fast instead, and either way the probe must
// return an error well inside the bound.
//
// The fake holds the stdout pipe itself (a shell busy-loop, no children) so
// SIGKILL on the process closes the pipe and unblocks cmd.Output; it needs
// no external binaries (`sleep` is not guaranteed on PATH inside the nix
// build sandbox).
func TestProbePRStateExec_HangingSubprocessKilledAtDeadline(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "gh")
	script := "#!/bin/sh\nwhile :; do :; done\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write hanging gh: %v", err)
	}
	t.Setenv("PATH", dir) // fake-only PATH: the real gh must be unreachable

	withGhProbeTimeout(t, 250*time.Millisecond)

	start := time.Now()
	state, err := probePRStateExec("", "stuck-branch")
	elapsed := time.Since(start)

	if err == nil {
		t.Errorf("expected error from hanging gh probe; got state=%q after %v", state, elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("probe took %v, want ≲250ms (timeout not enforced)", elapsed)
	}
}

// TestDecideClose_ProbeTimeoutFailsSafeToSoftClose wires the real
// probePRStateExec (hung runner, shrunk deadline) into decideClose and
// asserts the probe timeout routes to soft close — the fail-safe —
// without any wall-clock wait near the production 5s.
func TestDecideClose_ProbeTimeoutFailsSafeToSoftClose(t *testing.T) {
	// Empty DB — no coordinator rows; decideClose must consult the probe.
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, _ := db.Open(dbFile)
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	withGhProbeTimeout(t, 50*time.Millisecond)
	withGhExecStub(t, func(ctx context.Context, workdir string, args ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	soft, hard := decideClose("myrepo@stuck-branch", false, false)
	if !soft || hard {
		t.Errorf("decideClose(probe timeout) = (soft=%v, hard=%v), want (true, false) — fail-safe", soft, hard)
	}
}

// ── runCloseCmd flag validation ──────────────────────────────────────────────

// freshCloseCmd returns a copy of closeCmd with a clean flag set so each
// test invocation runs in isolation. We can't reuse closeCmd directly
// because parsed flag values persist between RunE invocations.
func freshCloseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:  "close",
		RunE: runCloseCmd,
	}
	cmd.Flags().Bool("yes", false, "")
	cmd.Flags().String("session", "", "")
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().Bool("keep-worktree", false, "")
	cmd.Flags().Bool("remove-worktree", false, "")
	return cmd
}

// TestCloseCmd_MutuallyExclusiveFlags verifies --keep-worktree and
// --remove-worktree cannot be combined.
func TestCloseCmd_MutuallyExclusiveFlags(t *testing.T) {
	cmd := freshCloseCmd()
	_ = cmd.Flags().Set("keep-worktree", "true")
	_ = cmd.Flags().Set("remove-worktree", "true")
	_ = cmd.Flags().Set("yes", "true")

	err := runCloseCmd(cmd, nil)
	if err == nil {
		t.Fatal("expected error for --keep-worktree + --remove-worktree, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %q does not mention 'mutually exclusive'", err.Error())
	}
}

// TestCloseCmd_JSONRequiresYes verifies the --json without --yes error message
// matches `prism cleanup` for parity.
func TestCloseCmd_JSONRequiresYes(t *testing.T) {
	cmd := freshCloseCmd()
	_ = cmd.Flags().Set("json", "true")

	err := runCloseCmd(cmd, nil)
	if err == nil {
		t.Fatal("expected error for --json without --yes, got nil")
	}
	if !strings.Contains(err.Error(), "--json requires --yes") {
		t.Errorf("error %q does not contain '--json requires --yes'", err.Error())
	}
}

// TestCloseCmd_UnknownSessionEnumerates verifies the "must be one of" error
// shape matches `prism cleanup` exactly.
func TestCloseCmd_UnknownSessionEnumerates(t *testing.T) {
	// Seed DB with one active session so the error lists it.
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.UpsertStatus("myrepo@known-session", "myrepo", "/tmp/wt", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })
	t.Setenv("PRISM_HOST_API", "") // run host-side path

	cmd := freshCloseCmd()
	_ = cmd.Flags().Set("session", "myrepo@does-not-exist")
	_ = cmd.Flags().Set("yes", "true")

	err = runCloseCmd(cmd, nil)
	if err == nil {
		t.Fatal("expected error for unknown --session, got nil")
	}
	if !strings.Contains(err.Error(), "must be one of") {
		t.Errorf("error %q does not contain 'must be one of'", err.Error())
	}
	if !strings.Contains(err.Error(), "myrepo@known-session") {
		t.Errorf("error %q does not enumerate known session", err.Error())
	}
	if !strings.Contains(err.Error(), "myrepo@does-not-exist") {
		t.Errorf("error %q does not name the bad input", err.Error())
	}
}

// TestCloseCmd_NoTmuxWithoutSessionFails verifies the tmux-required error
// matches `prism cleanup` for parity.
func TestCloseCmd_NoTmuxWithoutSessionFails(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("PRISM_HOST_API", "")

	cmd := freshCloseCmd()
	// No --session, no --yes — runCloseCmd's early guard fires.

	err := runCloseCmd(cmd, nil)
	if err == nil {
		t.Fatal("expected error when TMUX unset and --session absent, got nil")
	}
	if !strings.Contains(err.Error(), "tmux") {
		t.Errorf("error %q does not mention tmux", err.Error())
	}
}

// TestCloseCmd_ContainerProxyRequiresSession verifies that running inside a
// container (PRISM_HOST_API set) without --session is rejected client-side,
// matching `prism cleanup`.
func TestCloseCmd_ContainerProxyRequiresSession(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "unix:///tmp/never-reached.sock")
	cmd := freshCloseCmd()
	_ = cmd.Flags().Set("yes", "true")

	err := runCloseCmd(cmd, nil)
	if err == nil {
		t.Fatal("expected error when PRISM_HOST_API set without --session, got nil")
	}
	if !strings.Contains(err.Error(), "--session is required") {
		t.Errorf("error %q does not mention '--session is required'", err.Error())
	}
}

// TestCloseCmd_ContainerProxyForwardsFlags verifies that when PRISM_HOST_API is
// set, runCloseCmd forwards --session/--yes/--json/--keep-worktree/
// --remove-worktree to the /close endpoint as a JSON request body.
func TestCloseCmd_ContainerProxyForwardsFlags(t *testing.T) {
	type closeReq struct {
		Session        string `json:"session"`
		Yes            bool   `json:"yes"`
		JSON           bool   `json:"json"`
		KeepWorktree   bool   `json:"keep_worktree"`
		RemoveWorktree bool   `json:"remove_worktree"`
	}
	reqCh := make(chan closeReq, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/close" {
			http.Error(w, `{"error":"wrong path"}`, http.StatusBadRequest)
			return
		}
		var req closeReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqCh <- req
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"stdout":"","stderr":""}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())
	cmd := freshCloseCmd()
	_ = cmd.Flags().Set("session", "myrepo@feature")
	_ = cmd.Flags().Set("yes", "true")
	_ = cmd.Flags().Set("json", "true")
	_ = cmd.Flags().Set("keep-worktree", "true")

	if err := runCloseCmd(cmd, nil); err != nil {
		t.Fatalf("runCloseCmd: %v", err)
	}

	select {
	case req := <-reqCh:
		if req.Session != "myrepo@feature" {
			t.Errorf("session = %q, want myrepo@feature", req.Session)
		}
		if !req.Yes {
			t.Error("yes = false, want true")
		}
		if !req.JSON {
			t.Error("json = false, want true")
		}
		if !req.KeepWorktree {
			t.Error("keep_worktree = false, want true")
		}
		if req.RemoveWorktree {
			t.Error("remove_worktree = true, want false (not set)")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}

// ── close-cmd integration: smart-decide on a real session row ────────────────

// TestCloseCmd_SoftCloseOnOpenPR verifies the end-to-end soft-close path for
// a worker session with an OPEN PR: the DB row is marked ended, the worktree
// path is preserved (we just check it's still there on disk), and stdout is
// silent.
func TestCloseCmd_SoftCloseOnOpenPR(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	withPRProbeStub(t, func(workdir, branch string) (string, error) {
		return "OPEN", nil
	})

	worktreePath := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sessionName := "myrepo@open-feature"
	if err := d.UpsertStatus(sessionName, "myrepo", worktreePath, "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// Capture stdout via os.Pipe so the test can assert silence.
	stdout := captureStdout(t, func() {
		cmd := freshCloseCmd()
		_ = cmd.Flags().Set("session", sessionName)
		_ = cmd.Flags().Set("yes", "true")
		if err := runCloseCmd(cmd, nil); err != nil {
			t.Fatalf("runCloseCmd: %v", err)
		}
	})

	if stdout != "" {
		t.Errorf("expected silent stdout on success, got %q", stdout)
	}

	// Worktree directory still exists (soft close did not remove it).
	if _, err := os.Stat(worktreePath); err != nil {
		t.Errorf("worktree directory was removed; soft close should preserve it: %v", err)
	}

	// DB row is marked ended.
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
		t.Errorf("ended_at is nil — soft close did not mark the session as ended")
	}
}

// TestCloseCmd_JSONOnSoftCloseEmitsEnvelope verifies --json + --yes still emits
// the JSON envelope (the quiet rule only applies to the non-json case).
func TestCloseCmd_JSONOnSoftCloseEmitsEnvelope(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	withPRProbeStub(t, func(workdir, branch string) (string, error) {
		return "OPEN", nil
	})

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sessionName := "myrepo@json-feature"
	if err := d.UpsertStatus(sessionName, "myrepo", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	stdout := captureStdout(t, func() {
		cmd := freshCloseCmd()
		_ = cmd.Flags().Set("session", sessionName)
		_ = cmd.Flags().Set("yes", "true")
		_ = cmd.Flags().Set("json", "true")
		if err := runCloseCmd(cmd, nil); err != nil {
			t.Fatalf("runCloseCmd: %v", err)
		}
	})

	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		t.Fatal("expected JSON envelope on stdout, got empty")
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("stdout is not valid JSON: %v (stdout=%q)", err, stdout)
	}
	if envelope["session"] != sessionName {
		t.Errorf("envelope.session = %v, want %q", envelope["session"], sessionName)
	}
	if envelope["session_killed"] != true {
		t.Errorf("envelope.session_killed = %v, want true", envelope["session_killed"])
	}
	// Soft-close paths do not touch the worktree or branch.
	if envelope["worktree_removed"] != nil {
		t.Errorf("envelope.worktree_removed = %v, want null (soft close)", envelope["worktree_removed"])
	}
	if envelope["branch_deleted"] != nil {
		t.Errorf("envelope.branch_deleted = %v, want null (soft close)", envelope["branch_deleted"])
	}
}

// TestCloseCmd_ForcedKeepWorktreeNeverProbesGh verifies that --keep-worktree
// short-circuits the gh probe entirely. This is an end-to-end check on
// "force soft regardless of PR state".
func TestCloseCmd_ForcedKeepWorktreeNeverProbesGh(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	probeCalled := false
	withPRProbeStub(t, func(workdir, branch string) (string, error) {
		probeCalled = true
		return "MERGED", nil // would route to hard cleanup, but probe must not run
	})

	worktreePath := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sessionName := "myrepo@forced-soft"
	if err := d.UpsertStatus(sessionName, "myrepo", worktreePath, "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	cmd := freshCloseCmd()
	_ = cmd.Flags().Set("session", sessionName)
	_ = cmd.Flags().Set("yes", "true")
	_ = cmd.Flags().Set("keep-worktree", "true")
	if err := runCloseCmd(cmd, nil); err != nil {
		t.Fatalf("runCloseCmd: %v", err)
	}

	if probeCalled {
		t.Error("gh probe was called despite --keep-worktree")
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Errorf("worktree was removed despite --keep-worktree: %v", err)
	}
}

// ── proxy helper unit tests ──────────────────────────────────────────────────

// TestProxyCloseToHostAPI_ForwardsAllFlags verifies the proxy sends all flags
// through the JSON body and that absent flags are omitted (rather than sent
// as `false`). The omitted-when-absent contract matches the cleanup proxy
// shape so the sidecar's struct-decoder default of `false` does the right
// thing on both sides.
func TestProxyCloseToHostAPI_ForwardsAllFlags(t *testing.T) {
	gotBody := make(chan map[string]any, 1)
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case gotBody <- body:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"stdout":"","stderr":""}`))
	})

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := proxyCloseToHostAPIWithWriters(srv.apiURL(), "myrepo@b", true, true, true, false, &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proxyCloseToHostAPI: %v", err)
	}

	select {
	case body := <-gotBody:
		if body["session"] != "myrepo@b" {
			t.Errorf("session = %v, want myrepo@b", body["session"])
		}
		if body["yes"] != true {
			t.Errorf("yes = %v, want true", body["yes"])
		}
		if body["json"] != true {
			t.Errorf("json = %v, want true", body["json"])
		}
		if body["keep_worktree"] != true {
			t.Errorf("keep_worktree = %v, want true", body["keep_worktree"])
		}
		if _, present := body["remove_worktree"]; present {
			t.Errorf("remove_worktree should be omitted when false; got %v", body["remove_worktree"])
		}
	default:
		t.Fatal("no request body received")
	}
}

// TestProxyCloseToHostAPI_ForwardsErrorWithStdoutStderr verifies that on a
// non-2xx response the proxy forwards stdout/stderr to the caller's writers
// and returns an error containing the underlying message (parity with
// proxyCleanupToHostAPI).
func TestProxyCloseToHostAPI_ForwardsErrorWithStdoutStderr(t *testing.T) {
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{` +
			`"error":"close failed: exit status 1",` +
			`"stdout":"closing session myrepo@b...\n",` +
			`"stderr":"[prism] warning: archive: exists\n"` +
			`}`))
	})

	var stdoutBuf, stderrBuf bytes.Buffer
	err := proxyCloseToHostAPIWithWriters(srv.apiURL(), "myrepo@b", true, false, false, false, &stdoutBuf, &stderrBuf)
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
	if !strings.Contains(err.Error(), "close failed: exit status 1") {
		t.Errorf("error %q should contain underlying cause", err.Error())
	}
	if stdoutBuf.String() != "closing session myrepo@b...\n" {
		t.Errorf("forwarded stdout: %q", stdoutBuf.String())
	}
	if stderrBuf.String() != "[prism] warning: archive: exists\n" {
		t.Errorf("forwarded stderr: %q", stderrBuf.String())
	}
}
