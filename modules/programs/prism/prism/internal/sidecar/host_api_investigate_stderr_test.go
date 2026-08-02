package sidecar

// host_api_investigate_stderr_test.go — issue #2362 B1 / parent #2356.
//
// Regression tests for the /investigate handler's stream capture. Before
// this fix the handler used `cmd.Output()`, which discards child stderr on
// the success path and, on error, formats only the exit-status string —
// swallowing the actionable message from the child (e.g. `invoker session
// %q has no agent_status row`). The fix mirrors /escalate's shape (separate
// stdout/stderr buffers) so:
//
//   - On the error path, the JSON error body includes both the child's
//     stderr and stdout tails alongside the error string.
//   - On the success path, any non-fatal stderr warnings are logged to the
//     sidecar log rather than discarded.
//   - The stdout-parse for the returned session name is unchanged (streams
//     stay separate — CombinedOutput would corrupt the parse).
//
// # Isolation contract (#1608)
//
// Every test in this file constructs its Sidecar via
// sidecartest.NewIsolated(t, ""), so:
//   - XDG_STATE_HOME points at a t.TempDir(); host paths never escape the
//     test sandbox.
//   - PRISM_TEST_MODE_RESTRICT_HOSTAPI=1 prevents promptdelivery from
//     dialling a real host socket.
//   - The DB is an isolated SQLite file under the test tempdir; the test
//     session name uses the "prism-test@" prefix so a leaked write cannot
//     collide with any live coordinator on the developer's host.

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// newInvestigateStubSidecar builds an isolated Sidecar whose PrismBinaryPath
// points at a shell stub with the caller-supplied body. The session name is
// "prism-test-investigate-stderr-<sanitisedTestName>@main" so each subtest has
// its own row in the isolated DB and cannot collide with sibling tests.
//
// The session is a coordinator: /investigate is gated on requireCoordinator
// (issue #2588), so a worker session would be refused with 403 before the
// handler reached the stream-capture code these tests cover. The row is
// seeded with root_agent_name='coordinator' so the gate resolves from the DB
// rather than from the name heuristic alone.
//
// The returned buffer captures everything the sidecar logs during the request,
// so the success-path assertion "stderr warnings are logged" can be verified
// without touching a real file. The buffer's Bytes() are safe to Read after
// the handler returns because the sidecar's log.Logger is synchronous.
func newInvestigateStubSidecar(t *testing.T, stubBody string) (*Sidecar, *bytes.Buffer, string) {
	t.Helper()

	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	if err := os.WriteFile(stubPath, []byte(stubBody), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}

	bus := sidecartest.NewIsolated(t, "")
	sessionName := "prism-test-investigate-stderr-" + sanitiseTestName(t.Name()) + "@main"
	repo := "prism-test"
	worktree := t.TempDir()

	if err := bus.DB.UpsertStatusSeedRootAgentName(
		sessionName, repo, worktree, "active", nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed coordinator status: %v", err)
	}

	logBuf := &bytes.Buffer{}
	logger := log.New(logBuf, "", 0)

	clk := newTestClock()
	cfg := Config{
		SessionName:     sessionName,
		Repo:            repo,
		Worktree:        worktree,
		HarnessURL:      "http://localhost:14000",
		DB:              bus.DB,
		Clock:           clk,
		AgentRole:       "coordinator",
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
		Logger:          logger,
	}
	return New(cfg), logBuf, sessionName
}

