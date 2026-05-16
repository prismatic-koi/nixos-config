package main

// spawn_integration_test.go — end-to-end test of `iris spawn` against a real
// iris.ClientSocket (issue #1668).
//
// The test stands up an in-process ClientSocket whose SpawnSession callback
// is a recorder, then invokes runSpawnAt() with the socket path. This proves
// the wire path used by `iris spawn`:
//
//   CLI                 →  session_spawn frame on iris.sock
//   daemon ClientSocket →  invokes spawnFn(worktree, role, ...)
//   daemon              →  session_spawned frame back to CLI
//
// is wired correctly end-to-end. We do not start a real pi child here —
// the spawnFn returns a stub *iris.Supervisor whose SessionRecord has the
// fields needed to fill out the ack frame. The full pi-running session
// shape is covered by TestNewRestoreSupervisor_PopulatesBareRoot and the
// other supervisor tests.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
)

// TestSpawn_EndToEndAgainstRealClientSocket exercises the full wire path
// against a real ClientSocket. It asserts that the spawnFn receives the
// worktree and role from the CLI flags, which is sufficient to guarantee
// the resulting Supervisor (built by the real spawnFn in main.go) will
// populate BareRoot, Role and Worktree on the session record — see
// TestNewRestoreSupervisor_PopulatesBareRoot for the per-field assertion.
func TestSpawn_EndToEndAgainstRealClientSocket(t *testing.T) {
	// Per-session Unix sockets must fit under the 108-byte sun_path limit.
	// t.TempDir() can produce long paths when the test name is long, so anchor
	// the runDir and sock under a short os.MkdirTemp() prefix (same pattern
	// used by iristest.NewIsolated).
	shortPrefix, err := os.MkdirTemp("", "iris-spawn-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })

	dbPath := filepath.Join(shortPrefix, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	sockPath := filepath.Join(shortPrefix, "iris.sock")
	runDir := filepath.Join(shortPrefix, "run")

	// Record what the daemon-side spawnFn sees.
	var (
		recMu      sync.Mutex
		gotWtree   string
		gotRole    string
		spawnCalls int
	)

	// Build a stub Supervisor by constructing a SessionRecord with the
	// minimum needed for the spawn ack and BareRoot/role assertions.
	// We use iris.NewSupervisor with a real (empty) DB so the ack frame's
	// fields match the protocol contract.
	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: sockPath,
		Database: database,
		GetActiveSessions: func() []iris.SessionSnapshot {
			return nil
		},
		SpawnSession: func(ctx context.Context, _sessionName, worktree, role, _parent string, _ map[string]any) (*iris.Supervisor, error) {
			recMu.Lock()
			gotWtree = worktree
			gotRole = role
			spawnCalls++
			recMu.Unlock()

			// Build a real Supervisor. PIBinaryPath=/bin/true means the
			// child exits immediately — we don't Start() it so no process
			// is actually forked here. NewSupervisor binds the per-session
			// harness socket; t.TempDir() under runDir keeps it isolated.
			sup, err := iris.NewSupervisor(iris.SupervisorConfig{
				SessionName:  "iris-" + role + "@" + filepath.Base(worktree),
				Worktree:     worktree,
				Role:         role,
				BareRoot:     "/fake/bare-root-for-test",
				PIBinaryPath: "/bin/true",
				RunDir:       runDir,
				Database:     database,
			})
			return sup, err
		},
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	srvCtx, srvCancel := context.WithCancel(context.Background())
	t.Cleanup(srvCancel)
	go cs.Serve(srvCtx)

	// Drive the spawn from the CLI side.
	const worktree = "/abs/path/to/my-worktree"
	const role = "coordinator"

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := runSpawnAt(ctx, sockPath, runDir, worktree, role, &out); err != nil {
		t.Fatalf("runSpawnAt: %v", err)
	}

	// Assert: the daemon-side spawnFn was invoked exactly once with the
	// worktree and role from the CLI. This is the wire-level contract:
	// whatever the CLI sends, the daemon's spawnFn receives.
	recMu.Lock()
	defer recMu.Unlock()
	if spawnCalls != 1 {
		t.Errorf("spawnFn called %d times, want 1", spawnCalls)
	}
	if gotWtree != worktree {
		t.Errorf("spawnFn worktree = %q, want %q", gotWtree, worktree)
	}
	if gotRole != role {
		t.Errorf("spawnFn role = %q, want %q", gotRole, role)
	}

	// Assert: the CLI printed both the session name and a harness socket
	// path. The harness socket path is deterministic from runDir + the
	// instance UUID returned in the ack.
	output := out.String()
	if !strings.Contains(output, "iris-coordinator@my-worktree") {
		t.Errorf("expected session name in output; got %q", output)
	}
	if !strings.Contains(output, runDir) {
		t.Errorf("expected harness socket path (under %q) in output; got %q", runDir, output)
	}
}

// TestSpawn_ForwardsIRISSessionNameAsParent asserts that when `iris spawn`
// is invoked with $IRIS_SESSION_NAME set (the env variable that
// Supervisor.buildEnv injects into every iris-managed pi child), the
// daemon's spawnFn receives that value via the Parent argument. This is
// the wire half of issue #1700: the CLI must forward the calling session
// identity through the session_spawn frame so the daemon can record it on
// sessions.parent_session.
func TestSpawn_ForwardsIRISSessionNameAsParent(t *testing.T) {
	shortPrefix, err := os.MkdirTemp("", "iris-spawn-parent-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })

	dbPath := filepath.Join(shortPrefix, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	sockPath := filepath.Join(shortPrefix, "iris.sock")
	runDir := filepath.Join(shortPrefix, "run")

	var (
		recMu     sync.Mutex
		gotParent string
	)
	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath:          sockPath,
		Database:          database,
		GetActiveSessions: func() []iris.SessionSnapshot { return nil },
		SpawnSession: func(ctx context.Context, _sessionName, worktree, role, parent string, _ map[string]any) (*iris.Supervisor, error) {
			recMu.Lock()
			gotParent = parent
			recMu.Unlock()
			return iris.NewSupervisor(iris.SupervisorConfig{
				SessionName:   "iris-" + role + "@parent-test",
				Worktree:      worktree,
				Role:          role,
				ParentSession: parent,
				PIBinaryPath:  "/bin/true",
				RunDir:        runDir,
				Database:      database,
			})
		},
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	srvCtx, srvCancel := context.WithCancel(context.Background())
	t.Cleanup(srvCancel)
	go cs.Serve(srvCtx)

	// Set IRIS_SESSION_NAME so runSpawnAt picks it up the same way an
	// iris-managed pi child would (Supervisor.buildEnv injects it).
	t.Setenv("IRIS_SESSION_NAME", "iris-coordinator@upstream")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out bytes.Buffer
	if err := runSpawnAt(ctx, sockPath, runDir, "/abs/path/to/my-worktree", "worker", &out); err != nil {
		t.Fatalf("runSpawnAt: %v", err)
	}

	recMu.Lock()
	defer recMu.Unlock()
	if gotParent != "iris-coordinator@upstream" {
		t.Errorf("daemon spawnFn parent = %q, want %q", gotParent, "iris-coordinator@upstream")
	}
}

// TestSpawn_EmptyIRISSessionNameYieldsEmptyParent asserts that when
// $IRIS_SESSION_NAME is unset (the top-level-spawn case — user typed
// `iris spawn` from a fresh terminal), the daemon receives parent="" and
// will store NULL on sessions.parent_session. No notification will fire on
// terminal state, which is correct for top-level spawns.
func TestSpawn_EmptyIRISSessionNameYieldsEmptyParent(t *testing.T) {
	shortPrefix, err := os.MkdirTemp("", "iris-spawn-noparent-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })

	dbPath := filepath.Join(shortPrefix, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	sockPath := filepath.Join(shortPrefix, "iris.sock")
	runDir := filepath.Join(shortPrefix, "run")

	var (
		recMu     sync.Mutex
		gotParent string
		called    bool
	)
	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath:          sockPath,
		Database:          database,
		GetActiveSessions: func() []iris.SessionSnapshot { return nil },
		SpawnSession: func(ctx context.Context, _sessionName, worktree, role, parent string, _ map[string]any) (*iris.Supervisor, error) {
			recMu.Lock()
			gotParent = parent
			called = true
			recMu.Unlock()
			return iris.NewSupervisor(iris.SupervisorConfig{
				SessionName:  "iris-" + role + "@no-parent",
				Worktree:     worktree,
				Role:         role,
				PIBinaryPath: "/bin/true",
				RunDir:       runDir,
				Database:     database,
			})
		},
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	srvCtx, srvCancel := context.WithCancel(context.Background())
	t.Cleanup(srvCancel)
	go cs.Serve(srvCtx)

	// Explicitly unset IRIS_SESSION_NAME in case the test runner has it set.
	t.Setenv("IRIS_SESSION_NAME", "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out bytes.Buffer
	if err := runSpawnAt(ctx, sockPath, runDir, "/abs/path/to/my-worktree", "worker", &out); err != nil {
		t.Fatalf("runSpawnAt: %v", err)
	}

	recMu.Lock()
	defer recMu.Unlock()
	if !called {
		t.Fatal("daemon spawnFn was not invoked")
	}
	if gotParent != "" {
		t.Errorf("daemon spawnFn parent = %q, want empty string (top-level spawn)", gotParent)
	}
}
