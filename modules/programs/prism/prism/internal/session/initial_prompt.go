package session

// Per-session initial-prompt file.
//
// Host-mode SpawnSession writes the prompt to a small file in the per-session
// run directory. The constructed launch command reads it with $(cat …) inside
// the pane shell. This keeps the launch command a few hundred bytes whatever
// the prompt size, and delivers the prompt to the agent via argv, which
// handles 100s of KB on both Linux and Darwin.
//
// Do not interpolate the prompt directly onto the tmux command line. tmux has
// practical size limits on its command argument. A prompt above ~12 KB is
// silently truncated, so the agent cannot start and the operator sees only a
// session that idles forever.
//
// The file lives next to agent-startup.log, agent-run.log, and hostapi.sock
// under a single SessionDirName-derived directory, so the per-session run
// directory has a single authoritative location:
//
//	$XDG_STATE_HOME/prism/run/<sessionDirName>/initial-prompt.txt
//
// SessionDirName(sessionName) is the 12-hex SHA-256 prefix of the session
// name (see sidecar.go). This scheme keeps socket paths under the sun_path
// limit. The raw session name would scatter the forensic trail across two
// sibling directories.
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
// trail.
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
// callers that surface this to the operator must treat the spawn as failed. A
// failure in the prompt-delivery path must not be swallowed.
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
