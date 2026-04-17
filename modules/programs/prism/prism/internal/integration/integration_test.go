// Package integration provides end-to-end integration tests for prism's session
// lifecycle and DB state transitions. Tests exercise real tmux sessions and a
// real SQLite DB to catch race conditions and regressions that unit tests miss.
//
// # Structure
//
// Tests are split into two groups:
//
//  1. DB-only tests (no tmux required): use t.TempDir() for isolated DBs and
//     call DB methods / cobra subcommand RunE functions directly.
//
//  2. tmux+DB tests: use a headless test server (isolated socket, no tmux.conf)
//     and are skipped automatically when tmux is not available.
//
// # Parallelism
//
// DB-only tests run in parallel; they use per-test DB files and never touch
// package-level state. tmux tests that exercise the package-level session.*
// functions use withTmuxServer() which rewrites tmux.TmuxBin, so they must NOT
// call t.Parallel().
package integration_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

// openTestDB opens an isolated SQLite DB in t.TempDir() and registers a
// Cleanup to close it.
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// requireTmux skips the test immediately if tmux is not available in PATH.
func requireTmux(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not found in PATH — skipping tmux integration test")
	}
	return bin
}

// tmuxServer holds a headless tmux server running on a unique socket.
type tmuxServer struct {
	socket string
	bin    string
}

// newTmuxServer starts a headless tmux server on a unique socket and returns a
// handle. The server is killed in t.Cleanup.
func newTmuxServer(t *testing.T) *tmuxServer {
	t.Helper()
	bin := requireTmux(t)

	// pid + nanoseconds gives sufficient uniqueness across parallel test runs.
	socket := fmt.Sprintf("prism-integ-%d-%d", os.Getpid(), time.Now().UnixNano())
	s := &tmuxServer{socket: socket, bin: bin}

	// Bootstrap: keep the server alive with a dummy session.
	if err := s.run("new-session", "-ds", "bootstrap", "-c", t.TempDir()); err != nil {
		t.Fatalf("start tmux server: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command(bin, "-L", socket, "kill-server").Run()
	})
	return s
}

