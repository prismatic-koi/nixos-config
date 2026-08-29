package session

// Regression tests for the concurrent-spawn races in the spawn pipeline.
// This file covers the lost-prompt race:
//
//   The sidecar comes up cleanly (active), the harness handshake completes,
//   the agent connects — but the user --prompt never reaches the agent.
//   The session sits in idle state forever, prism spawn returns success,
//   prism list-sessions shows a normal-looking session, and only the
//   complete absence of agent activity hints something is wrong.
//
// WaitForReadyWithOpts provides the strict-mode readiness gate: when
// ReadinessOpts.RequirePromptDelivered is true, a bare state_change -> active
// (which fires on harness handshake even when the prompt is lost) is not
// sufficient evidence of a successful spawn. The gate additionally
// requires turn_start / msg_user / msg_assistant evidence that the agent is
// actually processing the prompt, OR a state_change to a non-"active"
// terminal state, OR harness_session_id set on agent_status.
//
// SpawnSession enables strict mode automatically when SpawnOpts.Prompt is
// non-empty.
//
// These tests are package-internal (`package session`, not session_test) so
// they can use spyTmuxBin from session_test.go. spyTmuxBin redirects tmux to
// a recording wrapper, so SpawnSession's tmux.NewSessionDetached calls do not
// hit the host's real tmux server — essential for deterministic testing on
// developer machines that already have prism sessions named like the test
// fixtures.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

func openLostPromptTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	SetTestDBPath(path)
	t.Cleanup(func() {
		d.Close()
		SetTestDBPath("")
	})
	return d
}

func seedLostPromptSession(t *testing.T, d *db.DB, sessionName string) {
	t.Helper()
	if err := d.UpsertStatusSeedRootAgentName(sessionName, "test", "/tmp", "idle", nil, nil, "worker", "", ""); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName(%q): %v", sessionName, err)
	}
}

// writeStateChangeFromGoroutine writes a state_change event from a goroutine
// the test owns. It does NOT call t.Fatalf — failures are dropped (best-effort)
// because Fatal'ing from a goroutine that may run after the test completes
// causes a panic. The tests only need the write to land best-effort; if it
// fails the gate trips on timeout, which the test will report.
func writeStateChangeFromGoroutine(d *db.DB, sessionName, state, idSuffix string) {
	evt := db.Event{
		ID:          "evt-state-" + idSuffix,
		SessionName: sessionName,
		Repo:        "test",
		Worktree:    "/tmp",
		Type:        "state_change",
		Payload:     `{"state":"` + state + `"}`,
	}
	_ = d.WriteEvent(evt)
}

func writeTurnStartFromGoroutine(d *db.DB, sessionName string) {
	evt := db.Event{
		ID:          "evt-ts",
		SessionName: sessionName,
		Repo:        "test",
		Worktree:    "/tmp",
		Type:        "turn_start",
		Payload:     `{}`,
	}
	_ = d.WriteEvent(evt)
}

func writeStateChange(t *testing.T, d *db.DB, sessionName, state, idSuffix string) {
	t.Helper()
	evt := db.Event{
		ID:          "evt-state-" + idSuffix,
		SessionName: sessionName,
		Repo:        "test",
		Worktree:    "/tmp",
		Type:        "state_change",
		Payload:     `{"state":"` + state + `"}`,
	}
	if err := d.WriteEvent(evt); err != nil {
		t.Fatalf("WriteEvent state_change: %v", err)
	}
}

func writeTurnStart(t *testing.T, d *db.DB, sessionName string) {
	t.Helper()
	evt := db.Event{
		ID:          "evt-ts",
		SessionName: sessionName,
		Repo:        "test",
		Worktree:    "/tmp",
		Type:        "turn_start",
		Payload:     `{}`,
	}
	if err := d.WriteEvent(evt); err != nil {
		t.Fatalf("WriteEvent turn_start: %v", err)
	}
}

