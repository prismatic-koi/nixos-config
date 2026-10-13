package tmux_test

// testmain_test.go — suite-wide tmux isolation for internal/tmux.
//
// The tests in this package start real tmux servers with `-L <name>`. tmux
// resolves such a socket under $TMUX_TMPDIR/tmux-<uid>/, or /tmp/tmux-<uid>/
// when $TMUX_TMPDIR is unset. The default lands in /private/tmp/tmux-<uid>/,
// which the sandbox-exec profile protects: section 3d denies file-write*
// there and section 17c denies the connect (issue #3066). A worker that runs
// `go test ./...` inside a sandbox-exec session then cannot create or reach a
// test server, and tmuxtest.RequireServer does not skip, because the failure
// is a socket-create denial, not the fork-denial signature it looks for.
//
// This TestMain points $TMUX_TMPDIR at a fresh short-path scratch directory,
// so every test socket resolves under /tmp/ptm-<rand>/tmux-<uid>/. That path
// does not match the section-3d or section-17c regexes, which anchor on
// tmux-<n> directly under /tmp or /private/tmp. The suite then behaves the
// same inside and outside the sandbox. It also stops the suite from touching
// the real user's tmux directory, the same isolation cmd and internal/session
// already apply (cmd/tmux_isolation_test.go).
//
// The scratch directory uses /tmp first, not os.MkdirTemp(""), because a tmux
// socket is a Unix domain socket and sun_path is about 104 bytes on Darwin.
// A /var/folders path overflows it once tmux appends tmux-<uid>/<name>.

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	restore := isolateTmuxTmpdir()
	code := m.Run()
	restore()
	os.Exit(code)
}

// isolateTmuxTmpdir clears $TMUX and points $TMUX_TMPDIR at a fresh scratch
// directory, and returns a function that restores the previous environment.
func isolateTmuxTmpdir() func() {
	origTmux, hadTmux := os.LookupEnv("TMUX")
	origTmpdir, hadTmpdir := os.LookupEnv("TMUX_TMPDIR")

	isoDir, err := os.MkdirTemp("/tmp", "ptm-")
	if err != nil {
		// /tmp is not writable (a restrictive build sandbox). The default
		// temp dir may overflow sun_path, which only makes tmux fail faster.
		// The isolation holds either way.
		isoDir, err = os.MkdirTemp("", "ptm-")
	}
	if err != nil {
		// Last resort: a path with no live socket.
		isoDir = "/nonexistent/prism-tmux-test"
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
