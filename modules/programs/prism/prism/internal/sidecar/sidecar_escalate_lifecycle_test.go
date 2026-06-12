package sidecar

// Regression tests for issue #2255 — sidecar coordination-channel death at
// `prism escalate`.
//
// Incident shape (2026-06-13, session nixos-config@chromium-iokit-rootdomain):
// the worker ran `prism escalate` mid-turn. The host-side child wrote the
// `escalated` state and delivered the escalation, but the very next agent-loop
// iteration emitted a turn_start frame whose unconditional active upsert
// clobbered the escalated state (escalated→active is a valid transition, so
// nothing flagged it). The finished debounce then saw `active`, wrote
// `finished`, and notifyCoordinator emitted the "has finished" notification
// the escalate contract requires to be suppressed. The session's DB row froze
// in `finished` at the escalate timestamp; the paused (frozen) row was later
// indistinguishable from a dead session.
//
// These tests drive the REAL inbound frame sequence a pi session produces
// around an escalate — including the same-turn turn_start that the
// pre-existing escalate tests omitted — and assert the post-escalate health
// contract from the issue's acceptance criteria:
//
//   - the escalated state survives the rest of the escalating turn (AC #4),
//   - the "has finished" notification stays suppressed while it holds (AC #4),
//   - agent events continue to be recorded after the escalate (AC #2),
//   - /prompt to the session succeeds in both steer and followUp modes (AC #3),
//   - an incoming prompt resumes the session: the next turn_start transitions
//     escalated→active per the documented contract,
//   - the no-coordinator (AC #6) and dedup-replay (AC #7) variants leave the
//     sidecar equally healthy.
//
// Isolation per #1608: every test calls sidecartest.NewIsolated, which
// redirects XDG_STATE_HOME to a tempdir, opens an isolated DB, and arms
// PRISM_TEST_MODE_RESTRICT_HOSTAPI so no host socket can be dialled.

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

const (
	escalateTestSession     = "prism-test@escalate-worker"
	escalateTestRepo        = "prism-test"
	escalateTestCoordinator = "prism-test@coordinator"
)

// notifyRecorder records notifyCoordinator delivery attempts via the
// notifyCoordinatorDeliverFn seam (#1856).
type notifyRecorder struct {
	mu    sync.Mutex
	calls []string // delivered text, in order
}

func (r *notifyRecorder) record(_ string, _ *db.Status, text string, _ func(string, *db.Status) map[string]any, _ string, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, text)
	return nil
}

func (r *notifyRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// newEscalateLifecycleSidecar builds an isolated socket-pipe sidecar wired
// with a fake test clock, an exit-0 stub prism binary for the host-API
// shell-outs (the real `prism escalate` child cannot run inside a unit
// test), and a notify recorder on the coordinator-delivery seam.
//
// When seedCoordinator is true, an active coordinator row for the same repo
// is seeded so a NON-suppressed finish path would attempt a delivery — making
// "recorder stayed empty" a real suppression signal rather than a
// no-coordinator silent skip.
func newEscalateLifecycleSidecar(t *testing.T, seedCoordinator bool) (*Sidecar, *testClock, *sidecartest.Bus, *notifyRecorder) {
	t.Helper()
	bus := sidecartest.NewIsolated(t, "")
	sockPath := shortSockPath(t)

	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	// The stub stands in for the host-side `prism escalate` child, which the
	// tests pair with an explicit UpsertStatus(escalated) on the test DB (the
	// same write the real child performs before exiting 0).
	stubScript := "#!/bin/sh\n" +
		"if [ \"$1\" = \"escalate\" ]; then\n" +
		"  echo 'prism escalate: OK delivered to " + escalateTestCoordinator + " (delivery_id=test)'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}

	if seedCoordinator {
		role := "coordinator"
		if err := bus.DB.UpsertStatusWithRootAgent(escalateTestCoordinator, escalateTestRepo, "/tmp/"+escalateTestCoordinator, "active", nil, nil, &role, nil); err != nil {
			t.Fatalf("seed coordinator: %v", err)
		}
	}

	clk := newTestClock()
	cfg := Config{
		SessionName:           escalateTestSession,
		Repo:                  escalateTestRepo,
		Worktree:              t.TempDir(),
		DB:                    bus.DB,
		Clock:                 clk,
		AgentRole:             "worker",
		HarnessName:           "pi",
		HarnessPipeSockPath:   sockPath,
		StartupConnectTimeout: 5 * time.Second,
		PipeReconnectTimeout:  200 * time.Millisecond,
		PrismBinaryPath:       stubPath,
		Harness:               pih.New("", "", ""),
	}
	sc := New(cfg)
	rec := &notifyRecorder{}
	sc.notifyCoordinatorDeliverFn = rec.record
	return sc, clk, bus, rec
}

// escalateOnTestSidecar performs the two halves of a worker-side escalate as
// production sequences them: the host-side child's DB transition to
// `escalated`, then the POST /escalate through the sidecar's own host-API
// handler (which arms the same-turn guard before the bash call returns).
func escalateOnTestSidecar(t *testing.T, sc *Sidecar) {
	t.Helper()
	if err := sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, string(agent.StateEscalated), nil, nil); err != nil {
		t.Fatalf("escalated transition (host child write): %v", err)
	}
	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", `{"prompt":"halp, need a decision"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("/escalate status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
}

