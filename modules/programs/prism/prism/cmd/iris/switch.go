package main

// switch.go — `iris switch` context-switcher subcommand (issue #1671).
//
// `iris switch` opens a bubbletea-based picker listing all daemon-known
// sessions plus a synthetic "[+] spawn new session" row. It is the iris
// analogue of prism's `prism switch` command, intended to be wired to a
// tmux `display-popup` keybinding (`prefix + i` — see modules/programs/
// prism/tmux.nix). prism's own `C-f` → `prism switch` binding is left
// untouched during the iris coexistence window.
//
// # Data source
//
// All session state is read from the iris daemon via the client IPC
// socket (`internal/iris/client_protocol.go`). The picker DOES NOT touch
// `iris.db` directly; it uses the same `sessions_snapshot` frame that
// `iris sessions list` uses (the contract settled in D-6 and reinforced
// by #1668/#1678).
//
// # User flow
//
//  1. Picker opens, shows `[+] spawn new session` + one row per session.
//  2. ↑/↓ to navigate, Enter to select, Esc/Ctrl+C to cancel, type to
//     filter.
//  3. On selection of an existing session: the picker closes and the
//     command execs `iris tui --session <name>` so the same popup chains
//     into the TUI focused on that session.
//  4. On selection of `[+] spawn new`: the picker prompts for worktree
//     (default: current pane's worktree) and role (default: ResolveAgent
//     for that worktree). The spawn request is sent to the daemon over
//     the client socket and the picker waits for `session_spawned`. On
//     success it chains into `iris tui --session <new-name>`; on error
//     the message is shown inline and the picker stays open.
//
// # Daemon-not-running
//
// If the client socket cannot be dialled, `iris switch` exits non-zero
// with the same canonical message used by `iris spawn`:
// "iris daemon not running … systemctl --user start iris".

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
)

// switchDialTimeout bounds how long `iris switch` will wait when dialling
// the daemon client socket for the initial sessions_list request. The
// daemon is local; a real dial completes in milliseconds.
const switchDialTimeout = 2 * time.Second

// switchListTimeout bounds how long the picker will wait for the
// sessions_snapshot reply before giving up.
const switchListTimeout = 5 * time.Second

// switchSpawnAckTimeout bounds how long the picker will wait for a
// session_spawned (or error) frame after sending a spawn request from
// the `[+] spawn new` flow.
const switchSpawnAckTimeout = 30 * time.Second

