package cmd

// Tests for issue #3027: a restored session continues its incarnation, and
// no restored session writes events with the instance ID of an ended
// session.

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/profile"
	"github.com/prismatic-koi/prism/internal/session"
)

// The slow-stop sidecar stub reads these variables. TestMain runs it when
// slowStopDBEnv is set.
const (
	slowStopDBEnv    = "PRISM_CMD_TEST_SLOW_STOP_DB"
	slowStopIIDEnv   = "PRISM_CMD_TEST_SLOW_STOP_IID"
	slowStopDelayEnv = "PRISM_CMD_TEST_SLOW_STOP_DELAY"
)

// runSlowStopSidecarStub acts as a sidecar that is slow to stop. On SIGTERM
// it waits for the delay and then writes one agent_events row with the
// instance ID it holds, as the Shutdown of a real sidecar does.
func runSlowStopSidecarStub() int {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	select {
	case <-sigCh:
	case <-time.After(60 * time.Second):
		return 0
	}
	delay, _ := time.ParseDuration(os.Getenv(slowStopDelayEnv))
	time.Sleep(delay)

	sessionName := ""
	for i := 1; i+1 < len(os.Args); i++ {
		if os.Args[i] == "--session" {
			sessionName = os.Args[i+1]
		}
	}
	d, err := db.Open(os.Getenv(slowStopDBEnv))
	if err != nil {
		return 1
	}
	defer d.Close()
	iid := os.Getenv(slowStopIIDEnv)
	if err := d.WriteEvent(db.Event{
		ID: uuid.New().String(), SessionName: sessionName, Repo: "testrepo",
		Worktree: "/wt", Type: "state_change", Payload: `{"state":"interrupted"}`,
		InstanceID: &iid,
	}); err != nil {
		return 1
	}
	return 0
}

const restoreTestParent = "testrepo@main"

