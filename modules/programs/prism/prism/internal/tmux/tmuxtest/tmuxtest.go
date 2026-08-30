// Package tmuxtest provides a shared capability probe for test suites that
// need to start a private tmux server (tmux -L <socket>).
//
// # Why this exists
//
// Inside a prism sandbox-exec worker session the sandbox profile denies the
// fork tmux needs to daemonise its server process. Every attempt to bootstrap
// a private test server fails with:
//
//	create window failed: fork failed: Operation not permitted
//
// Without a guard, the tmux-server-backed suites in cmd/, internal/tmux/, and
// internal/integration/ fail (rather than skip) on a clean tree whenever
// `go test ./...` runs inside such a session — training agents to dismiss red
// suites. RequireServer turns that environmental failure into an explicit,
// explanatory skip.
//
// # No over-skip guarantee
//
// The guard probes actual capability — it attempts to start (and immediately
// kill) a throwaway private tmux server once per test binary — instead of
// sniffing platform or sandbox heuristics. It skips only when the probe fails
// with the known fork-denial signature. On hosts where tmux server creation
// works (developer shells, CI Linux runners), the probe succeeds and the
// tests run as before. If the probe fails for any *other* reason, the guard
// deliberately does not skip: the test proceeds and surfaces the real error,
// preserving today's failure signal.
package tmuxtest

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// forkDenialSignature is the error tmux emits when the sandbox profile blocks
// the fork required to daemonise the tmux server. This is the exact signature
// observed inside prism sandbox-exec worker sessions.
const forkDenialSignature = "fork failed: Operation not permitted"

var (
	probeOnce sync.Once
	probeOut  string // combined output of the probe attempt
	probeErr  error  // non-nil when the probe failed to start a server
)

// RequireServer skips t when this environment cannot start a private tmux
// server, and otherwise returns the resolved path to the tmux binary.
//
// Two skip conditions:
//
//  1. tmux is absent from PATH (preserves the long-standing behaviour of the
//     per-package harnesses).
//  2. tmux is present but server creation is denied with the in-sandbox
//     fork-EPERM signature ("fork failed: Operation not permitted"), as
//     happens inside prism sandbox-exec worker sessions whose profile blocks
//     the tmux daemon fork.
//
// The capability probe runs at most once per test binary (sync.Once) and
// starts a throwaway server on a unique socket, killing it immediately. Probe
// failures that do not match the fork-denial signature do not cause a skip —
// the caller's own server bootstrap will reproduce and report the real error.
func RequireServer(t testing.TB) string {
	t.Helper()

	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not found in PATH — skipping tmux integration test")
	}

	probeOnce.Do(func() {
		probeOut, probeErr = probeServerStart(bin)
	})

	if probeErr != nil && IsSandboxForkDenial(probeOut) {
		t.Skipf("skipping: cannot start a private tmux server in this environment — "+
			"the fork tmux needs to daemonise its server is denied by the sandbox "+
			"(%q), as inside prism sandbox-exec worker sessions (issue #2204); "+
			"run from a host shell or CI to exercise this test. probe error: %v",
			forkDenialSignature, probeErr)
	}

	return bin
}

// IsSandboxForkDenial reports whether the given tmux output matches the
// in-sandbox fork-denial signature that blocks tmux server creation.
func IsSandboxForkDenial(output string) bool {
	return strings.Contains(output, forkDenialSignature)
}

// probeServerStart attempts to bootstrap a throwaway private tmux server on a
// unique socket and returns the combined output and error of the attempt. Any
// server it manages to start is killed before returning. -f /dev/null
// suppresses the user's tmux.conf so no hooks fire against live prism state.
func probeServerStart(bin string) (string, error) {
	socket := fmt.Sprintf("prism-tmuxtest-probe-%d-%d", os.Getpid(), time.Now().UnixNano())
	cmd := exec.Command(bin, "-L", socket, "-f", "/dev/null",
		"new-session", "-ds", "probe", "-c", os.TempDir())
	out, err := cmd.CombinedOutput()
	// Best-effort teardown: if the server started, kill it; if it did not,
	// this fails harmlessly.
	_ = exec.Command(bin, "-L", socket, "kill-server").Run()
	return string(out), err
}
