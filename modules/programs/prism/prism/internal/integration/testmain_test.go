package integration_test

// TestMain runs the fake stdio harness when PRISM_FAKE_STDIO_HARNESS is set,
// allowing the test binary to double as the harness process in integration
// tests. This keeps the fake harness code inside the test binary and out of
// the production binary.
//
// Modes (value of PRISM_FAKE_STDIO_HARNESS):
//
//   - "normal": writes 3 JSONL frames (state_change started, msg_assistant, state_change
//     finished) then exits 0.
//   - "silent": exits 0 immediately without writing any frames. Exercises the
//     "pipe closed before first frame" error path in runStartupStdio.
//
// TestMain also intercepts re-exec stub invocations of this test binary —
// see the env and argv checks below.
//
// When no interception applies, TestMain runs the normal test suite.

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

// isolateTmuxTmpdir points $TMUX_TMPDIR at a fresh short-path scratch
// directory and clears $TMUX, and returns a function that restores the
// previous environment. The tmux-harness integration tests start real tmux
// servers with `-L <name>`. Without this redirect the socket lands in
// /private/tmp/tmux-<uid>/, where a sandbox-exec session can neither write
// nor connect (#3066, #3150). The scratch path is /tmp/ptm-<rand> in a host
// shell, or <TMPDIR>/ptm-<rand> in a sandbox-exec session (the per-session
// TMPDIR). So the tmux tests behave the same inside and outside the sandbox,
// and the suite does not touch the real user's tmux directory. /tmp is used
// first, not os.MkdirTemp(""), because a /var/folders path overflows the
// Darwin sun_path limit once tmux appends tmux-<uid>/<name>. Mirrors
// cmd/tmux_isolation_test.go and internal/tmux/testmain_test.go.
func isolateTmuxTmpdir() func() {
	origTmux, hadTmux := os.LookupEnv("TMUX")
	origTmpdir, hadTmpdir := os.LookupEnv("TMUX_TMPDIR")

	isoDir, err := os.MkdirTemp("/tmp", "ptm-")
	if err != nil {
		isoDir, err = os.MkdirTemp("", "ptm-")
	}
	if err != nil {
		isoDir = "/nonexistent/prism-integration-test"
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

func TestMain(m *testing.M) {
	switch os.Getenv("PRISM_FAKE_STDIO_HARNESS") {
	case "normal":
		runFakeHarnessNormal()
		os.Exit(0)
	case "silent":
		// Exit immediately without writing any frames.
		os.Exit(0)
	}

	if os.Getenv("PRISM_TEST_SUBPROCESS") == "1" {
		// We are a child process acting as a stub subprocess — the
		// convention shared by the internal/review, internal/session, and
		// cmd TestMains. Sleep briefly so a parent can observe a live PID,
		// then exit instead of running the suite.
		time.Sleep(50 * time.Millisecond)
		os.Exit(0)
	}
	if len(os.Args) > 1 && (os.Args[1] == "sidecar" || os.Args[1] == "event") {
		// Re-invoked as a prism subcommand without the stub env var. This
		// package's reachable re-exec paths both exec os.Executable() — in
		// tests, THIS binary:
		//
		//   - session.StartSidecarWithOpts → `<self> sidecar --session …`
		//   - session.setupFullLayout's status seed →
		//     `<self> event tmux-session-start --session … --worktree …`
		//
		// No current test reaches either path (Create(LayoutFull) is
		// deliberately avoided — see TestTmuxHarness_ThreeWindowLayout). This
		// exit is a defence against a future test or forgotten stub that
		// re-invokes the binary. Without it, the re-invocation runs the entire
		// suite as a detached child, which can spawn further detached children
		// in a fork storm. Exit instead of running the suite — see
		// reexec_interception_test.go for the regression guard.
		os.Exit(0)
	}

	// Default: run the test suite, under the suite-wide tmux isolation. The
	// early-exit modes above are the binary acting as a harness or sidecar
	// child; they never start a tmux server, so the redirect applies only
	// here, around m.Run().
	restore := isolateTmuxTmpdir()
	restoreTmpRoot := sessionTmpRootForSuite()
	var restoreState func()
	suiteStateRoot, restoreState = stateguardtest.IsolateSuite()
	code := m.Run()
	restoreState()
	restoreTmpRoot()
	restore()
	os.Exit(code)
}

// sessionTmpRootForSuite moves the root of the per-session TMPDIRs to a new
// dir /var/tmp/pst<4 hex chars> when the test process can write /var/tmp.
// The TMPDIRs keep the /var symlink forms of production.
//
// The root is not /var/tmp itself. The stale-TMPDIR sweep of a production
// prism on the same host (#3170) removes each /var/tmp/prism-<hash> dir that
// its database does not know, and a test TMPDIR is such a dir. The sweep
// does not look below the children of /var/tmp. The name is short, so a
// socket at <TMPDIR>/<longSocketName> stays inside the Darwin sun_path
// limit.
//
// A test process in a sandbox-exec session cannot write /var/tmp, so
// Manager.PrepareSandboxExec fails there. In that case the root moves to a
// new dir under os.TempDir(), the per-session TMPDIR of the session. The
// sandbox-exec tests skip in such a session, and the profile-content tests
// still run.
func sessionTmpRootForSuite() (restore func()) {
	dir, err := mkdirShortUnderVarTmp()
	if err != nil {
		dir, err = os.MkdirTemp("", "pst-")
	}
	if err != nil {
		return func() {}
	}
	restoreRoot := container.SetSessionTmpRootForTest(dir)
	return func() {
		restoreRoot()
		_ = os.RemoveAll(dir)
	}
}

// mkdirShortUnderVarTmp creates /var/tmp/pst<4 hex chars> with mode 0700.
func mkdirShortUnderVarTmp() (string, error) {
	var err error
	for range 20 {
		dir := fmt.Sprintf("/var/tmp/pst%04x", rand.IntN(0x10000))
		if err = os.Mkdir(dir, 0o700); err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", err
}

// suiteStateRoot is the directory that TestMain gives to IsolateSuite.
var suiteStateRoot string

func TestSuiteStateIsolation_XDGInsideSuiteDir(t *testing.T) {
	stateguardtest.RequireIsolated(t, suiteStateRoot)
}

// runFakeHarnessNormal writes 3 valid JSONL event frames to stdout and exits.
// States use agent.AgentState values (active, finished) so the DB state
// machine does not reject the transitions.
func runFakeHarnessNormal() {
	frames := []string{
		`{"type":"state_change","state":"active"}`,
		`{"type":"msg_assistant","text":"hello from fake harness"}`,
		`{"type":"state_change","state":"finished"}`,
	}
	for _, f := range frames {
		fmt.Println(f)
	}
}
