package iris

// supervisor_extension_error_test.go — tests for handleRPCEvent's
// extension_error case and default (unknown-type) arm (issue #1757).
//
// Background. The supervisor reads pi's stdout RPC channel line-by-line
// and dispatches on the `type` field. Before #1757 the switch handled
// only agent_start / agent_end / response, and dropped every other frame
// silently. Fatal extension failures (the prism extension throwing in
// session_start) were invisible in the daemon log, even though pi emitted
// the diagnostic on the right channel.
//
// Coverage:
//
//   - An extension_error frame transitions the supervisor to StateError,
//     captures "extension_error" as the kill reason, and writes the error
//     message + extension path + event name into the per-session log.
//   - An extension_error frame does NOT trigger a restart (the failure is
//     non-retriable; the same fault will fire on every spawn until the
//     underlying extension bug is fixed).
//   - An unknown RPC type leaves the supervisor in StateActive and logs
//     the raw frame (truncated form is fine) plus the type field.
//   - Known types (agent_start, agent_end, response) continue to log /
//     no-op as before — no regression from the new switch arms.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// newExtensionErrorSupervisor returns a Supervisor wired with a per-session
// log file we can read back, plus the path to that log file. The supervisor
// has NO running pi child — these tests call handleRPCEvent directly, the
// same pattern as supervisor_state_change_test.go's handleStateChange tests.
func newExtensionErrorSupervisor(t *testing.T) (sup *Supervisor, logPath string, database *db.DB) {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	runDir, err := os.MkdirTemp("", "iris-extn-err-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	logDir := filepath.Join(tmp, "logs")
	sessionName := "iris-test@extension-error"

	cfg := SupervisorConfig{
		SessionName: sessionName,
		Worktree:    tmp,
		Role:        "worker",
		RunDir:      runDir,
		LogDir:      logDir,
		Database:    database,
	}
	sup, err = NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(sup.closeSessionLogFile)

	logPath = filepath.Join(logDir, sanitiseSessionFileName(sessionName)+".log")
	return sup, logPath, database
}

// readLog reads the per-session log file. Returns "" when the file does
// not exist yet (helpful when assertions run before any logf call has
// fired).
func readLog(t *testing.T, logPath string) string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("ReadFile %q: %v", logPath, err)
	}
	return string(b)
}

// TestHandleRPCEvent_ExtensionError_TransitionsToStateError pins the issue
// #1757 acceptance criteria: an extension_error frame must drive the
// supervisor to StateError with the error message, extension path, and
// event name in the daemon log.
func TestHandleRPCEvent_ExtensionError_TransitionsToStateError(t *testing.T) {
	sup, logPath, database := newExtensionErrorSupervisor(t)

	const extPath = "/nix/store/abc-prism-extension/prism.ts"
	const eventName = "session_start"
	// Embedded double quotes in the error message exercise the JSON-line
	// decode — marshal via encoding/json rather than string concat so the
	// wire frame is valid even when the error text contains " or \.
	const errMsg = "[iris-extension] fatal: canonical built-in \"read\" was not overridden"

	frameMap := map[string]any{
		"type":          "extension_error",
		"extensionPath": extPath,
		"event":         eventName,
		"error":         errMsg,
	}
	frameBytes, err := json.Marshal(frameMap)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	frame := append(frameBytes, '\n')

	sup.handleRPCEvent(frame)

	if got := sup.State(); got != StateError {
		t.Fatalf("State() = %q, want %q after extension_error", got, StateError)
	}

	logContent := readLog(t, logPath)
	// Substring assertions — the log line uses %q which JSON-escapes inner
	// quotes, so we look for the unique anchor tokens from the original
	// message rather than the exact string.
	for _, want := range []string{eventName, extPath, "extension_error",
		"[iris-extension] fatal:", "was not overridden"} {
		if !strings.Contains(logContent, want) {
			t.Errorf("log missing %q\nfull log:\n%s", want, logContent)
		}
	}

	// session_end event should be present with reason="extension_error" so
	// downstream consumers (journal, narrative CLI, parent-notify) see the
	// non-retriable terminal cause.
	assertSessionEndReason(t, database, sup.sess.SessionName, "extension_error")
}