// TestHostAPI_Investigate_FailureIncludesStderr covers AC #1: on a non-zero
// exit from the host-side child, the /investigate JSON error body must
// include the child's stderr so the caller can surface the actionable
// message instead of a bare "exit status 1".
func TestHostAPI_Investigate_FailureIncludesStderr(t *testing.T) {
	const stderrLine = `prism investigate: invoker session "myrepo@main" has no agent_status row` + "\n"
	// Stub writes the actionable message to stderr and exits 1. Stdout is
	// deliberately empty on the failure path — mirrors the real command.
	stubBody := "#!/bin/sh\n" +
		"printf '%s' " + shellSingleQuote(stderrLine) + " 1>&2\n" +
		"exit 1\n"
	sc, _, _ := newInvestigateStubSidecar(t, stubBody)

	rr := doHostAPI(t, sc, http.MethodPost, "/investigate", `{"prompt":"look into this"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %q, want 500", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (body=%q)", err, rr.Body.String())
	}
	if !strings.Contains(resp["error"], "investigate failed") {
		t.Errorf("error = %q, want substring 'investigate failed'", resp["error"])
	}
	if resp["stderr"] != stderrLine {
		t.Errorf("response stderr = %q, want %q", resp["stderr"], stderrLine)
	}
	// Sanity: stdout was empty on this path, so response stdout must also
	// be empty (proves the two streams stayed separate).
	if resp["stdout"] != "" {
		t.Errorf("response stdout = %q, want empty", resp["stdout"])
	}
}

// TestHostAPI_Investigate_SuccessLogsStderrToSidecarLog covers AC #2:
// on a successful (exit 0) child invocation, any stderr warnings emitted
// by the child are written to the sidecar log rather than discarded.
// The session-name parse on stdout must remain unaffected.
func TestHostAPI_Investigate_SuccessLogsStderrToSidecarLog(t *testing.T) {
	const wantSessionName = "myrepo@main~investigate-abc123"
	const stderrWarning = "prism investigate: warning: dedup window rounded up to 1m\n"
	// Stub writes the session name to stdout AND a warning to stderr, then
	// exits 0 — the exact "success with warnings" shape #2360 cares about.
	stubBody := "#!/bin/sh\n" +
		"printf '%s' " + shellSingleQuote(wantSessionName+"\n") + "\n" +
		"printf '%s' " + shellSingleQuote(stderrWarning) + " 1>&2\n" +
		"exit 0\n"
	sc, logBuf, _ := newInvestigateStubSidecar(t, stubBody)

	rr := doHostAPI(t, sc, http.MethodPost, "/investigate", `{"prompt":"look into this"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (body=%q)", err, rr.Body.String())
	}
	// AC #3: session-name parse on stdout is unaffected by the stderr
	// warning (streams stayed separate).
	if resp["session_name"] != wantSessionName {
		t.Errorf("response session_name = %q, want %q", resp["session_name"], wantSessionName)
	}
	// AC #2: the stderr warning was written to the sidecar log.
	// TrimSpace: the stub's trailing newline may or may not be present in the
	// logger output depending on Printf formatting, so match on substring.
	logOut := logBuf.String()
	if !strings.Contains(logOut, strings.TrimSpace(stderrWarning)) {
		t.Errorf("sidecar log missing stderr warning; log=%q want substring=%q",
			logOut, strings.TrimSpace(stderrWarning))
	}
	if !strings.Contains(logOut, "child stderr") {
		t.Errorf("sidecar log missing 'child stderr' marker; log=%q", logOut)
	}
}

// TestHostAPI_Investigate_SuccessSilentDoesNotLogStderrLine covers the
// no-op case for AC #2: when the child exits 0 with no stderr output,
// the sidecar log must NOT contain a spurious "child stderr" line.
// This guards against a naive fix that logs unconditionally.
func TestHostAPI_Investigate_SuccessSilentDoesNotLogStderrLine(t *testing.T) {
	const wantSessionName = "myrepo@main~investigate-quiet\n"
	stubBody := "#!/bin/sh\n" +
		"printf '%s' " + shellSingleQuote(wantSessionName) + "\n" +
		"exit 0\n"
	sc, logBuf, _ := newInvestigateStubSidecar(t, stubBody)

	rr := doHostAPI(t, sc, http.MethodPost, "/investigate", `{"prompt":"look into this"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	if strings.Contains(logBuf.String(), "child stderr") {
		t.Errorf("sidecar log contained 'child stderr' marker for a silent success: log=%q", logBuf.String())
	}
}
