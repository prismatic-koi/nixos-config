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

// TestAPI_TwoClientsGlobalStampIsolation is the package-API version of the
// core regression test. It uses tmux.* functions throughout.
//
// See TestTwoClientsGlobalStampIsolation_Direct for the parallel-safe version.
func TestAPI_TwoClientsGlobalStampIsolation(t *testing.T) {
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

// ─── SendKeysWhenReady tests ──────────────────────────────────────────────────
// These tests exercise SendKeysWhenReady against a real tmux server.
// They use withServer() and are intentionally sequential (no t.Parallel).
//
// Setup for each test:
//   - A session named "myrepo@feat" with an "agent" window that runs `cat`
//     (a simple command that accepts and echoes stdin — stands in for opencode).
//   - SendKeysWhenReady is called targeting that window.
//   - The test manipulates @agent_state to simulate opencode becoming ready,
//     then checks that the typed text appears in the pane.

// waitForPaneContent polls capture-pane on the given target until the pane
// content contains the expected substring, or the deadline is exceeded.
// Returns true if the content was found.
func waitForPaneContent(s *server, target, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(s.capturePane(target), want) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// TestSendKeysWhenReady_ImmediateReady verifies that when @agent_state is
// already "finished" before SendKeysWhenReady is called, the prompt arrives
// in the pane within a reasonable time (poll exits immediately).
func TestSendKeysWhenReady_ImmediateReady(t *testing.T) {
	s := newServer(t)
	withServer(t, s)

	const session = "myrepo@feat"
	s.newSession(session)
	// Create the "agent" window running cat so send-keys output is echoed.
	s.newWindow(session, 1, "agent")
	if err := s.run("send-keys", "-t", session+":agent", "cat", "Enter"); err != nil {
		t.Fatalf("start cat: %v", err)
	}
	// Pre-set @agent_state to "finished" — opencode is already ready.
	s.setWindowOption(session+":agent", "@agent_state", "finished")

	if err := tmux.SendKeysWhenReady(session+":agent", session, "hello-immediate", 10); err != nil {
		t.Fatalf("SendKeysWhenReady: %v", err)
	}

	// The prompt should appear within a few seconds (poll loop exits immediately,
	// then there is a 500 ms settle before sending).
	const timeout = 5 * time.Second
	if !waitForPaneContent(s, session+":agent", "hello-immediate", timeout) {
		t.Errorf("pane did not receive 'hello-immediate' within %v\npane content:\n%s",
			timeout, s.capturePane(session+":agent"))
	}
}

// TestSendKeysWhenReady_DelayedReady verifies that when @agent_state is not
// yet "finished" at call time but becomes "finished" shortly afterwards, the
// prompt is still delivered (the poll loop picks up the state change).
func TestSendKeysWhenReady_DelayedReady(t *testing.T) {
	s := newServer(t)
	withServer(t, s)

	const session = "myrepo@feat-delayed"
	s.newSession(session)
	s.newWindow(session, 1, "agent")
	if err := s.run("send-keys", "-t", session+":agent", "cat", "Enter"); err != nil {
		t.Fatalf("start cat: %v", err)
	}

	// Do NOT set @agent_state yet — simulate opencode still starting up.
	if err := tmux.SendKeysWhenReady(session+":agent", session, "hello-delayed", 15); err != nil {
		t.Fatalf("SendKeysWhenReady: %v", err)
	}

	// Set the state to "finished" after a short delay to simulate opencode
	// completing its initialisation.
	time.Sleep(1 * time.Second)
	s.setWindowOption(session+":agent", "@agent_state", "finished")

	// The prompt should arrive within a few seconds of the state being set.
	const timeout = 5 * time.Second
	if !waitForPaneContent(s, session+":agent", "hello-delayed", timeout) {
		t.Errorf("pane did not receive 'hello-delayed' within %v after state set\npane content:\n%s",
			timeout, s.capturePane(session+":agent"))
	}
}

// TestSendKeysWhenReady_TimeoutFallback verifies that when @agent_state never
// becomes "finished", the prompt is still sent after the timeout expires.
func TestSendKeysWhenReady_TimeoutFallback(t *testing.T) {
	s := newServer(t)
	withServer(t, s)

	const session = "myrepo@feat-timeout"
	s.newSession(session)
	s.newWindow(session, 1, "agent")
	if err := s.run("send-keys", "-t", session+":agent", "cat", "Enter"); err != nil {
		t.Fatalf("start cat: %v", err)
	}

	// Never set @agent_state — the poll loop should time out after timeoutSecs
	// and send the prompt anyway.
	const timeoutSecs = 3 // short timeout so the test finishes quickly
	if err := tmux.SendKeysWhenReady(session+":agent", session, "hello-timeout", timeoutSecs); err != nil {
		t.Fatalf("SendKeysWhenReady: %v", err)
	}

	// The prompt should arrive after the timeout (3 s) + settle (0.5 s) + margin.
	const waitFor = 6 * time.Second
	if !waitForPaneContent(s, session+":agent", "hello-timeout", waitFor) {
		t.Errorf("pane did not receive 'hello-timeout' within %v (expected after %ds timeout)\npane content:\n%s",
			waitFor, timeoutSecs, s.capturePane(session+":agent"))
	}
}
