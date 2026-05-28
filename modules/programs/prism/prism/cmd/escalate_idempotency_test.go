package cmd

// Tests for `prism escalate` Part B (unambiguous success signal) and Part C
// (sender-side idempotency guard). See issue #2018.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// captureStdoutStderr redirects os.Stdout and os.Stderr to pipes for the
// duration of fn, draining both concurrently so writes larger than the
// kernel pipe buffer cannot deadlock. See
// modules/programs/prism/prism/docs/stdout-capture-testing.md.
func captureStdoutStderr(t *testing.T, fn func()) (stdout string, stderr string) {
	t.Helper()
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stdout): %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stderr): %v", err)
	}
	oldOut := os.Stdout
	oldErr := os.Stderr
	os.Stdout = wOut
	os.Stderr = wErr

	var outBuf, errBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&outBuf, rOut)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&errBuf, rErr)
	}()

	fn()

	wOut.Close()
	wErr.Close()
	wg.Wait()
	os.Stdout = oldOut
	os.Stderr = oldErr
	return outBuf.String(), errBuf.String()
}

// busRowCount returns the number of bus_messages rows for the given pair.
func busRowCount(t *testing.T, d *db.DB, fromSession, toSession string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM bus_messages WHERE from_session = ? AND to_session = ?",
		fromSession, toSession,
	).Scan(&n); err != nil {
		t.Fatalf("count bus_messages: %v", err)
	}
	return n
}

// eventCount returns the number of agent_events rows for (session, type).
func eventCount(t *testing.T, d *db.DB, sessionName, evType string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM agent_events WHERE session_name = ? AND type = ?",
		sessionName, evType,
	).Scan(&n); err != nil {
		t.Fatalf("count agent_events: %v", err)
	}
	return n
}

// seedEscalatePair seeds a coordinator + worker pair for the escalate tests
// that drive the cobra command end-to-end. It returns the captured-call
// pointer so callers can assert on what (if anything) was delivered.
func seedEscalatePair(t *testing.T, d *db.DB, fromSession, toSession string) *capturedPromptCall {
	t.Helper()
	srv, captured := startEscalateHTTPStub(t)
	port := extractTestServerPort(t, srv.URL)
	httpClient = &http.Client{Timeout: 2 * time.Second}
	seedSession(t, d, toSession, "active", intPtr(port), strPtr("oc-sid-"+toSession), strPtr("coordinator"), nil)
	seedSession(t, d, fromSession, "active", nil, nil, strPtr("worker"), nil)
	return captured
}

// ---------------------------------------------------------------------------
// Part B — unambiguous success signal
// ---------------------------------------------------------------------------

func TestEscalate_PartB_HumanSuccessLine_StdoutAndStderr(t *testing.T) {
	d := openPromptTestDB(t)
	_ = seedEscalatePair(t, d, "repo@feature", "repo@main")

	stdout, stderr := captureStdoutStderr(t, func() {
		if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp", escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
			t.Fatalf("runEscalateForSessionOpts: %v", err)
		}
	})

	// AC: exactly one line on stdout, "OK" as the first word after "escalate: "
	stdoutLines := splitNonEmptyLines(stdout)
	if len(stdoutLines) != 1 {
		t.Fatalf("stdout line count = %d, want 1; got: %q", len(stdoutLines), stdout)
	}
	line := stdoutLines[0]
	if !strings.HasPrefix(line, "prism escalate: OK delivered to repo@main (delivery_id=") {
		t.Errorf("stdout success line = %q, want prefix 'prism escalate: OK delivered to repo@main (delivery_id='", line)
	}
	if !strings.HasSuffix(line, ")") {
		t.Errorf("stdout success line missing trailing ')': %q", line)
	}

	// AC: same line mirrored to stderr.
	stderrLines := splitNonEmptyLines(stderr)
	if len(stderrLines) != 1 {
		t.Fatalf("stderr line count = %d, want 1; got: %q", len(stderrLines), stderr)
	}
	if stderrLines[0] != line {
		t.Errorf("stderr line = %q, want %q (mirror of stdout)", stderrLines[0], line)
	}

	// AC: OK token is the first whitespace-delimited word after "escalate: ".
	after := strings.TrimPrefix(line, "prism escalate: ")
	firstWord := strings.SplitN(after, " ", 2)[0]
	if firstWord != "OK" {
		t.Errorf("first word after 'escalate: ' = %q, want 'OK'", firstWord)
	}
}