// waitForEventPayload polls agent_events until a row for session whose
// payload contains substr appears, or fails the test after 2s. Used as a
// sequencing barrier: handlePipeFrame writes events synchronously, so once
// the row is visible every earlier frame has been fully processed.
func waitForEventPayload(t *testing.T, d *db.DB, session, substr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var n int
		if err := d.QueryRow(
			"SELECT COUNT(*) FROM agent_events WHERE session_name = ? AND payload LIKE ?",
			session, "%"+substr+"%",
		).Scan(&n); err != nil {
			t.Fatalf("query agent_events: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for agent_events payload containing %q", substr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sendBarrier sends a uniquely-tagged tool_call frame and waits for its
// agent_events row, guaranteeing all previously-sent frames are processed.
func sendBarrier(t *testing.T, conn net.Conn, sc *Sidecar, tag string) {
	t.Helper()
	sendJSON(t, conn, map[string]any{"type": "tool_call", "id": tag, "name": "bash", "args": map[string]any{"command": tag}})
	waitForEventPayload(t, sc.cfg.DB, sc.cfg.SessionName, tag)
}

// fireFinishedDebounce sends state_change{finished}, waits for the debounce
// timer to be registered, and fires it.
func fireFinishedDebounce(t *testing.T, conn net.Conn, clk *testClock) {
	t.Helper()
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})
	deadline := time.Now().Add(2 * time.Second)
	var timer *testTimer
	for {
		timer = clk.LastTimer()
		if timer != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no finished debounce timer created")
		}
		time.Sleep(20 * time.Millisecond)
	}
	timer.Fire()
}

// postPromptAndReadFrame POSTs /prompt to the sidecar's own session with the
// given deliver_as mode and asserts (a) HTTP 200 and (b) the prompt frame
// arrives on the pipe connection with the right mode — the full AC #3
// "prompt delivery succeeds" signal.
func postPromptAndReadFrame(t *testing.T, sc *Sidecar, conn net.Conn, deliverAs, text string) {
	t.Helper()
	body := fmt.Sprintf(`{"session":%q,"prompt":%q,"deliver_as":%q}`, sc.cfg.SessionName, text, deliverAs)
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("/prompt (%s) status = %d, body = %q, want 200", deliverAs, rr.Code, rr.Body.String())
	}
	frame := readJSON(t, conn)
	if frame["type"] != "prompt" {
		t.Fatalf("frame type = %v, want prompt", frame["type"])
	}
	if frame["deliver_as"] != deliverAs {
		t.Errorf("frame deliver_as = %v, want %s", frame["deliver_as"], deliverAs)
	}
	if frame["text"] != text {
		t.Errorf("frame text = %v, want %q", frame["text"], text)
	}
}

