package iris

// session_kill_e2e_test.go — end-to-end integration test for the full
// session_kill wire path (issue #1674).
//
// Flow:
//
//   1. Start a real ClientSocket + a real Supervisor against a `/bin/sleep`
//      child (acting as a stand-in for pi).
//   2. Open a client connection to the daemon socket.
//   3. Send a session_kill frame.
//   4. Assert: session_killed ack arrives, the underlying process is gone
//      (cannot signal pid 0), the session DB row carries end_state set.
//
// This is the integration AC: "spawn a session, call session_kill via the
// client socket, and assert pi is no longer running and the session state
// is finished in the DB."

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSessionKill_E2E_KillsRealProcess(t *testing.T) {
	// Short-prefix tempdir so the harness socket path fits in sun_path.
	shortPrefix, err := os.MkdirTemp("", "iris-e2e-kill-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })

	dbPath := filepath.Join(shortPrefix, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	// Build a script that just sleeps — enough to stand in for pi.
	script := writeShellScript(t, "exec sleep 60\n")

	// Spawn the supervisor.
	cfg := SupervisorConfig{
		SessionName:     "iris-e2e-kill@test",
		Worktree:        shortPrefix,
		Role:            "worker",
		PIBinaryPath:    script,
		RunDir:          shortPrefix,
		LogDir:          filepath.Join(shortPrefix, "logs"),
		Database:        database,
		ShutdownTimeout: 100 * time.Millisecond,
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	supCtx, supCancel := context.WithCancel(context.Background())
	t.Cleanup(supCancel)
	go sup.Start(supCtx)

	// Wait until the supervisor is active and capture the pid.
	if !waitForState(sup, StateActive, 5*time.Second) {
		t.Fatalf("supervisor never reached active (state=%s)", sup.State())
	}
	sup.mu.Lock()
	proc := sup.process
	sup.mu.Unlock()
	if proc == nil {
		t.Fatal("supervisor reported active but process handle was nil")
	}
	pid := proc.Pid

	// Build a single-session ClientSocket that knows about this supervisor.
	sockPath := filepath.Join(shortPrefix, "iris.sock")
	var mu sync.Mutex
	cs := NewClientSocket(ClientSocketConfig{
		SockPath: sockPath,
		Database: database,
		GetActiveSessions: func() []SessionSnapshot {
			mu.Lock()
			defer mu.Unlock()
			rec := sup.SessionRecord()
			return []SessionSnapshot{{
				Name:       rec.SessionName,
				InstanceID: rec.InstanceID,
				State:      string(rec.State),
				Role:       rec.Role,
				Worktree:   rec.Worktree,
			}}
		},
		KillSession: func(killCtx context.Context, name string, timeout time.Duration) (string, error) {
			if name != cfg.SessionName {
				t.Fatalf("unexpected session name in KillSession: %q", name)
			}
			prior := sup.State()
			terminal, err := sup.Kill(killCtx, timeout)
			if err != nil {
				return "", err
			}
			if prior == StateFinished || prior == StateError {
				return "already_terminal", nil
			}
			return string(terminal), nil
		},
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("ClientSocket.Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	csCtx, csCancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(csCancel)
	go cs.Serve(csCtx)

	// Dial the client socket and send the kill frame.
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial client socket: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	killFrame := map[string]any{
		"type": "session_kill",
		"name": cfg.SessionName,
	}
	data, _ := json.Marshal(killFrame)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write kill frame: %v", err)
	}

	// Read frames until we see session_killed or timeout.
	conn.SetReadDeadline(time.Now().Add(15 * time.Second)) //nolint:errcheck
	r := bufio.NewReaderSize(conn, 1<<20)

	var (
		gotKilled bool
		killState string
	)
	for !gotKilled {
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read ack: %v", err)
		}
		var generic struct {
			Type  string `json:"type"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(line, &generic); err != nil {
			continue
		}
		if generic.Type == "session_killed" {
			gotKilled = true
			killState = generic.State
		}
		if generic.Type == "error" {
			t.Fatalf("daemon returned error frame: %s", string(line))
		}
	}

	if killState != string(StateFinished) {
		t.Errorf("kill state = %q, want %q (a /bin/sleep responds cleanly to SIGTERM)", killState, StateFinished)
	}

	// Wait for the supervisor goroutine to fully converge.
	select {
	case <-sup.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not converge within 5s after session_killed ack")
	}

	// The pi child must no longer be alive.
	if processAlive(pid) {
		t.Errorf("pid %d still alive after session_kill", pid)
	}

	// DB state assertions: sessions row has end_state set.
	sess, err := database.SessionByInstanceID(sup.sess.InstanceID)
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if sess == nil {
		t.Fatal("session row missing post-kill")
	}
	if sess.EndState == nil {
		t.Error("sessions.end_state is NULL after kill (expected non-nil)")
	} else if *sess.EndState != "finished" {
		t.Errorf("sessions.end_state = %q, want finished", *sess.EndState)
	}

	// And a session_end event was written with the kill reason.
	assertSessionEndReason(t, database, cfg.SessionName, "killed_sigterm")
}

// waitForState polls the supervisor's state until it equals want or the
// deadline expires. Returns true on match.
func waitForState(sup *Supervisor, want SessionState, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if sup.State() == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// processAlive returns true when a signal-0 probe of pid succeeds. On
// Unix this is the canonical "is pid alive" check.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 tests for the existence of the process without sending a
	// signal. ESRCH means the process is gone; EPERM means it exists but
	// we don't have permission (treat as alive for the test).
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}
