package sidecar

// Tests for #3345: a socket-pipe session after the reconnect timeout.
//
// If runStartupSocketPipe closes the pipe listener at the timeout, a
// respawned agent runs with no sidecar, and the reattach test fails. If a
// buffered review-complete prompt leaves the session in the active state
// that the review monitor wrote, or tells nobody, the detached tests fail.

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// startSocketPipe runs runStartupSocketPipe until the test ends. Unlike
// runSocketPipeSidecar it does not stop at the reconnect timeout.
func startSocketPipe(t *testing.T, sc *Sidecar) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sc.runStartupSocketPipe(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// waitDetached waits until the reconnect timeout has detached the session.
func waitDetached(t *testing.T, sc *Sidecar) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		sc.mu.Lock()
		detached := sc.harnessPipeDetached
		sc.mu.Unlock()
		if detached {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the reconnect timeout did not detach the session")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pipeClient is an agent connection that reads every frame through one
// buffered reader.
type pipeClient struct {
	conn net.Conn
	rd   *bufio.Reader
}

// dialHello connects to the pipe and sends hello. It does not read: the
// caller reads hello_ack and the frames after it with next.
func dialHello(t *testing.T, sockPath string) *pipeClient {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial the pipe: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	sendJSON(t, conn, map[string]any{
		"type": "hello", "protocol_version": 2, "harness": "pi", "harness_version": "0.1.0-test",
	})
	return &pipeClient{conn: conn, rd: bufio.NewReader(conn)}
}

// nextOfType returns the next frame of type typ, and skips other frames.
func (c *pipeClient) nextOfType(t *testing.T, typ string) map[string]any {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer c.conn.SetReadDeadline(time.Time{}) //nolint:errcheck
	for {
		line, err := c.rd.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read a frame of type %q: %v", typ, err)
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		if m["type"] == typ {
			return m
		}
	}
}

func countEvents(t *testing.T, d *db.DB, session, typ string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM agent_events WHERE session_name = ? AND type = ?`, session, typ).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", typ, err)
	}
	return n
}

// TestSocketPipe_ReconnectTimeout_NewAgentReattaches is the AC1 case: after
// the reconnect timeout, a new agent process connects to the same sidecar.
// Its events are recorded, and prompts reach it: a prompt held while no agent
// was connected, and a new prompt.
func TestSocketPipe_ReconnectTimeout_NewAgentReattaches(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, _ := newSocketPipeSidecarWithClock(t, sockPath)
	startSocketPipe(t, sc)

	// The first agent connects, then crashes.
	conn, _ := dialAndHandshake(t, sockPath)
	_ = conn.Close()
	waitForStateChangeNote(t, sc.cfg.DB, sc.cfg.SessionName, "reconnect timeout")
	waitDetached(t, sc)
	if got := getState(t, sc.cfg.DB, sc.cfg.SessionName); got != string(agent.StateError) {
		t.Fatalf("state after the reconnect timeout = %q, want %q", got, agent.StateError)
	}

	// A prompt while no agent is connected is held, not lost.
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"testrepo@main","prompt":"held while detached","delivery_id":"held-1"}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"buffered":true`) {
		t.Fatalf("POST /prompt while detached: status %d, body %s; want 200 and buffered", rr.Code, rr.Body.String())
	}

	// The respawned agent connects to the same sidecar.
	c := dialHello(t, sockPath)
	c.nextOfType(t, "hello_ack")
	held := c.nextOfType(t, "prompt")
	if held["text"] != "held while detached" || held["replay"] != true {
		t.Errorf("held prompt frame = %v, want the held text with replay=true", held)
	}
	waitForStateChangeNote(t, sc.cfg.DB, sc.cfg.SessionName, "agent reconnected")
	if got := getState(t, sc.cfg.DB, sc.cfg.SessionName); got != string(agent.StateActive) {
		t.Errorf("state after the agent reconnected = %q, want %q", got, agent.StateActive)
	}

	// Events of the new agent are recorded.
	sendJSON(t, c.conn, map[string]any{"type": "turn_start"})
	deadline := time.Now().Add(3 * time.Second)
	for countEvents(t, sc.cfg.DB, sc.cfg.SessionName, "turn_start") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the turn_start frame of the reconnected agent was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A new prompt is delivered at once.
	rr = doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"testrepo@main","prompt":"after reconnect","delivery_id":"live-1"}`)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "buffered") {
		t.Fatalf("POST /prompt after reconnect: status %d, body %s; want 200 and not buffered", rr.Code, rr.Body.String())
	}
	if p := c.nextOfType(t, "prompt"); p["text"] != "after reconnect" {
		t.Errorf("prompt frame after reconnect = %v, want text %q", p, "after reconnect")
	}
}

// TestSocketPipe_DisconnectWindow_PromptIsBuffered checks that a prompt sent
// in the reconnect window, with no agent connected, is buffered for the next
// agent. If the outbound channel stays set with no connection, /prompt
// reports a delivery and the writer drops the frame.
func TestSocketPipe_DisconnectWindow_PromptIsBuffered(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, _ := newSocketPipeSidecarWithClock(t, sockPath)
	sc.cfg.PipeReconnectTimeout = time.Minute
	getLogs := captureLogInto(sc)
	startSocketPipe(t, sc)

	conn, _ := dialAndHandshake(t, sockPath)
	_ = conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(getLogs(), "waiting up to") {
		if time.Now().After(deadline) {
			t.Fatal("the sidecar did not see the dropped connection")
		}
		time.Sleep(10 * time.Millisecond)
	}

	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"testrepo@main","prompt":"sent in the window","delivery_id":"window-1"}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"buffered":true`) {
		t.Fatalf("POST /prompt in the reconnect window: status %d, body %s; want 200 and buffered", rr.Code, rr.Body.String())
	}

	c := dialHello(t, sockPath)
	c.nextOfType(t, "hello_ack")
	if p := c.nextOfType(t, "prompt"); p["text"] != "sent in the window" {
		t.Errorf("prompt frame after reconnect = %v, want text %q", p, "sent in the window")
	}
}

