package cmd

// Tests for issue #3309: after kill-server, the old tmux server can still
// run its session-closed hooks. A new-session in that time fails with
// "server exited unexpectedly". prism restore must try that session again,
// and report it as failed only after the last attempt.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// failNewSessionWithServerExit wraps the current tmux.TmuxBin. The first
// failures calls of `new-session -ds sessionName` print "server exited
// unexpectedly" and exit 1, as a client of an exiting server does. Every
// other call goes to the wrapped tmux.
func failNewSessionWithServerExit(t *testing.T, sessionName string, failures int) {
	t.Helper()
	dir := t.TempDir()
	counter := dir + "/failures"
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = new-session ] && [ "$2" = -ds ] && [ "$3" = '%[1]s' ]; then
  echo x >> %[2]s
  if [ "$(wc -l < %[2]s)" -le %[3]d ]; then
    echo 'server exited unexpectedly' >&2
    exit 1
  fi
fi
exec %[4]s "$@"
`, sessionName, counter, failures, tmux.TmuxBin)
	if err := os.WriteFile(dir+"/tmux", []byte(script), 0o755); err != nil {
		t.Fatalf("write tmux wrapper: %v", err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = dir + "/tmux"
	t.Cleanup(func() { tmux.TmuxBin = orig })
}

func restoreWithServerExit3309(t *testing.T, sessionName string, failures int) (stdout, stderr string) {
	t.Helper()
	d := startRestoreServer(t)
	SetTestDBPath(d.Path())
	t.Cleanup(func() { SetTestDBPath("") })
	withRestoreConfig(t, config.Config{PIExtensionDir: "/test/prism-pi-extension", RestoreStaggerDelayMs: -1})
	stubRestoreStop(t, nil)
	t.Setenv("PRISM_CMD_TEST_STUB", "1")
	seedStatus(t, d, sessionName, t.TempDir(), nil)
	t.Cleanup(func() { session.KillSidecar(sessionName) })
	failNewSessionWithServerExit(t, sessionName, failures)

	var restoreErr error
	stdout, stderr = captureStdoutStderr(t, func() { restoreErr = Restore(false) })
	if restoreErr != nil {
		t.Fatalf("Restore: %v, want no error", restoreErr)
	}
	return stdout, stderr
}

// TestRestore_NewSessionServerExit_Restored: two server-exit failures, then
// a success. The session is restored.
func TestRestore_NewSessionServerExit_Restored(t *testing.T) {
	const name = "testrepo@server-exit-3309"
	stdout, stderr := restoreWithServerExit3309(t, name, 2)
	if want := "restore complete: 1 restored, 0 skipped, 0 failed"; !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want %q (stderr %q)", stdout, want, stderr)
	}
	if !session.HasExactSession(name) {
		t.Errorf("tmux session %q is missing after restore", name)
	}
}

// TestRestore_NewSessionServerExit_Failed: every attempt fails. Restore
// reports the session as failed and starts no sidecar for it.
func TestRestore_NewSessionServerExit_Failed(t *testing.T) {
	const name = "testrepo@server-exit-failed-3309"
	stdout, stderr := restoreWithServerExit3309(t, name, 1000)
	if want := "restore complete: 0 restored, 0 skipped, 1 failed"; !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	for _, want := range []string{`restore "` + name + `"`, "server exited unexpectedly", "tried 5 times"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	}
	if session.HasExactSession(name) {
		t.Errorf("tmux session %q exists, want none", name)
	}
	pidPath, err := session.SidecarPIDPath(name)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	if _, statErr := os.Stat(pidPath); !os.IsNotExist(statErr) {
		t.Errorf("sidecar PID file %s: %v, want absent", pidPath, statErr)
	}
}
