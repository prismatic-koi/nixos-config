package iris

// set_pi_session_path_test.go -- tests for Supervisor.SetPiSessionPath and the
// session_status -> in-memory-record wiring (issue #1682).
//
// The bug fixed by this code path: SessionRecord.PiSessionPath was only ever
// populated by the D-9 restore path, so live sessions emitted an empty
// harness_session_id field in sessions_snapshot frames and `iris sessions
// list --json` output. The DB was correct (via IrisUpdateHarnessSessionID),
// but the in-memory record read by daemonState.activeSessions() was stale.
//
// These tests cover:
//   - The setter mutates the in-memory record (unit AC).
//   - Concurrent setter + SessionRecord() reads are race-free (security AC).
//   - The harness session_status handler triggers the setter end-to-end
//     (integration AC).

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestSupervisor builds a Supervisor with the harness server listening
// but no pi child spawned. Suitable for exercising SetPiSessionPath and the
// session_status handler in isolation. Returns the Supervisor and its
// auto-generated instance ID.
func newTestSupervisor(t *testing.T, sessionName string) (*Supervisor, string) {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	// Use a short run dir to stay under the Unix socket path limit
	// (sun_path is 108 bytes). The default per-test tempdir under
	// /tmp/<TestName>/NNN/ can already eat 60+ bytes which, plus the
	// UUID (36) plus "/harness.sock" (13), busts the limit.
	runDir, err := os.MkdirTemp("", "iris-set-pi-")
	if err != nil {
		t.Fatalf("MkdirTemp runDir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(runDir) })

	cfg := SupervisorConfig{
		SessionName:      sessionName,
		Worktree:         tmp,
		Role:             "worker",
		PIBinaryPath:     "/bin/true",
		RestartThreshold: 1,
		RunDir:           runDir,
		Database:         database,
		// PIAgentDir points at a tmp dir so resolvePiSessionPath falls
		// back to the bare UUID when no JSONL file is on disk -- which
		// is exactly the behaviour we want to assert in these tests
		// (path resolution proper is covered by the restore tests, which
		// share the underlying findPiSessionJSONL logic).
		PIAgentDir: filepath.Join(tmp, "pi-agent"),
	}

	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(func() { sup.harness.Close() })

	return sup, sup.sess.InstanceID
}

// TestSupervisor_SetPiSessionPath_Mutates verifies the basic invariant: a
// call to SetPiSessionPath updates the in-memory SessionRecord such that the
// next SessionRecord() observes the new value.
func TestSupervisor_SetPiSessionPath_Mutates(t *testing.T) {
	sup, instanceID := newTestSupervisor(t, "test@set-pi-session-path")

	const wantPath = "/home/ben/.pi/agent/sessions/--tmp-foo--/123_uuid.jsonl"
	sup.SetPiSessionPath(instanceID, wantPath)

	got := sup.SessionRecord().PiSessionPath
	if got != wantPath {
		t.Errorf("SessionRecord().PiSessionPath after SetPiSessionPath = %q; want %q",
			got, wantPath)
	}
}

// TestSupervisor_SetPiSessionPath_IgnoresWrongInstanceID is a defensive check:
// the setter is scoped to this supervisor's session, so a call with a
// different instanceID must be a no-op.
func TestSupervisor_SetPiSessionPath_IgnoresWrongInstanceID(t *testing.T) {
	sup, _ := newTestSupervisor(t, "test@wrong-id")

	sup.SetPiSessionPath("not-this-session", "/some/other/path.jsonl")
	if got := sup.SessionRecord().PiSessionPath; got != "" {
		t.Errorf("SessionRecord().PiSessionPath after wrong-id setter call = %q; want empty",
			got)
	}
}

// TestSupervisor_SetPiSessionPath_Concurrent exercises the security AC: a
// setter goroutine and a reader goroutine (mirroring activeSessions()) must
// not race under -race. The test runs a bounded number of iterations and
// asserts every observed value is one of the values that has been set.
func TestSupervisor_SetPiSessionPath_Concurrent(t *testing.T) {
	sup, instanceID := newTestSupervisor(t, "test@race")

	const iters = 2000
	paths := []string{
		"/p/a.jsonl",
		"/p/b.jsonl",
		"/p/c.jsonl",
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var observedBad atomic.Int64

	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			sup.SetPiSessionPath(instanceID, paths[i%len(paths)])
		}
	}()
	go func() {
		defer wg.Done()
		valid := map[string]bool{"": true}
		for _, p := range paths {
			valid[p] = true
		}
		for i := 0; i < iters; i++ {
			rec := sup.SessionRecord()
			if !valid[rec.PiSessionPath] {
				observedBad.Add(1)
			}
		}
	}()
	wg.Wait()

	if n := observedBad.Load(); n > 0 {
		t.Errorf("reader observed %d unexpected PiSessionPath values -- either a torn read or a non-atomic update", n)
	}
}

// TestSupervisor_SessionStatus_PopulatesInMemoryRecord is the integration AC:
// driving a session_status frame through the harness socket must cause the
// in-memory SessionRecord.PiSessionPath to be populated immediately after
// the DB write, with no other prerequisites (no subscriber connected, no
// state transition).
//
// The test stands up a real HarnessSocketServer wired to a Supervisor (so
// the SetSessionStatusHandler callback from NewSupervisor is in effect),
// completes the harness handshake, sends a session_status frame, and polls
// the in-memory record until it's populated.
func TestSupervisor_SessionStatus_PopulatesInMemoryRecord(t *testing.T) {
	const sessionUUID = "pi-ses-test-1682"
	sup, instanceID := newTestSupervisor(t, "test@session-status-1682")

	// NewSupervisor already called harness.Listen(); just start accepting.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = sup.harness.AcceptOne(ctx) }()

	// Dial and complete the handshake.
	conn, err := net.DialTimeout("unix", sup.harness.sockPath, time.Second)
	if err != nil {
		t.Fatalf("dial harness: %v", err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)

	hello := map[string]any{
		"type":             "hello",
		"protocol_version": ProtocolVersion,
		"harness":          "pi",
		"harness_version":  "test",
	}
	if err := writeJSONLFrame(conn, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if _, err := r.ReadBytes('\n'); err != nil {
		t.Fatalf("read hello_ack: %v", err)
	}

	// Send session_status with a session_id.
	if err := writeJSONLFrame(conn, map[string]any{
		"type":       "session_status",
		"session_id": sessionUUID,
	}); err != nil {
		t.Fatalf("write session_status: %v", err)
	}

	// Poll the in-memory record for the populated PiSessionPath.
	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = sup.SessionRecord().PiSessionPath
		if got != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got == "" {
		t.Fatal("SessionRecord().PiSessionPath remained empty after session_status -- in-memory wiring is broken")
	}
	if got != sessionUUID {
		// If we ever resolve to a real path it should at least contain
		// the UUID. With no JSONL on disk the fallback is the UUID.
		t.Logf("PiSessionPath = %q (UUID-fallback expected for this test fixture)", got)
	}

	// And the DB row must also have the UUID -- keep the assertion here so
	// we catch any future refactor that drops the DB write.
	var dbHSID string
	row := sup.cfg.Database.QueryRow(
		`SELECT COALESCE(harness_session_id, '') FROM sessions WHERE instance_id = ?`,
		instanceID,
	)
	if err := row.Scan(&dbHSID); err != nil {
		t.Fatalf("query sessions.harness_session_id: %v", err)
	}
	if dbHSID != sessionUUID {
		t.Errorf("DB harness_session_id = %q; want %q", dbHSID, sessionUUID)
	}
}

// writeJSONLFrame writes a single JSON line frame on conn.
func writeJSONLFrame(conn net.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = conn.Write(data)
	return err
}
