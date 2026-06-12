package session

// Tests for SidecarAlive (issue #2255) — the socket-probe liveness signal
// used by the stale-zombie classification in cmd/switch.go and
// startupGuardKillOld. A paused-by-design session (escalated awaiting
// coordinator guidance, reviewing awaiting verdicts) stops bumping
// agent_status.last_seen, so the 60-second heuristic alone misclassifies it
// as a zombie and kills its healthy sidecar — severing prompt delivery,
// finish notifications, and event recording. SidecarAlive distinguishes a
// genuinely dead sidecar (absent socket / tombstone) from a healthy paused
// one (responsive listener).

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// setupAliveTestEnv redirects XDG_STATE_HOME to a short-prefix tempdir (so
// the derived socket path stays under sun_path limits — see #1050) and
// returns the hostapi.sock path for sessionName with its directory created.
func setupAliveTestEnv(t *testing.T, sessionName string) string {
	t.Helper()
	tmp, err := os.MkdirTemp("", "psa-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("XDG_STATE_HOME", tmp)

	sockDir := filepath.Join(tmp, "prism", "run", SessionDirName(sessionName))
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	return filepath.Join(sockDir, "hostapi.sock")
}

// TestSidecarAlive_NoSocket verifies that a session with no socket files at
// all (sidecar never started, or fully cleaned up) reports not-alive — the
// stale-zombie restart path stays available for genuinely dead sessions.
func TestSidecarAlive_NoSocket(t *testing.T) {
	_ = setupAliveTestEnv(t, "prism-test@alive-none")
	if SidecarAlive("prism-test@alive-none") {
		t.Error("SidecarAlive = true with no socket bound, want false")
	}
}

// TestSidecarAlive_LiveListener verifies that a responsive listener on the
// session's hostapi.sock reports alive — this is the paused-escalated-session
// case from #2255 where last_seen is stale but the sidecar is healthy.
func TestSidecarAlive_LiveListener(t *testing.T) {
	sockPath := setupAliveTestEnv(t, "prism-test@alive-live")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	// Accept and immediately discard connections so the probe's dial
	// completes; without a live accepter the dial still succeeds on a bound
	// Unix socket (backlog), which is fine — keep the accepter to mirror a
	// real sidecar.
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	if !SidecarAlive("prism-test@alive-live") {
		t.Error("SidecarAlive = false with a live listener on hostapi.sock, want true")
	}
}

// TestSidecarAlive_Tombstone verifies that a socket FILE whose listener has
// exited (the abnormal-exit tombstone: dial returns ECONNREFUSED) reports
// not-alive — a tombstone means the sidecar is dead and the restart path
// must remain reachable.
func TestSidecarAlive_Tombstone(t *testing.T) {
	sockPath := setupAliveTestEnv(t, "prism-test@alive-tomb")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	// Close the listener but leave the socket file on disk (UnixListener
	// removes the file on Close by default — suppress that).
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()

	if _, statErr := os.Stat(sockPath); statErr != nil {
		t.Fatalf("tombstone setup failed — socket file missing: %v", statErr)
	}
	if SidecarAlive("prism-test@alive-tomb") {
		t.Error("SidecarAlive = true for a tombstone socket (dead listener), want false")
	}
}

// TestSidecarAlive_PipeSocketFallback verifies that a host-mode session whose
// only socket is pipe.sock (no hostapi.sock — host isolation has no host-API
// socket) is still recognised as alive via the pipe socket.
func TestSidecarAlive_PipeSocketFallback(t *testing.T) {
	const sessionName = "prism-test@alive-pipe"
	_ = setupAliveTestEnv(t, sessionName)

	pipePath, err := SidecarHarnessPipePath(sessionName)
	if err != nil {
		t.Fatalf("SidecarHarnessPipePath: %v", err)
	}
	ln, lnErr := net.Listen("unix", pipePath)
	if lnErr != nil {
		t.Fatalf("listen unix %s: %v", pipePath, lnErr)
	}
	t.Cleanup(func() { _ = ln.Close() })

	if !SidecarAlive(sessionName) {
		t.Error("SidecarAlive = false with a live pipe.sock listener, want true")
	}
}