// TestWaitForReadyWithOpts_StrictMode_BareActiveDoesNotSatisfyGate is the
// primary deterministic regression test for the lost-prompt race. A bare
// "state_change -> active" event must not satisfy the strict gate: a
// lost-prompt spawn would otherwise return success even when the agent never
// processed the prompt. Strict-mode WaitForReadyWithOpts treats bare active as
// insufficient evidence and waits for prompt-processing evidence (turn_start /
// msg_user / msg_assistant) or a terminal transition.
//
// The test seeds exactly the post-handshake state the issue's symptom-2
// reproduction left behind ("state -> active" event present, no agent
// activity afterwards) and asserts the strict gate trips on timeout
// rather than returning success.
func TestWaitForReadyWithOpts_StrictMode_BareActiveDoesNotSatisfyGate(t *testing.T) {
	d := openLostPromptTestDB(t)
	const sess = "myrepo@lost-prompt-strict-bare"
	seedLostPromptSession(t, d, sess)

	// Replicate the issue's symptom-2 sidecar log post-condition: the
	// handshake completed and the sidecar wrote state -> active.
	writeStateChange(t, d, sess, "active", "1")

	const timeout = 400 * time.Millisecond
	start := time.Now()
	err := WaitForReadyWithOpts(d, sess, ReadinessOpts{
		Timeout:                timeout,
		RequirePromptDelivered: true,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("WaitForReadyWithOpts (strict): got nil, want *ReadinessTimeoutError — bare active should NOT satisfy the strict gate")
	}
	if !IsReadinessTimeout(err) {
		t.Errorf("WaitForReadyWithOpts (strict) error = %v (type %T), want *ReadinessTimeoutError", err, err)
	}
	if elapsed < timeout {
		t.Errorf("WaitForReadyWithOpts (strict) returned in %v, expected at least %v (gate must wait the full timeout when only bare active is present)", elapsed, timeout)
	}
}

// TestWaitForReadyWithOpts_StrictMode_TurnStartSatisfiesGate verifies that
// a turn_start event (the canonical evidence the agent has received the
// prompt and entered the turn loop) is sufficient to satisfy the strict
// gate even when only a bare "active" state_change has been seen.
func TestWaitForReadyWithOpts_StrictMode_TurnStartSatisfiesGate(t *testing.T) {
	d := openLostPromptTestDB(t)
	const sess = "myrepo@lost-prompt-happy"
	seedLostPromptSession(t, d, sess)

	writeStateChange(t, d, sess, "active", "1")
	writeTurnStart(t, d, sess)

	if err := WaitForReadyWithOpts(d, sess, ReadinessOpts{
		Timeout:                2 * time.Second,
		RequirePromptDelivered: true,
	}); err != nil {
		t.Errorf("WaitForReadyWithOpts (strict): got %v, want nil — turn_start should satisfy the strict gate", err)
	}
}

// TestWaitForReadyWithOpts_StrictMode_HarnessSessionIDSatisfiesGate verifies
// that harness_session_id non-NULL satisfies the strict gate. the agent emits
// session.created → sidecar writes harness_session_id when it has parsed
// --prompt and accepted the message; for the agent CLI-prompt mode this
// is on its own evidence the prompt landed.
func TestWaitForReadyWithOpts_StrictMode_HarnessSessionIDSatisfiesGate(t *testing.T) {
	d := openLostPromptTestDB(t)
	const sess = "myrepo@lost-prompt-hsid"
	seedLostPromptSession(t, d, sess)

	if err := d.UpdateHarnessSessionID(sess, "ses_xyz"); err != nil {
		t.Fatalf("UpdateHarnessSessionID: %v", err)
	}

	if err := WaitForReadyWithOpts(d, sess, ReadinessOpts{
		Timeout:                2 * time.Second,
		RequirePromptDelivered: true,
	}); err != nil {
		t.Errorf("WaitForReadyWithOpts (strict): got %v, want nil — harness_session_id should satisfy the strict gate", err)
	}
}

// TestWaitForReadyWithOpts_StrictMode_TerminalStateChangeSatisfiesGate
// verifies that a state_change to a non-"active" state (finished /
// interrupted / error) satisfies the strict gate. These transitions only
// fire after the agent has processed work (or hit a real failure), so they
// are unambiguous progress evidence — even without a turn_start event.
func TestWaitForReadyWithOpts_StrictMode_TerminalStateChangeSatisfiesGate(t *testing.T) {
	cases := []string{"finished", "interrupted", "error", "idle"}
	for _, terminalState := range cases {
		terminalState := terminalState
		t.Run(terminalState, func(t *testing.T) {
			d := openLostPromptTestDB(t)
			sess := "myrepo@lost-prompt-" + terminalState
			seedLostPromptSession(t, d, sess)

			writeStateChange(t, d, sess, "active", "1")
			writeStateChange(t, d, sess, terminalState, "2")

			if err := WaitForReadyWithOpts(d, sess, ReadinessOpts{
				Timeout:                2 * time.Second,
				RequirePromptDelivered: true,
			}); err != nil {
				t.Errorf("WaitForReadyWithOpts (strict, terminal=%s): got %v, want nil", terminalState, err)
			}
		})
	}
}

// TestWaitForReadyWithOpts_LooseMode_BareActiveStillSatisfies verifies that
// loose-mode WaitForReady still returns success when
// only a bare active event is present. This is the path used by callers
// that do not deliver a prompt (or that gate the prompt question downstream);
// the review-fan-out tests rely on this shape.
func TestWaitForReadyWithOpts_LooseMode_BareActiveStillSatisfies(t *testing.T) {
	d := openLostPromptTestDB(t)
	const sess = "myrepo@loose-bare-active"
	seedLostPromptSession(t, d, sess)

	writeStateChange(t, d, sess, "active", "1")

	if err := WaitForReadyWithOpts(d, sess, ReadinessOpts{
		Timeout:                2 * time.Second,
		RequirePromptDelivered: false,
	}); err != nil {
		t.Errorf("WaitForReadyWithOpts (loose): got %v, want nil — bare active should still satisfy the loose gate (legacy semantics)", err)
	}
	// And the package-level WaitForReady wrapper must keep this behaviour
	// (review fan-out and other callers depend on it).
	if err := WaitForReady(d, sess, 2*time.Second); err != nil {
		t.Errorf("WaitForReady (legacy): got %v, want nil", err)
	}
}

// TestSpawnSession_LostPromptRace_StrictGateFiresAndCleansUp is the
// end-to-end deterministic regression test for the lost-prompt race.
// It uses the same shape as
// TestSpawnSession_ReadinessTimeout_FiresAndCleansUp but with the precise
// post-handshake state the lost-prompt reproduction leaves behind:
// a state_change -> active event written by the sidecar (the harness
// handshake completed), but no further agent progress.
//
// The loose gate returns success here: a bare active satisfies WaitForReady,
// and the spawn driver says "session created" while the agent silently sits
// idle. The strict gate treats this as a readiness-timeout failure, cleans up
// the half-alive state, and
// surfaces *ReadinessTimeoutError to the caller.
func TestSpawnSession_LostPromptRace_StrictGateFiresAndCleansUp(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@lost-prompt-spawn"
	opts := SpawnOpts{
		SessionName:   sessionName,
		Repo:          "myrepo",
		Worktree:      "/worktrees/myrepo-lost",
		AgentRole:     "worker",
		Prompt:        "do the thing",
		Layout:        LayoutAgentOnly,
		IsolationMode: "host",
		HarnessName:   "pi",
		// Short timeout so the test runs quickly; the gate trips because
		// only a bare-active state_change will arrive (no turn_start).
		ReadinessTimeout: 600 * time.Millisecond,
		PIExtensionDir:   testPIExtensionDir,
	}

	// Inject the symptom-2 precondition partway through the wait: the
	// handshake completes and the sidecar writes state -> active, but no
	// turn_start ever arrives because the prompt was lost. We wrap in
	// best-effort writes (no t.Fatalf in goroutine) to avoid the post-test
	// goroutine-panic class.
	go func() {
		time.Sleep(100 * time.Millisecond)
		writeStateChangeFromGoroutine(d, sessionName, "active", "lost-1")
	}()

	start := time.Now()
	err := SpawnSession(d, opts)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("SpawnSession: got nil, want *ReadinessTimeoutError — bare active without prompt-delivered evidence must trip the strict gate")
	}
	if !IsReadinessTimeout(err) {
		t.Errorf("SpawnSession error = %v (type %T), want *ReadinessTimeoutError", err, err)
	}
	if !strings.Contains(err.Error(), "not ready within") {
		t.Errorf("SpawnSession error = %q, want substring %q", err.Error(), "not ready within")
	}
	if elapsed < 600*time.Millisecond {
		t.Errorf("SpawnSession returned in %v, expected at least 600ms (the readiness window)", elapsed)
	}

	// Cleanup verification: the half-alive session must be torn
	// down so a re-spawn does not see stale state.
	st, _ := d.CurrentStatus(sessionName)
	if st != nil && st.EndedAt == nil {
		t.Errorf("agent_status row %q is alive after lost-prompt cleanup; ended_at should be set", sessionName)
	}
}

