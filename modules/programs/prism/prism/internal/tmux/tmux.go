// Package tmux provides helpers for querying and controlling a running tmux server.
package tmux

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// TmuxBin is the path to the tmux binary. Injected at build time via ldflags
// so that prism can find tmux regardless of the invoking shell's PATH
// (e.g. when launched from a tmux display-popup on macOS with a stripped PATH).
// Falls back to "tmux" for local dev builds.
var TmuxBin = "tmux"

// Session represents a tmux session with its agent state.
type Session struct {
	Name        string
	AgentState  string // active | waiting | finished | compacting | error | ""
	AgentPath   string // pane_current_path of the agent window
	AgentTitle  string // @agent_title window option, set by the tmux-status plugin
	ClientCount int    // number of tmux clients currently attached
}

// run executes a tmux command and returns trimmed stdout. On failure, the
// returned error wraps the original *exec.ExitError (or other exec error) and
// includes the full tmux argv plus the trimmed contents of tmux's stderr.
// tmux writes its actual diagnostic ("can't find session", "duplicate session",
// "index in use", etc.) on stderr, so capturing it is essential — without it
// every failure surfaces as a context-free "exit status 1".
//
// Edge case: if tmux exits non-zero with no stderr output, the error still
// carries the argv and the wrapped exec error; the trailing ": " is followed
// by an empty string rather than a nil deref.
func run(args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command(TmuxBin, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// clientsPerSession returns a map of session name → number of attached clients.
func clientsPerSession() map[string]int {
	counts := map[string]int{}
	out, err := run("list-clients", "-F", "#{session_name}")
	if err != nil {
		return counts
	}
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name != "" {
			counts[name]++
		}
	}
	return counts
}

// Sessions returns all current tmux sessions.
// It uses two bulk tmux calls (list-sessions + list-windows -a + list-clients)
// regardless of the number of sessions, avoiding the previous O(N) per-session
// list-windows subprocesses that caused slowdowns with many sessions.
func Sessions() ([]Session, error) {
	// Single call: all sessions.
	sessOut, err := run("list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil, err
	}

	// Single call: all windows across all sessions.
	winOut, _ := run(
		"list-windows", "-a",
		"-F", "#{session_name}|#{window_name}|#{@agent_state}|#{pane_current_path}|#{@agent_title}",
	)

	// Build a lookup: session → (state, path, title) from the agent window.
	type agentInfo struct{ state, path, title string }
	agentBySession := make(map[string]agentInfo)
	for _, line := range strings.Split(winOut, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 5)
		if len(parts) != 5 {
			continue
		}
		if parts[1] == "agent" {
			agentBySession[parts[0]] = agentInfo{
				state: parts[2],
				path:  parts[3],
				title: parts[4],
			}
		}
	}

	clients := clientsPerSession()

	var sessions []Session
	for _, name := range strings.Split(sessOut, "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		info := agentBySession[name]
		sessions = append(sessions, Session{
			Name:        name,
			AgentState:  info.state,
			AgentPath:   info.path,
			AgentTitle:  info.title,
			ClientCount: clients[name],
		})
	}
	return sessions, nil
}

// SwitchClient switches the named client to the named session.
func SwitchClient(client, session string) error {
	_, err := run("switch-client", "-c", client, "-t", session)
	return err
}

// SwitchClientCurrent switches the current tmux client to the named session.
func SwitchClientCurrent(session string) (string, error) {
	return run("switch-client", "-t", session)
}

// SwitchClientLast switches the named client back to its previously-viewed
// session (equivalent to `tmux switch-client -c <client> -l`). This is used
// by the persistent dashboard on q/esc to return the viewer to where they came
// from without needing any stored caller state.
func SwitchClientLast(client string) error {
	_, err := run("switch-client", "-c", client, "-l")
	return err
}

// CurrentSession returns the session name for the current tmux client.
func CurrentSession() (string, error) {
	return run("display-message", "-p", "#{session_name}")
}

