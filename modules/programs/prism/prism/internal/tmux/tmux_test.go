// Package tmux_test exercises the tmux helper package against a real, isolated
// headless tmux server.
//
// Parallelism:
//
//   - Tests suffixed _Direct use s.* harness methods (bypassing TmuxBin) and
//     can safely run in parallel because they don't touch package-level state.
//
//   - Tests suffixed _API use the tmux.* package-level functions via withServer()
//     which rewrites TmuxBin. These are intentionally sequential (no t.Parallel)
//     to avoid races on that global.
package tmux_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/tmux"
)

// ─── Direct harness tests (parallel-safe) ─────────────────────────────────────
// These tests exercise tmux behaviour using s.* helpers directly, so they are
// safe to run in parallel.

// TestHasSession_Direct verifies that sessions appear and disappear correctly.
func TestHasSession_Direct(t *testing.T) {
	t.Parallel()
	s := newServer(t)

	if s.hasSession("does-not-exist") {
		t.Fatal("hasSession returned true for non-existent session")
	}

	s.newSession("test-session")
	if !s.hasSession("test-session") {
		t.Fatal("hasSession returned false after session created")
	}
}

// TestKillSession_Direct verifies that killing a session removes it.
func TestKillSession_Direct(t *testing.T) {
	t.Parallel()
	s := newServer(t)

	s.newSession("to-kill")
	if !s.hasSession("to-kill") {
		t.Fatal("session not present before kill")
	}
	if err := s.killSession("to-kill"); err != nil {
		t.Fatalf("killSession: %v", err)
	}
	if s.hasSession("to-kill") {
		t.Fatal("session still present after kill")
	}
}

// TestGlobalOption_Direct verifies global option set/get round-trips correctly.
func TestGlobalOption_Direct(t *testing.T) {
	t.Parallel()
	s := newServer(t)

	const opt = "@prism-test-option"
	const val = "hello-world"

	s.setGlobal(opt, val)
	got := s.getGlobal(opt)
	if got != val {
		t.Fatalf("getGlobal(%q) = %q, want %q", opt, got, val)
	}
}

// TestCallerClientStamp_Direct verifies that the @prism_caller_client stamp
// can be set and read back on a specific server.
func TestCallerClientStamp_Direct(t *testing.T) {
	t.Parallel()
	s := newServer(t)

	const clientName = "fake-client-A"
	s.setGlobal("@prism_caller_client", clientName)
	got := s.getGlobal("@prism_caller_client")
	if got != clientName {
		t.Fatalf("@prism_caller_client = %q, want %q", got, clientName)
	}
}

// TestCallerSessionStamp_Direct verifies that the @prism_caller stamp
// can be set and read back on a specific server.
func TestCallerSessionStamp_Direct(t *testing.T) {
	t.Parallel()
	s := newServer(t)

	const sessName = "nixos-config@main"
	s.setGlobal("@prism_caller", sessName)
	got := s.getGlobal("@prism_caller")
	if got != sessName {
		t.Fatalf("@prism_caller = %q, want %q", got, sessName)
	}
}

// TestClientSession_Direct verifies that clientSession returns the session
// a real attached client is viewing.
func TestClientSession_Direct(t *testing.T) {
	t.Parallel()
	s := newServer(t)

	s.newSession("target-session")
	clientName := s.attachClientToSession(t, "target-session")

	got, err := s.clientSession(clientName)
	if err != nil {
		t.Fatalf("clientSession(%q): %v", clientName, err)
	}
	if got != "target-session" {
		t.Fatalf("clientSession() = %q, want %q", got, "target-session")
	}
}

// TestSwitchClient_Direct verifies that switchClient moves a real client from
// one session to another.
func TestSwitchClient_Direct(t *testing.T) {
	t.Parallel()
	s := newServer(t)

	s.newSession("session-alpha")
	s.newSession("session-beta")

	clientName := s.attachClientToSession(t, "session-alpha")

	got, err := s.clientSession(clientName)
	if err != nil {
		t.Fatalf("clientSession before switch: %v", err)
	}
	if got != "session-alpha" {
		t.Fatalf("before switch: got %q, want %q", got, "session-alpha")
	}

	if err := s.switchClient(clientName, "session-beta"); err != nil {
		t.Fatalf("switchClient: %v", err)
	}

	got, err = s.clientSession(clientName)
	if err != nil {
		t.Fatalf("clientSession after switch: %v", err)
	}
	if got != "session-beta" {
		t.Fatalf("after switch: got %q, want %q", got, "session-beta")
	}
}