// TestSpawnSession_LostPromptRace_TurnStartUnblocksGate is the happy path:
// when the agent actually receives the prompt and emits turn_start within
// the readiness window, SpawnSession returns success.
func TestSpawnSession_LostPromptRace_TurnStartUnblocksGate(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@happy-prompt-spawn"
	opts := SpawnOpts{
		SessionName:   sessionName,
		Repo:          "myrepo",
		Worktree:      "/worktrees/myrepo-happy",
		AgentRole:     "worker",
		Prompt:        "do the thing",
		Layout:        LayoutAgentOnly,
		IsolationMode: "host",
		HarnessName:   "pi",
		// Generous timeout; the goroutine below will write turn_start
		// well before it expires.
		ReadinessTimeout: 5 * time.Second,
		PIExtensionDir:   testPIExtensionDir,
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		writeStateChangeFromGoroutine(d, sessionName, "active", "happy-1")
		time.Sleep(50 * time.Millisecond)
		writeTurnStartFromGoroutine(d, sessionName)
	}()

	if err := SpawnSession(d, opts); err != nil {
		t.Errorf("SpawnSession (happy path with turn_start): got %v, want nil", err)
	}

	st, _ := d.CurrentStatus(sessionName)
	if st == nil {
		t.Fatal("CurrentStatus: nil after successful spawn")
	}
	if st.EndedAt != nil {
		t.Errorf("agent_status.ended_at is set on a successful spawn — cleanup ran when it shouldn't")
	}
}