func TestEscalate_PartB_JSON_HappyPath(t *testing.T) {
	d := openPromptTestDB(t)
	_ = seedEscalatePair(t, d, "repo@feature", "repo@main")

	stdout, stderr := captureStdoutStderr(t, func() {
		if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp", escalateOptions{jsonOut: true, dedupWindow: escalateDefaultDedupWindow}); err != nil {
			t.Fatalf("runEscalateForSessionOpts: %v", err)
		}
	})

	stdoutLines := splitNonEmptyLines(stdout)
	if len(stdoutLines) != 1 {
		t.Fatalf("stdout line count = %d, want 1; got: %q", len(stdoutLines), stdout)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdoutLines[0]), &payload); err != nil {
		t.Fatalf("stdout is not a single JSON object: %q (err=%v)", stdoutLines[0], err)
	}
	if payload["delivered_to"] != "repo@main" {
		t.Errorf("delivered_to = %v, want repo@main", payload["delivered_to"])
	}
	if did, ok := payload["delivery_id"].(string); !ok || did == "" {
		t.Errorf("delivery_id missing or empty: %v", payload["delivery_id"])
	}
	if replayed, ok := payload["replayed"].(bool); !ok || replayed {
		t.Errorf("replayed = %v, want false", payload["replayed"])
	}

	// AC: human line may still print on stderr in --json mode for log capture.
	if !strings.Contains(stderr, "prism escalate: OK delivered to repo@main") {
		t.Errorf("stderr missing human mirror in --json mode: %q", stderr)
	}
	// AC: stdout in --json mode contains ONLY JSON, no human line.
	if strings.Contains(stdout, "prism escalate: OK") {
		t.Errorf("stdout contains human line in --json mode (mutual exclusion violated): %q", stdout)
	}
}

func TestEscalate_PartB_JSON_ErrorPath(t *testing.T) {
	d := openPromptTestDB(t)
	// Only the worker — explicit --to to a nonexistent session triggers an
	// error before any state transition (existing behaviour).
	seedSession(t, d, "repo@feature", "active", nil, nil, strPtr("worker"), nil)

	// Drive through the cobra command so the --json flag and error reporter
	// fire. resetRootCmdFlags is called by openPromptTestDB.
	rootCmd.SetArgs([]string{"escalate", "--prompt", "halp", "--to", "ghost-session", "--json"})
	t.Setenv("PRISM_SESSION_NAME", "repo@feature")
	stdout, stderr := captureStdoutStderr(t, func() {
		err := rootCmd.Execute()
		if err == nil {
			t.Fatalf("expected non-nil error from rootCmd.Execute()")
		}
	})

	// AC: stdout MUST be empty in --json error path.
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want empty in --json error path", stdout)
	}
	stderrLines := splitNonEmptyLines(stderr)
	if len(stderrLines) == 0 {
		t.Fatalf("stderr is empty in --json error path: %q", stderr)
	}
	// Find the JSON error line — there should be exactly one and it should
	// be the LAST non-empty stderr line (cobra may print nothing extra
	// because SilenceUsage etc., but the quietExitErr suppresses main's
	// fallback print).
	var jsonLine string
	for _, ln := range stderrLines {
		if strings.HasPrefix(ln, "{") {
			jsonLine = ln
		}
	}
	if jsonLine == "" {
		t.Fatalf("no JSON error envelope on stderr; got: %q", stderr)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(jsonLine), &payload); err != nil {
		t.Fatalf("stderr error envelope is not JSON: %q (err=%v)", jsonLine, err)
	}
	if msg, ok := payload["error"].(string); !ok || msg == "" {
		t.Errorf("error envelope missing 'error': %v", payload)
	}
}

func TestEscalate_PartB_HumanJSONMutualExclusion(t *testing.T) {
	d := openPromptTestDB(t)
	_ = seedEscalatePair(t, d, "repo@feature", "repo@main")

	stdout, _ := captureStdoutStderr(t, func() {
		if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp", escalateOptions{jsonOut: true, dedupWindow: escalateDefaultDedupWindow}); err != nil {
			t.Fatalf("runEscalateForSessionOpts: %v", err)
		}
	})
	if strings.Contains(stdout, "prism escalate: OK") {
		t.Errorf("--json stdout contains human-readable 'prism escalate: OK' (mutual exclusion violated): %q", stdout)
	}
	stdoutLines := splitNonEmptyLines(stdout)
	if len(stdoutLines) != 1 {
		t.Errorf("--json stdout line count = %d, want exactly 1; got: %q", len(stdoutLines), stdout)
	}
}

