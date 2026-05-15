package parity_test

// helpers_test.go — shared scaffolding for the iris parity suite.
//
// These helpers exist to keep each per-checklist-item test file readable.
// They wrap the boilerplate of:
//
//   - opening an isolated iris environment (via iristest.NewIsolated);
//   - constructing a HarnessSocketServer for one "session";
//   - dialling that socket as if we were the pi extension;
//   - performing the hello / hello_ack handshake;
//   - spinning up a daemon-flavoured ClientSocket for tests that need the
//     full client → daemon path.
//
// They DO NOT call any prism code path. They DO NOT touch the real host
// state — every path is rooted in t.TempDir() via iristest.NewIsolated.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

// fakeSession bundles a HarnessSocketServer + connected extension client +
// the SessionRecord they share. It is the parity-suite stand-in for a
// "running iris session" — the harness socket is real, the DB writes are
// real, only the pi child is faked by a Go-side goroutine.
type fakeSession struct {
	Iso         *iristest.Isolated
	InstanceID  string
	SessionName string
	Role        string
	Worktree    string

	HarnessServer *iris.HarnessSocketServer
	ExtConn       net.Conn
	ExtReader     *bufio.Reader
	HelloAck      map[string]any
}

// fakeSessionOptions describe how a fake session should be constructed.
type fakeSessionOptions struct {
	// Role is the session role written into hello_ack.session_role.
	Role string
	// SessionName is the parity-test-safe session name. Empty → derived
	// from the test name with the iris-test@ prefix.
	SessionName string
	// Worktree is the absolute worktree path. Empty → iso.Root/worktree.
	Worktree string
	// PreInsertSessionRow controls whether the helper writes a sessions row
	// before the harness starts so events with instance_id satisfy the FK.
	// Default true.
	PreInsertSessionRow *bool
	// SkipHandshake leaves the connection un-handshook (used by tests that
	// want to exercise the handshake path themselves).
	SkipHandshake bool
}