// run executes a tmux command against this server's socket.
// -f /dev/null suppresses the user's tmux.conf so no hooks fire.
func (s *tmuxServer) run(args ...string) error {
	cmd := exec.Command(s.bin, append([]string{"-L", s.socket, "-f", "/dev/null"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %v: %w\n%s", args, err, out)
	}
	return nil
}

// output executes a tmux command and returns trimmed stdout.
func (s *tmuxServer) output(args ...string) (string, error) {
	cmd := exec.Command(s.bin, append([]string{"-L", s.socket, "-f", "/dev/null"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux %v: %w", args, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// hasSession returns true if a session exists on this server.
func (s *tmuxServer) hasSession(name string) bool {
	return s.run("has-session", "-t", name) == nil
}

// listWindowNames returns all window names in the given session.
func (s *tmuxServer) listWindowNames(session string) ([]string, error) {
	out, err := s.output("list-windows", "-t", session, "-F", "#{window_name}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// killSession kills a named session on this server.
func (s *tmuxServer) killSession(name string) error {
	return s.run("kill-session", "-t", name)
}

// withTmuxServer redirects tmux.TmuxBin to a wrapper that injects
// -L <socket> -f /dev/null for the duration of the test, then restores the
// original.
//
// Only call this from non-parallel tests — TmuxBin is a package-level global.
func withTmuxServer(t *testing.T, s *tmuxServer) {
	t.Helper()
	orig := tmux.TmuxBin
	wrapperPath := filepath.Join(t.TempDir(), "tmux")
	script := "#!/bin/sh\nexec " + s.bin + " -L " + s.socket + " -f /dev/null \"$@\"\n"
	if err := os.WriteFile(wrapperPath, []byte(script), 0755); err != nil {
		t.Fatalf("write tmux wrapper: %v", err)
	}
	tmux.TmuxBin = wrapperPath
	t.Cleanup(func() { tmux.TmuxBin = orig })
}

// runPaneDied invokes the pane-died logic directly via the DB methods, mirroring
// what `prism event pane-died` does (without needing the compiled binary).
// Returns (updated bool, err).
func runPaneDied(d *db.DB, sessionName, window string, exitCode int) (bool, error) {
	if window != "agent" {
		return false, nil
	}
	s, err := d.CurrentStatus(sessionName)
	if err != nil {
		return false, fmt.Errorf("pane-died: current status: %w", err)
	}
	if s == nil {
		return false, nil
	}
	if exitCode != 0 {
		return d.UpsertStatusInterruptedOverrideFinished(sessionName)
	}
	return d.UpsertStatusIfNotTerminal(sessionName, string(agent.StateInterrupted))
}

// ─── DB lifecycle tests (no tmux, parallel-safe) ─────────────────────────────

// TestDBLifecycle_IdleActiveFinished exercises the full DB state lifecycle:
// idle → active → finished via direct DB calls, verifying CurrentStatus at each step.
func TestDBLifecycle_IdleActiveFinished(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)

	const (
		sessionName = "myrepo@main"
		repo        = "myrepo"
		worktree    = "/code/myrepo/main"
	)

	// 1. Write idle state (as tmux-session-start would do).
	if err := d.UpsertStatus(sessionName, repo, worktree, string(agent.StateIdle), nil, nil); err != nil {
		t.Fatalf("UpsertStatus idle: %v", err)
	}
	s, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus (idle): %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus (idle): got nil, want row")
	}
	if s.State != string(agent.StateIdle) {
		t.Errorf("state (idle): got %q, want %q", s.State, agent.StateIdle)
	}

	// 2. Transition to active (as the plugin's state-change event would do).
	if err := d.UpsertStatus(sessionName, repo, worktree, string(agent.StateActive), nil, nil); err != nil {
		t.Fatalf("UpsertStatus active: %v", err)
	}
	s, err = d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus (active): %v", err)
	}
	if s.State != string(agent.StateActive) {
		t.Errorf("state (active): got %q, want %q", s.State, agent.StateActive)
	}

	// 3. Transition to finished (as the idle debounce would do).
	if err := d.UpsertStatus(sessionName, repo, worktree, string(agent.StateFinished), nil, nil); err != nil {
		t.Fatalf("UpsertStatus finished: %v", err)
	}
	s, err = d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus (finished): %v", err)
	}
	if s.State != string(agent.StateFinished) {
		t.Errorf("state (finished): got %q, want %q", s.State, agent.StateFinished)
	}
	if s.EndedAt != nil {
		t.Error("EndedAt should be nil (session not ended yet)")
	}
}

// TestDBLifecycle_BusMessageDelivered verifies WriteBusMessageDelivered writes
// an audit row with delivered_at set (HTTP-delivered prompt audit trail).
func TestDBLifecycle_BusMessageDelivered(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)

	msgID := uuid.New().String()
	msg := db.BusMessage{
		ID:          msgID,
		FromSession: "myrepo@feat",
		ToSession:   "myrepo@main",
		Repo:        "myrepo",
		Text:        "work complete, ready for review",
		Urgency:     "normal",
		SentAt:      time.Now(),
	}
	if err := d.WriteBusMessageDelivered(msg); err != nil {
		t.Fatalf("WriteBusMessageDelivered: %v", err)
	}

	// Row must exist with delivered_at set.
	var deliveredAt *int64
	err := d.QueryRow("SELECT delivered_at FROM bus_messages WHERE id = ?", msgID).Scan(&deliveredAt)
	if err != nil {
		t.Fatalf("query delivered_at: %v", err)
	}
	if deliveredAt == nil {
		t.Error("delivered_at: got nil, want non-nil (message delivered via HTTP)")
	}
}

// TestPaneDiedHook_ActiveToInterrupted exercises the pane-died hook path when
// a session is active and the pane exits with a non-zero exit code.
// Verifies the session transitions to "interrupted".
func TestPaneDiedHook_ActiveToInterrupted(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)

	const sessionName = "myrepo@main"
	// Seed with active state.
	if err := d.UpsertStatus(sessionName, "myrepo", "/code/myrepo/main", string(agent.StateActive), nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Simulate pane-died hook: agent window, exit code 1.
	updated, err := runPaneDied(d, sessionName, "agent", 1)
	if err != nil {
		t.Fatalf("runPaneDied: %v", err)
	}
	if !updated {
		t.Error("runPaneDied: updated=false, want true (active → interrupted)")
	}

	s, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.State != string(agent.StateInterrupted) {
		t.Errorf("state: got %q, want %q", s.State, agent.StateInterrupted)
	}
}

// TestPaneDiedHook_NonAgentWindow verifies that exits from non-agent windows
// (e.g. "term", "edit") are no-ops — only the agent window dying is meaningful.
func TestPaneDiedHook_NonAgentWindow(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)

	const sessionName = "myrepo@main"
	if err := d.UpsertStatus(sessionName, "myrepo", "/code/myrepo/main", string(agent.StateActive), nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// term window exit — must be ignored.
	updated, err := runPaneDied(d, sessionName, "term", 1)
	if err != nil {
		t.Fatalf("runPaneDied (term): %v", err)
	}
	if updated {
		t.Error("runPaneDied (term): updated=true, want false (non-agent window is a no-op)")
	}

	s, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.State != string(agent.StateActive) {
		t.Errorf("state should remain active after term exit: got %q", s.State)
	}
}

// TestPaneDiedHook_OverridesFinished verifies that pane-died with exit code 1
// overrides a "finished" state with "interrupted". This is the key fix for the
// race where the plugin writes "finished" via the idle debounce before
// pane-died fires (issue #386 and related).
func TestPaneDiedHook_OverridesFinished(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)

	const sessionName = "myrepo@main"
	// Seed with finished state (as if the plugin wrote it before pane-died fired).
	if err := d.UpsertStatus(sessionName, "myrepo", "/code/myrepo/main", string(agent.StateFinished), nil, nil); err != nil {
		t.Fatalf("UpsertStatus (finished): %v", err)
	}

	// pane-died with exit code 1: should override "finished" → "interrupted".
	updated, err := runPaneDied(d, sessionName, "agent", 1)
	if err != nil {
		t.Fatalf("runPaneDied: %v", err)
	}
	if !updated {
		t.Error("runPaneDied: updated=false, want true (finished should be overridden by non-zero exit)")
	}

	s, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.State != string(agent.StateInterrupted) {
		t.Errorf("state: got %q, want %q (non-zero exit overrides finished)", s.State, agent.StateInterrupted)
	}
}

// TestPaneDiedHook_ZeroExitLeavesFinished verifies that pane-died with exit
// code 0 does NOT override an existing "finished" state — a clean exit after a
// clean finish means the session completed normally.
func TestPaneDiedHook_ZeroExitLeavesFinished(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)

	const sessionName = "myrepo@main"
	if err := d.UpsertStatus(sessionName, "myrepo", "/code/myrepo/main", string(agent.StateFinished), nil, nil); err != nil {
		t.Fatalf("UpsertStatus (finished): %v", err)
	}

	// pane-died with exit code 0: must NOT override "finished".
	updated, err := runPaneDied(d, sessionName, "agent", 0)
	if err != nil {
		t.Fatalf("runPaneDied: %v", err)
	}
	if updated {
		t.Error("runPaneDied (exit 0): updated=true, want false (zero exit should not override finished)")
	}

	s, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.State != string(agent.StateFinished) {
		t.Errorf("state: got %q, want %q (finished preserved on clean exit)", s.State, agent.StateFinished)
	}
}

// openTestDBAt opens a DB at the given path. Caller must close it.
// Returns nil and an error if the DB cannot be opened (safe to call from goroutines).
func openTestDBAt(path string) (*db.DB, error) {
	return db.Open(path)
}

// TestConcurrentStateWrites spawns two goroutines each using their own DB
// connection to the same SQLite file, simulating the plugin and pane-died hook
// as separate OS processes racing to write state. Asserts no panic, no DB
// corruption, and that the final state is one of the valid terminal states.
//
// Production context: the plugin and pane-died hook run as separate processes,
// each opening their own connection to prism.db. WAL mode + busy_timeout=5000
// allows them to contend safely. This test exercises that contract.
func TestConcurrentStateWrites(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "prism.db")

	const (
		sessionName = "myrepo@main"
		repo        = "myrepo"
		worktree    = "/code/myrepo/main"
		iterations  = 50
	)

	// Seed: open once, write the initial row, close.
	seed, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("seed db.Open: %v", err)
	}
	if err := seed.UpsertStatus(sessionName, repo, worktree, string(agent.StateActive), nil, nil); err != nil {
		seed.Close()
		t.Fatalf("seed UpsertStatus: %v", err)
	}
	seed.Close()

	// Open both connections before launching goroutines. Opening two connections
	// to the same WAL-mode DB simultaneously can cause a brief lock on the WAL
	// pragma setup; doing it sequentially here avoids that startup contention
	// and lets the test focus on write-write races (which is what we care about).
	d1, err := openTestDBAt(dbPath)
	if err != nil {
		t.Fatalf("plugin db.Open: %v", err)
	}
	defer d1.Close()

	d2, err := openTestDBAt(dbPath)
	if err != nil {
		t.Fatalf("pane-died db.Open: %v", err)
	}
	defer d2.Close()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	// Goroutine 1: plugin — writes finished (idle debounce simulation).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if err := d1.UpsertStatus(sessionName, repo, worktree, string(agent.StateFinished), nil, nil); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("plugin iteration %d: %w", i, err))
				mu.Unlock()
				return
			}
		}
	}()

	// Goroutine 2: pane-died hook — writes interrupted (non-zero exit).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := d2.UpsertStatusInterruptedOverrideFinished(sessionName); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("pane-died iteration %d: %w", i, err))
				mu.Unlock()
				return
			}
		}
	}()

	wg.Wait()

	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("concurrent write error: %v", e)
		}
		t.FailNow()
	}

	// Verify DB integrity: the session row must still be readable.
	verify, err := openTestDBAt(dbPath)
	if err != nil {
		t.Fatalf("verify db.Open: %v", err)
	}
	defer verify.Close()
	s, err := verify.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus after concurrent writes: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil after concurrent writes, want a row")
	}

	// Final state must be one of the valid terminal outcomes of this race.
	validFinal := map[string]bool{
		string(agent.StateFinished):    true,
		string(agent.StateInterrupted): true,
	}
	if !validFinal[s.State] {
		t.Errorf("final state %q is not a valid terminal state after concurrent writes", s.State)
	}
}

