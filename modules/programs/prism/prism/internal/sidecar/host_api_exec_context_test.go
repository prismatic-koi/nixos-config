// Tests for the per-endpoint exec.CommandContext + WithTimeout wiring.
// Without this wiring, a hung subprocess (e.g. `gh` blocked
// on the network, a wedged `prism spawn`) would pin the host-API handler
// goroutine indefinitely.
//
// Each test spawns a stub `prism` binary that sleeps for far longer than the
// test will wait, then drives the handler with a request whose context the
// test cancels before the child can return. The handler must:
//
//   1. Kill the child when the context fires.
//   2. Return promptly (well under the per-endpoint timeout).
//   3. Return the documented status code: 499 ("client closed request") for
//      caller-cancellation, 504 Gateway Timeout when the per-endpoint
//      WithTimeout deadline elapses.
//
// We exercise the caller-cancellation (499) path because forcing the 504 path
// would require waiting for the smallest per-endpoint timeout (30 s for
// /switch /event /escalate). The kill mechanism and the contextErrStatus
// branch are identical on both code paths — the only difference is which
// sentinel ctx.Err() returns.
//
// The stub also writes its own PID to a sentinel file so the test can verify
// the child process is no longer running after the handler returns.

package sidecar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// sleepForeverStub writes a shell script that records its PID to pidFile,
// then sleeps for 600 s. 600 s is far longer than any test will wait but
// short enough that a forgotten orphan is reaped by the OS within the test
// suite's run. Returns the path to the executable stub.
func sleepForeverStub(t *testing.T, pidFile string) string {
	t.Helper()
	stubPath := filepath.Join(t.TempDir(), "prism-sleep-stub")
	// `exec sleep 600` replaces the shell with sleep so the recorded PID
	// matches the long-running process the test wants to verify is killed.
	// (Without exec, the shell's PID is recorded but the child sleep — which
	// owns the process group — has a different PID, and the kill check
	// becomes ambiguous.)
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"$$\" > %s\nexec sleep 600\n", pidFile)
	if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write sleep-forever stub: %v", err)
	}
	return stubPath
}

// newSidecarWithSleepStub returns a sidecar configured with the sleep-forever
// stub binary. pidFile is the path the stub will write its PID to before
// blocking. The sidecar is otherwise identical to newSidecarWithRoleAndBinary.
func newSidecarWithSleepStub(t *testing.T, sessionName, repo, role, pidFile string, d *db.DB) *Sidecar {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	stubPath := sleepForeverStub(t, pidFile)
	clk := newTestClock()
	cfg := Config{
		SessionName:     sessionName,
		Repo:            repo,
		Worktree:        "/tmp/" + sessionName,
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           clk,
		AgentRole:       role,
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	}
	return New(cfg)
}