// TestEscalate_SameTurnFramesDoNotClobberEscalatedState is the core #2255
// regression test (AC #5). It reproduces the live incident's frame sequence:
//
//	turn_start → [bash: prism escalate] → turn_end(toolUse) → turn_start
//	→ msg_assistant → turn_end(stop) → state_change{finished}
//
// Pre-fix, the post-escalate turn_start clobbered `escalated` with `active`;
// the finished debounce then wrote `finished` and the coordinator received
// the "has finished" notification the escalate contract suppresses. Post-fix
// the escalated state survives the whole turn, the notification stays
// suppressed, and the session remains fully steerable.
func TestEscalate_SameTurnFramesDoNotClobberEscalatedState(t *testing.T) {
	sc, clk, _, rec := newEscalateLifecycleSidecar(t, true /* seedCoordinator */)
	wait := runSocketPipeSidecar(sc)
	conn, _ := dialAndHandshake(t, sc.cfg.HarnessPipeSockPath)

	// The escalating turn begins.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "active"})
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendBarrier(t, conn, sc, "barrier-pre-escalate")

	// Mid-turn: the bash tool runs `prism escalate` (host child writes the
	// escalated state; the proxy returns once the sidecar handler responds).
	escalateOnTestSidecar(t, sc)

	// The agent loop resumes: turn_end for the tool iteration, then the
	// next iteration's turn_start — the frame that clobbered the escalated
	// state in the incident.
	sendJSON(t, conn, map[string]any{"type": "turn_end"})
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "escalated; pausing for guidance"})
	sendJSON(t, conn, map[string]any{"type": "turn_end"})
	sendBarrier(t, conn, sc, "barrier-post-escalate")

	if st := getState(t, sc.cfg.DB, sc.cfg.SessionName); st != string(agent.StateEscalated) {
		t.Fatalf("after same-turn turn_start, state = %q, want %q (escalated clobbered — #2255 regression)",
			st, agent.StateEscalated)
	}

	// The turn ends cleanly; the finished debounce fires.
	fireFinishedDebounce(t, conn, clk)
	sc.WaitNotifies()

	if st := getState(t, sc.cfg.DB, sc.cfg.SessionName); st != string(agent.StateEscalated) {
		t.Errorf("after finished debounce, state = %q, want %q (suppression failed)", st, agent.StateEscalated)
	}
	if n := rec.count(); n != 0 {
		t.Errorf("coordinator received %d notification(s) while escalated, want 0 (escalate contract: session.escalated is the notification)", n)
	}

	// AC #2: agent events continue to be recorded after the escalate.
	sendBarrier(t, conn, sc, "barrier-events-still-recorded")

	// AC #3: prompt delivery to the escalated session succeeds in both modes.
	postPromptAndReadFrame(t, sc, conn, "steer", "coordinator guidance: proceed with option A")
	postPromptAndReadFrame(t, sc, conn, "followUp", "second instruction")

	// The incoming prompt cleared the same-turn guard: the turn it provokes
	// transitions escalated→active per the documented contract.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	deadline := time.Now().Add(2 * time.Second)
	for getState(t, sc.cfg.DB, sc.cfg.SessionName) != string(agent.StateActive) {
		if time.Now().After(deadline) {
			t.Fatalf("escalated did not clear on post-prompt turn_start; state=%q",
				getState(t, sc.cfg.DB, sc.cfg.SessionName))
		}
		time.Sleep(20 * time.Millisecond)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// TestEscalate_SessionShutdownWhileEscalated_SuppressesFinishNotification
// verifies the session_shutdown arm honours the escalate contract: a terminal
// exit while escalated may write `finished` (escalated→finished is a valid
// terminal transition) but must NOT emit the "has finished" notification —
// pre-fix the handler wrote finished first and notifyCoordinator's DB-read
// guard then saw `finished`, defeating the suppression.
func TestEscalate_SessionShutdownWhileEscalated_SuppressesFinishNotification(t *testing.T) {
	sc, _, _, rec := newEscalateLifecycleSidecar(t, true /* seedCoordinator */)
	wait := runSocketPipeSidecar(sc)
	conn, _ := dialAndHandshake(t, sc.cfg.HarnessPipeSockPath)

	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "active"})
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendBarrier(t, conn, sc, "barrier-pre-escalate")

	escalateOnTestSidecar(t, sc)

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Fatalf("runStartupSocketPipe returned error: %v", err)
	}
	sc.WaitNotifies()

	if st := getState(t, sc.cfg.DB, sc.cfg.SessionName); st != string(agent.StateFinished) {
		t.Errorf("after session_shutdown, state = %q, want %q (terminal exit is real)", st, agent.StateFinished)
	}
	if n := rec.count(); n != 0 {
		t.Errorf("coordinator received %d notification(s) for a shutdown while escalated, want 0", n)
	}
}