// newFakeSession spins up an isolated iris environment, registers one
// session, and dials in as the pi extension. The returned fakeSession is
// ready for tool_exec dispatch tests; t.Cleanup is registered for teardown.
func newFakeSession(t *testing.T, iso *iristest.Isolated, opts fakeSessionOptions) *fakeSession {
	t.Helper()

	role := opts.Role
	if role == "" {
		role = "worker"
	}

	sessionName := opts.SessionName
	if sessionName == "" {
		sessionName = iristest.SessionName(t.Name())
	}

	worktree := opts.Worktree
	if worktree == "" {
		worktree = filepath.Join(iso.Root, "worktree")
		if err := os.MkdirAll(worktree, 0o755); err != nil {
			t.Fatalf("newFakeSession: mkdir worktree: %v", err)
		}
	}

	instanceID := uuid.New().String()

	preInsert := true
	if opts.PreInsertSessionRow != nil {
		preInsert = *opts.PreInsertSessionRow
	}
	if preInsert {
		sess := db.Session{
			InstanceID:  instanceID,
			SessionName: sessionName,
			Worktree:    worktree,
			Harness:     "pi",
			AgentRole:   &role,
			StartedAt:   time.Now(),
		}
		if err := iso.DB.InsertSession(sess); err != nil {
			t.Fatalf("newFakeSession: InsertSession: %v", err)
		}
	}

	// Per-session run dir + harness socket path under iso.Paths.RunDir.
	if _, err := iris.EnsureSessionDir(iso.Paths.RunDir, instanceID); err != nil {
		t.Fatalf("newFakeSession: EnsureSessionDir: %v", err)
	}
	sockPath := iris.HarnessSockPath(iso.Paths.RunDir, instanceID)

	rec := &iris.SessionRecord{
		InstanceID:       instanceID,
		SessionName:      sessionName,
		Worktree:         worktree,
		Role:             role,
		BareRoot:         "", // tests do not need a real bare repo
		State:            iris.StateActive,
		HarnessSockPath:  sockPath,
		RestartThreshold: iris.DefaultRestartThreshold,
		StartedAt:        time.Now(),
	}

	srv, err := iris.NewHarnessSocketServer(rec, iso.DB)
	if err != nil {
		t.Fatalf("newFakeSession: NewHarnessSocketServer: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("newFakeSession: Listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	// Accept-one in the background.
	acceptCtx, acceptCancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(acceptCancel)
	go func() { _ = srv.AcceptOne(acceptCtx) }()

	fs := &fakeSession{
		Iso:           iso,
		InstanceID:    instanceID,
		SessionName:   sessionName,
		Role:          role,
		Worktree:      worktree,
		HarnessServer: srv,
	}

	if !opts.SkipHandshake {
		fs.dialAndHandshake(t)
	}
	return fs
}

// dialAndHandshake dials the harness socket as the pi extension and
// performs the hello / hello_ack exchange.
func (fs *fakeSession) dialAndHandshake(t *testing.T) {
	t.Helper()
	var conn net.Conn
	var err error
	for i := 0; i < 30; i++ {
		conn, err = net.DialTimeout("unix", fs.HarnessServer.SockPath(), 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dialAndHandshake: dial %q: %v", fs.HarnessServer.SockPath(), err)
	}
	t.Cleanup(func() { conn.Close() })

	r := bufio.NewReaderSize(conn, 1<<20)
	if err := writeJSONLine(conn, map[string]any{
		"type":             "hello",
		"protocol_version": iris.ProtocolVersion,
		"harness":          "pi",
		"harness_version":  "parity-test",
	}); err != nil {
		t.Fatalf("dialAndHandshake: write hello: %v", err)
	}
	ack, err := readJSONLine(r)
	if err != nil {
		t.Fatalf("dialAndHandshake: read hello_ack: %v", err)
	}
	if ack["type"] != "hello_ack" {
		t.Fatalf("dialAndHandshake: expected hello_ack, got %q", ack["type"])
	}
	fs.ExtConn = conn
	fs.ExtReader = r
	fs.HelloAck = ack
}

// writeJSONLine encodes v and writes it as one JSON-line.
func writeJSONLine(w net.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

// readJSONLine reads one '\n'-terminated line from r and JSON-decodes it.
func readJSONLine(r *bufio.Reader) (map[string]any, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, fmt.Errorf("parse %q: %w", line, err)
	}
	return m, nil
}

// readJSONLineWithTimeout reads a line, returning ok=false on timeout.
func readJSONLineWithTimeout(t *testing.T, conn net.Conn, r *bufio.Reader, d time.Duration) (map[string]any, bool) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(d))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Logf("parse %q: %v", line, err)
		return nil, false
	}
	return m, true
}

// runDispatchedToolExec sends a tool_exec frame and waits for the matching
// tool_exec_result. tool_exec_update frames are accumulated and returned
// alongside the result.
func (fs *fakeSession) runDispatchedToolExec(t *testing.T, frame map[string]any) (result map[string]any, updates []map[string]any) {
	t.Helper()
	if err := writeJSONLine(fs.ExtConn, frame); err != nil {
		t.Fatalf("runDispatchedToolExec: write: %v", err)
	}
	id, _ := frame["id"].(string)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_ = fs.ExtConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		m, err := readJSONLine(fs.ExtReader)
		if err != nil {
			t.Fatalf("runDispatchedToolExec: read: %v", err)
		}
		if m["type"] == "tool_exec_update" && m["id"] == id {
			updates = append(updates, m)
			continue
		}
		if m["type"] == "tool_exec_result" && m["id"] == id {
			_ = fs.ExtConn.SetReadDeadline(time.Time{})
			return m, updates
		}
	}
	t.Fatalf("runDispatchedToolExec: timed out waiting for tool_exec_result id=%s", id)
	return nil, updates
}