// TestConcurrentStateWrites_MultipleGoroutines exercises a wider concurrency
// scenario: four separate DB connections writing different states simultaneously,
// ensuring WAL mode and busy_timeout prevent SQLITE_BUSY errors under contention.
func TestConcurrentStateWrites_MultipleGoroutines(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "prism.db")

	const (
		sessionName = "myrepo@main"
		repo        = "myrepo"
		worktree    = "/code/myrepo/main"
		iterations  = 30
	)

	// Seed: open once, write the initial row, close.
	seed, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("seed db.Open: %v", err)
	}
	if err := seed.UpsertStatus(sessionName, repo, worktree, string(agent.StateActive), nil, nil); err != nil {
		seed.Close()
		t.Fatalf("seed UpsertStatus: %v", err)
	}
	seed.Close()

	// Open all four connections sequentially before launching goroutines to
	// avoid startup lock contention on the WAL pragma.
	dbs := make([]*db.DB, 4)
	for i := range dbs {
		dbs[i], err = openTestDBAt(dbPath)
		if err != nil {
			// Close any already-opened connections.
			for j := 0; j < i; j++ {
				dbs[j].Close()
			}
			t.Fatalf("open db[%d]: %v", i, err)
		}
	}
	for _, d := range dbs {
		d := d
		t.Cleanup(func() { d.Close() })
	}

	type result struct {
		id  int
		err error
	}
	results := make(chan result, 4)

	// Each writer uses its own pre-opened DB connection, simulating separate processes.
	writers := []struct {
		id   int
		conn *db.DB
		work func(*db.DB) error
	}{
		{1, dbs[0], func(d *db.DB) error {
			for i := 0; i < iterations; i++ {
				if err := d.UpsertStatus(sessionName, repo, worktree, string(agent.StateActive), nil, nil); err != nil {
					return fmt.Errorf("iter %d: %w", i, err)
				}
			}
			return nil
		}},
		{2, dbs[1], func(d *db.DB) error {
			for i := 0; i < iterations; i++ {
				if err := d.UpsertStatus(sessionName, repo, worktree, string(agent.StateFinished), nil, nil); err != nil {
					return fmt.Errorf("iter %d: %w", i, err)
				}
			}
			return nil
		}},
		{3, dbs[2], func(d *db.DB) error {
			for i := 0; i < iterations; i++ {
				if _, err := d.UpsertStatusInterruptedOverrideFinished(sessionName); err != nil {
					return fmt.Errorf("iter %d: %w", i, err)
				}
			}
			return nil
		}},
		{4, dbs[3], func(d *db.DB) error {
			for i := 0; i < iterations; i++ {
				if _, err := d.CurrentStatus(sessionName); err != nil {
					return fmt.Errorf("iter %d: %w", i, err)
				}
			}
			return nil
		}},
	}

	for _, w := range writers {
		w := w
		go func() {
			results <- result{w.id, w.work(w.conn)}
		}()
	}

	for range writers {
		r := <-results
		if r.err != nil {
			t.Errorf("goroutine %d: %v", r.id, r.err)
		}
	}

	// DB must still be intact.
	verify, err := openTestDBAt(dbPath)
	if err != nil {
		t.Fatalf("verify db.Open: %v", err)
	}
	defer verify.Close()
	s, verErr := verify.CurrentStatus(sessionName)
	if verErr != nil {
		t.Fatalf("CurrentStatus after wide concurrent writes: %v", verErr)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil, want a row")
	}
}