// ---------------------------------------------------------------------------
// Part C — sender-side idempotency guard
// ---------------------------------------------------------------------------

func TestEscalate_PartC_ReplayWithinWindow_SkipsAllWrites(t *testing.T) {
	d := openPromptTestDB(t)
	captured := seedEscalatePair(t, d, "repo@feature", "repo@main")

	// First invocation.
	if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp", escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
		t.Fatalf("first runEscalateForSessionOpts: %v", err)
	}
	busCountBefore := busRowCount(t, d, "repo@feature", "repo@main")
	escEventsBefore := eventCount(t, d, "repo@feature", "escalation")
	busEventsBefore := eventCount(t, d, "repo@feature", "session.escalated")
	if busCountBefore != 1 {
		t.Fatalf("bus_messages count after first call = %d, want 1", busCountBefore)
	}

	// Clear the captured prompt so we can detect any new delivery.
	captured.body = nil

	// Second invocation — same prompt, within window.
	stdout, stderr := captureStdoutStderr(t, func() {
		if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp", escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
			t.Fatalf("second runEscalateForSessionOpts: %v", err)
		}
	})

	// AC: no new bus_messages, no new agent_events.
	if got := busRowCount(t, d, "repo@feature", "repo@main"); got != busCountBefore {
		t.Errorf("bus_messages count after replay = %d, want %d (no new row)", got, busCountBefore)
	}
	if got := eventCount(t, d, "repo@feature", "escalation"); got != escEventsBefore {
		t.Errorf("escalation agent_events count after replay = %d, want %d", got, escEventsBefore)
	}
	if got := eventCount(t, d, "repo@feature", "session.escalated"); got != busEventsBefore {
		t.Errorf("session.escalated agent_events count after replay = %d, want %d", got, busEventsBefore)
	}

	// AC: no re-delivery (captured.body would be set if the HTTP stub
	// received a second prompt).
	if captured.body != nil {
		t.Errorf("replay re-delivered to coordinator (captured.body = %v); want no new delivery", captured.body)
	}

	// AC: state stays escalated.
	st, _ := d.CurrentStatus("repo@feature")
	if st == nil || st.State != string(agent.StateEscalated) {
		t.Errorf("state after replay = %v, want escalated", stateOf(st))
	}

	// AC: replay line shape.
	stdoutLines := splitNonEmptyLines(stdout)
	if len(stdoutLines) != 1 {
		t.Fatalf("replay stdout line count = %d, want 1; got: %q", len(stdoutLines), stdout)
	}
	line := stdoutLines[0]
	if !strings.HasPrefix(line, "prism escalate: OK already delivered to repo@main (delivery_id=") {
		t.Errorf("replay stdout = %q, want 'OK already delivered' prefix", line)
	}
	if !strings.Contains(line, "age=") {
		t.Errorf("replay stdout missing 'age=': %q", line)
	}
	if !strings.Contains(stderr, line) {
		t.Errorf("stderr does not mirror replay line: stderr=%q line=%q", stderr, line)
	}
}

func TestEscalate_PartC_ReplayOutsideWindow_ProceedsNormally(t *testing.T) {
	d := openPromptTestDB(t)
	captured := seedEscalatePair(t, d, "repo@feature", "repo@main")

	// First call — but set a window of effectively zero to force the second
	// call to land outside the window.
	if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp", escalateOptions{dedupWindow: time.Nanosecond}); err != nil {
		t.Fatalf("first runEscalateForSessionOpts: %v", err)
	}
	// Wait long enough that any sane window has passed.
	time.Sleep(10 * time.Millisecond)
	captured.body = nil

	if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp", escalateOptions{dedupWindow: time.Nanosecond}); err != nil {
		t.Fatalf("second runEscalateForSessionOpts: %v", err)
	}

	// Both deliveries should have landed.
	if got := busRowCount(t, d, "repo@feature", "repo@main"); got != 2 {
		t.Errorf("bus_messages count after second-outside-window = %d, want 2", got)
	}
	if captured.body == nil {
		t.Errorf("second invocation outside window did not re-deliver; captured.body is nil")
	}
}

