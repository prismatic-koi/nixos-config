// Tests in this file exercise notifyInvestigatorCompletion and related helpers.
//
// # Isolation contract
//
// Every test that constructs a sidecar.Sidecar MUST:
//   - Use sidecartest.NewIsolated(t, ...) to redirect XDG_STATE_HOME to a
//     tempdir and activate the PRISM_TEST_MODE_RESTRICT_HOSTAPI guard.
//   - Use session names with the "prism-test@" prefix — never "nixos-config@main"
//     or any other slug that could collide with a real coordinator on the host.
//
// These invariants ensure that running `go test ./internal/sidecar/...` on a
// host with a live nixos-config@main coordinator does NOT deliver any
// notifications to that coordinator, and does NOT write to the real prism DB.
package sidecar

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// ── investigateAgentInvokerSession ──────────────────────────────────────────

func TestInvestigateAgentInvokerSession(t *testing.T) {
	cases := []struct {
		name        string
		sessionName string
		wantInvoker string
		wantOK      bool
	}{
		{
			name:        "standard investigate session",
			sessionName: "nixos-config@main~investigate-abc123",
			wantInvoker: "nixos-config@main",
			wantOK:      true,
		},
		{
			name:        "investigate session with longer slug",
			sessionName: "myrepo@feature~investigate-task-abc",
			wantInvoker: "myrepo@feature",
			wantOK:      true,
		},
		{
			name:        "review session — not investigate",
			sessionName: "myrepo@feature~review-2-review-goal",
			wantInvoker: "",
			wantOK:      false,
		},
		{
			name:        "plain worker session",
			sessionName: "myrepo@feature",
			wantInvoker: "",
			wantOK:      false,
		},
		{
			name:        "coordinator session",
			sessionName: "myrepo@main",
			wantInvoker: "",
			wantOK:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := investigateAgentInvokerSession(tc.sessionName)
			if ok != tc.wantOK {
				t.Errorf("investigateAgentInvokerSession(%q): ok=%v want %v", tc.sessionName, ok, tc.wantOK)
			}
			if got != tc.wantInvoker {
				t.Errorf("investigateAgentInvokerSession(%q): invoker=%q want %q", tc.sessionName, got, tc.wantInvoker)
			}
		})
	}
}

// ── notifyInvestigatorCompletion ─────────────────────────────────────────────

// TestNotifyInvestigatorCompletion_Delivery verifies that a completion
// notification is delivered to the invoker session when the investigation
// finishes with a non-empty final text. The notification body must contain the
// sender label, the text block verbatim, and the steering-channel hint.
func TestNotifyInvestigatorCompletion_Delivery(t *testing.T) {
	invokerSession := "prism-test@invoker-delivery"
	investigatorSession := invokerSession + "~investigate-testslug"

	bus := sidecartest.NewIsolated(t, invokerSession)

	clk := newTestClock()
	cfg := Config{
		SessionName: investigatorSession,
		Repo:        "prism-test",
		Worktree:    "/tmp/investigate-test-wt",
		DB:          bus.DB,
		Clock:       clk,
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
	}
	s := New(cfg)

	const finalText = "I have found the root cause. The issue is in package X."
	s.notifyInvestigatorCompletion(agent.StateFinished, finalText)

	// Allow async HTTP call to complete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(bus.CopyBodies()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	bodies := bus.CopyBodies()
	if len(bodies) != 1 {
		t.Fatalf("want 1 delivery, got %d", len(bodies))
	}
	body := bodies[0]

	// The body is a JSON-encoded prompt_async payload; the text is inside.
	// Check that all required elements are present in the delivered payload.
	if !strings.Contains(body, "From investigator session: "+investigatorSession) {
		t.Errorf("notification body missing sender label; got: %s", body)
	}
	if !strings.Contains(body, finalText) {
		t.Errorf("notification body missing verbatim text block; got: %s", body)
	}
	if !strings.Contains(body, "prism prompt "+investigatorSession+" --prompt") {
		t.Errorf("notification body missing steering-channel hint; got: %s", body)
	}
}

