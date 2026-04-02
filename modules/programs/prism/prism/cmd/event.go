package cmd

// prism event — write lifecycle events to prism.db
//
// Sub-subcommands:
//
//	state-change       --session <name> --state <state> --worktree <path>
//	pane-died          --session <name>
//	tmux-session-start --session <name> --worktree <path>
//	tmux-session-end   --session <name>
//	compaction         --session <name>
//	error              --session <name> --message <text>

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// deriveBareRoot walks parent directories from worktree until it finds a
// directory containing a file named ".bare". Returns the bare root path, or
// an empty string if none is found.
func deriveBareRoot(worktree string) string {
	p := worktree
	for {
		info, err := os.Stat(filepath.Join(p, ".bare"))
		if err == nil && info.IsDir() {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return ""
}

// deriveRepo returns the repo name from a worktree path by walking up to find
// the .bare marker. Returns empty string if not found.
func deriveRepo(worktree string) string {
	bareRoot := deriveBareRoot(worktree)
	if bareRoot == "" {
		return ""
	}
	name := filepath.Base(bareRoot)
	name = strings.TrimSuffix(name, ".git")
	return name
}

var eventCmd = &cobra.Command{
	Use:   "event",
	Short: "Write lifecycle events to prism.db",
}

// --- state-change ---

var eventStateChangeCmd = &cobra.Command{
	Use:   "state-change",
	Short: "Write a state_change event and upsert agent status",
	RunE: func(cmd *cobra.Command, args []string) error {
		session, _ := cmd.Flags().GetString("session")
		state, _ := cmd.Flags().GetString("state")
		worktree, _ := cmd.Flags().GetString("worktree")

		repo := deriveRepo(worktree)
		if repo == "" {
			// Not inside a project worktree — exit silently.
			return nil
		}

		d, err := openDB()
		if err != nil {
			return fmt.Errorf("event state-change: %w", err)
		}
		defer d.Close()

		if err := d.UpsertStatus(session, repo, worktree, state, nil, nil); err != nil {
			return fmt.Errorf("event state-change: upsert status: %w", err)
		}

		e := db.Event{
			ID:          uuid.New().String(),
			SessionName: session,
			Repo:        repo,
			Worktree:    worktree,
			Type:        "state_change",
			Payload:     fmt.Sprintf(`{"state":%q}`, state),
			CreatedAt:   time.Now(),
		}
		if err := d.WriteEvent(e); err != nil {
			return fmt.Errorf("event state-change: write event: %w", err)
		}
		return nil
	},
}

// --- pane-died ---
//
// Called by the tmux pane-died hook when the opencode pane exits. If the
// session is currently in an active (non-terminal) state, this transitions it
// to "interrupted". Sessions already in finished, interrupted, or deleted state
// are left unchanged — this guards against stale hook fires after a clean exit.

var eventPaneDiedCmd = &cobra.Command{
	Use:   "pane-died",
	Short: "Transition an active session to interrupted when its pane dies",
	RunE: func(cmd *cobra.Command, args []string) error {
		session, _ := cmd.Flags().GetString("session")
		window, _ := cmd.Flags().GetString("window")

		// Only the agent window exit is meaningful — exits from term, edit,
		// or any other window should not mark the session as interrupted.
		if window != "agent" {
			return nil
		}

		d, err := openDB()
		if err != nil {
			return fmt.Errorf("event pane-died: %w", err)
		}
		defer d.Close()

		// Look up session metadata to get repo/worktree for the event row.
		s, err := d.CurrentStatus(session)
		if err != nil {
			return fmt.Errorf("event pane-died: current status: %w", err)
		}
		if s == nil {
			// No DB record — non-project session; exit silently.
			return nil
		}

		// Transition to interrupted only if not already in a terminal state.
		updated, err := d.UpsertStatusIfNotTerminal(session, "interrupted")
		if err != nil {
			return fmt.Errorf("event pane-died: %w", err)
		}
		if !updated {
			// Already finished / interrupted / deleted — nothing to do.
			return nil
		}

		// Update the tmux @agent_state window option so the dashboard can
		// read the interrupted state via list-windows. The pane-died hook
		// runs outside the opencode process (no TMUX_PANE), so we target the
		// agent window directly by session name.
		agentWindow := session + ":agent"
		_ = tmux.SetWindowOption(agentWindow, "@agent_state", "interrupted")
		// Also update the window-status-format so the status bar reflects the
		// interrupted state in red.
		statusFmt := fmt.Sprintf("#[fg=%s]#I:#W#{?window_flags,#{window_flags}, }", ColorRed)
		_ = tmux.SetWindowOption(agentWindow, "window-status-format", statusFmt)
		_ = tmux.SetWindowOption(agentWindow, "window-status-current-format", statusFmt)

		e := db.Event{
			ID:          uuid.New().String(),
			SessionName: session,
			Repo:        s.Repo,
			Worktree:    s.Worktree,
			Type:        "state_change",
			Payload:     `{"state":"interrupted"}`,
			CreatedAt:   time.Now(),
		}
		if err := d.WriteEvent(e); err != nil {
			return fmt.Errorf("event pane-died: write event: %w", err)
		}
		return nil
	},
}

// --- tmux-session-start ---

var eventTmuxSessionStartCmd = &cobra.Command{
	Use:   "tmux-session-start",
	Short: "Write a tmux_session_start event and upsert agent status",
	RunE: func(cmd *cobra.Command, args []string) error {
		session, _ := cmd.Flags().GetString("session")
		worktree, _ := cmd.Flags().GetString("worktree")

		// If no .bare marker is found, exit 0 silently — this is a non-project
		// session (scratchpad, prism-dashboard, etc.).
		repo := deriveRepo(worktree)
		if repo == "" {
			return nil
		}

		d, err := openDB()
		if err != nil {
			return fmt.Errorf("event tmux-session-start: %w", err)
		}
		defer d.Close()

		if err := d.UpsertStatus(session, repo, worktree, "idle", nil, nil); err != nil {
			return fmt.Errorf("event tmux-session-start: upsert status: %w", err)
		}

		e := db.Event{
			ID:          uuid.New().String(),
			SessionName: session,
			Repo:        repo,
			Worktree:    worktree,
			Type:        "tmux_session_start",
			Payload:     `{}`,
			CreatedAt:   time.Now(),
		}
		if err := d.WriteEvent(e); err != nil {
			return fmt.Errorf("event tmux-session-start: write event: %w", err)
		}

		// Prune old data once per new session (at most once per tmux server start).
		if err := d.Prune(90 * 24 * time.Hour); err != nil {
			// Non-fatal — log but don't fail the command.
			fmt.Fprintf(os.Stderr, "prism event tmux-session-start: prune: %v\n", err)
		}

		return nil
	},
}

// --- tmux-session-end ---

var eventTmuxSessionEndCmd = &cobra.Command{
	Use:   "tmux-session-end",
	Short: "Mark the session as ended and write a tmux_session_end event",
	RunE: func(cmd *cobra.Command, args []string) error {
		session, _ := cmd.Flags().GetString("session")

		d, err := openDB()
		if err != nil {
			return fmt.Errorf("event tmux-session-end: %w", err)
		}
		defer d.Close()

		// Look up the session's repo and worktree for the event row.
		s, err := d.CurrentStatus(session)
		if err != nil {
			return fmt.Errorf("event tmux-session-end: current status: %w", err)
		}
		if s == nil {
			// No DB record for this session — it was a non-project session; exit 0 silently.
			return nil
		}

		if err := d.SetEnded(session); err != nil {
			return fmt.Errorf("event tmux-session-end: set ended: %w", err)
		}

		// Delete sentinel file if it exists.
		stateHome := os.Getenv("XDG_STATE_HOME")
		if stateHome == "" {
			home, _ := os.UserHomeDir()
			stateHome = filepath.Join(home, ".local", "state")
		}
		sentinel := filepath.Join(stateHome, "prism", "bus", session+".signal")
		_ = os.Remove(sentinel) // ignore error — file may not exist

		// Write the end event using the known repo/worktree from the status row.
		repo := s.Repo
		worktree := s.Worktree

		e := db.Event{
			ID:          uuid.New().String(),
			SessionName: session,
			Repo:        repo,
			Worktree:    worktree,
			Type:        "tmux_session_end",
			Payload:     `{}`,
			CreatedAt:   time.Now(),
		}
		if err := d.WriteEvent(e); err != nil {
			return fmt.Errorf("event tmux-session-end: write event: %w", err)
		}

		return nil
	},
}

// --- compaction ---

var eventCompactionCmd = &cobra.Command{
	Use:   "compaction",
	Short: "Write a compaction event and upsert state=compacting",
	RunE: func(cmd *cobra.Command, args []string) error {
		session, _ := cmd.Flags().GetString("session")

		d, err := openDB()
		if err != nil {
			return fmt.Errorf("event compaction: %w", err)
		}
		defer d.Close()

		s, err := d.CurrentStatus(session)
		if err != nil {
			return fmt.Errorf("event compaction: current status: %w", err)
		}

		repo := ""
		worktree := ""
		if s != nil {
			repo = s.Repo
			worktree = s.Worktree
		}

		if err := d.UpsertStatus(session, repo, worktree, "compacting", nil, nil); err != nil {
			return fmt.Errorf("event compaction: upsert status: %w", err)
		}

		e := db.Event{
			ID:          uuid.New().String(),
			SessionName: session,
			Repo:        repo,
			Worktree:    worktree,
			Type:        "compaction",
			Payload:     `{}`,
			CreatedAt:   time.Now(),
		}
		if err := d.WriteEvent(e); err != nil {
			return fmt.Errorf("event compaction: write event: %w", err)
		}

		return nil
	},
}

// --- error ---

var eventErrorCmd = &cobra.Command{
	Use:   "error",
	Short: "Write an error event and upsert state=error",
	RunE: func(cmd *cobra.Command, args []string) error {
		session, _ := cmd.Flags().GetString("session")
		message, _ := cmd.Flags().GetString("message")

		d, err := openDB()
		if err != nil {
			return fmt.Errorf("event error: %w", err)
		}
		defer d.Close()

		s, err := d.CurrentStatus(session)
		if err != nil {
			return fmt.Errorf("event error: current status: %w", err)
		}

		repo := ""
		worktree := ""
		if s != nil {
			repo = s.Repo
			worktree = s.Worktree
		}

		if err := d.UpsertStatus(session, repo, worktree, "error", nil, nil); err != nil {
			return fmt.Errorf("event error: upsert status: %w", err)
		}

		payload := fmt.Sprintf(`{"message":%q}`, message)
		e := db.Event{
			ID:          uuid.New().String(),
			SessionName: session,
			Repo:        repo,
			Worktree:    worktree,
			Type:        "error",
			Payload:     payload,
			CreatedAt:   time.Now(),
		}
		if err := d.WriteEvent(e); err != nil {
			return fmt.Errorf("event error: write event: %w", err)
		}

		return nil
	},
}

func init() {
	// state-change flags
	eventStateChangeCmd.Flags().String("session", "", "tmux session name")
	eventStateChangeCmd.Flags().String("state", "", "new agent state")
	eventStateChangeCmd.Flags().String("worktree", "", "worktree path")
	_ = eventStateChangeCmd.MarkFlagRequired("session")
	_ = eventStateChangeCmd.MarkFlagRequired("state")
	_ = eventStateChangeCmd.MarkFlagRequired("worktree")

	// pane-died flags
	eventPaneDiedCmd.Flags().String("session", "", "tmux session name")
	eventPaneDiedCmd.Flags().String("window", "", "tmux window name")
	_ = eventPaneDiedCmd.MarkFlagRequired("session")
	_ = eventPaneDiedCmd.MarkFlagRequired("window")

	// tmux-session-start flags
	eventTmuxSessionStartCmd.Flags().String("session", "", "tmux session name")
	eventTmuxSessionStartCmd.Flags().String("worktree", "", "worktree path")
	_ = eventTmuxSessionStartCmd.MarkFlagRequired("session")
	_ = eventTmuxSessionStartCmd.MarkFlagRequired("worktree")

	// tmux-session-end flags
	eventTmuxSessionEndCmd.Flags().String("session", "", "tmux session name")
	_ = eventTmuxSessionEndCmd.MarkFlagRequired("session")

	// compaction flags
	eventCompactionCmd.Flags().String("session", "", "tmux session name")
	_ = eventCompactionCmd.MarkFlagRequired("session")

	// error flags
	eventErrorCmd.Flags().String("session", "", "tmux session name")
	eventErrorCmd.Flags().String("message", "", "error message")
	_ = eventErrorCmd.MarkFlagRequired("session")
	_ = eventErrorCmd.MarkFlagRequired("message")

	eventCmd.AddCommand(eventStateChangeCmd)
	eventCmd.AddCommand(eventPaneDiedCmd)
	eventCmd.AddCommand(eventTmuxSessionStartCmd)
	eventCmd.AddCommand(eventTmuxSessionEndCmd)
	eventCmd.AddCommand(eventCompactionCmd)
	eventCmd.AddCommand(eventErrorCmd)

	rootCmd.AddCommand(eventCmd)
}
