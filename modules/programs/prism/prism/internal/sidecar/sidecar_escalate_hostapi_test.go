package sidecar

// Tests for the host-API /escalate endpoint, focused on the cross-session
// integrity check that prevents a worker from escalating "from" any session
// other than its own. The check mirrors the rule applied by /prompt and
// /set-model in the same file. A regression here would let a non-coordinator
// mutate `agent_status.state`, emit a `session.escalated` bus event
// attributed to a victim, and pin that victim in `escalated` so legitimate
// `has finished` notifications are suppressed (review-security finding,
// PR #1524 round 1).

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestHostAPI_Escalate_WorkerCannotImpersonate verifies that a worker session
// is rejected with HTTP 403 when it sets `from` to any session other than its
// own. The shell-out path must NOT be reached.
func TestHostAPI_Escalate_WorkerCannotImpersonate(t *testing.T) {
	d := openTestDB(t)
	// Use the role-and-binary helper so that even if the auth check failed
	// (regression) the shell-out would exit code 1, not block the test.
	sc := newSidecarWithRoleAndBinary(t, "myrepo@feature", "myrepo", "worker", d)

	body := `{"prompt":"halp","from":"myrepo@victim"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", body)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %q, want 403", rr.Code, rr.Body.String())
	}
	// The error message should name both the legitimate session and the
	// rejected `from` value so the caller can diagnose the mistake.
	if !strings.Contains(rr.Body.String(), "myrepo@feature") {
		t.Errorf("body %q: want message naming the calling session", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "myrepo@victim") {
		t.Errorf("body %q: want message naming the rejected from value", rr.Body.String())
	}
}

// TestHostAPI_Escalate_WorkerOwnSessionAccepted verifies the legitimate path:
// a worker setting `from` to its own session name is accepted (the auth check
// passes; the request then reaches the shell-out which fails with 500 on the
// stub binary — that's expected and proves we got past the 403 gate).
func TestHostAPI_Escalate_WorkerOwnSessionAccepted(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRoleAndBinary(t, "myrepo@feature", "myrepo", "worker", d)

	body := `{"prompt":"halp","from":"myrepo@feature"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", body)

	// 500 means the auth check passed and the stub binary rejected the call.
	// 403 would mean the auth check rejected it (regression).
	if rr.Code == http.StatusForbidden {
		t.Fatalf("worker rejected for its own session: %q", rr.Body.String())
	}
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %q, want 500 (stub binary fails) or 200", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Escalate_WorkerEmptyFromAccepted verifies that the common path
// (worker omits `from`, sidecar substitutes its own session name) is accepted.
// Same shape as the own-session test: 500 from the stub binary, never 403.
func TestHostAPI_Escalate_WorkerEmptyFromAccepted(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRoleAndBinary(t, "myrepo@feature", "myrepo", "worker", d)

	body := `{"prompt":"halp"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", body)

	if rr.Code == http.StatusForbidden {
		t.Fatalf("worker rejected when from was empty: %q", rr.Body.String())
	}
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %q, want 500 (stub binary fails)", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Escalate_CoordinatorCanProxy verifies that a coordinator session
// IS allowed to escalate on behalf of a different session. This is needed so
// that future tooling (e.g. an admin UI or an automated supervisor) can drive
// escalations through a coordinator's host-API socket.
func TestHostAPI_Escalate_CoordinatorCanProxy(t *testing.T) {
	d := openTestDB(t)
	// Seed the coordinator row so isCoordinatorSession reads back the role.
	role := "coordinator"
	if err := d.UpsertStatusWithRootAgent("myrepo@main", "myrepo", "/wt", "active", nil, nil, &role, nil); err != nil {
		t.Fatalf("seed coordinator: %v", err)
	}
	sc := newSidecarWithRoleAndBinary(t, "myrepo@main", "myrepo", "coordinator", d)

	body := `{"prompt":"halp","from":"myrepo@feature"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", body)

	if rr.Code == http.StatusForbidden {
		t.Fatalf("coordinator rejected when proxying for another session: %q", rr.Body.String())
	}
	// Either OK or 500 (from the stub binary) is fine — we only care that the
	// auth gate did NOT 403.
	if rr.Code != http.StatusOK && rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %q, want 200 or 500", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Escalate_EmptyPromptRejected verifies the existing 400 guard
// for missing prompt remains intact alongside the new auth check.
func TestHostAPI_Escalate_EmptyPromptRejected(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRoleAndBinary(t, "myrepo@feature", "myrepo", "worker", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", `{"prompt":"   "}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "prompt is required") {
		t.Errorf("body %q: want 'prompt is required'", rr.Body.String())
	}
}

// TestHostAPI_Escalate_MalformedJSONRejected verifies the existing 400 guard
// for malformed JSON remains intact.
func TestHostAPI_Escalate_MalformedJSONRejected(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRoleAndBinary(t, "myrepo@feature", "myrepo", "worker", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", `{not valid`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Escalate_GetMethodRejected verifies the requirePost guard is
// applied (consistent with sibling endpoints).
func TestHostAPI_Escalate_GetMethodRejected(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRoleAndBinary(t, "myrepo@feature", "myrepo", "worker", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/escalate", "")

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

// newSidecarWithEscalateStub returns a Sidecar configured with a stub
// `prism` binary that, when invoked with "escalate", writes a deterministic
// success line to stdout and a mirror to stderr, then exits 0. This lets us
// verify that the host-side /escalate handler captures both streams
// separately and returns them in the JSON response body — the round-trip
// the container proxy needs to re-emit the OK signal locally.
func newSidecarWithEscalateStub(t *testing.T, sessionName, repo, role string, d *db.DB, stdoutLine, stderrLine string, exitCode int) *Sidecar {
	t.Helper()
	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	// Args: $1="escalate", rest=flags. We print fixed lines so tests can
	// assert byte-for-byte equality; we don't try to parse the flags here.
	stubScript := "#!/bin/sh\n" +
		"if [ \"$1\" = \"escalate\" ]; then\n" +
		"  printf '%s' " + shellSingleQuote(stdoutLine) + "\n" +
		"  printf '%s' " + shellSingleQuote(stderrLine) + " 1>&2\n" +
		"  exit " + itoa(exitCode) + "\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
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

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestHostAPI_Escalate_SuccessReturnsStdoutAndStderr verifies that on a
// successful host-side `prism escalate` invocation, the /escalate handler
// returns the captured stdout AND stderr in the response body, with the
// streams kept separate (NOT combined). The container-side proxy depends
// on this to re-emit each stream locally. See PR #2019 review-context
// blocker / issue #2018.
func TestHostAPI_Escalate_SuccessReturnsStdoutAndStderr(t *testing.T) {
	d := openTestDB(t)
	const wantStdout = "prism escalate: OK delivered to myrepo@main (delivery_id=abc-123)\n"
	const wantStderr = "prism escalate: OK delivered to myrepo@main (delivery_id=abc-123)\n"
	sc := newSidecarWithEscalateStub(t, "myrepo@feature", "myrepo", "worker", d, wantStdout, wantStderr, 0)

	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", `{"prompt":"halp"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (body=%q)", err, rr.Body.String())
	}
	if resp["stdout"] != wantStdout {
		t.Errorf("response stdout = %q, want %q", resp["stdout"], wantStdout)
	}
	if resp["stderr"] != wantStderr {
		t.Errorf("response stderr = %q, want %q", resp["stderr"], wantStderr)
	}
	// Streams must be kept separate (no merging). Verify by giving the
	// stub distinct lines on each stream and asserting they round-trip
	// to the correct response field.
	sc2 := newSidecarWithEscalateStub(t, "myrepo@feature", "myrepo", "worker", d,
		"STDOUT-only-line\n", "STDERR-only-line\n", 0)
	rr2 := doHostAPI(t, sc2, http.MethodPost, "/escalate", `{"prompt":"halp"}`)
	if rr2.Code != http.StatusOK {
		t.Fatalf("split-streams status = %d, body = %q", rr2.Code, rr2.Body.String())
	}
	var resp2 map[string]string
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal split-streams response: %v", err)
	}
	if resp2["stdout"] != "STDOUT-only-line\n" {
		t.Errorf("split-streams stdout = %q, want STDOUT-only-line", resp2["stdout"])
	}
	if resp2["stderr"] != "STDERR-only-line\n" {
		t.Errorf("split-streams stderr = %q, want STDERR-only-line", resp2["stderr"])
	}
}

// TestHostAPI_Escalate_FailureIncludesStdoutAndStderr verifies that on a
// non-zero exit from the host-side child, the /escalate handler returns
// 500 with stdout/stderr alongside the error message so the proxy can
// re-emit partial-success output and surface the underlying cause.
func TestHostAPI_Escalate_FailureIncludesStdoutAndStderr(t *testing.T) {
	d := openTestDB(t)
	const wantStdout = ""
	const wantStderr = "prism escalate: deliver prompt to myrepo@main: connection refused\n"
	sc := newSidecarWithEscalateStub(t, "myrepo@feature", "myrepo", "worker", d, wantStdout, wantStderr, 1)

	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", `{"prompt":"halp"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %q, want 500", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (body=%q)", err, rr.Body.String())
	}
	if !strings.Contains(resp["error"], "escalate failed") {
		t.Errorf("error = %q, want substring 'escalate failed'", resp["error"])
	}
	if resp["stderr"] != wantStderr {
		t.Errorf("failure stderr = %q, want %q", resp["stderr"], wantStderr)
	}
}

// TestHostAPI_Escalate_ForwardsJSONAndDedupWindowFlags verifies that the
// /escalate handler forwards json=true and dedup_window=<dur> to the
// host-side child as --json and --dedup-window flags. The stub captures
// its argv and prints it to stdout for the test to assert on.
func TestHostAPI_Escalate_ForwardsJSONAndDedupWindowFlags(t *testing.T) {
	d := openTestDB(t)
	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	// Echo the full argv to stdout so the test can assert flags.
	stubScript := "#!/bin/sh\necho \"$@\"\nexit 0\n"
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	clk := newTestClock()
	sc := New(Config{
		SessionName:     "myrepo@feature",
		Repo:            "myrepo",
		Worktree:        "/tmp/myrepo@feature",
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           clk,
		AgentRole:       "worker",
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	})

	rr := doHostAPI(t, sc, http.MethodPost, "/escalate",
		`{"prompt":"halp","json":true,"dedup_window":"10m"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	argv := resp["stdout"]
	if !strings.Contains(argv, "--json") {
		t.Errorf("argv %q missing --json", argv)
	}
	if !strings.Contains(argv, "--dedup-window 10m") {
		t.Errorf("argv %q missing --dedup-window 10m", argv)
	}
}