// ClientSession returns the session name that the named client is currently
// viewing. Unlike CurrentSession, this works correctly from inside a
// persistent session (e.g. prism-dashboard) where the process session differs
// from the client's actual session.
func ClientSession(client string) (string, error) {
	if client == "" {
		return CurrentSession()
	}
	return run("display-message", "-t", client, "-p", "#{session_name}")
}

// CallerSession returns the value of the @prism_caller global tmux option,
// which is stamped by the C-w / prefix+D bindings before attaching to
// prism-dashboard. This reliably identifies which session the viewer came from.
func CallerSession() string {
	val, _ := run("show-option", "-gv", "@prism_caller")
	return val
}

// CallerClient returns the @prism_caller_client global — the client name
// that opened the dashboard. Used to switch-client the right terminal on Enter.
func CallerClient() string {
	val, _ := run("show-option", "-gv", "@prism_caller_client")
	return val
}

// CurrentClient returns the client name for the current tmux client.
func CurrentClient() (string, error) {
	return run("display-message", "-p", "#{client_name}")
}

// CurrentPanePath returns the pane_current_path of the current pane.
func CurrentPanePath() (string, error) {
	return run("display-message", "-p", "#{pane_current_path}")
}

// HasSession returns true if a session with the given name exists.
func HasSession(name string) bool {
	err := exec.Command(TmuxBin, "has-session", "-t", name).Run()
	return err == nil
}

// AgentStateOf returns the @agent_state of the agent window in the named
// session. Returns an empty string if the session does not exist or has no
// agent window.
func AgentStateOf(name string) string {
	state, err := GetWindowOption(name+":agent", "@agent_state")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(state)
}

// NewSession creates a new detached session.
func NewSession(name, dir string) error {
	_, err := run("new-session", "-ds", name, "-c", dir)
	return err
}

// DetachClient detaches the named client from its current session.
// If client is empty, detaches the current client.
func DetachClient(client string) error {
	if client == "" {
		_, err := run("detach-client")
		return err
	}
	_, err := run("detach-client", "-c", client)
	return err
}

// RenameWindow renames a window in a session.
func RenameWindow(target, name string) error {
	_, err := run("rename-window", "-t", target, name)
	return err
}

// NewWindow creates a new window in a session. name may be empty.
// When cmd is non-empty, the window starts executing that command immediately
// via "sh -c <cmd>", bypassing tmux's command parser entirely. This avoids
// semicolons in the command being treated as tmux command separators (which
// would happen with SendKeys). Note: when a command is given, the pane exits
// when the command exits — the user's default shell is NOT started.
//
// envVars is an optional map of environment variables to set in the new pane
// via tmux's -e KEY=VALUE flag. Each entry produces one -e flag. Pass nil or
// an empty map for no additional env vars. The map is iterated in sorted key
// order so the resulting argument list is deterministic.
func NewWindow(session string, idx int, name, dir string, cmd string, envVars map[string]string) error {
	args := []string{"new-window", "-t", fmt.Sprintf("%s:%d", session, idx), "-n", name}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	// Emit -e KEY=VALUE flags in sorted key order for determinism.
	if len(envVars) > 0 {
		keys := make([]string, 0, len(envVars))
		for k := range envVars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "-e", k+"="+envVars[k])
		}
	}
	if cmd != "" {
		args = append(args, "sh", "-c", cmd)
	}
	_, err := run(args...)
	return err
}

// SendKeys sends key strokes to a target pane/window.
func SendKeys(target, keys string) error {
	_, err := run("send-keys", "-t", target, keys, "C-m")
	return err
}

// shellEscape wraps s in single quotes, escaping any single quotes within.
// Used for building shell one-liners passed to sh -c.
func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// SelectWindow selects a window by index in a session.
func SelectWindow(session string, idx int) error {
	_, err := run("select-window", "-t", fmt.Sprintf("%s:%d", session, idx))
	return err
}

// NewSessionDetached creates a new detached session with a working directory.
// Returns immediately; caller can then configure windows.
func NewSessionDetached(name, dir string) error {
	args := []string{"new-session", "-ds", name}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	_, err := run(args...)
	return err
}