// TestSpawnSession_NoPrompt_LayoutAgentOnly_Rejected verifies the layer-4
// empty-prompt guard: an agent-only layout requires a prompt. Without the
// guard, this combination silently produces a session that comes up
// successfully on every observable surface but sits idle forever because no
// prompt was ever delivered. SpawnSession refuses the spawn upfront.
func TestSpawnSession_NoPrompt_LayoutAgentOnly_Rejected(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@no-prompt-rejected"
	opts := SpawnOpts{
		SessionName: sessionName,
		Repo:        "myrepo",
		Worktree:    "/worktrees/myrepo-noprompt",
		AgentRole:   "worker",
		// Prompt deliberately empty.
		Layout:           LayoutAgentOnly,
		IsolationMode:    "host",
		HarnessName:      "pi",
		ReadinessTimeout: 2 * time.Second,
		PIExtensionDir:   testPIExtensionDir,
	}

	err := SpawnSession(d, opts)
	if err == nil {
		t.Fatal("SpawnSession (empty Prompt, LayoutAgentOnly): got nil, want rejection per issue #1891")
	}
	if !strings.Contains(err.Error(), "Prompt is required") {
		t.Errorf("error %q does not mention 'Prompt is required'", err.Error())
	}
	// Nothing should have been written to the DB: refusal must happen
	// before any side-effects.
	if st, _ := d.CurrentStatus(sessionName); st != nil {
		t.Errorf("agent_status row created despite empty-prompt rejection: %+v", st)
	}
}

// TestReadinessOpts_ZeroTimeoutFallsBack verifies that a zero or negative
// Timeout in ReadinessOpts falls back to DefaultReadinessTimeout.
func TestReadinessOpts_ZeroTimeoutFallsBack(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in short mode")
	}
	d := openLostPromptTestDB(t)
	const sess = "myrepo@zero-opts"
	seedLostPromptSession(t, d, sess)

	go func() {
		time.Sleep(150 * time.Millisecond)
		writeStateChangeFromGoroutine(d, sess, "active", "1")
	}()

	start := time.Now()
	err := WaitForReadyWithOpts(d, sess, ReadinessOpts{
		Timeout: 0,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("WaitForReadyWithOpts (zero timeout): %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("WaitForReadyWithOpts (zero timeout) took %v — expected fallback to DefaultReadinessTimeout, not no-op", elapsed)
	}
}

func TestReadinessOpts_RequiresDB(t *testing.T) {
	if err := WaitForReadyWithOpts(nil, "x", ReadinessOpts{Timeout: time.Second}); err == nil {
		t.Error("WaitForReadyWithOpts(nil, …): got nil, want error")
	}
}

func TestReadinessOpts_RequiresSessionName(t *testing.T) {
	d := openLostPromptTestDB(t)
	if err := WaitForReadyWithOpts(d, "", ReadinessOpts{Timeout: time.Second}); err == nil {
		t.Error("WaitForReadyWithOpts(_, \"\"): got nil, want error")
	}
}

// errors.Is/As parity check — IsReadinessTimeout must keep working through
// the new entry point's error returns. Defence-in-depth against any future
// refactor that wraps the error differently.
func TestWaitForReadyWithOpts_TimeoutErrorIsTypedConsistently(t *testing.T) {
	d := openLostPromptTestDB(t)
	const sess = "myrepo@timeout-typed"
	seedLostPromptSession(t, d, sess)

	err := WaitForReadyWithOpts(d, sess, ReadinessOpts{
		Timeout:                100 * time.Millisecond,
		RequirePromptDelivered: true,
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var rte *ReadinessTimeoutError
	if !errors.As(err, &rte) {
		t.Errorf("errors.As(*ReadinessTimeoutError) = false, want true (err=%v type=%T)", err, err)
	}
	if !IsReadinessTimeout(err) {
		t.Errorf("IsReadinessTimeout = false, want true")
	}
}
