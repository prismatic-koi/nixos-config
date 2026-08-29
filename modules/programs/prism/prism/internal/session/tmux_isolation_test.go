package session

// Suite-wide tmux isolation for the session package.
//
// Production code in this package talks to tmux on nearly every path:
// Create/setupFullLayout (NewSessionDetached, NewWindow, RenameWindow,
// SendKeys, SelectWindow), SpawnSession (NewSessionDetached, NewWindow,
// KillSession on the failure paths), Exists/kill helpers (HasSession,
// KillSession), and attachOrSwitch (CurrentClient, SwitchClient).
//
// Crucially, every one of those reaches the LIVE host tmux server when no
// per-test stub is installed: the tmux client falls back to the default
// socket ($TMUX_TMPDIR/tmux-<uid>/default, /tmp/tmux-<uid>/default when
// TMUX_TMPDIR is unset) even with $TMUX unset or empty. Per-test opt-in stubs
// (spyTmuxBin / failTmuxBin rewriting tmux.TmuxBin) alone are not enough: the
// same posture produced a real session leak in internal/review (5 real
// review-agent sessions on the live server). The suite-wide neutralisation
// below, applied by TestMain before m.Run():
//
//   1. clears $TMUX, so a socket path inherited from an enclosing live pane
//      is never used; and
//   2. points $TMUX_TMPDIR at a fresh, empty directory, so the
//      default-socket fallback cannot reach the host server either.
//
// The per-test stubs become defence-in-depth instead of the only line of
// defence, and a test that forgets the stub fails fast with "error
// connecting to ..." — the same shape CI (no tmux server) produces.
//
// One test in this package intentionally runs a REAL tmux session on the
// default socket: TestCreate_LayoutFull_FailsFastOnEmptyPIExtensionDir's
// LayoutBare subtest (session_test.go). Without the redirect that session
// would be created on the LIVE host server; under the redirect, tmux
// auto-starts a private throwaway server inside the redirected $TMUX_TMPDIR
// instead, and
// the server exits when the subtest's KillSession removes its only session.
// That is why the redirected directory must have a SHORT path: tmux sockets
// are Unix domain sockets and sun_path is ~104 bytes on Darwin, and
// os.MkdirTemp("", ...) on macOS yields /var/folders/... paths that
// overflow it once tmux appends tmux-<uid>/default. Hence /tmp first, with
// the generic temp dir as fallback.

import (
	"os"
	"testing"

	"github.com/prismatic-koi/prism/internal/tmux"
)

// isoTmuxTmpdir records the redirected $TMUX_TMPDIR applied by TestMain
// (sidecar_test.go). TestSuiteTmuxIsolation_HostServerUnreachable asserts
// against it so the guard fails deterministically — even on hosts with no
// live tmux server (the CI shape) — if the TestMain-level isolation is
// removed or reordered after m.Run().
var isoTmuxTmpdir string

// isolateSuiteFromHostTmux applies the suite-wide tmux neutralisation
// described in the package comment and returns a restore function that puts
// the original environment back. TestMain (sidecar_test.go) calls restore
// after m.Run() so the test process leaves the environment as it found it.
func isolateSuiteFromHostTmux() (restore func()) {
	origTmux, hadTmux := os.LookupEnv("TMUX")
	origTmpdir, hadTmpdir := os.LookupEnv("TMUX_TMPDIR")

	isoDir, err := os.MkdirTemp("/tmp", "pse-")
	if err != nil {
		// /tmp unwritable (e.g. restrictive build sandboxes). Fall back to
		// the default temp dir: the resulting socket paths may exceed
		// sun_path, which only makes tmux fail faster — isolation is
		// preserved either way (such hosts have no tmux available for the
		// intentional real-server subtest anyway, which skips its
		// tmux-available branch).
		isoDir, err = os.MkdirTemp("", "pse-")
	}
	if err != nil {
		// Last resort: a path guaranteed not to contain a live socket.
		// Isolation trumps the real-tmux subtest's ability to run.
		isoDir = "/nonexistent/prism-session-test-tmux"
	}

	os.Setenv("TMUX", "")
	os.Setenv("TMUX_TMPDIR", isoDir)
	isoTmuxTmpdir = isoDir

	return func() {
		if hadTmux {
			os.Setenv("TMUX", origTmux)
		} else {
			os.Unsetenv("TMUX")
		}
		if hadTmpdir {
			os.Setenv("TMUX_TMPDIR", origTmpdir)
		} else {
			os.Unsetenv("TMUX_TMPDIR")
		}
		os.RemoveAll(isoDir)
	}
}

// TestSuiteTmuxIsolation_HostServerUnreachable is the deterministic
// regression guard for the tmux session-leak class. It asserts that, under
// the TestMain-level neutralisation:
//
//   - the env redirect is actually in effect ($TMUX cleared, $TMUX_TMPDIR
//     pointing at the suite's private directory) — these assertions fail
//     even on hosts with NO live tmux server (the CI shape), so removing
//     the isolation cannot pass unnoticed anywhere; and
//   - the live host tmux server is behaviourally unreachable:
//     tmux.CurrentSession() must fail — the default-socket fallback chain
//     through which a forgotten spyTmuxBin would reach the host server.
//
// The behavioural probe is deterministic with respect to this package's own
// intentional real-server subtest (see the package comment): that subtest
// kills its only session before returning, which makes the private server
// exit, and the tests in this package run sequentially — so by the time
// this guard runs there is nothing listening on the redirected default
// socket.
//
// Verified non-vacuous by no-opping the isolateSuiteFromHostTmux call in
// TestMain and observing this test fail against a live host server.
func TestSuiteTmuxIsolation_HostServerUnreachable(t *testing.T) {
	if isoTmuxTmpdir == "" {
		t.Fatal("isoTmuxTmpdir is empty — TestMain did not apply isolateSuiteFromHostTmux before m.Run() (#2230)")
	}
	if got := os.Getenv("TMUX_TMPDIR"); got != isoTmuxTmpdir {
		t.Errorf("TMUX_TMPDIR = %q, want the suite's private dir %q — suite-wide tmux isolation is not in effect (#2230)", got, isoTmuxTmpdir)
	}
	if got := os.Getenv("TMUX"); got != "" {
		t.Errorf("TMUX = %q, want \"\" — an inherited live-pane socket path could reach the host server (#2230)", got)
	}

	if s, err := tmux.CurrentSession(); err == nil && s != "" {
		t.Errorf("tmux.CurrentSession() = %q, want error — the session suite must be isolated from the live host tmux server (#2230); check TestMain's isolateSuiteFromHostTmux", s)
	}
}
