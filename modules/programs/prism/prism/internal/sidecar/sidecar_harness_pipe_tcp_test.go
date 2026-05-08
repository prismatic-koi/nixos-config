package sidecar

// sidecar_harness_pipe_tcp_test.go — tests for the harness-pipe TCP listener
// bind address on Darwin (issue #1482).
//
// ACs covered:
//
//   - [security] The harness-pipe TCP listener binds 127.0.0.1:<port> rather
//     than 0.0.0.0:<port> when HarnessPipeTCPPort != 0.
//   - [edge-case] When the TCP listen fails (port already in use),
//     runStartupSocketPipe returns an error whose message contains the port
//     number, and the session is moved to error state.
//
// Both tests use an in-process Go listener via the real runStartupSocketPipe
// code path (by calling Sidecar.Run with HarnessPipeTCPPort set). The
// security AC is verified by confirming the listener's local address is
// 127.0.0.1, then attempting a connection from a non-loopback address and
// observing that it fails.

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
)

// allocateFreePort asks the OS for an available TCP port by binding 127.0.0.1:0
// and immediately closing the listener. The allocated port is returned; the
// caller can use it for a controlled listener in the same process.
func allocateFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocateFreePort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// newTCPPortSidecar creates a Sidecar configured to use HarnessPipeTCPPort
// (the Darwin TCP path) rather than HarnessPipeSockPath (the Linux Unix-socket
// path).
func newTCPPortSidecar(t *testing.T, port int) *Sidecar {
	t.Helper()
	d := openTestDB(t)
	clk := newTestClock()
	cfg := Config{
		SessionName:           "testrepo@tcp-bind-test",
		Repo:                  "testrepo",
		Worktree:              t.TempDir(),
		DB:                    d,
		Clock:                 clk,
		AgentRole:             "worker",
		HarnessName:           "pi",
		HarnessPipeTCPPort:    port,
		StartupConnectTimeout: 5 * time.Second,
		PipeReconnectTimeout:  200 * time.Millisecond,
		Harness:               pih.New("", "", ""),
	}
	return New(cfg)
}

// dialTCP dials a TCP address and returns the connection or an error. Unlike
// net.Dial, this helper does not retry; it attempts a single dial with a short
// deadline suitable for test assertions.
func dialTCP(addr string) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, 500*time.Millisecond)
}