// TestHandleRPCEvent_ExtensionError_NoRestart pins the watch-out: an
// extension_error frame is a non-retriable failure. The supervisor must
// not loop back into spawning. We verify this by driving Start() against
// a sleep-like binary, injecting an extension_error frame into
// handleRPCEvent, and asserting the Start goroutine exits in StateError
// without re-spawning.
func TestHandleRPCEvent_ExtensionError_NoRestart(t *testing.T) {
	// Use a sleep wrapper as the "pi" binary so spawnAndRun reaches
	// StateActive and the per-session ctx is wired up (handleExtensionError
	// needs s.cancel to terminate the child).
	script := writeShellScript(t, "exec sleep 60\n", "")
	sup, database := killTestSupervisor(t, script)

	preRestart := sup.sess.RestartCount

	// Inject the extension_error frame directly — same path the stdout
	// reader goroutine would take when pi emits one.
	frame := []byte(`{"type":"extension_error","extensionPath":"/x/prism.ts","event":"session_start","error":"boom"}` + "\n")
	sup.handleRPCEvent(frame)

	// Wait for Start to converge on StateError. Bounded so a regression
	// (e.g. restart loop) fails the test rather than hanging.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := sup.State()
		if st == StateError {
			break
		}
		if st == StateSpawning {
			t.Fatalf("supervisor re-entered StateSpawning after extension_error \u2014 must not restart")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := sup.State(); got != StateError {
		t.Fatalf("State() = %q, want %q (did Start return?)", got, StateError)
	}

	// RestartCount must not have advanced \u2014 the loop should have exited
	// via the ctx.Err() branch (suppressed by killReason guard), not via
	// the non-zero-exit restart path that bumps RestartCount.
	if sup.sess.RestartCount != preRestart {
		t.Errorf("RestartCount = %d, want %d (extension_error must not trigger restart)",
			sup.sess.RestartCount, preRestart)
	}

	// Exactly one session_end event with the extension_error reason. A
	// duplicate would indicate the ctx.Err() branch's StateFinished
	// override leaked past the killReason guard.
	if got := countSessionEndEvents(t, database, sup.sess.SessionName); got != 1 {
		t.Errorf("session_end event count = %d, want 1", got)
	}
	assertSessionEndReason(t, database, sup.sess.SessionName, "extension_error")
}

// TestHandleRPCEvent_UnknownType_LoggedButHarmless pins the default arm:
// an unrecognised RPC type must be logged (so future pi RPC additions are
// visible) and must not transition the supervisor or panic.
func TestHandleRPCEvent_UnknownType_LoggedButHarmless(t *testing.T) {
	sup, logPath, _ := newExtensionErrorSupervisor(t)

	// Drive to StateActive so the test can assert "unknown type does not
	// flip state". setState() emits a log line on the transition; we
	// snapshot the log AFTER that so the unknown-type assertion isn't
	// confused by setState's own line.
	sup.setState(StateActive)
	preLog := readLog(t, logPath)

	frame := []byte(`{"type":"some_future_event","data":42}` + "\n")
	sup.handleRPCEvent(frame)

	if got := sup.State(); got != StateActive {
		t.Errorf("State() = %q, want %q (unknown RPC type must not change state)", got, StateActive)
	}

	logContent := readLog(t, logPath)
	if logContent == preLog {
		t.Fatal("default arm produced no log output \u2014 unknown types must be surfaced")
	}
	// New content only.
	newContent := logContent[len(preLog):]
	for _, want := range []string{"some_future_event", "unknown RPC type"} {
		if !strings.Contains(newContent, want) {
			t.Errorf("new log content missing %q\nnew content:\n%s", want, newContent)
		}
	}
	// The raw frame body should appear (the truncation helper preserves
	// short frames intact).
	if !strings.Contains(newContent, `"data":42`) {
		t.Errorf("raw frame body missing from log; got:\n%s", newContent)
	}
}

// TestHandleRPCEvent_UnknownType_TruncatesHugeRawFrame guards the
// hot-path watch-out: a runaway frame must not blow the log file out.
// The default arm caps the raw payload at rpcLogMaxBytes.
func TestHandleRPCEvent_UnknownType_TruncatesHugeRawFrame(t *testing.T) {
	sup, logPath, _ := newExtensionErrorSupervisor(t)
	sup.setState(StateActive)
	preLog := readLog(t, logPath)

	// 100 KiB of junk inside a "data" field. This is well past
	// rpcLogMaxBytes (~1 KiB).
	huge := strings.Repeat("A", 100*1024)
	frame := []byte(`{"type":"future_huge","data":"` + huge + `"}` + "\n")
	sup.handleRPCEvent(frame)

	logContent := readLog(t, logPath)
	newContent := logContent[len(preLog):]

	// The new log content must be vastly smaller than the raw frame \u2014
	// truncation is doing its job.
	if len(newContent) > 4*1024 {
		t.Errorf("default-arm log line is %d bytes for a 100 KiB frame \u2014 truncation regression",
			len(newContent))
	}
	if !strings.Contains(newContent, "...(truncated)") {
		t.Errorf("expected truncation marker in log output, got:\n%s", newContent)
	}
	// Type must still be present so the line remains useful.
	if !strings.Contains(newContent, "future_huge") {
		t.Errorf("type missing from truncated log output, got:\n%s", newContent)
	}
}

// TestHandleRPCEvent_KnownTypes_NoRegression confirms the three pre-#1757
// known types continue to behave as before \u2014 agent_start / agent_end
// log a lifecycle line and response is intentionally silent.
func TestHandleRPCEvent_KnownTypes_NoRegression(t *testing.T) {
	sup, logPath, _ := newExtensionErrorSupervisor(t)
	sup.setState(StateActive)
	preLog := readLog(t, logPath)

	sup.handleRPCEvent([]byte(`{"type":"agent_start"}` + "\n"))
	sup.handleRPCEvent([]byte(`{"type":"agent_end"}` + "\n"))
	sup.handleRPCEvent([]byte(`{"type":"response","id":"x","ok":true}` + "\n"))

	if got := sup.State(); got != StateActive {
		t.Errorf("State() = %q, want %q (known types must not change state)", got, StateActive)
	}

	logContent := readLog(t, logPath)
	newContent := logContent[len(preLog):]
	for _, want := range []string{"agent_start", "agent_end"} {
		if !strings.Contains(newContent, want) {
			t.Errorf("expected %q in log after known-type frame; got:\n%s", want, newContent)
		}
	}
	// `response` is intentionally suppressed at debug level. The log must
	// not include "unknown RPC type" for it \u2014 that would mean the
	// default arm picked it up by mistake.
	if strings.Contains(newContent, "unknown RPC type") {
		t.Errorf("known types must not hit the default arm; got:\n%s", newContent)
	}
}

// TestHandleRPCEvent_MalformedExtensionError pins the defensive path:
// a malformed extension_error frame still surfaces a diagnostic and still
// drives the session to StateError. The raw line is captured so the
// operator can reconstruct what pi sent.
func TestHandleRPCEvent_MalformedExtensionError(t *testing.T) {
	sup, logPath, _ := newExtensionErrorSupervisor(t)

	// Valid JSON for the type-dispatch decode, but the body fields don't
	// decode into ExtensionErrorFrame as strings (event is an int).
	frame := []byte(`{"type":"extension_error","event":42,"extensionPath":["arr"],"error":null}` + "\n")
	sup.handleRPCEvent(frame)

	if got := sup.State(); got != StateError {
		t.Fatalf("State() = %q, want %q even on malformed extension_error", got, StateError)
	}
	logContent := readLog(t, logPath)
	if !strings.Contains(logContent, "extension_error") {
		t.Errorf("malformed extension_error still must log; got:\n%s", logContent)
	}
}
