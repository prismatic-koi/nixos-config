// Package stateguard keeps a prism binary that a test builds inside the
// state root of that test.
//
// A test that runs a real prism binary sets Root at link time:
//
//	go build -ldflags "-X github.com/prismatic-koi/prism/internal/stateguard.Root=<dir>"
//
// Check then refuses to let the binary run when its home, state, config, or
// tmux socket path is outside Root. The guard is in the binary, so it holds
// when a shell startup file, for example a home-manager ~/.zshenv, sets
// XDG_STATE_HOME or XDG_CONFIG_HOME back to the real paths of the user. Each
// process that the binary starts (the sidecar, `prism event`, `prism
// agent-run`) is the same binary, so the guard applies to each of them.
//
// A production build does not set Root, and Check does nothing.
package stateguard

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Root is the state root of the test that built this binary. It is empty in
// a production build.
var Root string

// Check returns an error when Root is set and a path that prism resolves
// from the environment is outside Root. getenv is os.Getenv in production.
func Check(getenv func(string) string) error {
	if Root == "" {
		return nil
	}
	home := getenv("HOME")
	state := getenv("XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(home, ".local", "state")
	}
	config := getenv("XDG_CONFIG_HOME")
	if config == "" {
		config = filepath.Join(home, ".config")
	}
	checks := []struct{ name, path string }{
		{"HOME", home},
		{"XDG_STATE_HOME", state},
		{"XDG_CONFIG_HOME", config},
		{"tmux socket", tmuxSocketDir(getenv)},
	}
	for _, c := range checks {
		if !within(c.path, Root) {
			return fmt.Errorf("prism: this binary is a test build for the state root %s. "+
				"The %s path %q is outside that root, so the binary does not run", Root, c.name, c.path)
		}
	}
	return nil
}

// tmuxSocketDir returns the path that decides which tmux server a tmux
// client contacts. With TMUX set, the client uses the socket named in it.
// Without TMUX, the client uses the default socket under $TMUX_TMPDIR, or
// under /tmp when TMUX_TMPDIR is empty: that is the server of the user.
func tmuxSocketDir(getenv func(string) string) string {
	if t := getenv("TMUX"); t != "" {
		sock, _, _ := strings.Cut(t, ",")
		return sock
	}
	if d := getenv("TMUX_TMPDIR"); d != "" {
		return d
	}
	return "/tmp"
}

// within reports whether path is root or a path below root. It compares the
// cleaned strings, and also the root with its symlinks resolved, because
// tmux puts the resolved form of TMUX_TMPDIR in TMUX (/private/tmp on
// macOS for /tmp).
func within(path, root string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	roots := []string{filepath.Clean(root)}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		roots = append(roots, resolved)
	}
	for _, r := range roots {
		rel, err := filepath.Rel(r, filepath.Clean(path))
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