func TestEscalate_PartC_DifferentPrompt_ProceedsNormally(t *testing.T) {
	d := openPromptTestDB(t)
	captured := seedEscalatePair(t, d, "repo@feature", "repo@main")

	if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp", escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
		t.Fatalf("first runEscalateForSessionOpts: %v", err)
	}
	captured.body = nil
	// Differ by one byte.
	if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp!", escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
		t.Fatalf("second runEscalateForSessionOpts: %v", err)
	}

	if got := busRowCount(t, d, "repo@feature", "repo@main"); got != 2 {
		t.Errorf("bus_messages count with different prompt = %d, want 2", got)
	}
	if captured.body == nil {
		t.Errorf("second invocation with different prompt did not deliver")
	}
}

func TestEscalate_PartC_DedupWindowFlagOverride(t *testing.T) {
	d := openPromptTestDB(t)
	_ = seedEscalatePair(t, d, "repo@feature", "repo@main")

	// First call — write a row.
	if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp", escalateOptions{dedupWindow: time.Hour}); err != nil {
		t.Fatalf("first runEscalateForSessionOpts: %v", err)
	}
	// Override window down to 1ns + sleep — second call should NOT dedup.
	time.Sleep(5 * time.Millisecond)
	if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp", escalateOptions{dedupWindow: time.Nanosecond}); err != nil {
		t.Fatalf("second runEscalateForSessionOpts: %v", err)
	}
	if got := busRowCount(t, d, "repo@feature", "repo@main"); got != 2 {
		t.Errorf("with --dedup-window=1ns, second call should NOT dedup; bus_messages count = %d, want 2", got)
	}
}

func TestEscalate_PartC_QueuedNotYetDelivered_ShortCircuits(t *testing.T) {
	d := openPromptTestDB(t)
	// Seed a worker but NOT a coordinator HTTP stub — we synthesise a queued
	// row directly to test the "delivered_at IS NULL" replay path.
	seedSession(t, d, "repo@main", "active", nil, nil, strPtr("coordinator"), nil)
	seedSession(t, d, "repo@feature", "active", nil, nil, strPtr("worker"), nil)

	// Mark the worker as already in escalated state — the dedup guard only
	// fires when current_state == escalated. The bus row would have been
	// written by a prior delivery that hasn't yet been flushed.
	if err := d.UpsertStatus("repo@feature", "repo", "/code/repo/main", string(agent.StateEscalated), nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.WriteBusMessage(db.BusMessage{
		ID:          "queued-prior-id",
		FromSession: "repo@feature",
		ToSession:   "repo@main",
		Repo:        "repo",
		Text:        "halp",
		Urgency:     "normal",
		SentAt:      time.Now(),
	}); err != nil {
		t.Fatalf("WriteBusMessage (queued): %v", err)
	}

	escEventsBefore := eventCount(t, d, "repo@feature", "escalation")
	busCountBefore := busRowCount(t, d, "repo@feature", "repo@main")

	stdout, _ := captureStdoutStderr(t, func() {
		if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp", escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
			t.Fatalf("runEscalateForSessionOpts: %v", err)
		}
	})

	// AC: no new bus_messages, no new escalation event.
	if got := busRowCount(t, d, "repo@feature", "repo@main"); got != busCountBefore {
		t.Errorf("bus_messages count after queued-replay = %d, want %d", got, busCountBefore)
	}
	if got := eventCount(t, d, "repo@feature", "escalation"); got != escEventsBefore {
		t.Errorf("escalation event count after queued-replay = %d, want %d", got, escEventsBefore)
	}
	// AC: replay line surfaces the prior delivery_id.
	if !strings.Contains(stdout, "delivery_id=queued-prior-id") {
		t.Errorf("replay stdout missing prior delivery_id: %q", stdout)
	}
}

func TestEscalate_PartC_StateNotEscalated_BypassesDedup(t *testing.T) {
	d := openPromptTestDB(t)
	captured := seedEscalatePair(t, d, "repo@feature", "repo@main")

	// First invocation — transitions to escalated and writes a bus row.
	if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp", escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
		t.Fatalf("first runEscalateForSessionOpts: %v", err)
	}

	// Simulate a turn_start clearing the escalated state.
	if err := d.UpsertStatus("repo@feature", "repo", "/code/repo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus(active): %v", err)
	}
	captured.body = nil

	// Second invocation: same prompt, within window, but state is now
	// `active` not `escalated`. The dedup guard MUST NOT fire — this is a
	// genuine re-escalation.
	if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp", escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
		t.Fatalf("second runEscalateForSessionOpts: %v", err)
	}
	if got := busRowCount(t, d, "repo@feature", "repo@main"); got != 2 {
		t.Errorf("bus_messages count after state-bypass = %d, want 2", got)
	}
	if captured.body == nil {
		t.Errorf("second invocation after state-cleared did not re-deliver")
	}
	st, _ := d.CurrentStatus("repo@feature")
	if st == nil || st.State != string(agent.StateEscalated) {
		t.Errorf("state after second invocation = %v, want escalated", stateOf(st))
	}
}

