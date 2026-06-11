package sidecar

// Regression guard for TestMain's re-exec stub interception (#2237, residual
// from #2230).
//
// The host-API handlers re-exec prismBinary() — which falls back to
// os.Executable(), i.e. THIS test binary — with the subcommand argv shapes
// below (the exec.CommandContext sites in host_api.go). Without the TestMain
// interception (testmain_test.go) such a re-invocation runs the ENTIRE test
// suite as a detached child — verified against the pre-#2237 binary:
// `<self> prompt prism-test@reexec-guard --prompt hi` ran the full suite for
// 47 seconds and wrote 2.9 MB of output ending in PASS.
//
// This guard execs the test binary the way the production re-exec would —
// the same argv shapes prismBinary() callers build, no stub env var — and
// asserts it exits 0 immediately with no output. A suite run instead prints
// test/log output and at minimum a trailing "PASS"/"FAIL", so the
// empty-output assertion fails if the interception is ever removed.

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

	// One case per host_api.go re-exec site, mirroring the production
	// `args := []string{…}` literals. Keep in sync with
	// hostAPIReExecSubcommands (testmain_test.go).
	cases := []struct {
		name string
		argv []string
	}{
		{"prompt", []string{"prompt", "prism-test@reexec-guard", "--prompt", "guard"}},
		{"spawn", []string{"spawn", "--branch", "prism-test-reexec-guard"}},
		{"review", []string{"review", "0"}},
		{"cleanup", []string{"cleanup", "--session", "prism-test@reexec-guard"}},
		{"close", []string{"close", "--session", "prism-test@reexec-guard"}},
		{"switch", []string{"switch", "--path", "/nonexistent/prism-reexec-guard"}},
		{"event", []string{"event", "guard-event", "--session", "prism-test@reexec-guard"}},
		{"escalate", []string{"escalate", "--prompt", "guard"}},
		{"investigate", []string{"investigate", "--prompt", "guard"}},
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
				t.Fatalf("re-invoked test binary %v did not exit within 30s — TestMain re-exec interception missing (#2237); output:\n%s", tc.argv, truncateBytes(out, 2048))
			}
			if runErr != nil {
				t.Fatalf("re-invoked test binary %v = %v, want immediate exit 0 from TestMain interception (#2237); output:\n%s", tc.argv, runErr, truncateBytes(out, 2048))
			}
			if len(out) != 0 {
				t.Errorf("re-invoked test binary %v wrote %d bytes — the suite ran instead of TestMain intercepting (#2237):\n%s", tc.argv, len(out), truncateBytes(out, 2048))
			}
		})
	}
}