// seedIncarnation gives the agent_status row of sessionName the instance ID
// iid, inserts its sessions row, and inserts a spawn_inputs row with profile
// and a spawn_outcome row. When endedAt is non-zero, the sessions row is
// ended at that time, as the Shutdown of a stopped sidecar leaves it.
func seedIncarnation(t *testing.T, d *db.DB, sessionName, iid, profileName string, endedAt time.Time) {
	t.Helper()
	if err := d.SetInstanceID(sessionName, iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	parent := restoreTestParent
	role := "worker"
	if err := d.InsertSession(db.Session{
		InstanceID: iid, SessionName: sessionName, Repo: "testrepo",
		Worktree: "/wt", Harness: "pi", AgentRole: &role, ParentSession: &parent,
		StartedAt: time.Now().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	p := profileName
	if err := d.InsertSpawnInputs(db.SpawnInputs{
		InstanceID: iid, ProfileName: &p, CreatedAt: time.Now().Add(-48 * time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}
	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
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

// assertContinuedIncarnation checks the Option A contract: the session keeps
// iid, its sessions row is open, and its spawn rows still resolve through it.
func assertContinuedIncarnation(t *testing.T, d *db.DB, sessionName, iid, profileName string) {
	t.Helper()
	if got := currentInstanceID(t, d, sessionName); got != iid {
		t.Errorf("agent_status.instance_id = %q, want the same instance %q", got, iid)
	}
	row := sessionRow(t, d, iid)
	if row == nil {
		t.Fatalf("sessions row %s is missing", iid)
	}
	if row.EndedAt != nil || row.EndState != nil {
		t.Errorf("sessions row %s: ended_at = %v, end_state = %v, want both NULL", iid, row.EndedAt, row.EndState)
	}
	if si, err := d.SpawnInputsByInstanceID(iid); err != nil || si == nil {
		t.Errorf("spawn_inputs for %s: %v, %v — want the row", iid, si, err)
	}
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM spawn_outcome WHERE instance_id = ?", iid).Scan(&n); err != nil || n != 1 {
		t.Errorf("spawn_outcome rows for %s = %d (%v), want 1", iid, n, err)
	}
	if got := profile.SpawnTimeForSession(d, sessionName); got != profileName {
		t.Errorf("SpawnTimeForSession = %q, want the pinned profile %q", got, profileName)
	}
}

func TestRestoredInstanceID(t *testing.T) {
	const sessionName = "testrepo@feature"
	const pinned = "tier-heavy"

	t.Run("old sidecar stopped and ended the row", func(t *testing.T) {
		d := openRestoreTestDB(t)
		status := seedStatus(t, d, sessionName, "/wt", nil)
		iid := uuid.New().String()
		seedIncarnation(t, d, sessionName, iid, pinned, time.Now().Add(-time.Minute))
		status.InstanceID = &iid

		if got := restoredInstanceID(d, status, "worker", true); got != iid {
			t.Fatalf("restoredInstanceID = %q, want the same instance %q", got, iid)
		}
		assertContinuedIncarnation(t, d, sessionName, iid, pinned)
	})

	t.Run("old sidecar stopped and the row is open", func(t *testing.T) {
		d := openRestoreTestDB(t)
		status := seedStatus(t, d, sessionName, "/wt", nil)
		iid := uuid.New().String()
		seedIncarnation(t, d, sessionName, iid, pinned, time.Time{})
		status.InstanceID = &iid

		if got := restoredInstanceID(d, status, "worker", true); got != iid {
			t.Fatalf("restoredInstanceID = %q, want the same instance %q", got, iid)
		}
		assertContinuedIncarnation(t, d, sessionName, iid, pinned)
	})

	t.Run("instance has no sessions row", func(t *testing.T) {
		d := openRestoreTestDB(t)
		status := seedStatus(t, d, sessionName, "/wt", nil)
		iid := uuid.New().String()
		if err := d.SetInstanceID(sessionName, iid); err != nil {
			t.Fatalf("SetInstanceID: %v", err)
		}
		status.InstanceID = &iid

		if got := restoredInstanceID(d, status, "worker", true); got != iid {
			t.Fatalf("restoredInstanceID = %q, want the same instance %q", got, iid)
		}
		if row := sessionRow(t, d, iid); row == nil || row.EndedAt != nil {
			t.Errorf("sessions row for %s = %+v, want a live row", iid, row)
		}
	})

	t.Run("old sidecar did not stop", func(t *testing.T) {
		d := openRestoreTestDB(t)
		status := seedStatus(t, d, sessionName, "/wt", nil)
		oldIID := uuid.New().String()
		seedIncarnation(t, d, sessionName, oldIID, pinned, time.Time{})
		status.InstanceID = &oldIID

		newIID := restoredInstanceID(d, status, "", false)

		if newIID == "" || newIID == oldIID {
			t.Fatalf("restoredInstanceID = %q, want a new instance (old %q)", newIID, oldIID)
		}
		if got := currentInstanceID(t, d, sessionName); got != newIID {
			t.Errorf("agent_status.instance_id = %q, want %q", got, newIID)
		}
		if old := sessionRow(t, d, oldIID); old == nil || old.EndedAt == nil {
			t.Error("old sessions row is still open: nothing else ends a row that agent_status no longer names")
		}
		row := sessionRow(t, d, newIID)
		if row == nil || row.EndedAt != nil {
			t.Fatalf("new sessions row = %+v, want a live row", row)
		}
		if row.ParentSession == nil || *row.ParentSession != restoreTestParent {
			t.Errorf("new sessions row parent_session = %v, want %s", row.ParentSession, restoreTestParent)
		}
		if row.AgentRole == nil || *row.AgentRole != "worker" {
			t.Errorf("new sessions row agent_role = %v, want it carried from the previous row", row.AgentRole)
		}
	})

	t.Run("old sidecar did not stop and the row already ended", func(t *testing.T) {
		d := openRestoreTestDB(t)
		status := seedStatus(t, d, sessionName, "/wt", nil)
		oldIID := uuid.New().String()
		oldEnd := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Millisecond)
		seedIncarnation(t, d, sessionName, oldIID, pinned, oldEnd)
		status.InstanceID = &oldIID

		if newIID := restoredInstanceID(d, status, "worker", false); newIID == "" || newIID == oldIID {
			t.Fatalf("restoredInstanceID = %q, want a new instance (old %q)", newIID, oldIID)
		}
		old := sessionRow(t, d, oldIID)
		if old == nil {
			t.Fatalf("old sessions row %s is missing", oldIID)
		}
		if old.EndedAt == nil || !old.EndedAt.Equal(oldEnd) {
			t.Errorf("old sessions row ended_at = %v, want it unchanged at %v", old.EndedAt, oldEnd)
		}
	})

	t.Run("no previous instance", func(t *testing.T) {
		d := openRestoreTestDB(t)
		status := seedStatus(t, d, sessionName, "/wt", nil)

		newIID := restoredInstanceID(d, status, "worker", true)

		if newIID == "" {
			t.Fatal("restoredInstanceID returned no instance ID")
		}
		if got := currentInstanceID(t, d, sessionName); got != newIID {
			t.Errorf("agent_status.instance_id = %q, want %q", got, newIID)
		}
		if row := sessionRow(t, d, newIID); row == nil || row.EndedAt != nil {
			t.Errorf("new sessions row = %+v, want a live row", row)
		}
	})
}

// sidecarInstanceIDArg reads the --instance-id argument of the running
// sidecar of sessionName from /proc.
func sidecarInstanceIDArg(t *testing.T, sessionName string) string {
	t.Helper()
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
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--instance-id" {
			return args[i+1]
		}
	}
	t.Fatalf("sidecar argv has no --instance-id: %q", args)
	return ""
}

// startRestoreTest prepares the tmux server, the DB, and a session row for
// a restoreSession test.
func startRestoreTest(t *testing.T) *db.DB {
	t.Helper()
	skipRestoreOnGHA(t)
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc/<pid>/cmdline")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stubNvimOnPath(t)
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	return openRestoreTestDB(t)
}

// TestRestoreSession_ContinuesIncarnation restores the #3027 shape: the old
// sidecar stopped and ended the sessions row of the instance that
// agent_status names. The restored session must keep that instance, with
// an open row, and its sidecar must start with it as --instance-id.
func TestRestoreSession_ContinuesIncarnation(t *testing.T) {
	d := startRestoreTest(t)
	const sessionName = "testrepo@restart-3027"
	status := seedStatus(t, d, sessionName, t.TempDir(), nil)
	iid := uuid.New().String()
	seedIncarnation(t, d, sessionName, iid, "tier-heavy", time.Now().Add(-time.Minute))
	status.InstanceID = &iid

	// The tmux server is already running, so its panes do not see this
	// variable. Only the sidecar, which restore starts from this process,
	// sees it and sleeps as a stub.
	t.Setenv("PRISM_CMD_TEST_STUB", "1")
	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	assertContinuedIncarnation(t, d, sessionName, iid, "tier-heavy")
	if got := sidecarInstanceIDArg(t, sessionName); got != iid {
		t.Errorf("sidecar --instance-id = %q, want %q", got, iid)
	}
}

// TestRestoreSession_OldSidecarDoesNotStop restores a session whose old
// sidecar does not stop inside the wait and writes an event with its
// instance ID later, as a slow Shutdown does. Restore must start a new
// incarnation, and the old sidecar must not write an event with the
// instance ID of an ended session.
func TestRestoreSession_OldSidecarDoesNotStop(t *testing.T) {
	d := startRestoreTest(t)
	prevTimeout := restoreSidecarStopTimeout
	restoreSidecarStopTimeout = 300 * time.Millisecond
	t.Cleanup(func() { restoreSidecarStopTimeout = prevTimeout })

	const sessionName = "testrepo@restart-slow-3027"
	status := seedStatus(t, d, sessionName, t.TempDir(), nil)
	oldIID := uuid.New().String()
	seedIncarnation(t, d, sessionName, oldIID, "tier-heavy", time.Time{})
	status.InstanceID = &oldIID

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	stub := exec.Command(self, "sidecar", "--session", sessionName)
	stub.Env = append(os.Environ(),
		slowStopDBEnv+"="+d.Path(), slowStopIIDEnv+"="+oldIID, slowStopDelayEnv+"=1s")
	if err := stub.Start(); err != nil {
		t.Fatalf("start slow-stop stub: %v", err)
	}
	t.Cleanup(func() { _ = stub.Process.Kill() })
	stubDone := make(chan syscall.WaitStatus, 1)
	go func() {
		_ = stub.Wait()
		ws, _ := stub.ProcessState.Sys().(syscall.WaitStatus)
		stubDone <- ws
	}()
	pidPath, err := session.SidecarPIDPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(stub.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}
	// Give the stub time to install its SIGTERM handler.
	time.Sleep(200 * time.Millisecond)

	t.Setenv("PRISM_CMD_TEST_STUB", "1")
	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	select {
	case ws := <-stubDone:
		if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Errorf("old sidecar wait status = %v, want killed by SIGKILL", ws)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old sidecar is still running after restore")
	}

	newIID := currentInstanceID(t, d, sessionName)
	if newIID == "" || newIID == oldIID {
		t.Fatalf("agent_status.instance_id = %q, want a new instance (old %q)", newIID, oldIID)
	}
	if got := sidecarInstanceIDArg(t, sessionName); got != newIID {
		t.Errorf("sidecar --instance-id = %q, want the new instance %q", got, newIID)
	}
	var n int
	if err := d.QueryRow(`
SELECT COUNT(*) FROM agent_events ae
  JOIN sessions s ON s.instance_id = ae.instance_id
 WHERE s.ended_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count events of ended sessions: %v", err)
	}
	if n != 0 {
		t.Errorf("%d agent_events row(s) use the instance ID of an ended session, want 0", n)
	}
}
