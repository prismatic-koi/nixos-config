package cmd

// Tests for issue #3303: prism restore does not start a second merge-queue
// watcher next to an old sidecar that the stop cannot prove gone. It does
// not restore that session, and it still restores the other sessions.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

// stubRestoreStop replaces the stop of the old sidecar in restore. A session
// in notGone gets the result of a stop that cannot prove that the old
// sidecar is gone, with that PID. Every other session gets the result of a
// stop that found no old sidecar.
func stubRestoreStop(t *testing.T, notGone map[string]int) {
	t.Helper()
	prev := restoreStopSidecar
	restoreStopSidecar = func(sessionName string, _ time.Duration) (int, bool, error) {
		if pid, ok := notGone[sessionName]; ok {
			return pid, false, fmt.Errorf("fake stop: sidecar pid %d still runs", pid)
		}
		return 0, true, nil
	}
	t.Cleanup(func() { restoreStopSidecar = prev })
}

// startRestoreServer prepares the state dir, the tmux server, and the DB
// for a restore test.
func startRestoreServer(t *testing.T) *db.DB {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stubNvimOnPath(t)
	withCmdServer(t, newCmdTestServer(t))
	return openRestoreTestDB(t)
}

// seedRestoreCoordinator seeds an active coordinator sessionName with a
// worktree, an open incarnation, and the merge queue prs, and returns its
// status and instance ID.
func seedRestoreCoordinator(t *testing.T, d *db.DB, sessionName string, prs ...int) (db.Status, string) {
	t.Helper()
	status := seedStatus(t, d, sessionName, t.TempDir(), nil)
	iid := uuid.New().String()
	seedIncarnation(t, d, sessionName, iid, "tier-heavy", time.Time{})
	status.InstanceID = &iid
	enqueueCoordinatorPRs(t, d, sessionName, iid, prs...)
	return status, iid
}

// assertNotRestored checks that restore did not restore sessionName and did
// not change its rows: no tmux session, no sidecar, no port, the same open
// incarnation iid, and the queue prs still owned by iid.
func assertNotRestored(t *testing.T, d *db.DB, sessionName, iid string, prs ...int) {
	t.Helper()
	if session.HasExactSession(sessionName) {
		t.Errorf("tmux session %q exists, want none: the session must not be restored", sessionName)
	}
	pidPath, err := session.SidecarPIDPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	if _, statErr := os.Stat(pidPath); !os.IsNotExist(statErr) {
		t.Errorf("sidecar PID file %s: %v, want absent: no sidecar may start", pidPath, statErr)
	}
	var port *int
	if err := d.QueryRow(`SELECT harness_port FROM agent_status WHERE session_name = ?`, sessionName).Scan(&port); err != nil {
		t.Fatalf("read harness_port: %v", err)
	}
	if port != nil {
		t.Errorf("harness_port = %d, want NULL: restore must not allocate a port", *port)
	}
	if got := currentInstanceID(t, d, sessionName); got != iid {
		t.Errorf("agent_status.instance_id = %q, want the same instance %q", got, iid)
	}
	if row := sessionRow(t, d, iid); row == nil || row.EndedAt != nil {
		t.Errorf("sessions row %s = %+v, want it open, as before", iid, row)
	}
	assertQueue(t, d, sessionName, iid, prs...)
}

// TestRestoreSession_OldSidecarNotGone_Refused restores a coordinator whose
// stop cannot prove that the old sidecar is gone. Restore must return an
// error before it starts a sidecar or changes a row of the session. The
// error names the PID and the PRs, and tells the user what to do.
func TestRestoreSession_OldSidecarNotGone_Refused(t *testing.T) {
	d := startRestoreServer(t)
	const sessionName = "testrepo@refused-3303"
	const oldPID = 424242
	status, iid := seedRestoreCoordinator(t, d, sessionName, 4, 5)
	stubRestoreStop(t, map[string]int{sessionName: oldPID})

	t.Setenv("PRISM_CMD_TEST_STUB", "1")
	err := callRestoreSession(d, status)
	t.Cleanup(func() { session.KillSidecar(sessionName) })
	if err == nil {
		t.Fatal("restoreSession: no error, want an error without proof that the old sidecar is gone")
	}
	pidPath, pathErr := session.SidecarPIDPath(sessionName)
	if pathErr != nil {
		t.Fatalf("SidecarPIDPath: %v", pathErr)
	}
	for _, want := range []string{
		"not restored",
		"process " + strconv.Itoa(oldPID),
		"merge queue is not resumed: PR #4, PR #5",
		"Stop process " + strconv.Itoa(oldPID) + ", then run prism restore again",
		pidPath,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	}
	assertNotRestored(t, d, sessionName, iid, 4, 5)
}

