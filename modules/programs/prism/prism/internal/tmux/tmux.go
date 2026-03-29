// Package tmux provides helpers for querying and controlling a running tmux server.
package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
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

// run executes a tmux command and returns trimmed stdout.
func run(args ...string) (string, error) {
	out, err := exec.Command(TmuxBin, args...).Output()
	if err != nil {
		return "", err
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
func Sessions() ([]Session, error) {
	out, err := run("list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil, err
	}

	clients := clientsPerSession()

	var sessions []Session
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		state, path, title := agentWindow(name)
		sessions = append(sessions, Session{
			Name:        name,
			AgentState:  state,
			AgentPath:   path,
			AgentTitle:  title,
			ClientCount: clients[name],
		})
	}
	return sessions, nil
}

// agentWindow returns the @agent_state, pane_current_path, and @agent_title
// for the agent window of a session.
func agentWindow(session string) (state, path, title string) {
	out, err := run(
		"list-windows", "-t", session,
		"-F", "#{window_name}|#{@agent_state}|#{pane_current_path}|#{@agent_title}",
	)
	if err != nil {
		return "", "", ""
	}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		if parts[0] == "agent" {
			return parts[1], parts[2], parts[3]
		}
	}
	return "", "", ""
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
func NewWindow(session string, idx int, name, dir string) error {
	args := []string{"new-window", "-t", fmt.Sprintf("%s:%d", session, idx), "-n", name}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	_, err := run(args...)
	return err
}

// SendKeys sends key strokes to a target pane/window.
func SendKeys(target, keys string) error {
	_, err := run("send-keys", "-t", target, keys, "C-m")
	return err
}

// SendKeysDelayed sends keystrokes to a target pane after a delay (milliseconds),
// followed by a separate Enter keypress 500ms later.
// Forks a detached child process so it survives after the parent exits.
func SendKeysDelayed(target, keys string, delayMs int) error {
	script := fmt.Sprintf(
		"sleep %.1f && %s send-keys -t %s %s && sleep 0.5 && %s send-keys -t %s Enter",
		float64(delayMs)/1000.0,
		TmuxBin, shellEscape(target), shellEscape(keys),
		TmuxBin, shellEscape(target),
	)
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start() // Start (not Run) — don't wait for it
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

// SessionClients returns the names of all clients currently attached to the
// named session.
func SessionClients(session string) ([]string, error) {
	out, err := run("list-clients", "-t", session, "-F", "#{client_name}")
	if err != nil {
		// list-clients exits non-zero when no clients are attached — not an error.
		return nil, nil
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

// KillSessionClients detaches all clients attached to the named session.
func KillSessionClients(session string) error {
	clients, err := SessionClients(session)
	if err != nil {
		return err
	}
	for _, c := range clients {
		_, _ = run("detach-client", "-t", c)
	}
	return nil
}

// CaptureResult holds the output of CapturePaneScreen along with any advisory
// warnings the caller should be aware of.
type CaptureResult struct {
	Screen          string
	ClientsAttached []string // non-empty if clients were attached during capture
}

// CapturePaneScreen captures the visible screen of the agent window in the
// named session. opencode uses alternate screen mode (no scrollback), so the
// only way to get more content is to make the window taller before capturing.
//
// The window is temporarily resized to CaptureWidth × height. When no clients
// are attached tmux honours the full size and opencode reflows to fill it,
// giving a much richer capture. When clients are attached their physical
// terminal size constrains the window and the resize has little effect —
// ClientsAttached will be non-empty as a warning to the caller.
// Original dimensions are restored after capture.
func CapturePaneScreen(session string, height int) (CaptureResult, error) {
	target := session + ":agent"

	clients, _ := SessionClients(session)

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
		Screen:          cleanCaptureOutput(raw),
		ClientsAttached: clients,
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