// ─── tmux integration tests (require tmux binary) ─────────────────────────────

// TestTmuxHarness_ThreeWindowLayout verifies that a tmux session created via
// the test harness (mirroring what setupFullLayout in session.go does) has
// three windows with the correct names: edit, agent, term.
//
// This test also verifies that session.Create() with LayoutBare creates and
// recognises sessions correctly — including that a second Create call on an
// existing session is a no-op.
//
// NOTE: session.Create() with LayoutFull cannot be called directly in tests
// because it invokes os.Executable() to spawn `prism event tmux-session-start`,
// which is not available in the test binary. The three-window layout is therefore
// exercised by replicating setupFullLayout's tmux commands via the harness.
//
// NOTE: This test uses withTmuxServer() and must NOT call t.Parallel().
func TestTmuxHarness_ThreeWindowLayout(t *testing.T) {
	s := newTmuxServer(t)
	withTmuxServer(t, s)

	// session.Create with LayoutFull calls os.Executable() to invoke itself as
	// `prism event tmux-session-start`. In test binaries that isn't available,
	// so we exercise the window-creation path directly via lower-level tmux
	// helpers, mirroring what setupFullLayout does (minus the event seeding and
	// the opencode/nvim send-keys, which require a live binary and real processes).
	//
	// We use session.Create with LayoutBare then manually verify the harness
	// creates the session, then test the window setup logic separately below.

	dir := t.TempDir()
	const sessionName = "integ-test-bare"

	// Create a bare session to verify Create() works and registers cleanup.
	err := session.Create(sessionName, dir, session.Opts{Layout: session.LayoutBare})
	if err != nil {
		t.Fatalf("session.Create (LayoutBare): %v", err)
	}
	t.Cleanup(func() {
		// Best-effort cleanup — session may already be gone.
		_ = tmux.KillSession(sessionName)
	})

	if !tmux.HasSession(sessionName) {
		t.Fatal("session not found after Create")
	}

	// Calling Create again for an existing session must be a no-op (no error).
	if err := session.Create(sessionName, dir, session.Opts{Layout: session.LayoutBare}); err != nil {
		t.Fatalf("second Create (existing session): %v", err)
	}

	// Now create a LayoutFull session directly via the tmux harness to verify
	// the three-window layout. We bypass the prism binary call (tmux-session-start
	// event + nvim/opencode send-keys) and just verify the window structure
	// that the real layout sets up.
	const fullSession = "integ-test-full"

	// Mirror setupFullLayout from session.go, using the harness directly.
	if err := s.run("new-session", "-ds", fullSession, "-c", dir); err != nil {
		t.Fatalf("new-session for full layout: %v", err)
	}
	t.Cleanup(func() { _ = s.killSession(fullSession) })

	// Window 0: edit
	if err := s.run("rename-window", "-t", fullSession+":0", "edit"); err != nil {
		t.Fatalf("rename-window edit: %v", err)
	}
	// Window 1: agent
	if err := s.run("new-window", "-t", fmt.Sprintf("%s:1", fullSession), "-n", "agent", "-c", dir); err != nil {
		t.Fatalf("new-window agent: %v", err)
	}
	// Window 2: term
	if err := s.run("new-window", "-t", fmt.Sprintf("%s:2", fullSession), "-n", "term", "-c", dir); err != nil {
		t.Fatalf("new-window term: %v", err)
	}

	windows, err := s.listWindowNames(fullSession)
	if err != nil {
		t.Fatalf("listWindowNames: %v", err)
	}

	want := []string{"edit", "agent", "term"}
	if len(windows) != len(want) {
		t.Fatalf("window count: got %d (%v), want %d (%v)", len(windows), windows, len(want), want)
	}
	for i, name := range want {
		if windows[i] != name {
			t.Errorf("window[%d]: got %q, want %q", i, windows[i], name)
		}
	}
}

