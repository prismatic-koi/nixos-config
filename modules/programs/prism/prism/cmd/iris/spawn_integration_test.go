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
		SpawnSession: func(ctx context.Context, worktree, role string, _ map[string]any) (*iris.Supervisor, error) {
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
