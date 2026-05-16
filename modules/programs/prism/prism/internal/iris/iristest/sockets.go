package iristest

// sockets.go — test-scaffold helpers for tests that wire a ClientSocket
// and/or a HarnessSocketServer against a per-test DB and tempdir.
//
// # The race these helpers fix (issue #1705)
//
// PR #1715 fixed the supervisor-goroutine-outlives-cleanup race for the
// restore tests by exposing iris.RunRestore's supervisor list and adding
// RunRestoreForTest, which Wait()s on every spawned supervisor before
// t.Cleanup tears the per-test tempdir / DB down.
//
// The same race class affects every test that wires a ClientSocket or a
// HarnessSocketServer directly: those servers spawn goroutines —
// HarnessSocketServer.dispatchToolExec, ClientSocket.handleConn,
// ClientSocket.runSubscription, every per-frame handler — and those
// goroutines write to the DB and to the per-session run directory. A test
// that returns before those goroutines finish leaves them racing against
// t.Cleanup's `database.Close()` and tempdir removal, surfacing as
// 'unlinkat .../001: directory not empty' or 'sql: database is closed'
// (see #1705 reopen comment, PR #1728 CI flake on TestHarnessToClientFanOut).
//
// # The discipline
//
// Both HarnessSocketServer and ClientSocket now expose a Wait() method
// (added in this change) that blocks until every goroutine the server
// has spawned has returned. The helpers below register a t.Cleanup that:
//
//  1. Cancels the per-server context (so context-driven select arms fire).
//  2. Closes the listener (so Accept returns; new dials fail fast).
//  3. Calls Wait() to drain in-flight goroutines.
//
// t.Cleanup runs in LIFO order, so a cleanup registered by these helpers
// AFTER the test obtains its DB / tempdir runs FIRST during teardown —
// exactly the ordering we need: drain goroutines first, then close DB,
// then remove tempdir.
//
// # No new synchronisation primitives
//
// The helpers reuse the WaitGroup-based Wait() methods on the servers.
// No new mutex, no atomic, no time.Sleep workaround.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
)

// drainTimeout bounds the Wait() call in the cleanup. A wedged goroutine
// is a real bug; we surface it via t.Errorf rather than blocking the test
// process indefinitely.
const drainTimeout = 10 * time.Second

// SocketScaffold bundles a ClientSocket and a HarnessSocketServer wired
// against a shared DB and tempdir, with cleanup that drains all spawned
// goroutines before the DB / tempdir are torn down.
//
// Use this for tests that exercise the full client-socket + harness-socket
// fan-out path (e.g. TestHarnessToClientFanOut) or any test that wires
// both servers together.
type SocketScaffold struct {
	// Tmp is the per-test t.TempDir() owned by the scaffold.
	Tmp string
	// DB is the open iris DB at $Tmp/iris.db.
	DB *db.DB
	// ClientSocket is the user-facing IPC socket; Listen and Serve have
	// already been called. SockPath is at $Tmp/iris.sock.
	ClientSocket *iris.ClientSocket
	// HarnessSocketServer is the per-session harness listener; Listen
	// has been called and AcceptOne is running in a background goroutine.
	HarnessSocketServer *iris.HarnessSocketServer
	// SessionName is the in-memory session record's logical name.
	SessionName string
	// InstanceID is the in-memory session record's instance UUID.
	InstanceID string
}

// SocketScaffoldOptions is the input to NewSocketScaffold. Both
// SessionName and InstanceID default to deterministic values when empty.
type SocketScaffoldOptions struct {
	// SessionName is the iris session name to register in the sessions row
	// and on the HarnessSocketServer. Default: "iris-test@fanout".
	SessionName string
	// InstanceID is the session instance UUID. Default: "iid-test".
	InstanceID string
	// Role is the agent role on the harness session record. Default:
	// "worker".
	Role string
	// GetActiveSessions overrides the ClientSocket's session list
	// getter. Default: a single-element list containing a SessionSnapshot
	// derived from SessionName / InstanceID / Role.
	GetActiveSessions func() []iris.SessionSnapshot
	// Publisher overrides the harness publisher. When non-nil, the helper
	// wires this publisher onto the HarnessSocketServer instead of the
	// scaffold's ClientSocket. When nil, the ClientSocket itself is wired
	// as the publisher (this is the common case — it matches the
	// harness→publisher→client fan-out the daemon configures in
	// production).
	Publisher iris.EventPublisher
	// NoPublisher, when true, suppresses publisher wiring entirely. Used
	// by tests that want to verify the no-publisher branch of
	// HarnessSocketServer.publishEvent.
	NoPublisher bool
}