// waitForPidFile blocks until pidFile exists and contains a parseable PID,
// or fails the test after timeout. Returns the recorded PID.
func waitForPidFile(t *testing.T, pidFile string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil && len(data) > 0 {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid file %q never appeared within %v", pidFile, timeout)
	return 0
}

// assertProcessKilled polls until the process with the given pid is gone, or
// fails after timeout. We use kill(pid, 0) which returns ESRCH when the
// process no longer exists. This is more reliable than wait/waitpid because
// the stub is not our child — the sidecar's exec.Cmd reaped it already.
func assertProcessKilled(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			return // gone
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Last-ditch: if the process is still around at the deadline, that's a
	// real regression — log its /proc status if available, then fail.
	statusBytes, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if len(statusBytes) > 0 {
		// Print just the first few lines so the failure message is readable.
		lines := strings.SplitN(string(statusBytes), "\n", 5)
		t.Fatalf("process pid=%d still alive after %v; /proc status (first lines):\n%s",
			pid, timeout, strings.Join(lines, "\n"))
	}
	t.Fatalf("process pid=%d still alive after %v (no /proc entry)", pid, timeout)
}

// endpointCase describes one host-API handler wired up with
// exec.CommandContext + WithTimeout. The role, sessionName, and body fields
// are picked to make the request reach the exec path (i.e. pass any
// pre-shell-out auth/validation gates).
type endpointCase struct {
	name        string // subtest name
	path        string // "/prompt", "/spawn", etc.
	method      string // always http.MethodPost for these
	role        string // "worker" or "coordinator"
	sessionName string
	repo        string
	body        string
}

// TestHostAPI_ExecContext_KillsChildOnCancel exercises the 7 host-API
// handlers wired up with exec.CommandContext + WithTimeout (every such site
// except /review, which already used CommandContext as the reference shape).
// For each endpoint:
//
//   - Start the handler with a request whose context will be cancelled.
//   - Wait until the stub has recorded its PID (proves the exec actually ran).
//   - Cancel the request context.
//   - Assert the handler returns within a small wall-clock budget.
//   - Assert the handler returns 499 (the documented "client closed request"
//     code at every site).
//   - Assert the child process is no longer alive.
//
// The contract: when the context fires (timeout or client disconnect),
// the child process is killed and the handler returns a documented non-200
// status.
func TestHostAPI_ExecContext_KillsChildOnCancel(t *testing.T) {
	cases := []endpointCase{
		{
			name:        "prompt",
			path:        "/prompt",
			method:      http.MethodPost,
			role:        "coordinator",
			sessionName: "myrepo@main",
			repo:        "myrepo",
			// Cross-session (not == own session) so the handler takes the
			// exec.Command path rather than the same-session pipe path.
			body: `{"session":"myrepo@worker-x","prompt":"hi"}`,
		},
		{
			name:        "spawn",
			path:        "/spawn",
			method:      http.MethodPost,
			role:        "coordinator",
			sessionName: "myrepo@main",
			repo:        "myrepo",
			body:        `{"branch":"feature-x","prompt":"do the thing"}`,
		},
		{
			name:        "cleanup",
			path:        "/cleanup",
			method:      http.MethodPost,
			role:        "coordinator",
			sessionName: "myrepo@main",
			repo:        "myrepo",
			body:        `{"session":"myrepo@feature-x","yes":true}`,
		},
		{
			name:        "switch",
			path:        "/switch",
			method:      http.MethodPost,
			role:        "coordinator",
			sessionName: "myrepo@main",
			repo:        "myrepo",
			body:        `{"session":"myrepo@feature-x"}`,
		},
		{
			name:        "event",
			path:        "/event",
			method:      http.MethodPost,
			role:        "coordinator",
			sessionName: "myrepo@main",
			repo:        "myrepo",
			body:        `{"kind":"state-change","session":"myrepo@feature-x"}`,
		},
		{
			name:        "escalate",
			path:        "/escalate",
			method:      http.MethodPost,
			role:        "worker",
			sessionName: "myrepo@feature-x",
			repo:        "myrepo",
			body:        `{"prompt":"halp"}`,
		},
		{
			// Coordinator: /investigate is gated on requireCoordinator,
			// so a worker session is refused with 403
			// before the handler ever execs the child.
			name:        "investigate",
			path:        "/investigate",
			method:      http.MethodPost,
			role:        "coordinator",
			sessionName: "myrepo@main",
			repo:        "myrepo",
			body:        `{"prompt":"look into this"}`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// No t.Parallel: newSidecarWithSleepStub calls t.Setenv, which is
			// incompatible with t.Parallel. The whole suite still finishes
			// quickly because each subtest waits a few hundred ms at most.

			d := openTestDB(t)
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			sc := newSidecarWithSleepStub(t, tc.sessionName, tc.repo, tc.role, pidFile, d)

			// /switch resolves the target session's worktree from the DB
			// before shelling out, so we must seed an agent_status row for
			// the target. Without this the handler returns 500 before exec.
			if tc.path == "/switch" {
				if err := d.UpsertStatus("myrepo@feature-x", "myrepo", "/tmp/myrepo-feature-x", "active", nil, nil); err != nil {
					t.Fatalf("seed switch target session: %v", err)
				}
			}

			// Drive the handler in a goroutine so we can synchronise on the
			// pid file before triggering cancellation. A direct
			// runWithCancelledContext call cancels on a timer, which races
			// with stub startup on a loaded test runner.
			handler := sc.hostAPIHandler()
			req := newHostAPIRequest(t, tc.method, tc.path, tc.body)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req = req.WithContext(ctx)
			rr := httptest.NewRecorder()

			done := make(chan struct{})
			start := time.Now()
			go func() {
				handler.ServeHTTP(rr, req)
				close(done)
			}()

			// Wait for the stub to record its PID — proves exec actually
			// reached the subprocess. 5 s is generous; the stub is a tiny
			// shell script.
			pid := waitForPidFile(t, pidFile, 5*time.Second)

			// Trigger client-disconnect.
			cancel()

			// The handler must return promptly. 5 s is far smaller than the
			// smallest per-endpoint WithTimeout (30 s for /switch /event
			// /escalate) so a pass here means the kill went through the
			// r.Context() cancellation, not the WithTimeout deadline.
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("handler %s did not return within 5 s of context cancellation", tc.path)
			}
			elapsed := time.Since(start)

			// Documented non-200: 499 ("client closed request") for the
			// client-cancellation path. See contextErrStatus in host_api.go.
			if rr.Code != statusClientClosedRequest {
				t.Errorf("%s: status = %d, body = %q; want %d (client closed request)",
					tc.path, rr.Code, rr.Body.String(), statusClientClosedRequest)
			}

			// Sanity: handler returned well before any per-endpoint timeout
			// could have fired.
			if elapsed > 10*time.Second {
				t.Errorf("%s: handler took %v to return; expected sub-second after cancel", tc.path, elapsed)
			}

			// And the child is gone.
			assertProcessKilled(t, pid, 5*time.Second)
		})
	}
}

