package cmd

// Regression guard for TestMain's re-exec stub interception.
//
// Production code reachable from this package re-execs os.Executable() — in
// tests, THIS test binary — as `<self> sidecar …` /  `<self> event …`
// (session.StartSidecarWithOpts and setupFullLayout's status seed) and
// `<self> monitor-review …` (review.RunAsync → StartMonitorProcess with
// prismBinary==""). An additional re-exec target reaches this binary via
// tmux rather than Go: setupFullLayout (internal/session/session.go) creates
// the agent pane by handing
// `<abs-prism-path> agent-run --session <name>` to tmux.NewWindow, and tmux
// invokes the binary from outside the Go parent. Without the agent-run arm
// of the argv check, the subprocess ran the real cmd/agent_run.go path which
// calls openAgentRunLog and creates `$XDG_STATE_HOME/prism/run/<hash>/
// agent-run.log` AFTER the test body has returned — racing t.TempDir's
// RemoveAll and surfacing as `unlinkat …/prism: directory not empty`.
//
// This guard execs the test binary the way the production re-exec would —
// the same argv shapes, no stub env var — and asserts it exits 0
// immediately with no output. A suite run instead prints test/log output and
// at minimum a trailing "PASS"/"FAIL", so the empty-output assertion fails
// if the interception is ever removed. Without it, the `agent-run …`
// re-invocation runs the full suite.

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// cmdReexecGuardChildEnv is a recursion brake for the mutated state only: if
// the TestMain interception is removed, the child invocation below runs the
// full suite — including this guard. The brake makes the child's copy of the
// guard skip instead of spawning grandchildren, so the failure is a bounded,
// visible guard FAIL rather than a fork storm. With the interception in
// place the child never reaches m.Run() and the brake is inert.
const cmdReexecGuardChildEnv = "PRISM_CMD_REEXEC_GUARD_CHILD"

// TestReExecInterception_ProductionArgvShapes mirrors the same-named guards
// in internal/integration and internal/sidecar. Each case is one
// production re-exec argv shape that this test binary can be invoked with;
// the TestMain argv defence in killsidecar_test.go must intercept all of
// them and exit 0 immediately. Adding a new re-exec target in production
// without adding it here will let the suite recurse silently — keep the
// list in sync with the killsidecar_test.go TestMain argv check.
func TestReExecInterception_ProductionArgvShapes(t *testing.T) {
	if os.Getenv(cmdReexecGuardChildEnv) == "1" {
		t.Skip("recursion brake: running inside a guard-spawned child (see cmdReexecGuardChildEnv)")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	cases := []struct {
		name string
		argv []string
	}{
		// session.StartSidecarWithOpts (internal/session/sidecar.go).
		{"sidecar", []string{"sidecar", "--session", "prism-test@reexec-guard", "--harness-url", "http://localhost:0"}},
		// setupFullLayout's status seed (internal/session/session.go).
		{"event", []string{"event", "tmux-session-start", "--session", "prism-test@reexec-guard", "--worktree", "/nonexistent/prism-reexec-guard"}},
		// review.StartMonitorProcess (internal/review/review.go).
		{"monitor-review", []string{"monitor-review", "--session", "prism-test@reexec-guard"}},
		// setupFullLayout's tmux-launched agent pane: bwrap / sandbox-exec
		// AgentPaneCmd renders `<self> agent-run --session <name>` and hands
		// it to tmux.NewWindow. tmux invokes the binary outside Go's
		// process tree.
		{"agent-run", []string{"agent-run", "--session", "prism-test@reexec-guard"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, self, tc.argv...)
			// Bound Wait on lingering pipe holders if a mutated run leaves
			// grandchildren sharing stdout/stderr.
			cmd.WaitDelay = 5 * time.Second
			cmd.Env = append(os.Environ(), cmdReexecGuardChildEnv+"=1")
			out, runErr := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("re-invoked test binary %v did not exit within 30s — TestMain re-exec interception missing; output:\n%s", tc.argv, cmdReexecGuardTruncate(out))
			}
			if runErr != nil {
				t.Fatalf("re-invoked test binary %v = %v, want immediate exit 0 from TestMain interception; output:\n%s", tc.argv, runErr, cmdReexecGuardTruncate(out))
			}
			if len(out) != 0 {
				t.Errorf("re-invoked test binary %v wrote %d bytes — the suite ran instead of TestMain intercepting:\n%s", tc.argv, len(out), cmdReexecGuardTruncate(out))
			}
		})
	}
}

// cmdReexecGuardTruncate bounds child output in failure messages.
func cmdReexecGuardTruncate(b []byte) string {
	const max = 2048
	if len(b) > max {
		return string(b[:max]) + "… (truncated)"
	}
	return string(b)
}
