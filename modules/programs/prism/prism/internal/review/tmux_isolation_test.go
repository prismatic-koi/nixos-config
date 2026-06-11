package review_test

// Suite-wide tmux isolation for the review package (#2230, pattern from
// #2214/#2224).
//
// Several production code paths in internal/review reach the live tmux
// server when invoked without a stub:
//
//   - review.LookupParentSession() → tmux.CurrentSession() when
//     PRISM_SESSION_NAME is empty (run.go)
//   - review.KillSessionPrefix() → tmux.Run("list-sessions", …) +
//     tmux.KillSession (lifecycle.go)
//   - the readiness-gate and Run/RunAsync cleanup paths → tmux.KillSession
//     (readiness.go, run.go)
//
// Crucially, `tmux list-sessions` / `tmux display-message` reach the live
// host server even when $TMUX is unset or empty: the tmux client falls back
// to the default socket ($TMUX_TMPDIR/tmux-<uid>/default, /tmp/tmux-<uid>/
// default when TMUX_TMPDIR is unset). Historically this package relied on
// per-test opt-in stubs (spawnSpyTmuxBin rewriting tmux.TmuxBin) as the ONLY
// line of defence — and #1732 is the proof it is not enough: a test that
// forgot the stub leaked 5 real review-agent sessions onto the live server,
// caught only incidentally by the cmd package's leak guard under parallel
// `go test ./...` scheduling. That incidental backstop was dropped in #2227
// (it false-positived on concurrent host activity), leaving nothing to
// detect this leak class. The suite-wide neutralisation below, applied by
// TestMain before m.Run():
//
//   1. clears $TMUX, so a socket path inherited from an enclosing live pane
//      is never used; and
//   2. points $TMUX_TMPDIR at a fresh, empty directory, so the
//      default-socket fallback cannot reach the host server either.
//
// Every tmux client invocation made by code under test then fails fast with
// "error connecting to ..." — indistinguishable from a host with no tmux
// server running, which is what CI sees. The per-test spawnSpyTmuxBin stubs
// become defence-in-depth instead of the only line of defence.
//
// The redirected directory must have a SHORT path: tmux sockets are Unix
// domain sockets and sun_path is ~104 bytes on Darwin. os.MkdirTemp("", ...)
// on macOS yields /var/folders/... paths that overflow it once tmux appends
// tmux-<uid>/<socket-name>. No review test runs an intentional real tmux
// server (audited for #2230: all tmux interaction goes through the
// spawnSpyTmuxBin stub), so the short path only matters for keeping the
// failure mode "connection refused" rather than "path too long" — but we
// follow the cmd package's /tmp-first convention anyway so a future
// intentional-real-server test does not trip over it.

import (
	"os"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// isoTmuxTmpdir records the redirected $TMUX_TMPDIR applied by TestMain.
// TestSuiteTmuxIsolation_HostServerUnreachable asserts against it so the
// guard fails deterministically — even on hosts with no live tmux server
// (the CI shape) — if the TestMain-level isolation is removed or reordered
// after m.Run().
var isoTmuxTmpdir string

// TestMain serves two purposes:
//
//  1. Stub sidecar: code under test (review.Run → session.SpawnSession →
//     session.StartSidecarWithOpts) re-invokes os.Executable() — THIS test
//     binary — as a detached `sidecar --session …` process. Without
//     interception the child would recursively run the entire review suite
//     in the background, which itself spawns more children (observed: 93
//     detached review.test processes after a single suite run). The
//     spawn_prompt_file tests set PRISM_TEST_SUBPROCESS=1 expecting a
//     TestMain to handle it (the session package's TestMain does the same
//     for ITS binary); the argv check additionally covers call paths that
//     reach StartSidecarWithOpts without setting the env var (e.g.
//     TestRun_RequireSlot_AllSlotsPresent_DoesNotAbort's 5-agent fan-out).
//     The 50ms sleep mirrors the session package's stub: long enough for a
//     parent to observe a live PID, short enough to be free.
//
//  2. Tmux isolation (#2230): clears $TMUX and redirects $TMUX_TMPDIR to an
//     empty directory so no code under test can reach the live host tmux
//     server via the default-socket fallback. See the package comment above
//     for the full rationale. The original environment is restored after
//     m.Run().
func TestMain(m *testing.M) {
	if os.Getenv("PRISM_TEST_SUBPROCESS") == "1" {
		// We are the child process acting as the sidecar stub.
		time.Sleep(50 * time.Millisecond)
		os.Exit(0)
	}
	if len(os.Args) > 1 && (os.Args[1] == "sidecar" || os.Args[1] == "monitor-review") {
		// Re-invoked as a prism subcommand without the stub env var. Exit
		// instead of recursively running the suite. "sidecar" comes from
		// session.StartSidecarWithOpts; "monitor-review" from
		// review.StartMonitorProcess when a test passes prismBinary==""
		// (RunAsync's default).
		os.Exit(0)
	}

	restore := isolateSuiteFromHostTmux()
	code := m.Run()
	restore()
	os.Exit(code)
}

// isolateSuiteFromHostTmux applies the suite-wide tmux neutralisation
// described in the package comment and returns a restore function that puts
// the original environment back. TestMain calls restore after m.Run() so the
// test process leaves the environment as it found it.
func isolateSuiteFromHostTmux() (restore func()) {
	origTmux, hadTmux := os.LookupEnv("TMUX")
	origTmpdir, hadTmpdir := os.LookupEnv("TMUX_TMPDIR")

	isoDir, err := os.MkdirTemp("/tmp", "prv-")
	if err != nil {
		// /tmp unwritable (e.g. restrictive build sandboxes). Fall back to
		// the default temp dir: the resulting socket paths may exceed
		// sun_path, which only makes tmux fail faster — isolation is
		// preserved either way.
		isoDir, err = os.MkdirTemp("", "prv-")
	}
	if err != nil {
		// Last resort: a path guaranteed not to contain a live socket.
		isoDir = "/nonexistent/prism-review-test-tmux"
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
// regression guard for the #1732 leak class (#2230). It asserts that, under
// the TestMain-level neutralisation:
//
//   - the env redirect is actually in effect ($TMUX cleared, $TMUX_TMPDIR
//     pointing at the suite's private directory) — these assertions fail
//     even on hosts with NO live tmux server (the CI shape), so removing
//     the isolation cannot pass unnoticed anywhere; and
//   - the live host tmux server is behaviourally unreachable:
//     tmux.CurrentSession() must fail, and review.LookupParentSession()
//     must return "" when PRISM_SESSION_NAME is empty — the exact fallback
//     chain through which a forgotten stub would reach the host server.
//
// Verified non-vacuous by no-opping isolateSuiteFromHostTmux in TestMain and
// observing this test fail against a live host server (see PR for #2230).
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
		t.Errorf("tmux.CurrentSession() = %q, want error — the review suite must be isolated from the live host tmux server (#2230); check TestMain's isolateSuiteFromHostTmux", s)
	}

	t.Setenv("PRISM_SESSION_NAME", "")
	if got := review.LookupParentSession(); got != "" {
		t.Errorf("review.LookupParentSession() = %q, want \"\" — host tmux state is leaking into review tests (#2230)", got)
	}
}