// newDetachedWorkerSidecar returns a socket-pipe worker sidecar, with a
// coordinator in the same repo, whose first agent connected and then
// crashed, so the reconnect timeout has detached it. The returned function
// returns the coordinator notices that the sidecar sent.
func newDetachedWorkerSidecar(t *testing.T, worker string, reconnectTimeout time.Duration, detach bool) (*Sidecar, func() []string) {
	t.Helper()
	const repo = "prism-test"
	coordinator := "prism-test@main"
	bus := sidecartest.NewIsolated(t, coordinator)
	seedTestCoordinator(t, bus.DB, coordinator)
	seedTestWorker(t, bus.DB, worker, repo)

	sockPath := shortSockPath(t)
	sc := New(Config{
		SessionName:           worker,
		Repo:                  repo,
		Worktree:              t.TempDir(),
		DB:                    bus.DB,
		Clock:                 newTestClock(),
		AgentRole:             "worker",
		HarnessName:           "pi",
		HarnessPipeSockPath:   sockPath,
		StartupConnectTimeout: 5 * time.Second,
		PipeReconnectTimeout:  reconnectTimeout,
		IsolationMode:         config.IsolationMode("host"),
		Harness:               pih.New("", "", ""),
	})
	var (
		mu      sync.Mutex
		notices []string
	)
	sc.notifyCoordinatorDeliverFn = func(sessionName string, _ *db.Status, text string, _ func(string, *db.Status) map[string]any, _ string, _ string) error {
		if sessionName != coordinator {
			t.Errorf("notice sent to %q, want the coordinator %q", sessionName, coordinator)
		}
		mu.Lock()
		notices = append(notices, text)
		mu.Unlock()
		return nil
	}
	startSocketPipe(t, sc)
	conn, _ := dialAndHandshake(t, sockPath)
	_ = conn.Close()
	if detach {
		waitDetached(t, sc)
	}
	return sc, func() []string {
		sc.WaitNotifies()
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), notices...)
	}
}

