package session

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/tmux"
)

// serverExitTmuxBin installs a fake tmux. Its first failures calls of
// new-session print stderrMsg and exit 1. Every other call succeeds, and a
// call with -P prints the pane ID %42. It returns the file that gets one
// line per new-session call.
func serverExitTmuxBin(t *testing.T, failures int, stderrMsg string) string {
	t.Helper()
	dir := t.TempDir()
	newSessionCalls := dir + "/new-session-calls"
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = new-session ]; then
  echo x >> %[1]s
  if [ "$(wc -l < %[1]s)" -le %[2]d ]; then
    echo '%[3]s' >&2
    exit 1
  fi
fi
case " $* " in *' -P '*) echo '%%42' ;; esac
exit 0
`, newSessionCalls, failures, stderrMsg)
	if err := os.WriteFile(dir+"/tmux", []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = dir + "/tmux"
	t.Cleanup(func() { tmux.TmuxBin = orig })

	prevDelay := newSessionRetryDelay
	newSessionRetryDelay = time.Millisecond
	t.Cleanup(func() { newSessionRetryDelay = prevDelay })
	return newSessionCalls
}

// countSidecarStarts replaces the sidecar start of setupFullLayout and
// returns a pointer to the number of starts.
func countSidecarStarts(t *testing.T) *int {
	t.Helper()
	starts := 0
	prev := startLayoutSidecar
	startLayoutSidecar = func(string, StartSidecarOpts) error {
		starts++
		return nil
	}
	t.Cleanup(func() { startLayoutSidecar = prev })
	return &starts
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Count(string(data), "\n")
}

func createFullLayout3309(t *testing.T, sessionName string) error {
	t.Helper()
	_, _ = openSpawnTestDB(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	opts := Opts{
		SessionName:    sessionName,
		Layout:         LayoutFull,
		IsolationMode:  "bwrap",
		PIExtensionDir: testPIExtensionDir,
		ForceFresh:     true,
		SkipStatusSeed: true,
		Port:           41234,
	}
	return Create(sessionName, t.TempDir(), opts)
}

// TestCreate_NewSessionServerExit_Retries_3309: when new-session fails with
// "server exited unexpectedly" and then succeeds, Create makes the session
// and starts exactly one sidecar.
func TestCreate_NewSessionServerExit_Retries_3309(t *testing.T) {
	calls := serverExitTmuxBin(t, 2, tmuxServerExitedMsg)
	starts := countSidecarStarts(t)

	if err := createFullLayout3309(t, "prism-test@retry-3309"); err != nil {
		t.Fatalf("Create: %v, want success after the server-exit failures", err)
	}
	if got := countLines(t, calls); got != 3 {
		t.Errorf("new-session calls = %d, want 3 (2 failures, then a success)", got)
	}
	if *starts != 1 {
		t.Errorf("sidecar starts = %d, want 1", *starts)
	}
}

// TestCreate_NewSessionServerExit_GivesUp_3309: when every new-session
// fails with "server exited unexpectedly", Create stops after
// newSessionAttempts, returns an error, and starts no sidecar.
func TestCreate_NewSessionServerExit_GivesUp_3309(t *testing.T) {
	calls := serverExitTmuxBin(t, 1000, tmuxServerExitedMsg)
	starts := countSidecarStarts(t)

	err := createFullLayout3309(t, "prism-test@giveup-3309")
	if err == nil {
		t.Fatal("Create: no error, want an error after the last attempt")
	}
	for _, want := range []string{tmuxServerExitedMsg, fmt.Sprintf("tried %d times", newSessionAttempts)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	}
	if got := countLines(t, calls); got != newSessionAttempts {
		t.Errorf("new-session calls = %d, want %d", got, newSessionAttempts)
	}
	if *starts != 0 {
		t.Errorf("sidecar starts = %d, want 0", *starts)
	}
}

// TestCreate_NewSessionOtherError_NoRetry_3309: a new-session error that is
// not a server exit is returned at once.
func TestCreate_NewSessionOtherError_NoRetry_3309(t *testing.T) {
	calls := serverExitTmuxBin(t, 1000, "duplicate session: prism-test@dup-3309")
	starts := countSidecarStarts(t)

	if err := createFullLayout3309(t, "prism-test@dup-3309"); err == nil {
		t.Fatal("Create: no error, want the new-session error")
	}
	if got := countLines(t, calls); got != 1 {
		t.Errorf("new-session calls = %d, want 1", got)
	}
	if *starts != 0 {
		t.Errorf("sidecar starts = %d, want 0", *starts)
	}
}
