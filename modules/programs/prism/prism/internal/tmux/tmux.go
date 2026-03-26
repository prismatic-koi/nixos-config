// Package tmux provides helpers for querying and controlling a running tmux server.
package tmux

import (
	"os/exec"
	"strings"
)

// Session represents a tmux session with its agent state.
type Session struct {
	Name       string
	AgentState string // active | waiting | finished | compacting | error | ""
	AgentPath  string // pane_current_path of the agent window
}

// run executes a tmux command and returns trimmed stdout.
func run(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Sessions returns all current tmux sessions.
func Sessions() ([]Session, error) {
	out, err := run("list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil, err
	}

	var sessions []Session
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		state, path := agentWindow(name)
		sessions = append(sessions, Session{
			Name:       name,
			AgentState: state,
			AgentPath:  path,
		})
	}
	return sessions, nil
}

// agentWindow returns the @agent_state and pane_current_path for the agent
// window of a session, falling back to the term window path if no agent window exists.
func agentWindow(session string) (state, path string) {
	out, err := run(
		"list-windows", "-t", session,
		"-F", "#{window_name}|#{@agent_state}|#{pane_current_path}",
	)
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "agent" {
			return parts[1], parts[2]
		}
	}
	return "", ""
}

// SwitchClient switches the named client to the named session.
func SwitchClient(client, session string) error {
	_, err := run("switch-client", "-c", client, "-t", session)
	return err
}

// CurrentSession returns the session name for the current tmux client.
func CurrentSession() (string, error) {
	return run("display-message", "-p", "#{session_name}")
}

// CurrentClient returns the client name for the current tmux client.
func CurrentClient() (string, error) {
	return run("display-message", "-p", "#{client_name}")
}

// HasSession returns true if a session with the given name exists.
func HasSession(name string) bool {
	err := exec.Command("tmux", "has-session", "-t", name).Run()
	return err == nil
}

// NewSession creates a new detached session.
func NewSession(name, dir string) error {
	_, err := run("new-session", "-ds", name, "-c", dir)
	return err
}

// SelectWindow selects the window with the given name in a session, returning
// true if the window was found and selected.
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
	// Fallback: switch to session without window selection
	return nil
}