// TestNotifyInvestigatorCompletion_EmptyText verifies that even with empty
// final text, a completion notice is still delivered (so the invoker learns
// the investigation finished, even if no output was recorded).
func TestNotifyInvestigatorCompletion_EmptyText(t *testing.T) {
	invokerSession := "prism-test@invoker-emptytext"
	investigatorSession := invokerSession + "~investigate-testslug"

	bus := sidecartest.NewIsolated(t, invokerSession)

	clk := newTestClock()
	cfg := Config{
		SessionName: investigatorSession,
		Repo:        "prism-test",
		Worktree:    "/tmp/investigate-test-empty",
		DB:          bus.DB,
		Clock:       clk,
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
	}
	s := New(cfg)

	// Even with empty final text, a completion notice must be delivered.
	s.notifyInvestigatorCompletion(agent.StateFinished, "")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(bus.CopyBodies()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	bodies := bus.CopyBodies()
	if len(bodies) != 1 {
		t.Fatalf("want 1 delivery for empty-text finished, got %d", len(bodies))
	}
	body := bodies[0]
	if !strings.Contains(body, "Investigation complete") {
		t.Errorf("empty-text completion notice missing expected phrase; got: %s", body)
	}
}

// TestNotifyInvestigatorCompletion_ErrorState verifies that an error-state
// completion delivers a notification containing the failure state name.
func TestNotifyInvestigatorCompletion_ErrorState(t *testing.T) {
	invokerSession := "prism-test@invoker-errorstate"
	investigatorSession := invokerSession + "~investigate-errslug"

	bus := sidecartest.NewIsolated(t, invokerSession)

	clk := newTestClock()
	cfg := Config{
		SessionName: investigatorSession,
		Repo:        "prism-test",
		Worktree:    "/tmp/investigate-test-error",
		DB:          bus.DB,
		Clock:       clk,
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
	}
	s := New(cfg)

	s.notifyInvestigatorCompletion(agent.StateError, "partial findings before crash")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(bus.CopyBodies()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	bodies := bus.CopyBodies()
	if len(bodies) != 1 {
		t.Fatalf("want 1 delivery for error state, got %d", len(bodies))
	}
	body := bodies[0]
	if !strings.Contains(body, string(agent.StateError)) {
		t.Errorf("error-state notification missing state name; got: %s", body)
	}
	if !strings.Contains(body, "partial findings before crash") {
		t.Errorf("error-state notification missing last output; got: %s", body)
	}
}

// TestNotifyInvestigatorCompletion_InvokerEnded verifies that the notification
// is dropped silently (no panic, no delivery) when the invoker session has ended.
func TestNotifyInvestigatorCompletion_InvokerEnded(t *testing.T) {
	invokerSession := "prism-test@invoker-ended"
	investigatorSession := invokerSession + "~investigate-ended"

	// NewIsolated with empty invoker — we seed an ended row manually.
	bus := sidecartest.NewIsolated(t, "")

	seedEndedSessionInvestigate(t, bus.DB, invokerSession, "prism-test")

	clk := newTestClock()
	cfg := Config{
		SessionName: investigatorSession,
		Repo:        "prism-test",
		Worktree:    "/tmp/investigate-test-ended",
		DB:          bus.DB,
		Clock:       clk,
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
	}
	s := New(cfg)

	s.notifyInvestigatorCompletion(agent.StateFinished, "some findings here")
	time.Sleep(50 * time.Millisecond)

	if bodies := bus.CopyBodies(); len(bodies) > 0 {
		t.Errorf("expected no delivery when invoker has ended, got %d", len(bodies))
	}
}

// TestNotifyInvestigatorCompletion_NotInvestigateSession verifies that a session
// NOT named with ~investigate does NOT trigger any delivery.
func TestNotifyInvestigatorCompletion_NotInvestigateSession(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")

	clk := newTestClock()
	cfg := Config{
		// A plain worker session — no ~investigate in the name.
		SessionName: "prism-test@feature",
		Repo:        "prism-test",
		Worktree:    "/tmp/investigate-test-not-investigate",
		DB:          bus.DB,
		Clock:       clk,
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
	}
	s := New(cfg)

	s.notifyInvestigatorCompletion(agent.StateFinished, "some text that should not be delivered")
	time.Sleep(50 * time.Millisecond)

	if bodies := bus.CopyBodies(); len(bodies) > 0 {
		t.Errorf("expected no delivery for non-investigate session, got %d", len(bodies))
	}
}

// TestNotifyCoordinator_InvestigateAgentSuppressed verifies that an
// investigate-agent session does NOT emit a bare "has finished" notification
// to the coordinator.
func TestNotifyCoordinator_InvestigateAgentSuppressed(t *testing.T) {
	// Use a test-prefixed coordinator session that cannot collide with any
	// live coordinator on the host.
	coordSession := "prism-test@coordinator-suppressed"
	investigatorSession := coordSession + "~investigate-abc"

	bus := sidecartest.NewIsolated(t, coordSession)

	// Mark the coordinator row so CoordinatorForRepo can find it.
	if err := bus.DB.QueryRow(
		"UPDATE agent_status SET root_agent_name = 'coordinator' WHERE session_name = ? RETURNING session_name",
		coordSession,
	).Scan(new(string)); err != nil {
		t.Fatalf("set root_agent_name: %v", err)
	}

	clk := newTestClock()
	cfg := Config{
		SessionName: investigatorSession,
		Repo:        "prism-test",
		Worktree:    "/tmp/investigate-coord-suppressed",
		DB:          bus.DB,
		Clock:       clk,
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
	}
	s := New(cfg)

	// Directly call notifyCoordinator — it should be suppressed.
	s.notifyCoordinator()

	time.Sleep(100 * time.Millisecond)

	if bodies := bus.CopyBodies(); len(bodies) != 0 {
		t.Errorf("investigate-agent should not emit bare_finish notification; got %d notifications", len(bodies))
	}
}

// TestNotifyInvestigatorCompletion_ConcurrentDelivery verifies that two
// concurrent investigator sessions can deliver completion notifications to the
// same invoker without mangling each other's sender labels.
func TestNotifyInvestigatorCompletion_ConcurrentDelivery(t *testing.T) {
	invokerSession := "prism-test@invoker-concurrent"
	investigator1 := invokerSession + "~investigate-alpha"
	investigator2 := invokerSession + "~investigate-beta"

	bus := sidecartest.NewIsolated(t, invokerSession)

	clk := newTestClock()
	cfg1 := Config{
		SessionName: investigator1,
		Repo:        "prism-test",
		Worktree:    "/tmp/investigate-concurrent-1",
		DB:          bus.DB,
		Clock:       clk,
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
	}
	cfg2 := Config{
		SessionName: investigator2,
		Repo:        "prism-test",
		Worktree:    "/tmp/investigate-concurrent-2",
		DB:          bus.DB,
		Clock:       clk,
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
	}
	s1 := New(cfg1)
	s2 := New(cfg2)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s1.notifyInvestigatorCompletion(agent.StateFinished, "alpha findings") }()
	go func() { defer wg.Done(); s2.notifyInvestigatorCompletion(agent.StateFinished, "beta findings") }()
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(bus.CopyBodies()) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	bodies := bus.CopyBodies()
	if len(bodies) != 2 {
		t.Fatalf("want 2 deliveries, got %d", len(bodies))
	}

	// Each body must contain the correct sender label.
	foundAlpha, foundBeta := false, false
	for _, body := range bodies {
		if strings.Contains(body, "From investigator session: "+investigator1) {
			foundAlpha = true
		}
		if strings.Contains(body, "From investigator session: "+investigator2) {
			foundBeta = true
		}
	}
	if !foundAlpha {
		t.Errorf("alpha investigator label not found in any delivery; bodies: %v", bodies)
	}
	if !foundBeta {
		t.Errorf("beta investigator label not found in any delivery; bodies: %v", bodies)
	}
}

// TestInvestigatorNoIntermediatePings verifies that intermediate turn_end
// events on an investigate-agent session do NOT produce notifications to the
// invoker — only completion should trigger delivery.
//
// This is the regression test for issue #1580: the old code called
// notifyInvestigatorTurnEnd on every turn_end, flooding the coordinator with
// noise notifications. The new code accumulates the text and fires exactly
// once at terminal state.
func TestInvestigatorNoIntermediatePings(t *testing.T) {
	invokerSession := "prism-test@invoker-nopings"
	investigatorSession := invokerSession + "~investigate-nopings"

	bus := sidecartest.NewIsolated(t, invokerSession)

	clk := newTestClock()
	cfg := Config{
		SessionName: investigatorSession,
		Repo:        "prism-test",
		Worktree:    "/tmp/investigate-nopings",
		DB:          bus.DB,
		Clock:       clk,
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
	}
	s := New(cfg)

	// Simulate 5 intermediate turns by directly updating lastInvestigatorText
	// (as the sidecar event loop does on turn_end), without calling any notify
	// function. None of these should produce deliveries.
	for i := 0; i < 5; i++ {
		s.mu.Lock()
		s.lastInvestigatorText = "intermediate turn text"
		s.mu.Unlock()
	}

	// No deliveries should have happened yet.
	time.Sleep(50 * time.Millisecond)
	if n := len(bus.CopyBodies()); n != 0 {
		t.Errorf("expected 0 deliveries after intermediate turns, got %d", n)
	}

	// Now fire completion — exactly one delivery should occur.
	s.mu.Lock()
	s.lastInvestigatorText = "final report text"
	finalText := s.lastInvestigatorText
	s.mu.Unlock()
	s.notifyInvestigatorCompletion(agent.StateFinished, finalText)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(bus.CopyBodies()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if n := len(bus.CopyBodies()); n != 1 {
		t.Errorf("expected exactly 1 delivery at completion, got %d", n)
	}
}

// TestNotifyInvestigatorCompletion_NoHostBusLeak asserts that constructing a
// sidecar with a session name that looks like a real coordinator
// ("nixos-config@main~investigate-leakcheck") against an isolated DB + bus
// cannot touch the real host prism state, and that the delivery lands on the
// isolated httptest.Server.
//
// This is the "defence in depth" test for issue #1608: it verifies that the
// isolation invariants hold even when the test session name collides with a
// live coordinator slug.
//
// Isolation is asserted BY CONSTRUCTION — every path the sidecar can write
// through (resolved DB path, host-API socket path, $XDG_STATE_HOME-derived
// resolution) must reside under the test-scoped XDG_STATE_HOME tempdir and
// must not reside under the real host state dir. Earlier versions of this
// test instead observed host quiescence (snapshotting the real prism/run/
// directory and probing the real prism.db mtime before/after the test). On a
// multi-worker host those probes raced live sidecars: any concurrent session
// writing the real prism.db during the test window tripped the mtime check,
// a false positive that asserted "the host is quiet", not "this test is
// isolated" (issue #2227). Path-comparison assertions are race-free and hold
// regardless of concurrent host activity.
func TestNotifyInvestigatorCompletion_NoHostBusLeak(t *testing.T) {
	// Capture the real XDG_STATE_HOME *before* NewIsolated redirects it, so we
	// can assert nothing resolves under it. It may legitimately be unset (CI,
	// nix sandbox where HOME=/homeless-shelter — issue #1857); the
	// by-construction assertions below don't need a real host path to exist,
	// so the real-path comparisons are simply skipped in that case rather than
	// falling back to UserHomeDir.
	realXDGStateHome := os.Getenv("XDG_STATE_HOME")

	// Use a session name that matches a real coordinator slug — this is the
	// exact scenario that caused the observed leak in issue #1608. The
	// investigatorSession parses to invokerSession="nixos-config@main", which
	// is what collided with the live coordinator on the host. We seed that
	// session in the ISOLATED DB so the delivery path exercises the same code
	// as the original bug — but routes to our test server, not the real bus.
	invokerSession := "nixos-config@main"
	investigatorSession := "nixos-config@main~investigate-leakcheck"

	// NewIsolated redirects XDG_STATE_HOME and sets the isolation guard.
	// We seed "nixos-config@main" into the isolated DB so that delivery is
	// attempted and we can verify it lands on the httptest.Server rather than
	// the real host coordinator socket.
	bus := sidecartest.NewIsolated(t, invokerSession)

	clk := newTestClock()
	cfg := Config{
		SessionName: investigatorSession,
		Repo:        "prism-test",
		Worktree:    "/tmp/investigate-leakcheck",
		DB:          bus.DB,
		Clock:       clk,
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
	}
	s := New(cfg)

	// ── Isolation by construction (#2227) ──────────────────────────────────

	// The env redirect must be in effect: any code that resolves prism paths
	// from $XDG_STATE_HOME during this test lands in the tempdir.
	if got := os.Getenv("XDG_STATE_HOME"); got != bus.XDGStateHome {
		t.Fatalf("XDG_STATE_HOME = %q, want test-scoped %q — NewIsolated env redirect not in effect", got, bus.XDGStateHome)
	}
	if realXDGStateHome != "" && bus.XDGStateHome == realXDGStateHome {
		t.Fatalf("test-scoped XDG_STATE_HOME equals the real host value %q — no isolation", realXDGStateHome)
	}

	// The sidecar's resolved DB path must reside under the test-scoped
	// XDG_STATE_HOME. This is the by-construction replacement for the old
	// real-prism.db mtime probe.
	if dbPath := cfg.DB.Path(); !pathWithin(dbPath, bus.XDGStateHome) {
		t.Errorf("test sidecar DB path %q resolves outside the test-scoped XDG_STATE_HOME %q — DB isolation breach", dbPath, bus.XDGStateHome)
	} else if realXDGStateHome != "" && pathWithin(dbPath, realXDGStateHome) {
		t.Errorf("test sidecar DB path %q resolves under the real host XDG_STATE_HOME %q — DB isolation breach", dbPath, realXDGStateHome)
	}

	// The host-API socket the bus listens on (the socket-pipe delivery path)
	// must also reside under the test-scoped XDG_STATE_HOME — the
	// by-construction replacement for the old real prism/run/ snapshot.
	if !pathWithin(bus.SockPath, bus.XDGStateHome) {
		t.Errorf("host-API socket path %q resolves outside the test-scoped XDG_STATE_HOME %q — socket isolation breach", bus.SockPath, bus.XDGStateHome)
	} else if realXDGStateHome != "" && pathWithin(bus.SockPath, realXDGStateHome) {
		t.Errorf("host-API socket path %q resolves under the real host XDG_STATE_HOME %q — socket isolation breach", bus.SockPath, realXDGStateHome)
	}

	// ── Delivery routing ────────────────────────────────────────────────────

	s.notifyInvestigatorCompletion(agent.StateFinished, "leakcheck findings")

	// Assert: all HTTP traffic went to the isolated httptest.Server.
	// The delivery must have been captured — not dropped silently.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(bus.CopyBodies()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	bodies := bus.CopyBodies()
	if len(bodies) == 0 {
		t.Error("expected delivery to be captured by the isolated httptest.Server, got none — delivery was either dropped or escaped to host")
		return
	}
	found := false
	for _, body := range bodies {
		if strings.Contains(body, "leakcheck findings") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("delivery bodies did not contain expected text 'leakcheck findings'; bodies: %v", bodies)
	}
}

// ── test DB helpers ──────────────────────────────────────────────────────────

// seedEndedSession seeds an agent_status row with ended_at set.
// This is used by TestNotifyInvestigatorCompletion_InvokerEnded to set up an
// invoker session that has already finished — delivery must be dropped.
func seedEndedSessionInvestigate(t *testing.T, database *db.DB, sessionName, repo string) {
	t.Helper()
	agentName := "coordinator"
	modelID := "anthropic/claude-sonnet-4-5"
	if err := database.UpsertStatusWithAgent(sessionName, repo, "/tmp/test-worktree", "finished", nil, nil, &agentName, &modelID); err != nil {
		t.Fatalf("seedEndedSessionInvestigate: UpsertStatusWithAgent(%q): %v", sessionName, err)
	}
	// Use Unix millisecond timestamp so SQLite can scan it as int64.
	nowMs := time.Now().UnixMilli()
	if err := database.QueryRow(
		"UPDATE agent_status SET ended_at = ? WHERE session_name = ? RETURNING session_name",
		nowMs, sessionName,
	).Scan(new(string)); err != nil {
		t.Fatalf("seedEndedSessionInvestigate: set ended_at for %q: %v", sessionName, err)
	}
}

// ── OS helpers ────────────────────────────────────────────────────────────────

// pathWithin reports whether path resides inside dir (or equals it). Pure
// lexical comparison — no filesystem access, so it cannot race concurrent
// host activity (#2227).
func pathWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