// TestListClients_Direct verifies that listClients returns attached clients.
func TestListClients_Direct(t *testing.T) {
	t.Parallel()
	s := newServer(t)

	s.newSession("list-test-session")
	clientName := s.attachClientToSession(t, "list-test-session")

	clients, err := s.listClients()
	if err != nil {
		t.Fatalf("listClients: %v", err)
	}

	var found bool
	for _, c := range clients {
		if c == clientName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("listClients() did not include %q; got %v", clientName, clients)
	}
}

// TestTwoClientsGlobalStampIsolation_Direct is the core regression test for
// the "wrong client gets switched" bug.
//
// Setup:
//   - Two clients both viewing "nixos-config@main"
//   - @prism_caller_client stamped to clientB (simulating B opened dashboard last)
//
// Correct behaviour (using the client captured at model-init time):
//   - clientA moves to "nixos-config@feature"
//   - clientB stays on "nixos-config@main"
//
// Bug behaviour (using CallerClient() inside the handler):
//   - clientB would be switched (stamp points to it)
//   - clientA would be left behind
func TestTwoClientsGlobalStampIsolation_Direct(t *testing.T) {
	skipIfSandboxPTY(t)
	t.Parallel()
	s := newServer(t)

	s.newSession("nixos-config@main")
	s.newSession("nixos-config@feature")

	clientA := s.attachClientToSession(t, "nixos-config@main")
	clientB := s.attachClientToSession(t, "nixos-config@main")

	// Stamp @prism_caller_client to clientB (simulating B opened the dashboard last).
	s.setGlobal("@prism_caller_client", clientB)

	// Verify the stamp is now clientB — this is what CallerClient() would return.
	if got := s.getGlobal("@prism_caller_client"); got != clientB {
		t.Fatalf("@prism_caller_client = %q, want %q", got, clientB)
	}

	// The CORRECT code path: clientA captured its own name at model-init time,
	// so it passes clientA explicitly — not the stale global stamp.
	if err := s.switchClient(clientA, "nixos-config@feature"); err != nil {
		t.Fatalf("switchClient clientA→feature: %v", err)
	}

	gotA, err := s.clientSession(clientA)
	if err != nil {
		t.Fatalf("clientSession(clientA): %v", err)
	}
	if gotA != "nixos-config@feature" {
		t.Errorf("clientA session = %q, want %q", gotA, "nixos-config@feature")
	}

	gotB, err := s.clientSession(clientB)
	if err != nil {
		t.Fatalf("clientSession(clientB): %v", err)
	}
	if gotB != "nixos-config@main" {
		t.Errorf("clientB session = %q, want %q (clientB should be unaffected)", gotB, "nixos-config@main")
	}
}

// TestTwoClientsCallerClientBug_Direct documents the bug that occurred when
// CallerClient() was read inside the Enter handler: the global stamp may point
// to a different client than the one that pressed Enter.
//
// This test uses t.Log only (no t.Error) to serve as living documentation
// of the incorrect behaviour without gate-keeping the build.
func TestTwoClientsCallerClientBug_Direct(t *testing.T) {
	skipIfSandboxPTY(t)
	t.Parallel()
	s := newServer(t)

	s.newSession("nixos-config@main")
	s.newSession("nixos-config@feature")

	clientA := s.attachClientToSession(t, "nixos-config@main")
	clientB := s.attachClientToSession(t, "nixos-config@main")

	// Stamp clientB as the caller (simulates B opened dashboard after A).
	s.setGlobal("@prism_caller_client", clientB)

	// BUG SIMULATION: old code reads the global stamp to decide who to switch.
	buggyClient := s.getGlobal("@prism_caller_client") // returns clientB
	if buggyClient != clientB {
		t.Fatalf("setup error: buggyClient should be clientB")
	}

	// Switch using the stale stamp — this sends the switch to clientB.
	if err := s.switchClient(buggyClient, "nixos-config@feature"); err != nil {
		t.Fatalf("switchClient: %v", err)
	}

	gotA, _ := s.clientSession(clientA)
	gotB, _ := s.clientSession(clientB)

	t.Logf("BUG DEMONSTRATION: using global stamp inside Enter handler:")
	t.Logf("  clientA (pressed Enter) ended up in: %s  (should be 'nixos-config@feature')", gotA)
	t.Logf("  clientB (bystander)     ended up in: %s  (should be 'nixos-config@main')", gotB)

	if gotB == "nixos-config@feature" {
		t.Logf("  → BUG confirmed: clientB was wrongly switched to feature")
	}
	if gotA == "nixos-config@main" {
		t.Logf("  → BUG confirmed: clientA was NOT switched despite being the actor")
	}
}

