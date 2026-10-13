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
	"fmt"
	"os"
	"testing"
	"time"
)

// isolateTmuxTmpdir points $TMUX_TMPDIR at a fresh short-path scratch
// directory and clears $TMUX, and returns a function that restores the
// previous environment. The tmux-harness integration tests start real tmux
// servers with `-L <name>`. Without this redirect the socket lands in
// /private/tmp/tmux-<uid>/, which the sandbox-exec profile protects: section
// 3d denies file-write* there and section 17c denies the connect (issue
// #3066). The scratch path /tmp/ptm-<rand>/tmux-<uid>/ matches neither regex,
// because both anchor on tmux-<n> directly under /tmp or /private/tmp. So the
// tmux tests behave the same inside and outside the sandbox, and the suite
// does not touch the real user's tmux directory. /tmp is used first, not
// os.MkdirTemp(""), because a /var/folders path overflows the Darwin
// sun_path limit once tmux appends tmux-<uid>/<name>. Mirrors
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
	code := m.Run()
	restore()
	os.Exit(code)
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
