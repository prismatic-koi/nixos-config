package cmd

// restore_unsafe_name_3308_test.go — prism restore reports a row whose
// session name is outside the safe set, and does not restore it (issue
// #3308). A row of that shape predates the check. tmux.nix puts the session
// name into shell commands, so a restored session of that name can run a
// command on the host.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/session"
)

func TestRestore_UnsafeSessionNameIsReportedAndNotRestored(t *testing.T) {
	d := startRestoreServer(t)
	SetTestDBPath(d.Path())
	t.Cleanup(func() { SetTestDBPath("") })
	withRestoreConfig(t, config.Config{PIExtensionDir: "/test/prism-pi-extension", RestoreStaggerDelayMs: -1})
	stubRestoreStop(t, nil)
	t.Setenv("PRISM_CMD_TEST_STUB", "1")

	marker := filepath.Join(t.TempDir(), "pwned")
	unsafe := "testrepo@x$(touch${IFS}" + marker + ")"
	const safe = "testrepo@safe-3308"
	seedStatus(t, d, unsafe, t.TempDir(), nil)
	seedStatus(t, d, safe, t.TempDir(), nil)
	t.Cleanup(func() { session.KillSidecar(safe) })

	for _, dryRun := range []bool{true, false} {
		var restoreErr error
		stdout, stderr := captureStdoutStderr(t, func() { restoreErr = Restore(dryRun) })
		if restoreErr != nil {
			t.Fatalf("Restore(%v): %v, want no error", dryRun, restoreErr)
		}
		for _, want := range []string{`restore "` + unsafe + `": not restored: session name`, `"$"`, `"("`, `")"`} {
			if !strings.Contains(stderr, want) {
				t.Errorf("Restore(%v) stderr = %q, want %q", dryRun, stderr, want)
			}
		}
		if want := "restore complete: 1 restored, 0 skipped, 1 failed"; !strings.Contains(stdout, want) {
			t.Errorf("Restore(%v) stdout = %q, want %q", dryRun, stdout, want)
		}
		if strings.Contains(stdout, "would restore: "+unsafe) {
			t.Errorf("Restore(%v) stdout = %q, want no restore line for %q", dryRun, stdout, unsafe)
		}
	}

	if session.HasExactSession(unsafe) {
		t.Errorf("tmux session %q exists, want none", unsafe)
	}
	if !session.HasExactSession(safe) {
		t.Errorf("tmux session %q is missing: the check must not stop the restore of a safe name", safe)
	}
	if isEnded(t, d, unsafe) {
		t.Errorf("agent_status row %q is ended, want it kept for the user to decide", unsafe)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker %s: %v, want absent: the session name ran as a command", marker, err)
	}
}
