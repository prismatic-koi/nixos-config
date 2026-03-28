// Package tmux provides helpers for querying and controlling a running tmux server.
package tmux

import (
	"fmt"
	"os/exec"
	"strings"
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

// StartDisplayMessage fires a display-message asynchronously. It blocks only
// long enough to query the client width and start the child process, then
// returns — it does not wait for the display duration to elapse.
func StartDisplayMessage(client, style, text string, durationMs int) {
	width := clientWidth(client)
	total := width + len(style)
	padded := fmt.Sprintf("%-*s", total, style+text)
	cmd := exec.Command(TmuxBin, "display-message", "-c", client, "-d", fmt.Sprintf("%d", durationMs), padded)
	if err := cmd.Start(); err == nil {
		go cmd.Wait() //nolint:errcheck
	}
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