// AttachSession attaches the current terminal to the named tmux session.
// Used when running outside of tmux. Inherits stdin/stdout/stderr so tmux
// can take over the terminal directly.
func AttachSession(name string) error {
	cmd := exec.Command(TmuxBin, "attach-session", "-t", name)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// KillSession kills a tmux session.
func KillSession(name string) error {
	_, err := run("kill-session", "-t", name)
	return err
}

// Run executes an arbitrary tmux command and returns trimmed stdout.
// Use this for commands not covered by the typed helpers.
func Run(args ...string) (string, error) {
	return run(args...)
}

// CaptureWidth is the fixed column width used when expanding a window for
// screen capture. 220 columns is wider than most physical terminals, so the
// full opencode layout is always visible.
const CaptureWidth = 220

// DefaultCaptureHeight is the number of rows the window is expanded to for a
// normal checkin. opencode renders a fixed TUI (alternate screen mode — no
// scrollback), so a taller window = more conversation visible. This only takes
// full effect when no client is attached.
const DefaultCaptureHeight = 100

// CaptureResult holds the output of CapturePaneScreen.
type CaptureResult struct {
	Screen string
}

// CapturePaneScreen captures the visible screen of the agent window in the
// named session. opencode uses alternate screen mode (no scrollback), so the
// only way to get more content is to make the window taller before capturing.
//
// The window is temporarily resized to CaptureWidth × height. opencode reflows
// its TUI to fill the new dimensions, giving a richer capture. Use a larger
// height value to see more conversation history. Original dimensions are
// restored after capture.
func CapturePaneScreen(session string, height int) (CaptureResult, error) {
	target := session + ":agent"

	// Read current dimensions so we can restore them.
	wStr, err := run("display-message", "-t", target, "-p", "#{window_width}")
	if err != nil {
		return CaptureResult{}, fmt.Errorf("read window width: %w", err)
	}
	hStr, err := run("display-message", "-t", target, "-p", "#{window_height}")
	if err != nil {
		return CaptureResult{}, fmt.Errorf("read window height: %w", err)
	}
	var origW, origH int
	fmt.Sscan(wStr, &origW)
	fmt.Sscan(hStr, &origH)

	// Expand to capture dimensions.
	expanded := origW < CaptureWidth || origH < height
	if expanded {
		_, _ = run("resize-window", "-t", target, "-x", fmt.Sprintf("%d", CaptureWidth), "-y", fmt.Sprintf("%d", height))
		// Give opencode time to reflow its TUI to fill the new dimensions.
		time.Sleep(500 * time.Millisecond)
	}

	raw, captureErr := run("capture-pane", "-t", target, "-p")

	// Restore original dimensions regardless of capture outcome.
	// resize-window implicitly sets window-size to "manual", so we unset it
	// afterwards to restore automatic sizing (inheriting the global default).
	if expanded && origW > 0 && origH > 0 {
		_, _ = run("resize-window", "-t", target, "-x", fmt.Sprintf("%d", origW), "-y", fmt.Sprintf("%d", origH))
		_, _ = run("set-window-option", "-t", target, "-u", "window-size")
		_, _ = run("set-option", "-t", session, "-u", "window-size")
	}

	if captureErr != nil {
		return CaptureResult{}, fmt.Errorf("capture-pane: %w", captureErr)
	}
	return CaptureResult{
		Screen: cleanCaptureOutput(raw),
	}, nil
}

// cleanCaptureOutput strips tmux TUI chrome from a raw capture-pane output,
// producing clean plain text suitable for agent consumption.
func cleanCaptureOutput(raw string) string {
	lines := strings.Split(raw, "\n")
	var out []string
	for _, line := range lines {
		// Strip trailing whitespace first.
		line = strings.TrimRight(line, " \t")
		// Drop lines that are nothing but scrollbar (█) and/or whitespace.
		stripped := strings.TrimLeft(line, " \t")
		if stripped == "" || strings.Trim(stripped, "█") == "" {
			continue
		}
		// Strip leading ┃ border (opencode conversation pane left edge).
		// ┃ is a 3-byte UTF-8 sequence; handle optional leading spaces.
		if idx := strings.Index(line, "┃"); idx >= 0 && strings.TrimSpace(line[:idx]) == "" {
			line = line[idx+len("┃"):]
			// Remove one optional leading space after the border.
			line = strings.TrimPrefix(line, " ")
		}
		// Truncate at the scrollbar column — everything to the right of █ is
		// sidebar content that wraps badly; the sidebar summary in the header
		// is sufficient.
		if i := strings.Index(line, "█"); i >= 0 {
			line = strings.TrimRight(line[:i], " \t")
		}
		// Skip if now empty.
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// ListClients returns the names of all attached tmux clients.
func ListClients() ([]string, error) {
	out, err := run("list-clients", "-F", "#{client_name}")
	if err != nil {
		return nil, err
	}
	var clients []string
	for _, c := range strings.Split(out, "\n") {
		c = strings.TrimSpace(c)
		if c != "" {
			clients = append(clients, c)
		}
	}
	return clients, nil
}

// clientWidth returns the terminal width of the named client in columns.
func clientWidth(client string) int {
	out, err := run("display-message", "-t", client, "-p", "#{client_width}")
	if err != nil {
		return 80
	}
	w := 0
	fmt.Sscan(out, &w)
	if w <= 0 {
		return 80
	}
	return w
}

// DisplayMessage sends a styled, full-width message to the named client's
// status bar for the given duration (milliseconds).
// style is a tmux format string like "#[fg=colour,bg=colour]" that will be
// prepended to the visible text. The message is padded to fill the client
// width so the background colour spans the whole bar.
func DisplayMessage(client, style, text string, durationMs int) error {
	width := clientWidth(client)
	// style bytes are invisible — pad total string length by len(style) extra
	// so that the visible portion fills the bar.
	total := width + len(style)
	padded := fmt.Sprintf("%-*s", total, style+text)
	_, err := run("display-message", "-c", client, "-d", fmt.Sprintf("%d", durationMs), padded)
	return err
}

// SetWindowOption sets a window option on the given window target.
func SetWindowOption(target, option, value string) error {
	_, err := run("set-window-option", "-t", target, option, value)
	return err
}

// UnsetWindowOption unsets a window option on the given window target,
// reverting it to the global default.
func UnsetWindowOption(target, option string) error {
	_, err := run("set-window-option", "-t", target, "-u", option)
	return err
}

// GetWindowOption returns the value of a window option on the given target.
func GetWindowOption(target, option string) (string, error) {
	return run("show-window-options", "-t", target, "-v", option)
}

// SetGlobalOption sets a global tmux server option.
func SetGlobalOption(option, value string) error {
	_, err := run("set-option", "-g", option, value)
	return err
}

// UnsetGlobalOption unsets a global tmux server option.
func UnsetGlobalOption(option string) error {
	_, err := run("set-option", "-gu", option)
	return err
}

// GetGlobalOption returns the value of a global tmux server option.
func GetGlobalOption(option string) (string, error) {
	return run("show-option", "-gv", option)
}

// WindowID returns the window ID for the given pane.
func WindowID(pane string) (string, error) {
	return run("display-message", "-t", pane, "-p", "#{window_id}")
}

// SessionNameOf returns the session name for the given window target.
func SessionNameOf(target string) (string, error) {
	return run("display-message", "-t", target, "-p", "#{session_name}")
}

// ListWindows returns raw "name|path" lines for all windows in a session.
func ListWindows(session string) (string, error) {
	return run("list-windows", "-t", session, "-F", "#{window_name}|#{pane_current_path}")
}

// SelectAgentWindow selects the agent window in a session.
func SelectAgentWindow(session string) error {
	out, err := run("list-windows", "-t", session, "-F", "#{window_index}|#{window_name}")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 2)
		if len(parts) == 2 && parts[1] == "agent" {
			_, err = run("select-window", "-t", session+":"+parts[0])
			return err
		}
	}
	return nil
}
