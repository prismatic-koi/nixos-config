package cmd

// Tests for issue #3027: a restored session must write events with the
// instance ID of a new incarnation, never with the instance ID of the ended
// one.

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

// seedIncarnation gives the agent_status row of sessionName the instance ID
// iid and inserts its sessions row. When endedAt is non-zero, the sessions
// row is ended at that time.
func seedIncarnation(t *testing.T, d *db.DB, sessionName, iid string, endedAt time.Time) {
	t.Helper()
	if err := d.SetInstanceID(sessionName, iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	parent := "testrepo@main"
	role := "worker"
	if err := d.InsertSession(db.Session{
		InstanceID: iid, SessionName: sessionName, Repo: "testrepo",
		Worktree: "/wt", Harness: "pi", AgentRole: &role, ParentSession: &parent,
		StartedAt: time.Now().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if !endedAt.IsZero() {
		if err := d.UpdateSessionEnded(iid, "finished"); err != nil {
			t.Fatalf("UpdateSessionEnded: %v", err)
		}
		execRaw(t, d.Path(), "UPDATE sessions SET ended_at = ? WHERE instance_id = ?", endedAt.UnixMilli(), iid)
	}
}

func currentInstanceID(t *testing.T, d *db.DB, sessionName string) string {
	t.Helper()
	st, err := d.CurrentStatus(sessionName)
	if err != nil || st == nil {
		t.Fatalf("CurrentStatus %q: %v", sessionName, err)
	}
	if st.InstanceID == nil {
		return ""
	}
	return *st.InstanceID
}

func sessionRow(t *testing.T, d *db.DB, iid string) *db.Session {
	t.Helper()
	s, err := d.SessionByInstanceID(iid)
	if err != nil {
		t.Fatalf("SessionByInstanceID %s: %v", iid, err)
	}
	return s
}

func TestStartRestoredIncarnation(t *testing.T) {
	const sessionName = "testrepo@feature"

	t.Run("previous incarnation already ended", func(t *testing.T) {
		d := openRestoreTestDB(t)
		status := seedStatus(t, d, sessionName, "/wt", nil)
		oldIID := uuid.New().String()
		oldEnd := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Millisecond)
		seedIncarnation(t, d, sessionName, oldIID, oldEnd)
		status.InstanceID = &oldIID

		newIID := startRestoredIncarnation(d, status, "worker")

		if newIID == "" || newIID == oldIID {
			t.Fatalf("startRestoredIncarnation = %q, want a new instance ID (old %q)", newIID, oldIID)
		}
		if got := currentInstanceID(t, d, sessionName); got != newIID {
			t.Errorf("agent_status.instance_id = %q, want %q", got, newIID)
		}
		row := sessionRow(t, d, newIID)
		if row == nil {
			t.Fatalf("no sessions row for the new instance %s", newIID)
		}
		if row.EndedAt != nil {
			t.Errorf("new sessions row ended_at = %v, want NULL", row.EndedAt)
		}
		if row.ParentSession == nil || *row.ParentSession != "testrepo@main" {
			t.Errorf("new sessions row parent_session = %v, want testrepo@main", row.ParentSession)
		}
		old := sessionRow(t, d, oldIID)
		if old == nil {
			t.Fatalf("old sessions row %s is missing", oldIID)
		}
		if old.EndedAt == nil || !old.EndedAt.Equal(oldEnd) {
			t.Errorf("old sessions row ended_at = %v, want it unchanged at %v", old.EndedAt, oldEnd)
		}
	})

	t.Run("previous incarnation still open", func(t *testing.T) {
		d := openRestoreTestDB(t)
		status := seedStatus(t, d, sessionName, "/wt", nil)
		oldIID := uuid.New().String()
		seedIncarnation(t, d, sessionName, oldIID, time.Time{})
		status.InstanceID = &oldIID

		newIID := startRestoredIncarnation(d, status, "")

		if newIID == "" || newIID == oldIID {
			t.Fatalf("startRestoredIncarnation = %q, want a new instance ID (old %q)", newIID, oldIID)
		}
		old := sessionRow(t, d, oldIID)
		if old == nil || old.EndedAt == nil {
			t.Error("old sessions row is still open: nothing else ends a row that agent_status no longer names")
		}
		row := sessionRow(t, d, newIID)
		if row == nil || row.AgentRole == nil || *row.AgentRole != "worker" {
			t.Errorf("new sessions row agent_role = %v, want it carried from the previous row", row)
		}
	})

	t.Run("no previous instance", func(t *testing.T) {
		d := openRestoreTestDB(t)
		status := seedStatus(t, d, sessionName, "/wt", nil)

		newIID := startRestoredIncarnation(d, status, "worker")

		if newIID == "" {
			t.Fatal("startRestoredIncarnation returned no instance ID")
		}
		if got := currentInstanceID(t, d, sessionName); got != newIID {
			t.Errorf("agent_status.instance_id = %q, want %q", got, newIID)
		}
		if row := sessionRow(t, d, newIID); row == nil || row.EndedAt != nil {
			t.Errorf("new sessions row = %+v, want a live row", row)
		}
	})
}

// TestRestoreSession_SidecarGetsNewInstanceID drives restoreSession against
// the #3027 shape: agent_status names an instance whose sessions row has
// ended. The restored sidecar must start with --instance-id set to a new
// instance, or it reads the ended one from agent_status and writes every
// event with it.
func TestRestoreSession_SidecarGetsNewInstanceID(t *testing.T) {
	skipRestoreOnGHA(t)
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc/<pid>/cmdline")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stubNvimOnPath(t)
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	d := openRestoreTestDB(t)

	const sessionName = "testrepo@restart-3027"
	worktreeDir := t.TempDir()
	status := seedStatus(t, d, sessionName, worktreeDir, nil)
	oldIID := uuid.New().String()
	seedIncarnation(t, d, sessionName, oldIID, time.Now().Add(-time.Minute))
	status.InstanceID = &oldIID

	// The tmux server is already running, so its panes do not see this
	// variable. Only the sidecar, which restore starts from this process,
	// sees it and sleeps as a stub.
	t.Setenv("PRISM_CMD_TEST_STUB", "1")
	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	newIID := currentInstanceID(t, d, sessionName)
	if newIID == "" || newIID == oldIID {
		t.Fatalf("agent_status.instance_id = %q after restore, want a new instance (old %q)", newIID, oldIID)
	}

	pidPath, err := session.SidecarPIDPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read sidecar PID file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatalf("parse sidecar PID %q: %v", pidData, err)
	}
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		t.Fatalf("read sidecar cmdline: %v", err)
	}
	args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	got := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--instance-id" {
			got = args[i+1]
		}
	}
	if got != newIID {
		t.Errorf("sidecar --instance-id = %q, want the new instance %q (argv %q)", got, newIID, args)
	}
}