// TestHeadlessCleanupRedirectsOnlyTargetClients_Direct verifies that the
// headless cleanup client-iteration logic only redirects clients that are
// viewing the target session, leaving others unaffected.
func TestHeadlessCleanupRedirectsOnlyTargetClients_Direct(t *testing.T) {
	skipIfSandboxPTY(t)
	t.Parallel()
	s := newServer(t)

	s.newSession("nixos-config@feature")
	s.newSession("nixos-config@main")
	s.newSession("scratchpad")

	// clientA is on the session being cleaned up.
	clientA := s.attachClientToSession(t, "nixos-config@feature")
	// clientB is on a different session — must be unaffected.
	clientB := s.attachClientToSession(t, "nixos-config@main")

	// Replicate the headless cleanup loop from cleanup.go:headlessCleanup.
	targetSession := "nixos-config@feature"
	clients, err := s.listClients()
	if err != nil {
		t.Fatalf("listClients: %v", err)
	}
	for _, c := range clients {
		sess, err := s.clientSession(c)
		if err != nil {
			continue
		}
		if sess == targetSession {
			_ = s.switchClient(c, "scratchpad")
		}
	}

	gotA, err := s.clientSession(clientA)
	if err != nil {
		t.Fatalf("clientSession(clientA): %v", err)
	}
	if gotA != "scratchpad" {
		t.Errorf("clientA = %q after cleanup, want %q", gotA, "scratchpad")
	}

	gotB, err := s.clientSession(clientB)
	if err != nil {
		t.Fatalf("clientSession(clientB): %v", err)
	}
	if gotB != "nixos-config@main" {
		t.Errorf("clientB = %q after cleanup, want %q (unaffected)", gotB, "nixos-config@main")
	}
}

// TestSwitchUsesCurrentClient_Direct verifies that the switch command correctly
// uses the current client's identity (not the global stamp). We simulate this
// by deliberately poisoning the global stamp with clientB, then performing a
// switch using clientA's name directly.
func TestSwitchUsesCurrentClient_Direct(t *testing.T) {
	skipIfSandboxPTY(t)
	t.Parallel()
	s := newServer(t)

	s.newSession("scratchpad")
	s.newSession("nixos-config@main")
	s.newSession("nixos-config@new")

	clientA := s.attachClientToSession(t, "scratchpad")
	clientB := s.attachClientToSession(t, "nixos-config@main")

	// Poison the global stamp with clientB.
	s.setGlobal("@prism_caller_client", clientB)

	// Correct: use clientA's own name (as CurrentClient() would return).
	if err := s.switchClient(clientA, "nixos-config@new"); err != nil {
		t.Fatalf("switchClient: %v", err)
	}

	gotA, _ := s.clientSession(clientA)
	gotB, _ := s.clientSession(clientB)

	if gotA != "nixos-config@new" {
		t.Errorf("clientA = %q, want %q", gotA, "nixos-config@new")
	}
	if gotB != "nixos-config@main" {
		t.Errorf("clientB = %q, want %q (should be unaffected)", gotB, "nixos-config@main")
	}
}

// ─── Package API tests (sequential, use withServer) ───────────────────────────
// These tests specifically exercise the tmux.* package-level functions.
// They are intentionally NOT parallel because withServer() mutates TmuxBin.

// TestAPI_HasSession verifies tmux.HasSession against an isolated server.
func TestAPI_HasSession(t *testing.T) {
	s := newServer(t)
	withServer(t, s)

	if tmux.HasSession("does-not-exist") {
		t.Fatal("HasSession returned true for non-existent session")
	}

	s.newSession("api-test-session")
	if !tmux.HasSession("api-test-session") {
		t.Fatal("HasSession returned false after session created")
	}
}

// TestAPI_NewSessionDetached verifies tmux.NewSessionDetached creates a session.
func TestAPI_NewSessionDetached(t *testing.T) {
	s := newServer(t)
	withServer(t, s)

	if err := tmux.NewSessionDetached("my-session", "/tmp"); err != nil {
		t.Fatalf("NewSessionDetached: %v", err)
	}

	sessions, err := tmux.Sessions()
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}

	var found bool
	for _, sess := range sessions {
		if sess.Name == "my-session" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("created session 'my-session' not found in Sessions()")
	}
}

