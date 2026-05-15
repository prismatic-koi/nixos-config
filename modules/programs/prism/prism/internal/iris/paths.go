// Package iris provides the core startup logic for the iris binary.
//
// This package is the D-2 skeleton: config loading and DB open. No daemon
// behaviour, no socket, no RPC. See docs/daemon-mode-design.md §10.1 for the
// canonical path table.
package iris

import (
	"os"
	"path/filepath"
)

// Paths holds all canonical filesystem paths used by iris (§10.1 of the
// daemon-mode design doc). All paths are derived from $XDG_STATE_HOME /
// $XDG_CONFIG_HOME / $HOME so that the binary works correctly under both
// a normal home directory and a test-controlled temp directory.
type Paths struct {
	// StateDir is ~/.local/state/iris/
	StateDir string
	// DB is ~/.local/state/iris/iris.db
	DB string
	// Sock is ~/.local/state/iris/iris.sock (path reserved; not bound in D-2)
	Sock string
	// RunDir is ~/.local/state/iris/run/ (reserved; not used in D-2)
	RunDir string
	// LogDir is ~/.local/state/iris/logs/. The daemon writes one log file
	// per session at <LogDir>/<session-name>.log. See SessionLogPath.
	LogDir string
	// ConfigFile is ~/.config/iris/config.json
	ConfigFile string
	// ArchiveRoot is ~/code/archives/iris/ (reserved; not used in D-2)
	ArchiveRoot string
}

// SessionLogPath returns the per-session log file path for the given session
// name. The path is <LogDir>/<sanitised-name>.log. Session names already use
// the form "<repo>@<branch>" — both halves are filesystem-safe except for
// path separators which we replace with '_'. The sanitisation is intentionally
// permissive: callers that pass a malicious name only see their own log file.
func (p Paths) SessionLogPath(sessionName string) string {
	return filepath.Join(p.LogDir, sanitiseSessionFileName(sessionName)+".log")
}

// sanitiseSessionFileName replaces path separators in a session name so the
// result is safe to use as a filename. Other characters are left as-is —
// session names are user-controlled but iris itself is single-user, so the
// only risk is a user accidentally embedding a '/'.
func sanitiseSessionFileName(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '/' || c == '\\' || c == 0 {
			out = append(out, '_')
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

// ResolvePaths resolves the iris path layout from environment variables,
// following XDG base-directory conventions. The returned Paths struct
// contains only derived path strings — no files are created or accessed.
//
// Resolution order:
//   - $XDG_STATE_HOME / $HOME/.local/state  → state directory
//   - $XDG_CONFIG_HOME / $HOME/.config      → config directory
//   - $HOME/code/archives/iris              → archive root
func ResolvePaths() Paths {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}

	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, _ := os.UserHomeDir()
		configHome = filepath.Join(home, ".config")
	}

	home, _ := os.UserHomeDir()

	stateDir := filepath.Join(stateHome, "iris")
	return Paths{
		StateDir:    stateDir,
		DB:          filepath.Join(stateDir, "iris.db"),
		Sock:        filepath.Join(stateDir, "iris.sock"),
		RunDir:      filepath.Join(stateDir, "run"),
		LogDir:      filepath.Join(stateDir, "logs"),
		ConfigFile:  filepath.Join(configHome, "iris", "config.json"),
		ArchiveRoot: filepath.Join(home, "code", "archives", "iris"),
	}
}