func TestEscalate_PartC_DedupScopedToFromSession(t *testing.T) {
	d := openPromptTestDB(t)
	captured := seedEscalatePair(t, d, "repo@feature", "repo@main")
	// Also seed a second worker in the same repo.
	seedSession(t, d, "repo@feature-2", "active", nil, nil, strPtr("worker"), nil)

	if err := runEscalateForSessionOpts(d, "repo@feature", "", "halp", escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
		t.Fatalf("first runEscalateForSessionOpts: %v", err)
	}
	captured.body = nil
	// A DIFFERENT worker sending the same prompt to the same coordinator
	// must NOT be deduped against the first worker's bus row.
	if err := runEscalateForSessionOpts(d, "repo@feature-2", "", "halp", escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
		t.Fatalf("second-worker runEscalateForSessionOpts: %v", err)
	}
	// Two distinct from_sessions → two bus rows total to repo@main.
	totalToCoord := busRowCount(t, d, "repo@feature", "repo@main") + busRowCount(t, d, "repo@feature-2", "repo@main")
	if totalToCoord != 2 {
		t.Errorf("cross-worker dedup leak: total bus rows to repo@main = %d, want 2", totalToCoord)
	}
	if captured.body == nil {
		t.Errorf("cross-worker invocation did not deliver")
	}
}

// ---------------------------------------------------------------------------
// Integration test using sidecartest.NewIsolated — verifies that a replayed
// `prism escalate` does not produce a second delivery to the target session.
// ---------------------------------------------------------------------------

func TestEscalate_PartC_ReplayDoesNotHitSidecar(t *testing.T) {
	// Use the prism-test@ session-name prefix per AGENTS.md isolation contract.
	target := "prism-test@coord-" + t.Name()
	bus := sidecartest.NewIsolated(t, target)

	// Seed a worker row in the same isolated DB.
	worker := "prism-test@worker-" + t.Name()
	if err := bus.DB.UpsertStatusWithRootAgent(worker, "prism-test", "/tmp/test-worker", "active", nil, nil, strPtr("worker"), nil); err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	if err := bus.DB.QueryRow("UPDATE agent_status SET harness = '' WHERE session_name = ? RETURNING 1", worker).Scan(new(int)); err != nil {
		t.Fatalf("clear worker harness: %v", err)
	}
	// Set the target's root_agent_name so resolveEscalationTarget picks it up.
	if err := bus.DB.QueryRow("UPDATE agent_status SET root_agent_name = 'coordinator' WHERE session_name = ? RETURNING 1", target).Scan(new(int)); err != nil {
		t.Fatalf("set target root_agent_name: %v", err)
	}

	// Point the cmd-package DB resolver at the isolated DB by setting the
	// test override and routing all openDB() calls there. Since runEscalateForSessionOpts
	// takes the *db.DB directly, we don't need to override openDB.

	const promptText = "stuck on review, please advise"

	if err := runEscalateForSessionOpts(bus.DB, worker, target, promptText, escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
		t.Fatalf("first runEscalateForSessionOpts: %v", err)
	}
	firstBodies := len(bus.CopyBodies())
	if firstBodies == 0 {
		t.Fatalf("first invocation did not deliver to sidecar; bodies=%v", bus.CopyBodies())
	}

	// Replay — expect NO new body delivered to the sidecar's HTTP server.
	if err := runEscalateForSessionOpts(bus.DB, worker, target, promptText, escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
		t.Fatalf("replay runEscalateForSessionOpts: %v", err)
	}
	secondBodies := len(bus.CopyBodies())
	if secondBodies != firstBodies {
		t.Errorf("replay produced new sidecar delivery: bodies grew from %d to %d", firstBodies, secondBodies)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func splitNonEmptyLines(s string) []string {
	out := []string{}
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

func stateOf(st *db.Status) string {
	if st == nil {
		return "<nil status>"
	}
	return st.State
}
