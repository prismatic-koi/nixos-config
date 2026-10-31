package cmd

// Tests for #3380: a podman client that the sidecar started can still run
// when the sidecar dies. `prism cleanup`, `prism close`, and the stop
// before a conversion stop that client before the container sweep, so it
// cannot create a container after the sweep.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
	prismSession "github.com/prismatic-koi/prism/internal/session"
)

// dirPodman is a fake podman whose containers are files in dir. The file
// name is the container ID, and the file holds the instance ID. A fake
// podman client in another process creates a container with a file.
type dirPodman struct{ dir string }

func (p dirPodman) Run(_ context.Context, stdout, _ io.Writer, args ...string) (int, error) {
	switch args[0] {
	case "ps":
		entries, err := os.ReadDir(p.dir)
		if err != nil {
			return 125, nil
		}
		list := []map[string]any{}
		for _, e := range entries {
			iid, err := os.ReadFile(filepath.Join(p.dir, e.Name()))
			if err != nil || strings.HasSuffix(e.Name(), ".tmp") {
				continue
			}
			list = append(list, map[string]any{
				"Id": e.Name(), "State": "running",
				"Labels": map[string]string{prismcontainer.LabelInstanceID: strings.TrimSpace(string(iid))},
			})
		}
		if err := json.NewEncoder(stdout).Encode(list); err != nil {
			return 125, nil
		}
	case "rm":
		// rm --force --ignore --volumes --time 0 <id>...
		for _, id := range args[6:] {
			_ = os.Remove(filepath.Join(p.dir, id))
		}
	case "images":
		_, _ = io.WriteString(stdout, "[]")
	}
	return 0, nil
}

// orphanClientSidecar is a fake sidecar with a fake podman client in its
// process group. At SIGTERM the sidecar tells the client, waits until the
// client has created the container "late", and exits. The client ignores
// SIGTERM and SIGHUP, and it creates "late" again each time that it is
// gone, until SIGKILL.
const orphanClientSidecar = `sh -c 'trap "" TERM HUP
echo $$ > "$STUB_DIR/client.tmp"; mv "$STUB_DIR/client.tmp" "$STUB_DIR/client.pid"
while :; do
  if [ -e "$STUB_DIR/stopping" ] && [ ! -e "$PODMAN_DIR/late" ]; then
    echo "$STUB_IID" > "$PODMAN_DIR/late.tmp"; mv "$PODMAN_DIR/late.tmp" "$PODMAN_DIR/late"
  fi
  sleep 0.02
done' &
while [ ! -s "$STUB_DIR/client.pid" ]; do sleep 0.01; done
trap ': > "$STUB_DIR/stopping"; while [ ! -e "$PODMAN_DIR/late" ]; do sleep 0.01; done; exit 0' TERM
: > "$STUB_DIR/ready"
while :; do sleep 0.05; done
`

// startOrphanClientSidecar starts orphanClientSidecar as the sidecar of
// session, in a session of its own as StartSidecarWithOpts starts the
// sidecar, and writes its PID file. It returns the PID of the client.
func startOrphanClientSidecar(t *testing.T, session, iid, podmanDir string) int {
	t.Helper()
	stubDir := t.TempDir()
	cmd := exec.Command("/bin/sh", "-c", orphanClientSidecar, "sh", "sidecar", "--session", session)
	cmd.Env = append(os.Environ(), "STUB_DIR="+stubDir, "PODMAN_DIR="+podmanDir, "STUB_IID="+iid)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the fake sidecar: %v", err)
	}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	pidPath, err := prismSession.SidecarPIDPath(session)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(stubDir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake sidecar did not get ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	data, err := os.ReadFile(filepath.Join(stubDir, "client.pid"))
	if err != nil {
		t.Fatalf("read the client PID: %v", err)
	}
	client, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("client PID %q: %v", data, err)
	}
	return client
}

// processRuns reports whether pid runs and is not a zombie.
func processRuns(pid int) bool {
	if syscall.Kill(pid, 0) == syscall.ESRCH {
		return false
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	return err != nil || !strings.Contains(string(status), "State:\tZ")
}

// AC: after `prism cleanup`, `prism close`, or the stop before a
// conversion, no container that the sidecar started remains, also when a
// podman client of the sidecar still runs when the sidecar dies and
// creates the container late. The client creates the container again each
// time that it is gone, so a client that outlives the stop leaves a
// container after the sweep.
func TestSessionEndingPaths_OrphanedPodmanClientLeavesNoContainer_3380(t *testing.T) {
	const iid = "0f0e0d0c-0b0a-4908-8706-050403023380"
	for _, c := range cleanupCallers {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			// A descendant name makes doCleanup skip git, so that the path
			// needs no repository.
			const session = "myrepo@orphan-client~investigate-x"
			dbFile, _ := setupCleanupCaller(t, session)
			d, err := db.Open(dbFile)
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			if err := d.SetInstanceID(session, iid); err != nil {
				t.Fatalf("SetInstanceID: %v", err)
			}
			d.Close()
			writeFakePrismContainerAuditLog(t, iid, session)
			podmanDir := t.TempDir()
			prismContainerRunnerForTest = dirPodman{dir: podmanDir}
			prismContainerBuildExecutorForTest = &prismcontainertest.Fake{}
			t.Cleanup(func() {
				prismContainerRunnerForTest = nil
				prismContainerBuildExecutorForTest = nil
			})
			client := startOrphanClientSidecar(t, session, iid, podmanDir)

			if err := c.run(t, session, ""); err != nil {
				t.Fatalf("error = %v, want nil", err)
			}

			if processRuns(client) {
				t.Errorf("the podman client (pid %d) of the sidecar still runs after the stop", client)
			}
			// A client that still ran would create the container again
			// within 20 ms.
			time.Sleep(200 * time.Millisecond)
			if _, err := os.Stat(filepath.Join(podmanDir, "late")); err == nil {
				t.Error("the container that the podman client created late remains after the stop")
			}
			if readStatus(t, dbFile, session).EndedAt == nil {
				t.Error("row open, want ended")
			}
		})
	}
}