// TestContextErrStatus_DeadlineExceededReturns504 verifies the timeout branch
// of the helper used by every wired-up handler. The 504 path is unit-tested
// here (rather than driven end-to-end through a handler, which would require
// waiting for the smallest per-endpoint deadline of 30 s) so that the AC
// "504 Gateway Timeout for timeout" is exercised in CI on every run.
func TestContextErrStatus_DeadlineExceededReturns504(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	// Wait for the deadline to definitely have fired.
	<-ctx.Done()
	time.Sleep(2 * time.Millisecond)

	status, ok := contextErrStatus(ctx)
	if !ok {
		t.Fatalf("contextErrStatus: ok=false on a context whose deadline has elapsed; want ok=true")
	}
	if status != http.StatusGatewayTimeout {
		t.Errorf("contextErrStatus on deadline-exceeded: status = %d, want %d (504 Gateway Timeout)",
			status, http.StatusGatewayTimeout)
	}
}

// TestContextErrStatus_CancelReturns499 verifies the client-disconnect branch
// of the helper.
func TestContextErrStatus_CancelReturns499(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	status, ok := contextErrStatus(ctx)
	if !ok {
		t.Fatalf("contextErrStatus: ok=false on a cancelled context; want ok=true")
	}
	if status != statusClientClosedRequest {
		t.Errorf("contextErrStatus on cancel: status = %d, want %d (client closed request)",
			status, statusClientClosedRequest)
	}
}

// TestContextErrStatus_LiveContextReturnsFalse verifies the no-context-error
// branch: when ctx.Err() is nil the helper returns ok=false so the caller
// knows to fall through to the normal subprocess-failure path.
func TestContextErrStatus_LiveContextReturnsFalse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	status, ok := contextErrStatus(ctx)
	if ok {
		t.Fatalf("contextErrStatus on live context: ok=true (status=%d); want ok=false", status)
	}
}
