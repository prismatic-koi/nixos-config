package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// socketDirHashLen matches the per-session directory length used by
// internal/session.SessionDirName (12 hex chars = 48 bits of SHA-256).
// We mirror it so the mux socket directory looks structurally identical
// to a sidecar's per-session run/ directory and any path-walking tool
// that already understands one understands the other.
const socketDirHashLen = 12

// socketDirInput is the fixed SHA-256 input used to derive the mux
// daemon's run/ subdirectory name. It is deliberately distinct from any
// real session name (sessions are conventionally "<repo>@<branch>") so
// that the daemon's mux.sock never collides with a session's
// hostapi.sock — and even if it did, the file names differ.
const socketDirInput = "prism-mux"

// socketFileName is the well-known basename of the mux daemon's Unix
// socket. The two-part path (dir name = hash, file name = "mux.sock")
// mirrors the sidecar's (dir name = hash, file name = "hostapi.sock")
// layout from internal/session.SidecarHostAPIPath.
const socketFileName = "mux.sock"

// DefaultSocketPath returns the canonical Unix socket path for the
// prism mux daemon: $XDG_STATE_HOME/prism/run/<12-hex>/mux.sock.
//
// The $XDG_STATE_HOME fallback mirrors what internal/session does:
// when the variable is unset we use $HOME/.local/state. An error is
// returned only when $HOME itself cannot be resolved.
func DefaultSocketPath() (string, error) {
	base, err := stateBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", socketDirName(), socketFileName), nil
}

// stateBaseDir returns the $XDG_STATE_HOME/prism base directory,
// resolving $HOME if the variable is unset.
func stateBaseDir() (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism"), nil
}

// socketDirName returns the 12-hex-char SHA-256 prefix of socketDirInput.
// Exposed as a function so tests can assert on its determinism without
// duplicating the hashing.
func socketDirName() string {
	sum := sha256.Sum256([]byte(socketDirInput))
	return hex.EncodeToString(sum[:])[:socketDirHashLen]
}