// switchCmd is the `iris switch` Cobra command.
var switchCmd = &cobra.Command{
	Use:   "switch",
	Short: "Open the iris context-switcher picker (list sessions, pick or spawn)",
	Long: `iris switch opens a context-switcher picker that lists all daemon-known
iris sessions and lets you pick one, or trigger a fresh spawn, without
leaving tmux.

The picker queries the iris daemon's client IPC socket — it does not read
iris.db directly. The daemon must be running ('systemctl --user start iris'
on Linux, 'launchctl load …' on Darwin).

Keyboard:
  ↑/↓        move between rows (also k/j)
  enter      select the highlighted row
  esc/^C     cancel (close popup, no spawn)
  type       fuzzy-filter the visible rows
  backspace  delete one character from the filter

Selecting an existing session execs 'iris tui --session <name>' so the
same tmux popup chains into the TUI focused on that session. Selecting
'[+] spawn new session' prompts for the worktree (default: current pane's
directory) and the role (default: ResolveAgent for that worktree), sends
a session_spawn frame to the daemon, and on the resulting session_spawned
ack chains into 'iris tui --session <new-name>'.

Intended to be opened from tmux via 'prefix + i' (see modules/programs/
prism/tmux.nix). prism's own 'C-f' → 'prism switch' binding is unaffected.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSwitch(cmd.Context())
	},
}

func init() {
	rootCmd.AddCommand(switchCmd)
}

// runSwitch is the top-level entry point: dial the daemon, fetch a
// snapshot, run the picker, and dispatch on the user's selection.
func runSwitch(ctx context.Context) error {
	p := iris.ResolvePaths()
	return runSwitchAt(ctx, p.Sock, defaultSpawnWorktree(), os.Stderr)
}

// runSwitchAt is the testable entry point. sockPath, defaultWorktree, and
// the writers are passed explicitly so tests can stand up a mock daemon
// and capture output.
//
// On success returns nil. The picker may cancel with no action (returns
// nil), select a session (chains into `iris tui --session <name>` and
// returns its error if any), or spawn a new session (chains into the TUI
// on the new name).
func runSwitchAt(ctx context.Context, sockPath, defaultWorktree string, stderr io.Writer) error {
	sessions, err := fetchSessions(ctx, sockPath)
	if err != nil {
		return err
	}

	// Retry loop: if the user picks `[+] spawn new` and the daemon
	// rejects the spawn, re-enter the picker with the error message so
	// they can retry or escape (AC: "shown inline, picker stays open").
	errMsg := ""
	for {
		chosen, action := runPickerWith(sessions, defaultWorktree, errMsg)
		switch action {
		case pickerCancel:
			// Esc or Ctrl+C — nothing to do, popup closes cleanly.
			return nil

		case pickerExisting:
			return execIrisTUI(chosen.sessionName, stderr)

		case pickerSpawn:
			newName, spawnErr := sendSpawn(ctx, sockPath, chosen.worktree, chosen.role)
			if spawnErr != nil {
				errMsg = spawnErr.Error()
				// Refresh the snapshot before re-entering the picker —
				// other clients may have changed state in the meantime.
				if refreshed, refErr := fetchSessions(ctx, sockPath); refErr == nil {
					sessions = refreshed
				}
				continue
			}
			return execIrisTUI(newName, stderr)
		}
	}
}

// defaultSpawnWorktree returns the most sensible default value for the
// `[+] spawn new` worktree prompt. Resolution order:
//
//  1. $PRISM_SPAWN_PATH or $IRIS_SPAWN_PATH (set by tmux popup wrappers).
//  2. The current working directory, if it's inside a git worktree
//     (BareRoot returns non-empty).
//  3. The current working directory verbatim.
//
// Returning "" is acceptable — the prompt simply opens with an empty
// default and the user types one.
func defaultSpawnWorktree() string {
	for _, env := range []string{"IRIS_SPAWN_PATH", "PRISM_SPAWN_PATH"} {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// ---------------------------------------------------------------------------
// Daemon client helpers
// ---------------------------------------------------------------------------

// dialSwitchDaemon dials the daemon socket with a bounded timeout and
// returns the canonical "daemon not running" error on failure.
func dialSwitchDaemon(ctx context.Context, sockPath string) (net.Conn, error) {
	d := net.Dialer{Timeout: switchDialTimeout}
	conn, err := d.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return nil, daemonNotRunningError(sockPath, err)
	}
	return conn, nil
}

// fetchSessions dials the daemon, sends one sessions_list frame, reads the
// sessions_snapshot reply, and returns the session slice. Unknown frames
// are skipped (forward-compat).
func fetchSessions(ctx context.Context, sockPath string) ([]iris.SessionSnapshot, error) {
	conn, err := dialSwitchDaemon(ctx, sockPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	req := iris.ClientSessionsListFrame{Type: iris.ClientFrameSessionsList}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("iris switch: send sessions_list: %w", err)
	}

	// Bound the wait for the snapshot reply.
	if err := conn.SetReadDeadline(time.Now().Add(switchListTimeout)); err != nil {
		return nil, fmt.Errorf("iris switch: set read deadline: %w", err)
	}

	r := bufio.NewReaderSize(conn, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, errors.New("iris switch: daemon closed connection before sending sessions_snapshot")
			}
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				return nil, fmt.Errorf("iris switch: timed out after %s waiting for sessions_snapshot", switchListTimeout)
			}
			return nil, fmt.Errorf("iris switch: read snapshot: %w", err)
		}

		var generic struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &generic); err != nil {
			continue
		}
		switch generic.Type {
		case iris.DaemonFrameSessionsSnapshot:
			var f iris.DaemonSessionsSnapshotFrame
			if err := json.Unmarshal(line, &f); err != nil {
				return nil, fmt.Errorf("iris switch: decode sessions_snapshot: %w", err)
			}
			return f.Sessions, nil
		case iris.DaemonFrameError:
			var f iris.DaemonErrorFrame
			if err := json.Unmarshal(line, &f); err != nil {
				return nil, fmt.Errorf("iris switch: decode error frame: %w", err)
			}
			return nil, fmt.Errorf("iris switch: daemon rejected sessions_list: %s", f.Message)
		default:
			// Unknown frame — skip and keep reading.
			continue
		}
	}
}

// sendSpawn dials the daemon, sends a session_spawn frame for the given
// worktree+role, and reads frames until session_spawned (success) or
// error (failure). Returns the new session name on success.
//
// This is functionally the same as runSpawnAt's inner machinery, but
// lives here so the picker package keeps a single dependency on the
// client protocol. Refactoring runSpawnAt to share this code is a
// follow-up — out of scope for the picker PR.
func sendSpawn(ctx context.Context, sockPath, worktree, role string) (string, error) {
	conn, err := dialSwitchDaemon(ctx, sockPath)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	frame := iris.ClientSessionSpawnFrame{
		Type:     iris.ClientFrameSessionSpawn,
		Worktree: worktree,
		Role:     role,
	}
	data, _ := json.Marshal(frame)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return "", fmt.Errorf("send session_spawn: %w", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(switchSpawnAckTimeout)); err != nil {
		return "", fmt.Errorf("set read deadline: %w", err)
	}

	r := bufio.NewReaderSize(conn, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return "", errors.New("daemon closed connection before session_spawned ack")
			}
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				return "", fmt.Errorf("timed out after %s waiting for session_spawned ack", switchSpawnAckTimeout)
			}
			return "", fmt.Errorf("read ack: %w", err)
		}

		var generic struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &generic); err != nil {
			continue
		}
		switch generic.Type {
		case iris.DaemonFrameSessionSpawned:
			var f iris.DaemonSessionSpawnedFrame
			if err := json.Unmarshal(line, &f); err != nil {
				return "", fmt.Errorf("decode session_spawned: %w", err)
			}
			return f.Name, nil
		case iris.DaemonFrameError:
			var f iris.DaemonErrorFrame
			if err := json.Unmarshal(line, &f); err != nil {
				return "", fmt.Errorf("decode error frame: %w", err)
			}
			return "", fmt.Errorf("daemon rejected spawn: %s", f.Message)
		default:
			// Unknown frame — skip.
			continue
		}
	}
}

// execIrisTUI runs `iris tui --session <name>` in-place. The current
// process's stdin/stdout/stderr are wired through so the TUI takes over
// the tmux popup's terminal. Returns the TUI's exit error, if any.
//
// We deliberately use exec.Command rather than syscall.Exec: the parent
// is short-lived inside a tmux popup, and waiting on the child keeps the
// popup open for the lifetime of the TUI (which is the desired UX). On
// TUI exit, the popup closes naturally.
func execIrisTUI(sessionName string, stderr io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		// Fall back to PATH-resolution of "iris". This is the normal
		// case during development with a non-canonical binary path.
		exe = "iris"
	}
	cmd := exec.Command(exe, "tui", "--session", sessionName)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(stderr, "iris switch: iris tui exited with error: %v\n", err)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Misc helpers
// ---------------------------------------------------------------------------

// shortInstanceID truncates an instance UUID to 12 characters, matching
// the AC's "12-char prefix" column requirement. If the input is shorter
// than 12 chars, it is returned verbatim.
func shortInstanceID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// worktreeBasename returns the basename of a worktree path. Empty input
// returns "". Trailing slashes are stripped first so that
// "/home/ben/code/foo/" returns "foo".
func worktreeBasename(path string) string {
	if path == "" {
		return ""
	}
	// filepath.Clean strips trailing slashes; filepath.Base then returns
	// the last element. For "/" this returns "/" — acceptable.
	return filepath.Base(filepath.Clean(path))
}

// uptimeSince formats a duration since the given RFC3339 timestamp as a
// short human-readable string (e.g. "12s", "5m", "2h", "3d"). On parse
// failure, returns "".
func uptimeSince(rfc3339 string) string {
	if rfc3339 == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