// TestRestoreSession_RefusedThenOldSidecarExits covers an old sidecar that
// the stop cannot prove gone, and that exits after the restore. Its
// Shutdown then abandons the queue. The next restore must restore the
// session in the same incarnation and resume the queue.
func TestRestoreSession_RefusedThenOldSidecarExits(t *testing.T) {
	d := startRestoreServer(t)
	const sessionName = "testrepo@refused-then-gone-3303"
	status, iid := seedRestoreCoordinator(t, d, sessionName, 8, 7)
	t.Setenv("PRISM_CMD_TEST_STUB", "1")
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	stubRestoreStop(t, map[string]int{sessionName: 434343})
	if err := callRestoreSession(d, status); err == nil {
		t.Fatal("restoreSession: no error, want an error without proof that the old sidecar is gone")
	}
	assertNotRestored(t, d, sessionName, iid, 8, 7)

	shutDownSidecar(t, d, iid)

	stubRestoreStop(t, nil)
	var restoreErr error
	stdout, stderr := captureStdoutStderr(t, func() { restoreErr = callRestoreSession(d, status) })
	if restoreErr != nil {
		t.Fatalf("restoreSession: %v (stderr %q)", restoreErr, stderr)
	}
	if !session.HasExactSession(sessionName) {
		t.Errorf("tmux session %q is missing after the second restore", sessionName)
	}
	assertContinuedIncarnation(t, d, sessionName, iid, "tier-heavy")
	assertQueue(t, d, sessionName, iid, 8, 7)
	if want := "merge queue resumed: PR #8, PR #7"; !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestRestore_RefusedSessionDoesNotStopOthers runs prism restore, as the
// prism-restore service does at login, with two coordinators. The stop of
// one cannot prove that its old sidecar is gone. The other stopped at the
// host shutdown. Restore must refuse only the first, restore the second and
// resume its queue, and return no error, so the service does not fail.
func TestRestore_RefusedSessionDoesNotStopOthers(t *testing.T) {
	d := startRestoreServer(t)
	SetTestDBPath(d.Path())
	t.Cleanup(func() { SetTestDBPath("") })
	withRestoreConfig(t, config.Config{PIExtensionDir: "/test/prism-pi-extension", RestoreStaggerDelayMs: -1})

	const refused = "testrepo@login-refused-3303"
	const restored = "testrepo@login-restored-3303"
	const oldPID = 454545
	_, refusedIID := seedRestoreCoordinator(t, d, refused, 14, 15)
	_, restoredIID := seedRestoreCoordinator(t, d, restored, 24, 23)
	shutDownSidecar(t, d, restoredIID)
	stubRestoreStop(t, map[string]int{refused: oldPID})

	t.Setenv("PRISM_CMD_TEST_STUB", "1")
	t.Cleanup(func() {
		session.KillSidecar(refused)
		session.KillSidecar(restored)
	})
	var restoreErr error
	stdout, stderr := captureStdoutStderr(t, func() { restoreErr = Restore(false) })
	if restoreErr != nil {
		t.Fatalf("Restore: %v, want no error: one refused session must not fail the restore of the others", restoreErr)
	}

	for _, want := range []string{
		`session "` + restored + `" restored`,
		`session "` + restored + `": merge queue resumed: PR #24, PR #23`,
		"restore complete: 1 restored, 0 skipped, 1 failed",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}
	}
	for _, want := range []string{
		`restore "` + refused + `": the session is not restored`,
		"process " + strconv.Itoa(oldPID),
		"merge queue is not resumed: PR #14, PR #15",
		"run prism restore again",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	}
	if !session.HasExactSession(restored) {
		t.Errorf("tmux session %q is missing: the other session must be restored", restored)
	}
	assertQueue(t, d, restored, restoredIID, 24, 23)
	assertNotRestored(t, d, refused, refusedIID, 14, 15)
}
