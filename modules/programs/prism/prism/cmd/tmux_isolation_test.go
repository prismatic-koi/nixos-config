package cmd

// Suite-wide tmux isolation for the cmd package (#2214).
//
// Several production code paths in cmd/ fall back to querying the live tmux
// server when no explicit session is provided:
//
//   - review.LookupParentSession() → tmux.CurrentSession() when
//     PRISM_SESSION_NAME is empty (resolveCoordinatorSession,
//     rejectIfCoordinator, runReview, runMerge, runMergesList/runMergesCancel,
//     runProfileUse)
//   - tmux.CurrentSession() directly (close, cleanup, nav, dashboard)
//   - tmux.HasSession / tmux.ListWindows / window-option helpers
//     (cleanup, escalate idempotency)
//
// Crucially, `tmux display-message -p '#{session_name}'` reaches the live
// host server even when $TMUX is unset or empty: the tmux client falls back
// to the default socket ($TMUX_TMPDIR/tmux-<uid>/default, /tmp/tmux-<uid>/
// default when TMUX_TMPDIR is unset). When invoked outside any pane, the
// server resolves the "current" session via its most-recently-used
// heuristics — i.e. whatever session the developer happens to be attached
// to. Five cmd/ tests failed nondeterministically on developer hosts because
// of exactly this (#2214): three independent sessions observed three
// different leaked names ("actions-runner@main", "staging-db-refresh@main",
// "aws-identity@main"), each the host's most-recently-used live session.
//
// t.Setenv("TMUX", "") is therefore NOT sufficient isolation on its own.
// The suite-wide neutralisation below, applied by TestMain before m.Run():
//
//   1. clears $TMUX, so a socket path inherited from an enclosing live pane
//      is never used; and
//   2. points $TMUX_TMPDIR at a fresh, empty directory, so the
//      default-socket fallback cannot reach the host server either.
//
// Every tmux client invocation made by code under test then fails fast with
// "error connecting to ..." — indistinguishable from a host with no tmux
// server running, which is what CI sees. This neutralises the live-server
// leak for every current and future test in the package without per-test
// boilerplate (the same philosophy as sidecartest.NewIsolated, #1608).
//
// The redirected directory must have a SHORT path: tmux sockets are Unix
// domain sockets and sun_path is ~104 bytes on Darwin. os.MkdirTemp("", ...)
// on macOS yields /var/folders/... paths that overflow it once tmux appends
// tmux-<uid>/<socket-name>, which would break the tests that intentionally
// run real isolated tmux servers via -L (cleanup, dashboard, restore,
// switch, event). Hence /tmp first, with the generic temp dir as fallback.
//
// Tests that intentionally exercise a real tmux server keep working: they
// use unique `-L prism-cmd-test-…` socket names, which simply resolve under
// the redirected $TMUX_TMPDIR. Tests that set their own $TMUX (e.g. the nav
// tests' "/tmp/tmux-test,1234,0" guard-satisfier) override the cleared value
// via t.Setenv and point at sockets that do not exist — never the live
// server.

import (
	"os"
	"testing"

	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// isolateSuiteFromHostTmux applies the suite-wide tmux neutralisation
// described above and returns a restore function that puts the original
// environment back. TestMain calls restore after m.Run() so the test
// process leaves the environment as it found it.
//
// Because this isolation makes the live host server unreachable from code
// under test by construction, the #1180 leak guard's live-tmux-server
// before/after session diff was dropped (#2227): it could no longer catch a
// suite leak — only misattribute concurrent host activity (parallel
// workers' spawns and review rounds) to the suite. The deterministic
// regression guard for this isolation is
// TestSuiteTmuxIsolation_HostServerUnreachable below.
func isolateSuiteFromHostTmux() (restore func()) {
	origTmux, hadTmux := os.LookupEnv("TMUX")
	origTmpdir, hadTmpdir := os.LookupEnv("TMUX_TMPDIR")

	isoDir, err := os.MkdirTemp("/tmp", "ptm-")
	if err != nil {
		// /tmp unwritable (e.g. restrictive build sandboxes). Fall back to
		// the default temp dir: the resulting socket paths may exceed
		// sun_path, which only makes tmux fail faster — isolation is
		// preserved either way (such hosts have no live tmux server for the
		// -L tests anyway).
		isoDir, err = os.MkdirTemp("", "ptm-")
	}
	if err != nil {
		// Last resort: a path guaranteed not to contain a live socket.
		// Isolation trumps the real-tmux -L tests' ability to run.
		isoDir = "/nonexistent/prism-cmd-test-tmux"
	}

	os.Setenv("TMUX", "")
	os.Setenv("TMUX_TMPDIR", isoDir)

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

// TestSuiteTmuxIsolation_HostServerUnreachable is the regression guard for
// the #2214 leak class. It asserts that, under the TestMain-level
// neutralisation, the live host tmux server is unreachable from code under
// test:
//
//   - tmux.CurrentSession() must fail (no default-socket fallback to the
//     host server), and
//   - review.LookupParentSession() must return "" when PRISM_SESSION_NAME is
//     empty (the exact fallback chain that leaked into
//     resolveCoordinatorSession and rejectIfCoordinator).
//
// On a host with no tmux server (CI) this passes trivially. On a developer
// host with a live server it fails if — and only if — the suite isolation is
// removed, which is precisely the regression it exists to catch. Verified
// non-vacuous by disabling the isolateSuiteFromHostTmux call in TestMain and
// observing this test fail against a live server.
func TestSuiteTmuxIsolation_HostServerUnreachable(t *testing.T) {
	if s, err := tmux.CurrentSession(); err == nil && s != "" {
		t.Errorf("tmux.CurrentSession() = %q, want error — the cmd suite must be isolated from the live host tmux server (#2214); check TestMain's isolateSuiteFromHostTmux", s)
	}

	t.Setenv("PRISM_SESSION_NAME", "")
	if got := review.LookupParentSession(); got != "" {
		t.Errorf("review.LookupParentSession() = %q, want \"\" — host tmux state is leaking into cmd tests (#2214)", got)
	}
}
