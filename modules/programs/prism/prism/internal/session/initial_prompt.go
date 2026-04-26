package session

// Per-session initial-prompt file (#1064 Option A).
//
// In host-mode sessions, the initial prompt was historically interpolated
// directly onto the opencode launch command (`opencode --prompt '<text>'`),
// which was then handed to `tmux new-window ... sh -c <cmd>`. tmux's command
// argument handling has practical size limits; prompts above ~12 KB were
// observed to be silently truncated, leaving opencode unable to start and
// the operator with no visible signal beyond a session that idles forever
// (#1064 root cause).
//
// To remove the size coupling between the prompt and the tmux command line,
// host-mode SpawnSession writes the prompt to a small file in the per-session
// run directory and the constructed launch command reads it with $(cat …)
// inside the pane shell. The launch command itself stays a few hundred bytes
// regardless of prompt size, while the prompt content reaches opencode via
// argv (which comfortably handles 100s of KB on both Linux and Darwin).
//
// The file lives next to agent-startup.log (#1051 / #1062), agent-run.log
// (#1061), and hostapi.sock under a single SessionDirName-derived directory
// so the per-session run directory has a single authoritative location:
//
//	$XDG_STATE_HOME/prism/run/<sessionDirName>/initial-prompt.txt
//
// SessionDirName(sessionName) is the 12-hex SHA-256 prefix of the session
// name (see sidecar.go) — the same scheme #1061 introduced to keep socket
// paths under the sun_path limit. Using the raw session name here would
// scatter the forensic trail across two sibling directories — see #1066.
//
// Cleanup is best-effort and lifecycle-tied:
//   - Pre-spawn: any stale file from a previous incarnation is removed
//     before the new file is written, so a recycled session name never
//     re-uses a previous prompt.
//   - On readiness-gate timeout: the file is removed alongside the rest of
//     the half-alive session cleanup.
//   - On normal shutdown / archive: the surrounding run/<sessionDirName>/
//     directory is reaped by the existing per-session-dir cleanup paths;
//     this file does not need a separate hook.

import (
	"fmt"
	"os"
	"path/filepath"
)

// InitialPromptPath returns the initial-prompt.txt file path for the named
// session. It lives in the per-session run directory keyed on
// SessionDirName(sessionName), so it is co-located with agent-startup.log
// (see startup_log.go), agent-run.log, and hostapi.sock — a single
// `ls $XDG_STATE_HOME/prism/run/<sessionDirName>/` shows the full forensic
// trail. See #1066 for the alignment rationale and #1061 for the original
// switch to SessionDirName.
func InitialPromptPath(sessionName string) (string, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", SessionDirName(sessionName), "initial-prompt.txt"), nil
}

// WriteInitialPrompt writes the prompt to the per-session initial-prompt.txt
// file, creating the per-session run directory (mode 0o700) if it does not
// already exist. Any existing file at the path is overwritten — a fresh spawn
// must not inherit a stale prompt from a previous incarnation of the same
// session name.
//
// Returns the path that was written so the caller can plumb it through into
// the constructed launch command. On error the path may be partially written;
// callers that surface this to the operator should treat the spawn as failed
// (per #1064 AC-5 — the prompt-delivery path failing must not be swallowed).
func WriteInitialPrompt(sessionName, prompt string) (string, error) {
	path, err := InitialPromptPath(sessionName)
	if err != nil {
		return "", fmt.Errorf("resolve initial-prompt path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create per-session run dir %s: %w", filepath.Dir(path), err)
	}
	// 0o600 keeps the prompt contents readable only by the operator, since
	// prompts can contain credentials, branch context, or secret URLs.
	if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
		return "", fmt.Errorf("write initial-prompt file %s: %w", path, err)
	}
	return path, nil
}

// removeInitialPrompt deletes the per-session initial-prompt.txt file.
// Best-effort: a missing file or unreachable path is not an error. Used by
// the readiness-gate cleanup path in SpawnSession so a half-alive session's
// prompt does not linger across spawn retries.
func removeInitialPrompt(sessionName string) {
	path, err := InitialPromptPath(sessionName)
	if err != nil {
		return
	}
	_ = os.Remove(path)
}
