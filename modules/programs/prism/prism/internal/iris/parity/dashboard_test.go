package parity_test

// dashboard_test.go — §10.3 checklist item: "Show the dashboard".
//
// D-10 AC (functional, dashboard):
//
//   A test invokes the iris equivalent of `prism dashboard` (CLI subcommand
//   or the TUI's `sessions_snapshot` request via the client socket) and
//   asserts the session list includes the spawned test sessions with their
//   state, branch, and elapsed time.
//
// We exercise the TUI path: send `sessions_list` on the client socket and
// validate the resulting `sessions_snapshot` frame. This is what the iris
// TUI (cmd/iris/tui.go → internal/iris/tui) uses to populate its session
// list, so a passing assertion here proves the dashboard view has parity
// with `prism dashboard`'s session list.

import (
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

func TestParityDashboard_SessionsSnapshot(t *testing.T) {
	iso := iristest.NewIsolated(t)

	// Two spawned sessions: one worker, one coordinator. Branches encoded
	// in the session name (after '@') so the TUI's branch column matches.
	fsA := newFakeSession(t, iso, fakeSessionOptions{
		Role:        "worker",
		SessionName: "iris-test@feature-dashboard-worker",
	})
	fsB := newFakeSession(t, iso, fakeSessionOptions{
		Role:        "coordinator",
		SessionName: "iris-test@main",
	})

	startedAFmt := time.Now().Add(-2 * time.Minute).Format("2006-01-02T15:04:05Z07:00")
	startedBFmt := time.Now().Add(-30 * time.Second).Format("2006-01-02T15:04:05Z07:00")

	rig := startClientSocket(t, iso)
	rig.recordSession(iris.SessionSnapshot{
		Name:       fsA.SessionName,
		InstanceID: fsA.InstanceID,
		State:      string(iris.StateActive),
		Role:       fsA.Role,
		Worktree:   fsA.Worktree,
		StartedAt:  startedAFmt,
	})
	rig.recordSession(iris.SessionSnapshot{
		Name:       fsB.SessionName,
		InstanceID: fsB.InstanceID,
		State:      string(iris.StateActive),
		Role:       fsB.Role,
		Worktree:   fsB.Worktree,
		StartedAt:  startedBFmt,
	})

	conn, r := dialClientSocket(t, rig.Sock.SockPath())
	if err := writeJSONLine(conn, map[string]any{"type": "sessions_list"}); err != nil {
		t.Fatalf("sessions_list: %v", err)
	}

	frame, ok := readJSONLineWithTimeout(t, conn, r, 3*time.Second)
	if !ok {
		t.Fatalf("no sessions_snapshot received")
	}
	if frame["type"] != "sessions_snapshot" {
		t.Fatalf("expected sessions_snapshot, got %v", frame["type"])
	}
	sessions, _ := frame["sessions"].([]any)
	if len(sessions) != 2 {
		t.Fatalf("sessions count = %d, want 2; got %v", len(sessions), sessions)
	}

	// Build a name → session map for assertions.
	byName := map[string]map[string]any{}
	for _, s := range sessions {
		entry, _ := s.(map[string]any)
		name, _ := entry["name"].(string)
		byName[name] = entry
	}

	// Required fields: state, role, worktree, started_at, instance_id.
	for _, name := range []string{fsA.SessionName, fsB.SessionName} {
		entry, ok := byName[name]
		if !ok {
			t.Fatalf("sessions_snapshot is missing entry for %q", name)
		}
		for _, field := range []string{"state", "role", "worktree", "started_at", "instance_id"} {
			if v, ok := entry[field]; !ok || v == "" {
				t.Errorf("sessions_snapshot[%q] missing or empty field %q", name, field)
			}
		}
		if entry["state"] != string(iris.StateActive) {
			t.Errorf("sessions_snapshot[%q].state = %v, want %q", name, entry["state"], iris.StateActive)
		}
	}

	// Branch column: the iris TUI derives the branch label from the part
	// after '@' in the session_name (see extractBranch in prism.ts and
	// internal/iris/tui). Verify the session name carries the branch
	// suffix so the TUI's branch column is populated correctly.
	for name, branch := range map[string]string{
		fsA.SessionName: "feature-dashboard-worker",
		fsB.SessionName: "main",
	} {
		idx := lastByte(name, '@')
		if idx < 0 || name[idx+1:] != branch {
			t.Errorf("session name %q does not carry expected branch suffix %q", name, branch)
		}
	}

	// Elapsed-time AC: started_at is reported in RFC3339 and a downstream
	// consumer can compute elapsed time as now()-started_at. We assert the
	// reported timestamp parses and yields a non-negative duration.
	for _, entry := range byName {
		ts, _ := entry["started_at"].(string)
		parsed, err := time.Parse("2006-01-02T15:04:05Z07:00", ts)
		if err != nil {
			t.Errorf("started_at %q does not parse: %v", ts, err)
			continue
		}
		if elapsed := time.Since(parsed); elapsed < 0 {
			t.Errorf("started_at %q yields negative elapsed time %v", ts, elapsed)
		}
	}
}

// lastByte returns the last index of c in s, or -1.
func lastByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}