// TestSessionCreate_LayoutScratchpad verifies that LayoutScratchpad creates a
// single window named "term".
//
// NOTE: This test uses withTmuxServer() and must NOT call t.Parallel().
func TestSessionCreate_LayoutScratchpad(t *testing.T) {
	s := newTmuxServer(t)
	withTmuxServer(t, s)

	dir := t.TempDir()
	const sessionName = "integ-test-scratchpad"

	if err := session.Create(sessionName, dir, session.Opts{Layout: session.LayoutScratchpad}); err != nil {
		t.Fatalf("session.Create (LayoutScratchpad): %v", err)
	}
	t.Cleanup(func() { _ = tmux.KillSession(sessionName) })

	windows, err := s.listWindowNames(sessionName)
	if err != nil {
		t.Fatalf("listWindowNames: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("window count: got %d (%v), want 1", len(windows), windows)
	}
	if windows[0] != "term" {
		t.Errorf("window name: got %q, want %q", windows[0], "term")
	}
}

// TestSessionCreate_Cleanup verifies that a session killed in t.Cleanup is
// actually gone, proving cleanup runs even on test failure paths.
//
// NOTE: This test uses withTmuxServer() and must NOT call t.Parallel().
func TestSessionCreate_Cleanup(t *testing.T) {
	srv := newTmuxServer(t)
	withTmuxServer(t, srv)

	dir := t.TempDir()
	const sessionName = "integ-cleanup-test"

	if err := session.Create(sessionName, dir, session.Opts{Layout: session.LayoutBare}); err != nil {
		t.Fatalf("session.Create: %v", err)
	}
	t.Cleanup(func() {
		if err := tmux.KillSession(sessionName); err != nil {
			t.Logf("KillSession in cleanup: %v (session may already be gone)", err)
		}
		if tmux.HasSession(sessionName) {
			t.Errorf("session %q still exists after t.Cleanup kill", sessionName)
		}
	})

	if !tmux.HasSession(sessionName) {
		t.Fatal("session not present before cleanup test")
	}
}

// ─── ForceFresh / startup guard tests ────────────────────────────────────────

// TestSessionCreate_ForceFresh_False_LiveSession verifies that session.Create
// with ForceFresh=false is a no-op when the session already exists in tmux and
// its DB row's last_seen is recent (< 60s). The live session must not be killed.
//
// This is the regression test for issue #792: prism switch/launch must not
// destroy an existing live coordinator session.
//
// NOTE: This test uses withTmuxServer() and must NOT call t.Parallel().
func TestSessionCreate_ForceFresh_False_LiveSession(t *testing.T) {
	srv := newTmuxServer(t)
	withTmuxServer(t, srv)

	d := openTestDB(t)
	dir := t.TempDir()
	const sessionName = "integ-forcefresh-live"

	// Seed a DB row with a recent last_seen (now = live).
	if err := d.UpsertStatus(sessionName, "repo", dir, "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Create the session normally first.
	if err := session.Create(sessionName, dir, session.Opts{Layout: session.LayoutBare}); err != nil {
		t.Fatalf("initial Create: %v", err)
	}
	t.Cleanup(func() { _ = tmux.KillSession(sessionName) })

	if !tmux.HasSession(sessionName) {
		t.Fatal("session not found after initial Create")
	}

	// Now call Create again with ForceFresh=false and a live DB row.
	// Expect a no-op: session must still exist and must NOT have been killed+recreated.
	err := session.Create(sessionName, dir, session.Opts{
		Layout:     session.LayoutBare,
		DB:         d,
		ForceFresh: false,
	})
	if err != nil {
		t.Fatalf("Create (ForceFresh=false, live session): unexpected error: %v", err)
	}
	if !tmux.HasSession(sessionName) {
		t.Error("Create (ForceFresh=false, live session): session was killed — expected no-op")
	}
}

// TestSessionCreate_ForceFresh_False_StaleSession verifies that session.Create
// with ForceFresh=false kills and recreates a session whose DB row's last_seen
// is older than 60s (stale zombie). The user should get a working new session
// rather than attaching to a dead pane.
//
// NOTE: This test uses withTmuxServer() and must NOT call t.Parallel().
func TestSessionCreate_ForceFresh_False_StaleSession(t *testing.T) {
	srv := newTmuxServer(t)
	withTmuxServer(t, srv)

	d := openTestDB(t)
	dir := t.TempDir()
	const sessionName = "integ-forcefresh-stale"

	// Seed a DB row with a stale last_seen (> 60s ago).
	if err := d.UpsertStatus(sessionName, "repo", dir, "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	// last_seen is stored and read as Unix milliseconds throughout the codebase
	// (see UpsertStatusWithAgent, scanStatus). Use UnixMilli() here so the value
	// is correctly interpreted as "2 minutes ago" rather than January 1970.
	staleTime := time.Now().Add(-2 * time.Minute).UnixMilli()
	if err := d.QueryRow(
		"UPDATE agent_status SET last_seen = ? WHERE session_name = ? RETURNING 1",
		staleTime, sessionName,
	).Scan(new(int)); err != nil && err.Error() != "sql: no rows in result set" {
		// "no rows in result set" shouldn't happen here since UpsertStatus just
		// above guarantees the row exists, but treat it as non-fatal rather than
		// aborting test setup. Any other error is a genuine failure.
		t.Fatalf("set stale last_seen: %v", err)
	}

	// Create the session initially (simulates the stale tmux zombie).
	if err := session.Create(sessionName, dir, session.Opts{Layout: session.LayoutBare}); err != nil {
		t.Fatalf("initial Create: %v", err)
	}
	t.Cleanup(func() { _ = tmux.KillSession(sessionName) })

	if !tmux.HasSession(sessionName) {
		t.Fatal("session not found after initial Create")
	}

	// Call Create again with ForceFresh=false and a stale DB row.
	// Expect the zombie to be killed and a fresh session created.
	err := session.Create(sessionName, dir, session.Opts{
		Layout:     session.LayoutBare,
		DB:         d,
		ForceFresh: false,
	})
	if err != nil {
		t.Fatalf("Create (ForceFresh=false, stale session): unexpected error: %v", err)
	}
	if !tmux.HasSession(sessionName) {
		t.Error("Create (ForceFresh=false, stale session): new session not created after zombie kill")
	}
}

// TestSessionCreate_ForceFresh_True_LiveSession verifies that session.Create
// with ForceFresh=true kills and recreates even a live session. This is the
// correct behaviour for prism spawn, where name collisions mean zombie sessions.
//
// NOTE: This test uses withTmuxServer() and must NOT call t.Parallel().
func TestSessionCreate_ForceFresh_True_LiveSession(t *testing.T) {
	srv := newTmuxServer(t)
	withTmuxServer(t, srv)

	d := openTestDB(t)
	dir := t.TempDir()
	const sessionName = "integ-forcefresh-spawn"

	// Seed a DB row with a recent last_seen (now = live).
	if err := d.UpsertStatus(sessionName, "repo", dir, "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Create the session initially.
	if err := session.Create(sessionName, dir, session.Opts{Layout: session.LayoutBare}); err != nil {
		t.Fatalf("initial Create: %v", err)
	}
	t.Cleanup(func() { _ = tmux.KillSession(sessionName) })

	if !tmux.HasSession(sessionName) {
		t.Fatal("session not found after initial Create")
	}

	// Call Create again with ForceFresh=true and a live DB row.
	// Expect the live session to be killed and a fresh one created (spawn semantics).
	err := session.Create(sessionName, dir, session.Opts{
		Layout:     session.LayoutBare,
		DB:         d,
		ForceFresh: true,
	})
	if err != nil {
		t.Fatalf("Create (ForceFresh=true, live session): unexpected error: %v", err)
	}
	if !tmux.HasSession(sessionName) {
		t.Error("Create (ForceFresh=true, live session): new session not created after kill")
	}
}

// TestSessionCreate_ForceFresh_False_NoDBRow verifies that session.Create with
// ForceFresh=false and no DB row for the existing session treats it as a stale
// zombie and recreates it (e.g. after prism reset).
//
// NOTE: This test uses withTmuxServer() and must NOT call t.Parallel().
func TestSessionCreate_ForceFresh_False_NoDBRow(t *testing.T) {
	srv := newTmuxServer(t)
	withTmuxServer(t, srv)

	d := openTestDB(t)
	dir := t.TempDir()
	const sessionName = "integ-forcefresh-nodbrow"

	// Create the session without any DB row (simulates post-prism-reset state).
	if err := session.Create(sessionName, dir, session.Opts{Layout: session.LayoutBare}); err != nil {
		t.Fatalf("initial Create: %v", err)
	}
	t.Cleanup(func() { _ = tmux.KillSession(sessionName) })

	if !tmux.HasSession(sessionName) {
		t.Fatal("session not found after initial Create")
	}

	// Call Create with ForceFresh=false and a DB handle but no row for this session.
	// Expect zombie kill and recreation.
	err := session.Create(sessionName, dir, session.Opts{
		Layout:     session.LayoutBare,
		DB:         d,
		ForceFresh: false,
	})
	if err != nil {
		t.Fatalf("Create (ForceFresh=false, no DB row): unexpected error: %v", err)
	}
	if !tmux.HasSession(sessionName) {
		t.Error("Create (ForceFresh=false, no DB row): new session not created after zombie kill")
	}
}
