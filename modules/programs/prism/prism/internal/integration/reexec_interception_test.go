package integration_test

// Regression guard for TestMain's re-exec stub interception (#2237, residual
// from #2230).
//
// Production code reachable from this package re-execs os.Executable() — in
// tests, THIS test binary — as a prism subcommand: session.StartSidecarWithOpts
// launches `<self> sidecar --session …` and setupFullLayout's status seed runs
// `<self> event tmux-session-start …`. Without the TestMain interception
// (testmain_test.go) such a re-invocation runs the ENTIRE test suite as a
// detached child, which can spawn further children — the #2230 landmine class
// (93 detached test processes observed after a single internal/review run
// before its TestMain gained the same defence in #2236).
//
// This guard execs the test binary the way the production re-exec would —
// the same argv shapes, no stub env var — and asserts it exits 0 immediately
// with no output. A suite run instead prints test/log output and at minimum
// a trailing "PASS"/"FAIL", so the empty-output assertion fails if the
// interception is ever removed (verified non-vacuous against the pre-#2237
// binary: the `event …` re-invocation ran the full suite and printed PASS).

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// reexecGuardChildEnv is a recursion brake for the mutated state only: if the
// TestMain interception is removed, the child invocation below runs the full
// suite — including this guard. The brake makes the child's copy of the guard
// skip instead of spawning grandchildren, so the failure is a bounded, visible
// guard FAIL rather than a fork storm. With the interception in place the
// child never reaches m.Run() and the brake is inert.
const reexecGuardChildEnv = "PRISM_REEXEC_GUARD_CHILD"

func TestReExecInterception_ProductionArgvShapes(t *testing.T) {
	if os.Getenv(reexecGuardChildEnv) == "1" {
		t.Skip("recursion brake: running inside a guard-spawned child (see reexecGuardChildEnv)")
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, self, tc.argv...)
			// Bound Wait on lingering pipe holders if a mutated run leaves
			// grandchildren sharing stdout/stderr.
			cmd.WaitDelay = 5 * time.Second
			cmd.Env = append(os.Environ(), reexecGuardChildEnv+"=1")
			out, runErr := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("re-invoked test binary %v did not exit within 30s — TestMain re-exec interception missing (#2237); output:\n%s", tc.argv, reexecGuardTruncate(out))
			}
			if runErr != nil {
				t.Fatalf("re-invoked test binary %v = %v, want immediate exit 0 from TestMain interception (#2237); output:\n%s", tc.argv, runErr, reexecGuardTruncate(out))
			}
			if len(out) != 0 {
				t.Errorf("re-invoked test binary %v wrote %d bytes — the suite ran instead of TestMain intercepting (#2237):\n%s", tc.argv, len(out), reexecGuardTruncate(out))
			}
		})
	}
}

// reexecGuardTruncate bounds child output in failure messages.
func reexecGuardTruncate(b []byte) string {
	const max = 2048
	if len(b) > max {
		return string(b[:max]) + "… (truncated)"
	}
	return string(b)
}