// TestEscalate_NoCoordinator_SidecarHealthyAfterEscalate covers AC #6: an
// escalate that finds no coordinator candidate still transitions the session
// to escalated (host-side child exits 0) and must leave the sidecar healthy —
// events recorded, prompt delivery functional, escalated state preserved.
func TestEscalate_NoCoordinator_SidecarHealthyAfterEscalate(t *testing.T) {
	sc, clk, _, rec := newEscalateLifecycleSidecar(t, false /* no coordinator */)
	wait := runSocketPipeSidecar(sc)
	conn, _ := dialAndHandshake(t, sc.cfg.HarnessPipeSockPath)

	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "active"})
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendBarrier(t, conn, sc, "barrier-pre-escalate")

	escalateOnTestSidecar(t, sc)

	sendJSON(t, conn, map[string]any{"type": "turn_end"})
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendBarrier(t, conn, sc, "barrier-post-escalate")

	if st := getState(t, sc.cfg.DB, sc.cfg.SessionName); st != string(agent.StateEscalated) {
		t.Fatalf("state = %q, want %q", st, agent.StateEscalated)
	}

	fireFinishedDebounce(t, conn, clk)
	sc.WaitNotifies()

	if st := getState(t, sc.cfg.DB, sc.cfg.SessionName); st != string(agent.StateEscalated) {
		t.Errorf("after finished debounce, state = %q, want %q", st, agent.StateEscalated)
	}
	if n := rec.count(); n != 0 {
		t.Errorf("unexpected notification attempts: %d", n)
	}

	// Sidecar healthy: events recorded, prompts deliverable.
	sendBarrier(t, conn, sc, "barrier-events-still-recorded")
	postPromptAndReadFrame(t, sc, conn, "steer", "human guidance after no-coordinator escalate")
	postPromptAndReadFrame(t, sc, conn, "followUp", "follow-up after no-coordinator escalate")

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// TestEscalate_DedupReplay_SidecarHealthy covers AC #7: a second escalate
// invocation within the dedup window (the host-side child short-circuits as a
// replay and exits 0) re-arms the same-turn guard idempotently and leaves the
// sidecar healthy.
func TestEscalate_DedupReplay_SidecarHealthy(t *testing.T) {
	sc, clk, _, rec := newEscalateLifecycleSidecar(t, true /* seedCoordinator */)
	wait := runSocketPipeSidecar(sc)
	conn, _ := dialAndHandshake(t, sc.cfg.HarnessPipeSockPath)

	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "active"})
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendBarrier(t, conn, sc, "barrier-pre-escalate")

	// First escalate.
	escalateOnTestSidecar(t, sc)
	sendJSON(t, conn, map[string]any{"type": "turn_end"})
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendBarrier(t, conn, sc, "barrier-between-escalates")

	// Replay within the window: the session is still escalated, the host
	// child short-circuits with exit 0 and no new writes. The handler arms
	// the guard again (idempotent).
	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", `{"prompt":"halp, need a decision"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("replay /escalate status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	sendJSON(t, conn, map[string]any{"type": "turn_end"})
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendBarrier(t, conn, sc, "barrier-post-replay")

	if st := getState(t, sc.cfg.DB, sc.cfg.SessionName); st != string(agent.StateEscalated) {
		t.Fatalf("after replay, state = %q, want %q", st, agent.StateEscalated)
	}

	fireFinishedDebounce(t, conn, clk)
	sc.WaitNotifies()

	if st := getState(t, sc.cfg.DB, sc.cfg.SessionName); st != string(agent.StateEscalated) {
		t.Errorf("after finished debounce, state = %q, want %q", st, agent.StateEscalated)
	}
	if n := rec.count(); n != 0 {
		t.Errorf("unexpected notification attempts: %d", n)
	}

	// Sidecar healthy after the replay.
	sendBarrier(t, conn, sc, "barrier-events-still-recorded")
	postPromptAndReadFrame(t, sc, conn, "steer", "guidance after replayed escalate")
	postPromptAndReadFrame(t, sc, conn, "followUp", "follow-up after replayed escalate")

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// TestEscalate_WaitingFrameDoesNotClobberEscalatedState verifies the
// state_change{waiting} arm is also covered by the same-turn guard: a
// permission prompt raised after the escalate (same turn) must not overwrite
// the escalated state (escalated→waiting is not even a valid transition; the
// pre-fix code applied it anyway since checkTransition is advisory).
func TestEscalate_WaitingFrameDoesNotClobberEscalatedState(t *testing.T) {
	sc, _, _, _ := newEscalateLifecycleSidecar(t, true /* seedCoordinator */)
	wait := runSocketPipeSidecar(sc)
	conn, _ := dialAndHandshake(t, sc.cfg.HarnessPipeSockPath)

	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "active"})
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendBarrier(t, conn, sc, "barrier-pre-escalate")

	escalateOnTestSidecar(t, sc)

	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "waiting"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "active"})
	sendBarrier(t, conn, sc, "barrier-post-waiting")

	if st := getState(t, sc.cfg.DB, sc.cfg.SessionName); st != string(agent.StateEscalated) {
		t.Errorf("after waiting/active churn, state = %q, want %q", st, agent.StateEscalated)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}
