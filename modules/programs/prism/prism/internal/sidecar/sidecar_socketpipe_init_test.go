package sidecar

// Tests for the socket-pipe startup init path.
//
// These tests exercise Sidecar.Run with a TransportSocketPipe harness and
// assert that the transport-agnostic init blocks (instance_id mint/load and
// merge-queue watcher start) run correctly for PI sessions, not just for
// HTTP-port sessions.

import (
	"context"
	"os"
	"testing"
	"time"

	pih "github.com/prismatic-koi/prism/internal/harness/pi"
)

// newSocketPipeCoordinatorSidecar creates a Sidecar configured for socket-pipe
// testing with AgentRole=coordinator and a @main session name. This matches
// a PI coordinator session.
func newSocketPipeCoordinatorSidecar(t *testing.T, sockPath string) *Sidecar {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	d := openTestDB(t)
	clk := newTestClock()
	cfg := Config{
		SessionName:           "testrepo@main",
		Repo:                  "testrepo",
		Worktree:              t.TempDir(),
		DB:                    d,
		Clock:                 clk,
		AgentRole:             "coordinator",
		HarnessName:           "pi",
		HarnessPipeSockPath:   sockPath,
		StartupConnectTimeout: 5 * time.Second,
		Harness:               pih.New("", "coordinator", ""),
	}
	return New(cfg)
}

// newSocketPipeWorkerSidecar creates a Sidecar configured for socket-pipe
// testing with AgentRole=worker. This matches a PI worker session.
func newSocketPipeWorkerSidecar(t *testing.T, sockPath string) *Sidecar {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	d := openTestDB(t)
	clk := newTestClock()
	cfg := Config{
		SessionName:           "testrepo@worker-branch",
		Repo:                  "testrepo",
		Worktree:              t.TempDir(),
		DB:                    d,
		Clock:                 clk,
		AgentRole:             "worker",
		HarnessName:           "pi",
		HarnessPipeSockPath:   sockPath,
		StartupConnectTimeout: 5 * time.Second,
		Harness:               pih.New("", "worker", ""),
	}
	return New(cfg)
}

// runSidecarRun starts sc.Run(ctx) in a goroutine and returns a function that
// waits for it to finish and returns its error.
func runSidecarRun(ctx context.Context, sc *Sidecar) func() error {
	errc := make(chan error, 1)
	go func() {
		errc <- sc.Run(ctx)
	}()
	return func() error {
		return <-errc
	}
}

// TestSocketPipeInit_CoordinatorInstanceIDAndWatcher verifies that after
// Sidecar.Run starts for a PI coordinator session (TransportSocketPipe):
//   - s.cfg.InstanceID is non-empty (instance_id was minted or loaded)
//   - s.mergeWatcherCancel is set (merge-queue watcher was started)
//
// This is the primary regression test for socket-pipe startup init: without
// the fix, both of these assertions fail because runStartupSocketPipe returns
// before the post-switch init block runs.
func TestSocketPipeInit_CoordinatorInstanceIDAndWatcher(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeCoordinatorSidecar(t, sockPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wait := runSidecarRun(ctx, sc)

	// Wait for the socket file to appear — proves Run() has entered
	// runStartupSocketPipe (that is, the transport-shape switch was reached and
	// the transport-agnostic init blocks before it have completed).
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket file never appeared — sidecar may not have dispatched to runStartupSocketPipe")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Assert instance_id is populated.
	sc.mu.Lock()
	instanceID := sc.cfg.InstanceID
	watcherCancel := sc.mergeWatcherCancel
	sc.mu.Unlock()

	if instanceID == "" {
		t.Error("s.cfg.InstanceID is empty after socket-pipe startup — instance_id mint/load did not run (#1437)")
	}

	if watcherCancel == nil {
		t.Error("s.mergeWatcherCancel is nil after socket-pipe coordinator startup — merge-queue watcher did not start (#1437)")
	}

	// Clean shutdown.
	conn, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()

	if err := wait(); err != nil {
		t.Errorf("Run() returned error: %v", err)
	}
}

// TestSocketPipeInit_WorkerInstanceIDNoWatcher verifies that after
// Sidecar.Run starts for a PI worker session (TransportSocketPipe):
//   - s.cfg.InstanceID is non-empty (instance_id was minted or loaded)
//   - s.mergeWatcherCancel is NOT set (merge-queue watcher is coordinator-only)
//
// PI worker sessions must get an instance_id (for identity) but must NOT
// start the merge-queue watcher (which is coordinator-only behaviour).
func TestSocketPipeInit_WorkerInstanceIDNoWatcher(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeWorkerSidecar(t, sockPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wait := runSidecarRun(ctx, sc)

	// Wait for the socket file to appear.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket file never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Assert instance_id is populated.
	sc.mu.Lock()
	instanceID := sc.cfg.InstanceID
	watcherCancel := sc.mergeWatcherCancel
	sc.mu.Unlock()

	if instanceID == "" {
		t.Error("s.cfg.InstanceID is empty after socket-pipe worker startup — instance_id mint/load did not run")
	}

	if watcherCancel != nil {
		t.Error("s.mergeWatcherCancel is set for a worker session — merge-queue watcher must not start for workers")
	}

	// Clean shutdown.
	conn, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()

	if err := wait(); err != nil {
		t.Errorf("Run() returned error: %v", err)
	}
}