// TestPrompt_Detached_ReviewCompleteKeepsErrorAndNotifiesCoordinator is the
// incident path of #3345, with no agent connected after the timeout. The
// review monitor writes active and then delivers the review-complete prompt.
// The session must show error again (AC2), and the coordinator must learn
// that the review result is held (AC3).
func TestPrompt_Detached_ReviewCompleteKeepsErrorAndNotifiesCoordinator(t *testing.T) {
	const worker = "prism-test@detached-review"
	sc, notices := newDetachedWorkerSidecar(t, worker, 150*time.Millisecond, true)
	d := sc.cfg.DB

	groupID, err := d.RegisterGroupWithPR(worker, "3344", 1)
	if err != nil {
		t.Fatalf("register group: %v", err)
	}
	if err := d.SetPendingDisagreement(worker, "a disagreement for the finish"); err != nil {
		t.Fatalf("seed disagreement: %v", err)
	}

	// The review monitor clears reviewing with a write of active.
	if err := d.UpsertStatus(worker, "prism-test", sc.cfg.Worktree, string(agent.StateActive), nil, nil); err != nil {
		t.Fatalf("write active: %v", err)
	}
	body, _ := json.Marshal(map[string]string{
		"session":     worker,
		"prompt":      "## Review complete: PR #3344 (round 1)",
		"source":      "review-complete",
		"delivery_id": review.RecoveryDeliveryID(groupID),
	})
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt", string(body))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"buffered":true`) {
		t.Fatalf("POST /prompt: status %d, body %s; want 200 and buffered", rr.Code, rr.Body.String())
	}

	if got := getState(t, d, worker); got != string(agent.StateError) {
		t.Errorf("state after the buffered review-complete prompt = %q, want %q", got, agent.StateError)
	}
	waitForStateChangeNote(t, d, worker, "no agent connected")

	got := notices()
	if len(got) != 1 {
		t.Fatalf("coordinator notices = %d (%q), want 1", len(got), got)
	}
	for _, want := range []string{
		"Agent " + worker + " is not connected to its sidecar",
		"review round 1 for PR #3344",
		"prism restore --session " + worker,
	} {
		if !strings.Contains(got[0], want) {
			t.Errorf("notice %q does not contain %q", got[0], want)
		}
	}
	if strings.Contains(got[0], "a disagreement for the finish") {
		t.Errorf("notice %q carries the pending disagreement; only a finish notification may take it", got[0])
	}
	if _, ok, err := d.ConsumePendingDisagreement(worker); err != nil || !ok {
		t.Errorf("pending disagreement after the notice: ok=%v err=%v, want it kept for the finish", ok, err)
	}

	// Another prompt while detached keeps the state, and sends no second
	// notice for the same review result.
	rr = doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"`+worker+`","prompt":"coordinator follow-up","delivery_id":"follow-1"}`)
	if !strings.Contains(rr.Body.String(), `"buffered":true`) {
		t.Fatalf("second POST /prompt body %s, want buffered", rr.Body.String())
	}
	if got := notices(); len(got) != 1 {
		t.Errorf("coordinator notices after a second prompt = %d, want still 1", len(got))
	}
}

// TestSocketPipe_ReconnectTimeout_NotifiesHeldReviewComplete covers a
// review-complete prompt that the sidecar buffered in the reconnect window.
// No notice is sent in the window, because the agent can still reconnect.
// The timeout sends the notice.
func TestSocketPipe_ReconnectTimeout_NotifiesHeldReviewComplete(t *testing.T) {
	const worker = "prism-test@window-review"
	sc, notices := newDetachedWorkerSidecar(t, worker, 500*time.Millisecond, false)

	groupID, err := sc.cfg.DB.RegisterGroupWithPR(worker, "77", 2)
	if err != nil {
		t.Fatalf("register group: %v", err)
	}
	// The window starts when the sidecar sees the drop; until then a prompt
	// is not buffered.
	body, _ := json.Marshal(map[string]string{
		"session": worker, "prompt": "## Review complete: PR #77 (round 2)",
		"source": "review-complete", "delivery_id": review.RecoveryDeliveryID(groupID),
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		sc.mu.Lock()
		dropped := sc.harnessPipeOutCh == nil
		sc.mu.Unlock()
		if dropped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sidecar did not see the dropped connection")
		}
		time.Sleep(5 * time.Millisecond)
	}
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt", string(body))
	if !strings.Contains(rr.Body.String(), `"buffered":true`) {
		t.Fatalf("POST /prompt in the window: body %s, want buffered", rr.Body.String())
	}
	if got := notices(); len(got) != 0 {
		t.Fatalf("coordinator notices in the reconnect window = %q, want none", got)
	}

	// The timeout sets the detached flag before it sends the notice, so
	// poll for the notice.
	waitDetached(t, sc)
	deadline = time.Now().Add(3 * time.Second)
	got := notices()
	for len(got) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		got = notices()
	}
	if len(got) != 1 || !strings.Contains(got[0], "review round 2 for PR #77") {
		t.Fatalf("coordinator notices after the timeout = %q, want one notice for PR #77 round 2", got)
	}
}
