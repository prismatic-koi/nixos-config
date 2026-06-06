// Package pty wraps creack/pty for the spike's PTY lifecycle.
//
// The whole package is ~50 LoC because that's the whole prism multiplexer's
// PTY surface area too — open, resize, copy bytes both ways, wait, close.
// Nothing here is novel.
package pty

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// Session is a child process bound to a PTY pair.
type Session struct {
	Cmd  *exec.Cmd
	File *os.File // master side; reads from child stdout, writes to child stdin
}

// Start opens a PTY pair sized (cols, rows), forks the child with argv, and
// returns a Session whose File is the master side. The caller must Close()
// the session when done.
func Start(argv []string, cols, rows uint16) (*Session, error) {
	return StartWithEnv(argv, cols, rows, "", nil)
}

// StartWithEnv is the env-and-cwd-aware form of Start. It exists so the
// server's pane.create handler (internal/mux/server) can spawn the
// caller-supplied process under a custom working directory and a
// caller-controlled environment without re-implementing the pty wiring.
//
//   - argv MUST be non-empty; argv[0] is the executable to spawn.
//   - cwd is the child's working directory. Empty means "inherit".
//   - env is the child's environment in os/exec form (["KEY=VALUE", …]).
//     Nil means "inherit the parent's environment" (the default Start
//     behaviour); a non-nil but empty slice means "start with an empty
//     environment".
func StartWithEnv(argv []string, cols, rows uint16, cwd string, env []string) (*Session, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	if env != nil {
		cmd.Env = env
	}
	// Inherit env. We want $TERM=xterm-256color so apps drive truecolor where
	// they can — x/vt is the one rendering, so colour negotiation is up to it.
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	return &Session{Cmd: cmd, File: f}, nil
}

// Resize updates the PTY winsize, which the kernel will signal to the child
// process via SIGWINCH.
func (s *Session) Resize(cols, rows uint16) error {
	return pty.Setsize(s.File, &pty.Winsize{Cols: cols, Rows: rows})
}

// Close kills the child if still running and closes the master file.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	if s.Cmd != nil && s.Cmd.Process != nil {
		_ = s.Cmd.Process.Kill()
		_, _ = s.Cmd.Process.Wait()
	}
	if s.File != nil {
		return s.File.Close()
	}
	return nil
}