// NewSocketScaffold creates a SocketScaffold for a test. It:
//
//  1. Creates a per-test tempdir and opens an iris DB inside it.
//  2. Inserts a sessions row so agent_events writes satisfy the FK.
//  3. Constructs a ClientSocket on $Tmp/iris.sock, calls Listen and
//     launches Serve in a goroutine under a scaffold-owned context.
//  4. Constructs a HarnessSocketServer on $Tmp/harness.sock, calls Listen,
//     wires the ClientSocket as the harness publisher (unless opts
//     override), and launches AcceptOne in a goroutine under a separate
//     scaffold-owned context.
//  5. Registers t.Cleanup hooks that cancel both contexts, close the
//     listeners, Wait() for in-flight goroutines, then close the DB.
//
// Cleanup-ordering invariant (load-bearing): the helper registers its
// drain cleanup AFTER t.TempDir() and the DB-close cleanup, so LIFO
// ordering runs the drain FIRST, then the DB close, then the tempdir
// removal. Any reordering re-introduces the #1705 race.
func NewSocketScaffold(t *testing.T, opts SocketScaffoldOptions) *SocketScaffold {
	t.Helper()

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("iristest: OpenDB(%q): %v", dbPath, err)
	}
	// DB close is registered BEFORE the drain cleanup below so LIFO
	// ordering runs the drain first, then this close.
	t.Cleanup(func() { _ = database.Close() })

	sessionName := opts.SessionName
	if sessionName == "" {
		sessionName = "iris-test@fanout"
	}
	instanceID := opts.InstanceID
	if instanceID == "" {
		instanceID = "iid-test"
	}
	role := opts.Role
	if role == "" {
		role = "worker"
	}

	// FK on agent_events.instance_id requires the sessions row to exist
	// before any harness event is written.
	if err := database.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Worktree:    tmp,
		Harness:     "pi",
		StartedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("iristest: InsertSession: %v", err)
	}

	getActiveSessions := opts.GetActiveSessions
	if getActiveSessions == nil {
		snap := iris.SessionSnapshot{
			Name:       sessionName,
			InstanceID: instanceID,
			State:      "active",
			Role:       role,
		}
		getActiveSessions = func() []iris.SessionSnapshot {
			return []iris.SessionSnapshot{snap}
		}
	}

	clientSockPath := filepath.Join(tmp, "iris.sock")
	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath:          clientSockPath,
		Database:          database,
		GetActiveSessions: getActiveSessions,
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("iristest: ClientSocket.Listen: %v", err)
	}

	harnessSockPath := filepath.Join(tmp, "harness.sock")
	sess := &iris.SessionRecord{
		InstanceID:      instanceID,
		SessionName:     sessionName,
		Worktree:        tmp,
		Role:            role,
		HarnessSockPath: harnessSockPath,
	}
	harness, err := iris.NewHarnessSocketServer(sess, database)
	if err != nil {
		t.Fatalf("iristest: NewHarnessSocketServer: %v", err)
	}

	if !opts.NoPublisher {
		if opts.Publisher != nil {
			harness.SetPublisher(opts.Publisher)
		} else {
			harness.SetPublisher(cs)
		}
	}
	if err := harness.Listen(); err != nil {
		t.Fatalf("iristest: HarnessSocketServer.Listen: %v", err)
	}

	// Background contexts for Serve and AcceptOne. We own the cancellers
	// so the cleanup can stop both servers deterministically.
	csCtx, csCancel := context.WithCancel(context.Background())
	go cs.Serve(csCtx)

	harnessCtx, harnessCancel := context.WithCancel(context.Background())
	go func() { _ = harness.AcceptOne(harnessCtx) }()

	// Drain cleanup. Registered LAST among the cleanups this helper owns,
	// so LIFO ordering runs it FIRST during teardown: cancel contexts,
	// close listeners, Wait() for goroutines. Only after Wait() returns
	// is the DB closed (registered above) and the tempdir removed
	// (registered by t.TempDir before any of this helper's cleanups).
	t.Cleanup(func() {
		// Cancel contexts so context-driven select arms fire.
		harnessCancel()
		csCancel()
		// Close listeners so Accept returns and pending dials fail fast.
		harness.Close()
		cs.Close()
		// Drain in-flight goroutines. A bounded wait protects against
		// a wedged goroutine hanging the test process indefinitely; a
		// real wedge is a bug and is surfaced via t.Errorf.
		waitOrFail(t, "HarnessSocketServer", harness.Wait)
		waitOrFail(t, "ClientSocket", cs.Wait)
	})

	return &SocketScaffold{
		Tmp:                 tmp,
		DB:                  database,
		ClientSocket:        cs,
		HarnessSocketServer: harness,
		SessionName:         sessionName,
		InstanceID:          instanceID,
	}
}

// waitOrFail runs wait() in a goroutine and either returns when it
// completes or surfaces a t.Errorf when drainTimeout elapses. The bound
// is high enough that a non-wedged goroutine drains comfortably; a
// wedge indicates a real bug, not a flake, and the error is loud so it
// cannot hide.
func waitOrFail(t *testing.T, label string, wait func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(drainTimeout):
		t.Errorf("iristest: %s did not drain within %v after cancel+close \u2014 wedged goroutine", label, drainTimeout)
	}
}
