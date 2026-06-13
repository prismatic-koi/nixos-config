package sidecar

// monitor_recovery_race_test.go — F14 integration test for issue #1885.
//
// Regression guard: drives MonitorFunc (the happy-path delivery path) and
// DeliverGroupResults (the recovery-watcher delivery path) against the same
// sidecar instance under a race condition and asserts DeliverPrompt was
// called exactly once.
//
// Root cause being tested: before the F2 fix, MonitorFunc called
// promptdelivery.DeliverToSession with an empty deliveryID, minting a fresh
// UUID per call. DeliverGroupResults called DeliverToSessionWithID with
// deliveryID = RecoveryDeliveryID(groupID) — a deterministic ID. The sidecar's
// /prompt dedup set only drops repeats with the same delivery_id, so when
// both paths delivered for the same group the dedup did not fire and two
// review-complete prompts were forwarded to PI.
//
// After the F2 fix, MonitorFunc also uses RecoveryDeliveryID(groupID). The
// dedup set then fires on the second delivery and drops it, producing exactly
// one frame on the pipe channel.
//
// This test fails before the F2 fix and passes after. Verify with:
//
//	git stash    # stashes just the monitor.go change
//	go test ./internal/sidecar/ -run TestMonitorAndRecovery_ExactlyOneDelivery
//	git stash pop

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// startWorkerSidecarWithSocket builds a worker Sidecar, binds its host-API
// Unix socket (at session.SidecarHostAPIPath), and starts the http.Server on
// it. Returns the sidecar and a cleanup function that shuts the server down.
//
// The caller must call sidecartest.NewIsolated (or t.Setenv XDG_STATE_HOME)
// before calling this function so that the socket path is inside the tempdir.
func startWorkerSidecarWithSocket(t *testing.T, workerSession string, d *db.DB, pipeCh chan []byte) (*Sidecar, func()) {
	t.Helper()

	sockPath, err := session.SidecarHostAPIPath(workerSession)
	if err != nil {
		t.Fatalf("resolve host-API socket path for %q: %v", workerSession, err)
	}
	if err := os.MkdirAll(fmt.Sprintf("%s", sockPath[:len(sockPath)-len("/hostapi.sock")]), 0o700); err != nil {
		t.Fatalf("create socket dir: %v", err)
	}

	cfg := Config{
		SessionName:     workerSession,
		Repo:            "prism-test",
		Worktree:        "/tmp/worktree",
		DB:              d,
		Clock:           newTestClock(),
		AgentRole:       "worker",
		HarnessName:     "pi",
		Harness:         pih.New("", "", ""),
		HostAPISockPath: sockPath,
		// Negative interval disables the goroutine-based recovery watcher;
		// we drive it explicitly via ReviewRecoveryTickForTest.
		ReviewRecoveryInterval: -1,
	}
	sc := New(cfg)

	// Set reviewingInFlight so the /prompt handler takes the same-session
	// socket-pipe path (TransportSocketPipe) and routes through DeliverPrompt.
	sc.mu.Lock()
	sc.reviewingInFlight = true
	sc.harnessPipeOutCh = pipeCh
	sc.mu.Unlock()

	// Bind the Unix socket and start the host-API server.
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	srv := &http.Server{Handler: sc.hostAPIHandler()}
	go func() { _ = srv.Serve(ln) }()

	cleanup := func() {
		_ = srv.Close()
		_ = ln.Close()
	}
	return sc, cleanup
}

// setupMonitorRecoveryFixture seeds the DB with a worker session and a
// completed review group ready for delivery. It mirrors setupStuckReviewGroup
// but is self-contained so the test can also invoke MonitorFunc directly.
func setupMonitorRecoveryFixture(t *testing.T) (d *db.DB, workerSession, groupID string, agentSessions []string) {
	t.Helper()

	workerSession = "prism-test@worker-monitor-recovery"
	_ = sidecartest.NewIsolated(t, workerSession) // sets XDG_STATE_HOME, PRISM_TEST_MODE_RESTRICT_HOSTAPI

	// Re-open DB using the path NewIsolated set up. NewIsolated also seeds
	// the worker row and starts its own socket listener — but we want a
	// different socket (the sidecar's) so we'll use the same DB.
	// Retrieve it from the bus.
	bus := sidecartest.NewIsolated(t, workerSession)
	d = bus.DB

	// Flip the worker to harness='pi' and state='reviewing'.
	if err := d.QueryRow(
		`UPDATE agent_status SET harness = 'pi', state = 'reviewing' WHERE session_name = ? RETURNING session_name`,
		workerSession,
	).Scan(new(string)); err != nil {
		t.Fatalf("set harness=pi, state=reviewing: %v", err)
	}

	const prNumber = "1885"
	const round = 1
	var err error
	groupID, err = d.RegisterGroupWithPR(workerSession, prNumber, round)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}

	agentRoles := []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"}
	agentSessions = make([]string, len(agentRoles))
	for i, role := range agentRoles {
		sessName := fmt.Sprintf("%s~review-%d-%s", workerSession, round, role)
		agentSessions[i] = sessName
		if err := d.UpsertStatus(sessName, "prism-test", "/tmp/worktree", "finished", nil, nil); err != nil {
			t.Fatalf("upsert agent_status for %s: %v", sessName, err)
		}
		if err := d.SetGroupID(sessName, groupID); err != nil {
			t.Fatalf("SetGroupID for %s: %v", sessName, err)
		}
		payload, _ := json.Marshal(map[string]string{
			"text": fmt.Sprintf("<summary>%s: ok</summary>\n<verdict>PASS</verdict>", role),
		})
		evt := db.Event{
			ID:          fmt.Sprintf("evt-monrec-%s", role),
			SessionName: sessName,
			Repo:        "prism-test",
			Worktree:    "/tmp/worktree",
			Type:        "msg_assistant",
			Payload:     string(payload),
			CreatedAt:   time.Now(),
		}
		if err := d.WriteEvent(evt); err != nil {
			t.Fatalf("WriteEvent for %s: %v", sessName, err)
		}
	}

	// Sanity: group should be complete.
	if done, err := d.GroupCompleted(groupID); err != nil || !done {
		t.Fatalf("GroupCompleted = %v, err = %v; want true/nil", done, err)
	}

	return d, workerSession, groupID, agentSessions
}