// waitForTCPListening polls addr until a connection succeeds or the deadline
// passes. Returns true when the listener is ready, false on timeout.
func waitForTCPListening(addr string, deadline time.Duration) bool {
	until := time.Now().Add(deadline)
	for time.Now().Before(until) {
		conn, err := dialTCP(addr)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestHarnessPipeTCP_ListenerBinds127001 verifies that the harness-pipe TCP
// listener is bound exclusively to 127.0.0.1 (loopback), not 0.0.0.0 (all
// interfaces). This is the security AC from issue #1482:
//
//	[security] The sidecar's harness-pipe TCP listener binds 127.0.0.1:<port>
//	rather than 0.0.0.0:<port>.
//
// We verify the bind address by:
//  1. Starting a Sidecar with HarnessPipeTCPPort set.
//  2. Confirming the loopback address (127.0.0.1:<port>) is reachable.
//  3. Confirming 0.0.0.0:<port> is NOT used by checking that any local
//     non-loopback interface address is rejected. On CI without non-loopback
//     interfaces we fall back to checking the listener's local addr string.
func TestHarnessPipeTCP_ListenerBinds127001(t *testing.T) {
	port := allocateFreePort(t)
	addr127 := fmt.Sprintf("127.0.0.1:%d", port)

	sc := newTCPPortSidecar(t, port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := make(chan error, 1)
	go func() { errc <- sc.Run(ctx) }()

	// Wait for the TCP listener to be up.
	if !waitForTCPListening(addr127, 3*time.Second) {
		t.Fatalf("TCP listener on %s never became ready", addr127)
	}

	// Confirm we can connect to 127.0.0.1:<port>.
	conn127, err := dialTCP(addr127)
	if err != nil {
		t.Fatalf("could not connect to loopback listener at %s: %v", addr127, err)
	}
	conn127.Close()

	// Confirm the listener's local address is 127.0.0.1 (not 0.0.0.0).
	sc.mu.Lock()
	ln := sc.harnessPipeListener
	sc.mu.Unlock()

	if ln == nil {
		t.Fatal("harnessPipeListener is nil after TCP startup — listener was not stored")
	}
	localAddr := ln.Addr().String()
	if !strings.HasPrefix(localAddr, "127.0.0.1:") {
		t.Errorf("harnessPipeListener bound to %q, want 127.0.0.1:<port> (issue #1482)", localAddr)
	}

	// Now attempt a connection via the non-loopback dial address (0.0.0.0).
	// A 0.0.0.0-bound listener accepts connections from any local addr;
	// a 127.0.0.1-bound listener only accepts connections from loopback.
	// We test this by dialling the machine's outbound IP (if available).
	// If the machine has no non-loopback interface (e.g. stripped CI), we
	// skip the external-dial assertion but the localAddr check above is
	// sufficient.
	nonLoopback := findNonLoopbackIP(t)
	if nonLoopback != "" {
		nonLoopbackAddr := fmt.Sprintf("%s:%d", nonLoopback, port)
		conn, err := dialTCP(nonLoopbackAddr)
		if err == nil {
			conn.Close()
			t.Errorf("connection to non-loopback address %s succeeded — listener should bind 127.0.0.1 only (issue #1482)", nonLoopbackAddr)
		}
		// err != nil is the expected outcome: the loopback-only listener
		// refuses connections from non-loopback source/destination addresses.
	}

	// Shut down cleanly: dial loopback, complete handshake, send session_shutdown.
	shutdownConn, err := dialTCP(addr127)
	if err == nil {
		// Complete the PI handshake and send shutdown.
		_ = shutdownConn.SetDeadline(time.Now().Add(2 * time.Second))
		fmt.Fprintf(shutdownConn, `{"type":"hello","protocol_version":2,"harness":"pi","harness_version":"0.0.0"}`+"\n")
		buf := make([]byte, 4096)
		n, _ := shutdownConn.Read(buf)
		_ = n // hello_ack — ignore content
		fmt.Fprintf(shutdownConn, `{"type":"session_shutdown"}`+"\n")
		shutdownConn.Close()
	} else {
		// Could not dial for shutdown — cancel context instead.
		cancel()
	}

	select {
	case err := <-errc:
		if err != nil {
			// Run() may return an error after context cancel; that is fine.
			_ = err
		}
	case <-time.After(5 * time.Second):
		t.Error("Sidecar.Run did not exit within 5s after session_shutdown")
	}
}

// TestHarnessPipeTCP_ListenFail_ReturnsErrorWithPort verifies that when the
// harness-pipe TCP listen fails (e.g. the port is already in use), the sidecar
//
//   - records the failure as a startup_error event whose reason contains the
//     port number, and
//   - transitions the session to StateError,
//
// and that Run() then exits promptly when the outer context is cancelled.
//
// Originally written for the edge-case AC from issue #1482:
//
//	[edge-case] When the sidecar's harness-pipe TCP listen fails (e.g. port
//	already in use), runStartupSocketPipe returns an error whose message
//	contains the port number, the session is moved to error, and no extension
//	is left dialling a non-existent listener.
//
// Updated for #1493 to match the post-#1490 contract: a non-nil return from
// runStartupSocketPipe is absorbed by Run() (host-API stays alive for the
// in-sandbox `prism` CLI) and Run() blocks on ctx.Done() rather than returning
// the listen error directly. The contract under test is therefore the
// observable side effects (DB state + startup_error event) plus prompt exit on
// context cancel — not Run()'s return value during the listen failure itself.
func TestHarnessPipeTCP_ListenFail_ReturnsErrorWithPort(t *testing.T) {
	port := allocateFreePort(t)

	// Pre-occupy the port so the sidecar's net.Listen fails.
	blocker, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("could not pre-occupy port %d: %v", port, err)
	}
	defer blocker.Close()

	sc := newTCPPortSidecar(t, port)

	// Pre-seed an instance_id and a matching sessions row so that the
	// startup_error event written by writeStartupError satisfies the
	// agent_events.instance_id REFERENCES sessions(instance_id) FK constraint.
	// Without this, the startup_error event would be silently dropped (logged
	// as a WriteEvent error), leaving the test unable to read back the
	// recorded error reason. The instance_id is the contract bridge between
	// agent_status (FK-free) and agent_events (FK-enforced).
	instanceID := uuid.New().String()
	sc.cfg.InstanceID = instanceID
	if err := sc.cfg.DB.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sc.cfg.SessionName,
		Repo:        sc.cfg.Repo,
		Worktree:    sc.cfg.Worktree,
		Harness:     sc.cfg.HarnessName,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := make(chan error, 1)
	go func() { errc <- sc.Run(ctx) }()

	// Observe the listen failure via DB state — Run() does NOT return the
	// error directly under the post-#1490 contract.
	//
	// Synchronisation (issue #1515): waitForState polls the DB at 1ms intervals
	// up to a 10s deadline rather than the previous 20ms / 2s loop. The
	// previous bound was tight enough to flake under the contended scheduling
	// of the Nix build sandbox, where the path Run() → instance_id mint →
	// transport switch → net.Listen → writeStartupError can take noticeably
	// longer than 2s when the host is under heavy load. The fix is structural
	// because waitForState observes the actual DB transition rather than
	// trusting an arbitrary timeout magnitude — the only knob that changed is
	// the upper bound of the wait, which does not affect production behaviour.
	if state := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateError), 10*time.Second); state != string(agent.StateError) {
		t.Fatalf("session state = %q after TCP listen failure, want %q within 10s", state, agent.StateError)
	}

	// Verify the recorded startup-error message contains the port number.
	// waitForStartupErrorMessage polls because writeStartupError commits the
	// state transition (StateError) and the startup_error event in two separate
	// DB writes — a read between them returns the new state but no event yet.
	errMsg := waitForStartupErrorMessage(t, sc.cfg.DB, sc.cfg.SessionName, 5*time.Second)
	if errMsg == "" {
		t.Fatal("no startup_error event recorded after TCP listen failure")
	}
	portStr := fmt.Sprintf("%d", port)
	if !strings.Contains(errMsg, portStr) {
		t.Errorf("startup_error reason %q does not contain port number %q", errMsg, portStr)
	}

	// Cancel the context and assert Run() exits promptly. Post-#1490 the
	// host-API is held open until external shutdown; cancel is the trigger.
	cancel()
	select {
	case err := <-errc:
		// Run() may return nil or ctx.Err() after cancel; either is acceptable
		// per the post-#1490 absorb contract. We deliberately do NOT require
		// the listen error to surface here.
		_ = err
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not exit within 2s after context cancel")
	}
}

// findNonLoopbackIP returns the first non-loopback IPv4 address on the host,
// or "" if none is available (e.g. stripped CI environment).
func findNonLoopbackIP(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				return ip4.String()
			}
		}
	}
	return ""
}