// pollForEventOnce polls the DB up to d for the named session_event of the
// given type. Returns the first matching event payload or fails t.
func pollForEventOnce(t *testing.T, database *db.DB, sessionName, eventType string, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		events, err := database.AllSessionEvents(sessionName)
		if err != nil {
			t.Fatalf("pollForEventOnce: AllSessionEvents: %v", err)
		}
		for _, e := range events {
			if e.Type == eventType {
				return e.Payload
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pollForEventOnce: no %q event for session %q within %v", eventType, sessionName, d)
	return ""
}

// pollForHarnessSessionID waits until agent_status.harness_session_id is
// set for instanceID. Returns the harness_session_id or fails t. Mirrors the
// "wait for event row" pattern recommended in the D-10 watch-outs notes
// (PR #1657 fixed a session_status race).
func pollForHarnessSessionID(t *testing.T, database *db.DB, instanceID string, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		sess, err := database.SessionByInstanceID(instanceID)
		if err != nil {
			t.Fatalf("pollForHarnessSessionID: SessionByInstanceID: %v", err)
		}
		if sess != nil && sess.HarnessSessionID != nil && *sess.HarnessSessionID != "" {
			return *sess.HarnessSessionID
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pollForHarnessSessionID: harness_session_id not set for %s within %v", instanceID, d)
	return ""
}

// startClientSocket wires a ClientSocket against the isolated environment
// and returns it with cleanup registered. It exposes a spawnSession hook
// that records every spawn through a per-test mutex-guarded recorder.
type clientSocketRig struct {
	Sock              *iris.ClientSocket
	Spawned           []spawnedSession
	SpawnedMu         sync.Mutex
	Sessions          []iris.SessionSnapshot
	SessionsMu        sync.Mutex
	DeliverPromptCalls []deliveredPrompt
	DeliverPromptMu    sync.Mutex
}

type spawnedSession struct {
	Name       string
	Worktree   string
	Role       string
}

type deliveredPrompt struct {
	Name      string
	Text      string
	DeliverAs string
}

func startClientSocket(t *testing.T, iso *iristest.Isolated) *clientSocketRig {
	t.Helper()
	rig := &clientSocketRig{}
	rig.Sock = iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: iso.Paths.Sock,
		Database: iso.DB,
		GetActiveSessions: func() []iris.SessionSnapshot {
			rig.SessionsMu.Lock()
			defer rig.SessionsMu.Unlock()
			out := make([]iris.SessionSnapshot, len(rig.Sessions))
			copy(out, rig.Sessions)
			return out
		},
		DeliverPrompt: func(_ context.Context, name, text, deliverAs string, _ []string) error {
			rig.DeliverPromptMu.Lock()
			defer rig.DeliverPromptMu.Unlock()
			rig.DeliverPromptCalls = append(rig.DeliverPromptCalls, deliveredPrompt{
				Name: name, Text: text, DeliverAs: deliverAs,
			})
			return nil
		},
	})
	if err := rig.Sock.Listen(); err != nil {
		t.Fatalf("startClientSocket: Listen: %v", err)
	}
	t.Cleanup(rig.Sock.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	go rig.Sock.Serve(ctx)
	return rig
}

// recordSession appends a SessionSnapshot to the rig's active-session list.
func (rig *clientSocketRig) recordSession(s iris.SessionSnapshot) {
	rig.SessionsMu.Lock()
	defer rig.SessionsMu.Unlock()
	rig.Sessions = append(rig.Sessions, s)
}

// dialClientSocket dials the rig's client socket and returns conn + reader.
func dialClientSocket(t *testing.T, sockPath string) (net.Conn, *bufio.Reader) {
	t.Helper()
	var conn net.Conn
	var err error
	for i := 0; i < 30; i++ {
		conn, err = net.DialTimeout("unix", sockPath, 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dialClientSocket: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, bufio.NewReaderSize(conn, 1<<20)
}