// TestMonitorAndRecovery_ExactlyOneDelivery is the F14 integration test.
//
// It starts a real Sidecar with its host-API Unix socket, then concurrently
// drives MonitorFunc (the happy-path monitor process) and DeliverGroupResults
// (the recovery watcher's delivery primitive) at the same sidecar. After
// both calls complete, the test asserts that exactly one frame was forwarded
// to the PI extension (i.e. the pipe channel received exactly one entry).
//
// Before the F2 fix: MonitorFunc used a random UUID delivery_id, so the
// sidecar's dedup set did not recognise the second delivery as a repeat →
// two frames were forwarded → this test failed.
//
// After the F2 fix: both MonitorFunc and DeliverGroupResults use
// RecoveryDeliveryID(groupID) → the second delivery is deduped → one frame.
func TestMonitorAndRecovery_ExactlyOneDelivery(t *testing.T) {
	d, workerSession, groupID, agentSessions := setupMonitorRecoveryFixture(t)

	// Pipe channel: counts how many frames the sidecar forwards to PI.
	pipeCh := make(chan []byte, 32)

	sc, cleanup := startWorkerSidecarWithSocket(t, workerSession, d, pipeCh)
	defer cleanup()
	_ = sc // used implicitly via the socket

	// Allow the socket to become ready.
	time.Sleep(20 * time.Millisecond)

	// Build MonitorOpts using the same DB and session details.
	agents := make([]review.Agent, len(agentSessions))
	for i, sess := range agentSessions {
		// Extract agent name from session suffix.
		parts := strings.SplitN(sess, "~review-1-", 2)
		if len(parts) == 2 {
			agents[i] = review.Agent{Name: parts[1]}
		} else {
			agents[i] = review.Agent{Name: sess}
		}
	}

	opts := review.MonitorOpts{
		GroupID:              groupID,
		WorkerSession:        workerSession,
		PRNumber:             "1885",
		Round:                1,
		Agents:               agents,
		AgentSessions:        agentSessions,
		DBPath:               d.Path(),
		PollInterval:         1 * time.Millisecond,
		MaxDeliveryRetries:   0,
		DeliveryRetryBackoff: 1 * time.Millisecond,
	}

	// Race MonitorFunc and DeliverGroupResults concurrently. Both target the
	// same sidecar socket. The order doesn't matter: whichever wins, the
	// second is deduped by the sidecar's /prompt handler.
	var wg sync.WaitGroup
	var monitorErr, recoveryErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		monitorErr = review.MonitorFunc(opts)
	}()
	go func() {
		defer wg.Done()
		_, recoveryErr = review.DeliverGroupResults(d, groupID, review.RecoveryDeliveryID(groupID))
	}()
	wg.Wait()

	// Both paths are expected to return nil or a dedup-related non-fatal error.
	// The recovery path returns an error when the monitor already delivered
	// (the sidecar responded 200 with {"replayed":true}), but DeliverGroupResults
	// treats a non-2xx response as an error. A 200 {"replayed":true} is still
	// 200, so it should succeed.
	// Log errors for visibility but do not fail the test on delivery errors —
	// the key assertion is the frame count.
	if monitorErr != nil {
		t.Logf("MonitorFunc error (may be non-fatal): %v", monitorErr)
	}
	if recoveryErr != nil {
		t.Logf("DeliverGroupResults error (may be non-fatal): %v", recoveryErr)
	}

	// Give the goroutines a brief settling window.
	time.Sleep(50 * time.Millisecond)

	// Count frames forwarded to PI. We only care about the prompt-frame
	// delivery here; the sidecar may legitimately emit other control frames
	// (e.g. reviewing_state on the in-memory flag flip after delivery, #2050)
	// alongside the prompt, which are unrelated to the dedup behaviour under
	// test. Filter to prompt frames only so the assertion remains stable.
	var promptFrames [][]byte
	var allFrames [][]byte
drainLoop:
	for {
		select {
		case f := <-pipeCh:
			allFrames = append(allFrames, f)
			if strings.Contains(string(f), "\"type\":\"prompt\"") {
				promptFrames = append(promptFrames, f)
			}
		default:
			break drainLoop
		}
	}

	if len(promptFrames) != 1 {
		t.Errorf("monitor + recovery race: got %d prompt frame(s) forwarded to PI, want exactly 1 (all frames: %d)\n"+
			"Before the F2 fix this would be 2 (different delivery_ids).\n"+
			"After the F2 fix both paths use RecoveryDeliveryID(%s) and the sidecar dedup drops the second.",
			len(promptFrames), len(allFrames), groupID)
	}
}
