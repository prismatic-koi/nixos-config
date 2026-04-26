package session

// Tests for SpawnSession's integrated readiness gate (#1051 AC-14). The
// gate fires when SpawnOpts.ReadinessTimeout > 0 and waits for a readiness
// signal in the DB; on timeout it cleans up the half-alive session and
// returns *ReadinessTimeoutError.
//
// We cannot exercise the full happy path here without a real sidecar
// (which would write the state_change event the gate looks for), so the
// test that proves the gate is wired in correctly is the timeout path:
//
//   - With ReadinessTimeout > 0 set and no signal arriving, SpawnSession
//     returns a *ReadinessTimeoutError after the configured deadline AND
//     cleans up the half-alive DB row.
//   - With ReadinessTimeout = 0, SpawnSession returns immediately after
//     tmux/sidecar setup (legacy behaviour, used by the review fan-out
//     which runs WaitForReady itself in goroutines).
//
// The full happy path (with a real sidecar emitting state_change events) is
// covered indirectly by the existing TestSpawnSession_AgentOnly_*
// tests — they call SpawnSession with ReadinessTimeout=0 (default), so the
// gate is bypassed and the existing assertions still hold.

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestSpawnSession_ReadinessTimeout_FiresAndCleansUp verifies AC-14:
// SpawnSession runs the readiness gate when ReadinessTimeout > 0. Since no
// sidecar is actually writing state_change events in this test, the gate
// should trip on timeout, return *ReadinessTimeoutError, and clean up the
// half-alive session (port released, ended_at set) so a second spawn with
// the same name does not see stale state.
func TestSpawnSession_ReadinessTimeout_FiresAndCleansUp(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@branch~ready-timeout"
	opts := SpawnOpts{
		SessionName:      sessionName,
		Repo:             "myrepo",
		Worktree:         "/worktrees/myrepo-branch",
		AgentRole:        "review-code",
		Layout:           LayoutAgentOnly,
		ReadinessTimeout: 300 * time.Millisecond, // short; no signal will arrive
	}

	start := time.Now()
	err := SpawnSession(d, opts)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("SpawnSession: got nil error, want *ReadinessTimeoutError (no sidecar to signal readiness)")
	}
	if !IsReadinessTimeout(err) {
		t.Errorf("SpawnSession error = %v (type %T), want *ReadinessTimeoutError", err, err)
	}
	// Sanity-check the timing — gate must wait at least the configured
	// deadline before tripping.
	if elapsed < 300*time.Millisecond {
		t.Errorf("SpawnSession returned in %v, expected at least 300ms (the readiness window)", elapsed)
	}

	// Cleanup verification: the agent_status row should now show ended_at
	// (cleanupHalfAliveSession calls SetEnded). Either nil status or a
	// row with EndedAt != nil is acceptable evidence of cleanup.
	st, _ := d.CurrentStatus(sessionName)
	if st != nil && st.EndedAt == nil {
		t.Errorf("agent_status row %q is alive after readiness-timeout cleanup; ended_at should be set", sessionName)
	}
}

// TestSpawnSession_ReadinessTimeoutZero_SkipsGate verifies the legacy
// behaviour: with ReadinessTimeout = 0, SpawnSession does NOT run the gate
// and returns success as soon as tmux/sidecar setup completes. This is the
// path the review fan-out uses (it runs WaitForReady itself in goroutines).
func TestSpawnSession_ReadinessTimeoutZero_SkipsGate(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@branch~ready-skip"
	opts := SpawnOpts{
		SessionName: sessionName,
		Repo:        "myrepo",
		Worktree:    "/worktrees/myrepo-branch",
		AgentRole:   "review-code",
		Layout:      LayoutAgentOnly,
		// ReadinessTimeout omitted (zero) — gate must be skipped.
	}

	start := time.Now()
	err := SpawnSession(d, opts)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("SpawnSession with zero ReadinessTimeout: got %v, want nil", err)
	}
	// With the gate skipped, the call must return promptly — well under
	// the DefaultReadinessTimeout (30s).
	if elapsed > 5*time.Second {
		t.Errorf("SpawnSession took %v with ReadinessTimeout=0; expected prompt return without polling for readiness", elapsed)
	}
}

// TestSpawnSession_WritesStartupLog verifies AC-3: a per-session
// agent-startup.log is created during spawn, with at least one line
// containing the spawn-session breadcrumbs.
func TestSpawnSession_WritesStartupLog(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@branch~startup-log"
	opts := SpawnOpts{
		SessionName: sessionName,
		Repo:        "myrepo",
		Worktree:    "/worktrees/myrepo-branch",
		AgentRole:   "review-code",
		Layout:      LayoutAgentOnly,
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	// AC-3: the log file must exist after spawn.
	if !AgentStartupLogExists(sessionName) {
		t.Fatal("AgentStartupLogExists returned false after SpawnSession")
	}

	// And it must contain breadcrumbs: at minimum the begin line and a port
	// allocation line.
	path, _ := AgentStartupLogPath(sessionName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	contents := string(data)
	if !strings.Contains(contents, "spawn-session: begin") {
		t.Errorf("startup log missing 'spawn-session: begin' breadcrumb:\n%s", contents)
	}
	if !strings.Contains(contents, "agent_status seeded") {
		t.Errorf("startup log missing 'agent_status seeded' breadcrumb:\n%s", contents)
	}
	if !strings.Contains(contents, "allocated port") {
		t.Errorf("startup log missing 'allocated port' breadcrumb:\n%s", contents)
	}
	if !strings.Contains(contents, "tmux session and sidecar kicked off") {
		t.Errorf("startup log missing 'tmux session and sidecar kicked off' breadcrumb:\n%s", contents)
	}
}

// TestSpawnSession_ReadinessTimeoutWritesFailureToStartupLog verifies that
// when the readiness gate trips, the failure is recorded in the startup
// log so the operator has a forensic trail.
func TestSpawnSession_ReadinessTimeoutWritesFailureToStartupLog(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@branch~startup-fail"
	opts := SpawnOpts{
		SessionName:      sessionName,
		Repo:             "myrepo",
		Worktree:         "/worktrees/myrepo-branch",
		AgentRole:        "review-code",
		Layout:           LayoutAgentOnly,
		ReadinessTimeout: 250 * time.Millisecond,
	}

	if err := SpawnSession(d, opts); err == nil {
		t.Fatal("SpawnSession: got nil, want timeout error")
	}

	path, _ := AgentStartupLogPath(sessionName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	contents := string(data)
	if !strings.Contains(contents, "waiting for readiness") {
		t.Errorf("startup log missing 'waiting for readiness' breadcrumb:\n%s", contents)
	}
	if !strings.Contains(contents, "readiness gate FAILED") {
		t.Errorf("startup log missing 'readiness gate FAILED' breadcrumb:\n%s", contents)
	}
}