// TestAPI_KillSession verifies tmux.KillSession removes a session.
func TestAPI_KillSession(t *testing.T) {
	s := newServer(t)
	withServer(t, s)

	s.newSession("to-kill")
	if !tmux.HasSession("to-kill") {
		t.Fatal("session not present before kill")
	}
	if err := tmux.KillSession("to-kill"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if tmux.HasSession("to-kill") {
		t.Fatal("session still present after kill")
	}
}

// TestAPI_SetAndGetGlobalOption verifies SetGlobalOption/GetGlobalOption.
func TestAPI_SetAndGetGlobalOption(t *testing.T) {
	s := newServer(t)
	withServer(t, s)

	const opt = "@prism-test-option"
	const val = "hello-world"

	if err := tmux.SetGlobalOption(opt, val); err != nil {
		t.Fatalf("SetGlobalOption: %v", err)
	}
	got, err := tmux.GetGlobalOption(opt)
	if err != nil {
		t.Fatalf("GetGlobalOption: %v", err)
	}
	if got != val {
		t.Fatalf("got %q, want %q", got, val)
	}
}

// TestAPI_CallerClientStamp verifies tmux.CallerClient reads @prism_caller_client.
func TestAPI_CallerClientStamp(t *testing.T) {
	s := newServer(t)
	withServer(t, s)

	const clientName = "fake-client-A"
	s.setGlobal("@prism_caller_client", clientName)

	got := tmux.CallerClient()
	if got != clientName {
		t.Fatalf("CallerClient() = %q, want %q", got, clientName)
	}
}

// TestAPI_CallerSessionStamp verifies tmux.CallerSession reads @prism_caller.
func TestAPI_CallerSessionStamp(t *testing.T) {
	s := newServer(t)
	withServer(t, s)

	const sessName = "nixos-config@main"
	s.setGlobal("@prism_caller", sessName)

	got := tmux.CallerSession()
	if got != sessName {
		t.Fatalf("CallerSession() = %q, want %q", got, sessName)
	}
}

// TestAPI_ClientSession verifies tmux.ClientSession against a real client.
func TestAPI_ClientSession(t *testing.T) {
	s := newServer(t)
	withServer(t, s)

	s.newSession("target-session")
	clientName := s.attachClientToSession(t, "target-session")

	got, err := tmux.ClientSession(clientName)
	if err != nil {
		t.Fatalf("ClientSession(%q): %v", clientName, err)
	}
	if got != "target-session" {
		t.Fatalf("ClientSession() = %q, want %q", got, "target-session")
	}
}

// TestAPI_SwitchClient verifies tmux.SwitchClient moves a real client.
func TestAPI_SwitchClient(t *testing.T) {
	s := newServer(t)
	withServer(t, s)

	s.newSession("session-alpha")
	s.newSession("session-beta")

	clientName := s.attachClientToSession(t, "session-alpha")

	if err := tmux.SwitchClient(clientName, "session-beta"); err != nil {
		t.Fatalf("SwitchClient: %v", err)
	}

	got, err := tmux.ClientSession(clientName)
	if err != nil {
		t.Fatalf("ClientSession after switch: %v", err)
	}
	if got != "session-beta" {
		t.Fatalf("after switch: got %q, want %q", got, "session-beta")
	}
}

// TestAPI_SwitchClientLast verifies that SwitchClientLast returns a client to
// its previously-viewed session (equivalent to switch-client -l).
func TestAPI_SwitchClientLast(t *testing.T) {
	s := newServer(t)
	withServer(t, s)

	s.newSession("session-origin")
	s.newSession("session-dest")

	clientName := s.attachClientToSession(t, "session-origin")

	// Move to session-dest so that "last" = session-origin.
	if err := tmux.SwitchClient(clientName, "session-dest"); err != nil {
		t.Fatalf("SwitchClient to dest: %v", err)
	}

	// Switch back to last session via SwitchClientLast.
	if err := tmux.SwitchClientLast(clientName); err != nil {
		t.Fatalf("SwitchClientLast: %v", err)
	}

	// Poll for the client to land back on session-origin.
	var got string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sess, err := tmux.ClientSession(clientName); err == nil {
			got = sess
			if sess == "session-origin" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got != "session-origin" {
		t.Errorf("after SwitchClientLast: session = %q, want %q", got, "session-origin")
	}
}

// TestAPI_ListClients verifies tmux.ListClients returns attached clients.
func TestAPI_ListClients(t *testing.T) {
	s := newServer(t)
	withServer(t, s)

	s.newSession("list-test-session")
	clientName := s.attachClientToSession(t, "list-test-session")

	clients, err := tmux.ListClients()
	if err != nil {
		t.Fatalf("ListClients: %v", err)
	}

	var found bool
	for _, c := range clients {
		if c == clientName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ListClients() did not include %q; got %v", clientName, clients)
	}
}

// TestThreeClientsDirectSwitch_Direct verifies that three clients on the same
// session can each be independently switched to separate targets using their own
// captured client name, even when the global @prism_caller_client stamp has been
// rotated through all three (so the stamp is "stale" for A and B and points only
// to C).
//
// This test exercises the direct per-client switch path: it does NOT route through
// any code that reads the global stamp. It verifies that tmux switch-client -c is
// per-client correct with three simultaneous clients, which is the primitive
// relied on by the model-layer tests (TestPersistentModelEnterMultiClient, etc.).
func TestThreeClientsDirectSwitch_Direct(t *testing.T) {
	skipIfSandboxPTY(t)
	t.Parallel()
	s := newServer(t)

	s.newSession("nixos-config@main")
	s.newSession("target-A")
	s.newSession("target-B")
	s.newSession("target-C")

	clientA := s.attachClientToSession(t, "nixos-config@main")
	clientB := s.attachClientToSession(t, "nixos-config@main")
	clientC := s.attachClientToSession(t, "nixos-config@main")

	// Rotate stamp: A → B → C (C is the last to "open the dashboard").
	// After this, the global stamp points only to clientC. This simulates the
	// real-world state where the stamp is stale for A and B.
	s.setGlobal("@prism_caller_client", clientA)
	s.setGlobal("@prism_caller_client", clientB)
	s.setGlobal("@prism_caller_client", clientC)

	// Verify stamp now points to C (what CallerClient() would return).
	if got := s.getGlobal("@prism_caller_client"); got != clientC {
		t.Fatalf("@prism_caller_client = %q, want clientC=%q after rotation", got, clientC)
	}

	// Each client is switched using its own captured name (not the global stamp).
	// This is the correct pattern: production code captures m.client at init time
	// and uses it here, ignoring the stale stamp.
	if err := s.switchClient(clientA, "target-A"); err != nil {
		t.Fatalf("switchClient clientA→target-A: %v", err)
	}
	if err := s.switchClient(clientB, "target-B"); err != nil {
		t.Fatalf("switchClient clientB→target-B: %v", err)
	}
	if err := s.switchClient(clientC, "target-C"); err != nil {
		t.Fatalf("switchClient clientC→target-C: %v", err)
	}

	gotA, err := s.clientSession(clientA)
	if err != nil {
		t.Fatalf("clientSession(clientA): %v", err)
	}
	if gotA != "target-A" {
		t.Errorf("clientA session = %q, want %q", gotA, "target-A")
	}

	gotB, err := s.clientSession(clientB)
	if err != nil {
		t.Fatalf("clientSession(clientB): %v", err)
	}
	if gotB != "target-B" {
		t.Errorf("clientB session = %q, want %q", gotB, "target-B")
	}

	gotC, err := s.clientSession(clientC)
	if err != nil {
		t.Fatalf("clientSession(clientC): %v", err)
	}
	if gotC != "target-C" {
		t.Errorf("clientC session = %q, want %q", gotC, "target-C")
	}
}

// TestCleanupRedirectMultiClient_Direct verifies the cleanup redirect pattern
// with three clients: two on the session being cleaned up and one bystander.
// Only the two clients on the target session must be redirected to scratchpad;
// the bystander must remain unaffected.
//
// Note: this test replicates the redirect loop pattern from
// cleanup.go:headlessCleanup using harness helpers (s.switchClient), not by
// calling production code directly. The test validates that the conditional
// redirect logic (only clients on the target session are moved) is correct for
// the three-client scenario, extending the two-client coverage in
// TestHeadlessCleanupRedirectsOnlyTargetClients_Direct.
func TestCleanupRedirectMultiClient_Direct(t *testing.T) {
	skipIfSandboxPTY(t)
	t.Parallel()
	s := newServer(t)

	s.newSession("nixos-config@feature")
	s.newSession("nixos-config@main")
	s.newSession("scratchpad")

	// Two clients viewing the session being cleaned up.
	clientA := s.attachClientToSession(t, "nixos-config@feature")
	clientB := s.attachClientToSession(t, "nixos-config@feature")
	// One bystander on a different session.
	clientC := s.attachClientToSession(t, "nixos-config@main")

	// Replicate headlessCleanup's client-redirect loop.
	targetSession := "nixos-config@feature"
	clients, err := s.listClients()
	if err != nil {
		t.Fatalf("listClients: %v", err)
	}
	for _, c := range clients {
		sess, err := s.clientSession(c)
		if err != nil {
			continue
		}
		if sess == targetSession {
			_ = s.switchClient(c, "scratchpad")
		}
	}

	gotA, err := s.clientSession(clientA)
	if err != nil {
		t.Fatalf("clientSession(clientA): %v", err)
	}
	if gotA != "scratchpad" {
		t.Errorf("clientA = %q after cleanup redirect, want %q", gotA, "scratchpad")
	}

	gotB, err := s.clientSession(clientB)
	if err != nil {
		t.Fatalf("clientSession(clientB): %v", err)
	}
	if gotB != "scratchpad" {
		t.Errorf("clientB = %q after cleanup redirect, want %q", gotB, "scratchpad")
	}

	gotC, err := s.clientSession(clientC)
	if err != nil {
		t.Fatalf("clientSession(clientC): %v", err)
	}
	if gotC != "nixos-config@main" {
		t.Errorf("clientC = %q after cleanup redirect, want %q (bystander must not be redirected)", gotC, "nixos-config@main")
	}
}

// ─── NewWindow env-var tests ──────────────────────────────────────────────────

// spyTmux creates a fake tmux binary that records its arguments (one per line)
// to argsFile and exits 0. It redirects TmuxBin for the duration of the test.
//
// Only call this from non-parallel tests — TmuxBin is a package-level global.
func spyTmux(t *testing.T) string {
	t.Helper()
	argsFile := t.TempDir() + "/tmux-args"
	wrapperPath := t.TempDir() + "/tmux"
	// The script appends each argument on a separate line to argsFile.
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> " + argsFile + "; done\n"
	if err := os.WriteFile(wrapperPath, []byte(script), 0755); err != nil {
		t.Fatalf("write spy tmux: %v", err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = wrapperPath
	t.Cleanup(func() { tmux.TmuxBin = orig })
	return argsFile
}

// readArgs reads the recorded args from a spy tmux invocation.
func readArgs(argsFile string) []string {
	data, err := os.ReadFile(argsFile)
	if err != nil {
		return nil
	}
	var args []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			args = append(args, line)
		}
	}
	return args
}

// containsSequence returns true when needle appears as a contiguous sub-slice of haystack.
func containsSequence(haystack, needle []string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j, n := range needle {
			if haystack[i+j] != n {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestNewWindow_EnvVar_WithPrompt verifies that NewWindow emits "-e KEY=VALUE"
// in its args when envVars contains an entry.
func TestNewWindow_EnvVar_WithPrompt(t *testing.T) {
	argsFile := spyTmux(t)

	err := tmux.NewWindow("mysession", 1, "agent", "/tmp", "echo hi", map[string]string{
		"PRISM_INITIAL_PROMPT": "hello world",
	})
	if err != nil {
		// The spy script exits 0, so an error here is unexpected.
		t.Fatalf("NewWindow: %v", err)
	}

	args := readArgs(argsFile)
	if !containsSequence(args, []string{"-e", "PRISM_INITIAL_PROMPT=hello world"}) {
		t.Errorf("args %v do not contain [-e PRISM_INITIAL_PROMPT=hello world]", args)
	}
}

// TestNewWindow_EnvVar_Empty verifies that NewWindow does NOT emit any "-e"
// flag when envVars is nil.
func TestNewWindow_EnvVar_Empty(t *testing.T) {
	argsFile := spyTmux(t)

	err := tmux.NewWindow("mysession", 1, "agent", "/tmp", "echo hi", nil)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}

	args := readArgs(argsFile)
	for _, a := range args {
		if a == "-e" {
			t.Errorf("args %v contain -e flag, expected none when envVars is nil", args)
			break
		}
	}
}

// TestNewWindow_EnvVar_EmptyMap verifies that NewWindow does NOT emit any "-e"
// flag when envVars is an empty (non-nil) map.
func TestNewWindow_EnvVar_EmptyMap(t *testing.T) {
	argsFile := spyTmux(t)

	err := tmux.NewWindow("mysession", 1, "agent", "/tmp", "echo hi", map[string]string{})
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}

	args := readArgs(argsFile)
	for _, a := range args {
		if a == "-e" {
			t.Errorf("args %v contain -e flag, expected none when envVars is empty", args)
			break
		}
	}
}

// TestNewWindow_EnvVar_MultipleKeys verifies that multiple env vars each
// produce their own "-e KEY=VALUE" pair, in sorted key order.
func TestNewWindow_EnvVar_MultipleKeys(t *testing.T) {
	argsFile := spyTmux(t)

	err := tmux.NewWindow("mysession", 1, "agent", "/tmp", "", map[string]string{
		"ZEBRA": "z",
		"ALPHA": "a",
	})
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}

	args := readArgs(argsFile)
	// Both env vars must be present.
	if !containsSequence(args, []string{"-e", "ALPHA=a"}) {
		t.Errorf("args %v do not contain [-e ALPHA=a]", args)
	}
	if !containsSequence(args, []string{"-e", "ZEBRA=z"}) {
		t.Errorf("args %v do not contain [-e ZEBRA=z]", args)
	}
	// ALPHA must appear before ZEBRA (sorted key order).
	alphaIdx := -1
	zebraIdx := -1
	for i, a := range args {
		if a == "ALPHA=a" {
			alphaIdx = i
		}
		if a == "ZEBRA=z" {
			zebraIdx = i
		}
	}
	if alphaIdx == -1 || zebraIdx == -1 {
		t.Fatalf("ALPHA or ZEBRA not found in args: %v", args)
	}
	if alphaIdx > zebraIdx {
		t.Errorf("ALPHA (idx %d) must come before ZEBRA (idx %d) in args: %v", alphaIdx, zebraIdx, args)
	}
}

// TestNewWindow_EnvVar_EqualInValue verifies that a prompt containing '='
// characters is passed verbatim — the env var is split on the FIRST '=' only.
func TestNewWindow_EnvVar_EqualInValue(t *testing.T) {
	argsFile := spyTmux(t)

	err := tmux.NewWindow("mysession", 1, "agent", "/tmp", "", map[string]string{
		"PRISM_INITIAL_PROMPT": "KEY=value is part of the message",
	})
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}

	args := readArgs(argsFile)
	if !containsSequence(args, []string{"-e", "PRISM_INITIAL_PROMPT=KEY=value is part of the message"}) {
		t.Errorf("args %v do not contain expected env var with = in value", args)
	}
}

// TestSwitchClientRaceCondition_Direct verifies that two concurrent switch-
// client calls for different clients do not cross-target each other. Client A
// and client B start on the same session; goroutines simultaneously move each
// to its own dedicated target. After both complete, each client must be on its
// intended destination.
func TestSwitchClientRaceCondition_Direct(t *testing.T) {
	skipIfSandboxPTY(t)
	t.Parallel()
	s := newServer(t)

	s.newSession("nixos-config@main")
	s.newSession("session1")
	s.newSession("session2")

	clientA := s.attachClientToSession(t, "nixos-config@main")
	clientB := s.attachClientToSession(t, "nixos-config@main")

	// Run both switches concurrently.
	errA := make(chan error, 1)
	errB := make(chan error, 1)
	go func() { errA <- s.switchClient(clientA, "session1") }()
	go func() { errB <- s.switchClient(clientB, "session2") }()

	if err := <-errA; err != nil {
		t.Fatalf("concurrent switchClient clientA→session1: %v", err)
	}
	if err := <-errB; err != nil {
		t.Fatalf("concurrent switchClient clientB→session2: %v", err)
	}

	// tmux switch-client is synchronous: once the command exits the client is
	// already on the new session. Verify immediately after both channels drain.
	gotA, err := s.clientSession(clientA)
	if err != nil {
		t.Fatalf("clientSession(clientA) after concurrent switch: %v", err)
	}
	gotB, err := s.clientSession(clientB)
	if err != nil {
		t.Fatalf("clientSession(clientB) after concurrent switch: %v", err)
	}

	if gotA != "session1" {
		t.Errorf("clientA = %q after concurrent switch, want %q (no cross-targeting)", gotA, "session1")
	}
	if gotB != "session2" {
		t.Errorf("clientB = %q after concurrent switch, want %q (no cross-targeting)", gotB, "session2")
	}
}

// TestAPI_TwoClientsGlobalStampIsolation is the package-API version of the
// core regression test. It uses tmux.* functions throughout.
//
// See TestTwoClientsGlobalStampIsolation_Direct for the parallel-safe version.
func TestAPI_TwoClientsGlobalStampIsolation(t *testing.T) {
	skipIfSandboxPTY(t)
	s := newServer(t)
	withServer(t, s)

	s.newSession("nixos-config@main")
	s.newSession("nixos-config@feature")

	clientA := s.attachClientToSession(t, "nixos-config@main")
	clientB := s.attachClientToSession(t, "nixos-config@main")

	// Stamp @prism_caller_client to clientB (simulating B opened dashboard last).
	s.setGlobal("@prism_caller_client", clientB)

	// Assert the global stamp points to clientB (what CallerClient() would return).
	if got := tmux.CallerClient(); got != clientB {
		t.Fatalf("@prism_caller_client = %q, want clientB=%q", got, clientB)
	}

	// CORRECT path: use clientA's own name (captured at model-init time).
	if err := tmux.SwitchClient(clientA, "nixos-config@feature"); err != nil {
		t.Fatalf("SwitchClient clientA→feature: %v", err)
	}

	gotA, err := tmux.ClientSession(clientA)
	if err != nil {
		t.Fatalf("ClientSession(clientA): %v", err)
	}
	if gotA != "nixos-config@feature" {
		t.Errorf("clientA session = %q, want %q", gotA, "nixos-config@feature")
	}

	gotB, err := tmux.ClientSession(clientB)
	if err != nil {
		t.Fatalf("ClientSession(clientB): %v", err)
	}
	if gotB != "nixos-config@main" {
		t.Errorf("clientB session = %q, want %q (should be unaffected)", gotB, "nixos-config@main")
	}
}

// ─── run() error-format tests ─────────────────────────────────────────────────
// These verify the diagnostic shape of errors returned when tmux exits
// non-zero. They install a fake tmux that fails predictably and exercise the
// public API (which routes through run()).
//
// These tests mutate TmuxBin via fakeTmux/spyTmux helpers, so they are
// intentionally NOT parallel.

// fakeTmux installs a fake tmux binary that writes the given string to stderr
// and exits with the given code. Returns nothing — TmuxBin is restored via
// t.Cleanup.
func fakeTmux(t *testing.T, stderrMsg string, exitCode int) {
	t.Helper()
	wrapperPath := t.TempDir() + "/tmux"
	// printf to stderr, then exit with the requested code.
	script := "#!/bin/sh\nprintf '%s\\n' " + shellSingleQuote(stderrMsg) + " >&2\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(wrapperPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = wrapperPath
	t.Cleanup(func() { tmux.TmuxBin = orig })
}

// shellSingleQuote single-quotes s for safe inclusion in a /bin/sh script.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// itoa avoids pulling strconv into the test file's import block for one digit.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestRunError_IncludesArgvAndStderr verifies that when tmux exits non-zero,
// the error returned by a public helper that goes through run() contains both
// the tmux subcommand name and a substring derived from tmux's stderr.
//
// This is the core diagnostic regression test for issue #1054.
func TestRunError_IncludesArgvAndStderr(t *testing.T) {
	const stderrMsg = "can't find session: bogus-session"
	fakeTmux(t, stderrMsg, 1)

	err := tmux.KillSession("bogus-session")
	if err == nil {
		t.Fatal("KillSession returned nil error; expected failure from fake tmux")
	}
	msg := err.Error()
	if !strings.Contains(msg, "kill-session") {
		t.Errorf("error %q does not contain tmux subcommand name 'kill-session'", msg)
	}
	if !strings.Contains(msg, stderrMsg) {
		t.Errorf("error %q does not contain stderr substring %q", msg, stderrMsg)
	}
	// The wrapped exec error must still be unwrappable to *exec.ExitError.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("error %q does not unwrap to *exec.ExitError", msg)
	}
}

// TestRunError_EmptyStderrDegradesGracefully verifies the edge case where tmux
// exits non-zero but writes nothing to stderr. The error must still carry the
// argv and the wrapped exec error — no panic, no empty error string.
func TestRunError_EmptyStderrDegradesGracefully(t *testing.T) {
	fakeTmux(t, "", 2)

	err := tmux.KillSession("anything")
	if err == nil {
		t.Fatal("KillSession returned nil error; expected failure from fake tmux")
	}
	msg := err.Error()
	if msg == "" {
		t.Fatal("error string is empty; expected argv + wrapped exec error")
	}
	if !strings.Contains(msg, "kill-session") {
		t.Errorf("error %q does not contain 'kill-session' (argv missing)", msg)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("error %q does not unwrap to *exec.ExitError", msg)
	}
}