// AC: a stop or a conversion is refused when a process that the sidecar
// started still runs after the wait. The error names the PID and the
// session, and the session keeps its row.
func TestSessionEndingPaths_GroupProcessStillRunsRefuses_3380(t *testing.T) {
	for _, c := range cleanupCallers {
		t.Run(c.name, func(t *testing.T) {
			const session = "myrepo@group-runs~investigate-x"
			dbFile, tmuxLog := setupCleanupCaller(t, session)
			groupErr := &prismSession.SidecarGroupError{Session: session, PGID: 4242, PIDs: []int{4243}, Signalled: true}
			prevCleanup, prevSwitch := cleanupStopSidecar, switchProjectStopSidecar
			stop := func(string) error { return groupErr }
			cleanupStopSidecar, switchProjectStopSidecar = stop, stop
			t.Cleanup(func() { cleanupStopSidecar, switchProjectStopSidecar = prevCleanup, prevSwitch })

			var err error
			captureStderrDuringFn(t, func() { err = c.run(t, session, "") })

			if !errors.Is(err, errSidecarNotStopped) || !errors.Is(err, error(groupErr)) {
				t.Errorf("error = %v, want the group error and errSidecarNotStopped", err)
			}
			if err != nil && (!strings.Contains(err.Error(), "4243") || !strings.Contains(err.Error(), strconv.Quote(session))) {
				t.Errorf("error %q does not name the PID 4243 and the session %q", err, session)
			}
			if st := readStatus(t, dbFile, session); st.EndedAt != nil || st.HarnessPort == nil {
				t.Errorf("ended_at=%v port=%v, want an open row with its port", st.EndedAt, st.HarnessPort)
			}
			if tmuxKilled(t, tmuxLog, session) {
				t.Error("tmux session killed, want kept")
			}
		})
	}
}

// assertGroupRunsError checks the restore or switch error for a process of
// the group of the old sidecar. It names the process and the session. It
// does not tell the owner to remove the PID file, because the next stop
// finds the group only through that file.
func assertGroupRunsError(t *testing.T, err error, sessionName string, member int) {
	t.Helper()
	if err == nil {
		t.Fatal("no error, want a refusal while a process of the group of the old sidecar runs")
	}
	for _, want := range []string{strconv.Itoa(member), strconv.Quote(sessionName), "Stop each process that the error names"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	}
	pidPath, pathErr := prismSession.SidecarPIDPath(sessionName)
	if pathErr != nil {
		t.Fatalf("SidecarPIDPath: %v", pathErr)
	}
	if strings.Contains(err.Error(), pidPath) || strings.Contains(err.Error(), "remove its PID file") {
		t.Errorf("error = %q, want no advice to remove the PID file %s", err, pidPath)
	}
}

// prism restore refuses a session while a process of the group of the old
// sidecar runs, and its error gives the advice for that process.
func TestRestoreSession_GroupProcessRuns_Refused_3380(t *testing.T) {
	d := startRestoreServer(t)
	const sessionName = "testrepo@group-runs-3380"
	const oldPID, member = 424242, 424243
	status, iid := seedRestoreCoordinator(t, d, sessionName, 4)
	prev := restoreStopSidecar
	restoreStopSidecar = func(string, time.Duration) (int, bool, error) {
		return oldPID, true, &prismSession.SidecarGroupError{Session: sessionName, PGID: oldPID, PIDs: []int{member}, Signalled: true}
	}
	t.Cleanup(func() { restoreStopSidecar = prev })

	t.Setenv("PRISM_CMD_TEST_STUB", "1")
	err := callRestoreSession(d, status)
	t.Cleanup(func() { prismSession.KillSidecar(sessionName) })
	assertGroupRunsError(t, err, sessionName, member)
	if err != nil && !strings.Contains(err.Error(), "merge queue is not resumed: PR #4") {
		t.Errorf("error = %q, want the merge queue that is not resumed", err)
	}
	assertNotRestored(t, d, sessionName, iid, 4)
}

// prism switch does not restart a stale session while a process of the
// group of the old sidecar runs, and its error gives the advice for that
// process.
func TestEnsureAndSwitch_StaleSidecarGroupRuns_KeepsSession_3380(t *testing.T) {
	f := newStaleSwitchFixture(t, 62)
	const member = 424243
	prev := switchStopSidecar
	switchStopSidecar = func(string, time.Duration) error {
		return &prismSession.SidecarGroupError{Session: f.sessionName, PGID: 424242, PIDs: []int{member}, Signalled: true}
	}
	t.Cleanup(func() { switchStopSidecar = prev })

	err := ensureAndSwitch(f.dir, "", switchTestOpts())
	assertGroupRunsError(t, err, f.sessionName, member)
	if got := f.sessionID(t); got != f.tmuxID {
		t.Errorf("tmux session_id = %s, want %s: the session must not be recreated", got, f.tmuxID)
	}
	assertQueue(t, f.d, f.sessionName, f.iid, 62)
}
