package cmd

// prism event — write lifecycle events to prism.db
//
// Sub-subcommands:
//
//	state-change          --session <name> --state <state> --worktree <path>
//	pane-died             --session <name> --window <name>
//	tmux-session-start    --session <name> --worktree <path>
//	tmux-session-end      --session <name>
//	compaction            --session <name>
//	error                 --session <name> --message <text>
//	doom-loop-detected    --session <name> --tool <tool> --pattern <pattern> --count <n>

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	prismSession "github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// touchDashboardSentinel creates or updates the modification time of the
// dashboard sentinel file, which causes the dashboard's watcher goroutine to
// send a RefreshMsg and re-fetch session state from the DB.
//
// Errors are silently ignored — the sentinel touch is best-effort. If the
// dashboard is not running or the file cannot be created, the event write
// already succeeded and the dashboard will refresh on its next poll anyway.
func touchDashboardSentinel() {
	sentinel := dashSentinelPath()
	_ = os.MkdirAll(filepath.Dir(sentinel), 0o755)
	now := time.Now()
	if err := os.Chtimes(sentinel, now, now); err != nil {
		// File doesn't exist yet — create it.
		f, err := os.OpenFile(sentinel, os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
		}
	}
}

// deriveBareRoot walks parent directories from worktree until it finds a
// directory containing a ".bare" entry. Returns the bare root path, or an
// empty string if none is found.
//
// The check uses err == nil (i.e. the entry exists) rather than info.IsDir()
// so it works whether .bare is a directory (standard git clone --bare layout)
// or a regular file (gitdir pointer in some alternate configurations). This
// matches the permissiveness of git.IsBareRepo.
func deriveBareRoot(worktree string) string {
	p := worktree
	for {
		_, err := os.Stat(filepath.Join(p, ".bare"))
		if err == nil {
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

		// Skip the meta-sessions (scratchpad and prism-dashboard) that must
		// not appear in agent_status. All other sessions — including
		// non-worktree sessions like "obsidian" — are tracked. Mirror the
		// guard used by tmux-session-start so state transitions flow through
		// for every session that start already wrote a row for.
		if prismSession.IsMetaSession(session) {
			return nil
		}

		// For non-worktree sessions deriveRepo returns "" — that matches the
		// row tmux-session-start already wrote, so pass it through as-is.
		repo := deriveRepo(worktree)

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
		touchDashboardSentinel()
		return nil
	},
}

// --- pane-died ---
//
// Called by the tmux pane-exited hook when the opencode pane exits. If the
// session is currently in an active (non-terminal) state, this transitions it
// to "interrupted". Sessions already in interrupted or deleted state are left
// unchanged — this guards against stale hook fires after a clean exit.
//
// When exitCode is non-zero (signal-based exit or crash), "finished" is also
// overridden with "interrupted" — a non-zero exit means the process did not
// complete cleanly regardless of what the plugin wrote to the DB before dying.

var eventPaneDiedCmd = &cobra.Command{
	Use:   "pane-died",
	Short: "Transition an active session to interrupted when its pane dies",
	RunE: func(cmd *cobra.Command, args []string) error {
		session, _ := cmd.Flags().GetString("session")
		window, _ := cmd.Flags().GetString("window")
		exitCode, _ := cmd.Flags().GetInt("exit-code")

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
			// No DB record for this session — exit silently. This can happen
			// for races (session ended before the hook wrote a row) or for
			// genuinely untracked sessions.
			return nil
		}

		var updated bool
		if exitCode != 0 {
			// Non-zero exit: the process was killed or crashed.  Override even
			// a prior "finished" state — the session was not completed cleanly.
			// This is the primary fix for the race where the plugin writes
			// "finished" via the idle debounce before the pane-died hook fires.
			updated, err = d.UpsertStatusInterruptedOverrideFinished(session)
		} else {
			// Clean exit (exit code 0): only transition if not already in a
			// terminal state.  A zero exit after "finished" means the session
			// completed cleanly; leave "finished" intact.
			updated, err = d.UpsertStatusIfNotTerminal(session, string(agent.StateInterrupted))
		}
		if err != nil {
			return fmt.Errorf("event pane-died: %w", err)
		}
		if !updated {
			// No update applied.
			// exitCode == 0 path: session was already finished/interrupted/deleted.
			// exitCode != 0 path: session was already interrupted/deleted or ended_at is set.
			return nil
		}

		e := db.Event{
			ID:          uuid.New().String(),
			SessionName: session,
			Repo:        s.Repo,
			Worktree:    s.Worktree,
			Type:        "state_change",
			Payload:     fmt.Sprintf(`{"state":%q}`, string(agent.StateInterrupted)),
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
		agentRole, _ := cmd.Flags().GetString("agent-role")

		repo := deriveRepo(worktree)
		if repo == "" {
			// Non-worktree session: skip only the meta-sessions (scratchpad and
			// prism-dashboard) that must not appear in agent_status. All other
			// non-worktree sessions (e.g. "obsidian") are legitimate user
			// sessions and get a row with repo="" — consistent with the port
			// allocation path in switch.go:allocatePortForSession.
			if prismSession.IsMetaSession(session) {
				return nil
			}
			// Fall through: UpsertStatus will be called with repo="" below.
		}

		d, err := openDB()
		if err != nil {
			return fmt.Errorf("event tmux-session-start: %w", err)
		}
		defer d.Close()

		// When --agent-role is provided, seed root_agent_name immediately so
		// the DB reflects the agent type from the first moment (before the
		// sidecar's first upsertState() call). When omitted, fall back to the
		// plain UpsertStatus path which leaves root_agent_name as-is (NULL on
		// insert, preserved on update).
		if agentRole != "" {
			if err := d.UpsertStatusSeedRootAgentName(session, repo, worktree, string(agent.StateIdle), nil, nil, agentRole); err != nil {
				return fmt.Errorf("event tmux-session-start: upsert status: %w", err)
			}
		} else {
			if err := d.UpsertStatus(session, repo, worktree, string(agent.StateIdle), nil, nil); err != nil {
				return fmt.Errorf("event tmux-session-start: upsert status: %w", err)
			}
		}
		if err := d.ClearEnded(session); err != nil {
			return fmt.Errorf("event tmux-session-start: clear ended: %w", err)
		}

		// Determine the instance_id for this session incarnation. When the
		// caller (ensureAndSwitch) has already written an instance_id to the DB
		// (e.g. before starting the sidecar), we preserve it so the sidecar and
		// the DB remain in sync. For standalone invocations (tmux hook, restore
		// path) where no instance_id has been pre-written, generate a fresh UUID.
		var instanceID string
		if existing, stErr := d.CurrentStatus(session); stErr == nil && existing != nil && existing.InstanceID != nil {
			instanceID = *existing.InstanceID
		} else {
			instanceID = uuid.New().String()
			if err := d.SetInstanceID(session, instanceID); err != nil {
				// Non-fatal: instance isolation degrades gracefully.
				fmt.Fprintf(os.Stderr, "prism event tmux-session-start: set instance_id: %v\n", err)
			}
		}

		// Purge any undelivered bus messages addressed to a previous incarnation
		// of this session so stale messages don't leak into the new instance.
		if err := d.PurgeStaleInstanceMessages(session, instanceID); err != nil {
			// Non-fatal: log and continue.
			fmt.Fprintf(os.Stderr, "prism event tmux-session-start: purge stale instance messages: %v\n", err)
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
		touchDashboardSentinel()

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

		// Guard: reject blank session names before touching tmux or the DB.
		if strings.TrimSpace(session) == "" {
			return fmt.Errorf("event tmux-session-end: session name must not be empty")
		}

		// Liveness check: the session-closed hook fires spuriously when a
		// display-popup is dismissed (tmux reports the popup's pseudo-session
		// name as the outer session). If the session still exists in tmux it
		// is a spurious fire — exit 0 without writing anything to the DB.
		if tmux.HasSession(session) {
			return nil
		}

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
			// No DB record for this session — exit 0 silently. This can happen
			// for races (session ended before the hook wrote a row) or for
			// genuinely untracked sessions.
			return nil
		}

		if err := d.ReleasePort(session); err != nil {
			return fmt.Errorf("event tmux-session-end: release port: %w", err)
		}
		if err := d.SetEnded(session); err != nil {
			return fmt.Errorf("event tmux-session-end: set ended: %w", err)
		}
		// Clear instance_id so the session is no longer associated with a
		// specific incarnation. Any undelivered bus messages for this instance
		// are purged below via PurgeBusMessages.
		if err := d.ClearInstanceID(session); err != nil {
			// Non-fatal: log and continue.
			fmt.Fprintf(os.Stderr, "prism event tmux-session-end: clear instance_id: %v\n", err)
		}
		if err := d.PurgeBusMessages(session); err != nil {
			return fmt.Errorf("event tmux-session-end: purge bus messages: %w", err)
		}

		// Clean up any per-session bus sentinel files that may have been written
		// by older versions of `prism prompt`. The sentinel (<session>.signal in
		// prism/bus/) is no longer created as of the HTTP-only delivery refactor,
		// but the removal is retained to clean up files left from prior runs.
		// It is separate from the shared .dashboard.signal used by the dashboard
		// watcher. The removal is best-effort — the file may not exist.
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
		touchDashboardSentinel()

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

		if err := d.UpsertStatus(session, repo, worktree, string(agent.StateCompacting), nil, nil); err != nil {
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
		touchDashboardSentinel()

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

		if err := d.UpsertStatus(session, repo, worktree, string(agent.StateError), nil, nil); err != nil {
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
		touchDashboardSentinel()

		return nil
	},
}

// --- doom-loop-detected ---

var eventDoomLoopDetectedCmd = &cobra.Command{
	Use:   "doom-loop-detected",
	Short: "Write a doom_loop_detected event to prism.db",
	Long: `Write a doom_loop_detected event to prism.db.

Called by the prism-hooks plugin when it detects an agent repeating the same
tool call with near-identical arguments N consecutive times in the same session.
This is a hook-side log command — the detection and suppression logic lives in
the plugin; this command only writes the event.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		session, _ := cmd.Flags().GetString("session")
		tool, _ := cmd.Flags().GetString("tool")
		pattern, _ := cmd.Flags().GetString("pattern")
		count, _ := cmd.Flags().GetInt("count")

		d, err := openDB()
		if err != nil {
			return fmt.Errorf("event doom-loop-detected: %w", err)
		}
		defer d.Close()

		s, err := d.CurrentStatus(session)
		if err != nil {
			return fmt.Errorf("event doom-loop-detected: current status: %w", err)
		}

		repo := ""
		worktree := ""
		if s != nil {
			repo = s.Repo
			worktree = s.Worktree
		}

		now := time.Now()
		payload := fmt.Sprintf(`{"tool":%q,"pattern":%q,"count":%d,"timestampMs":%d}`, tool, pattern, count, now.UnixMilli())
		e := db.Event{
			ID:          uuid.New().String(),
			SessionName: session,
			Repo:        repo,
			Worktree:    worktree,
			Type:        "doom_loop_detected",
			Payload:     payload,
			CreatedAt:   now,
		}
		if err := d.WriteEvent(e); err != nil {
			return fmt.Errorf("event doom-loop-detected: write event: %w", err)
		}
		touchDashboardSentinel()
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
	eventPaneDiedCmd.Flags().Int("exit-code", 0, "pane exit code (#{pane_dead_status}); non-zero overrides finished→interrupted")
	_ = eventPaneDiedCmd.MarkFlagRequired("session")
	_ = eventPaneDiedCmd.MarkFlagRequired("window")

	// tmux-session-start flags
	eventTmuxSessionStartCmd.Flags().String("session", "", "tmux session name")
	eventTmuxSessionStartCmd.Flags().String("worktree", "", "worktree path")
	eventTmuxSessionStartCmd.Flags().String("agent-role", "", "agent role to seed into root_agent_name (e.g. worker, coordinator, review-code); when omitted, root_agent_name is left unchanged")
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

	// doom-loop-detected flags
	eventDoomLoopDetectedCmd.Flags().String("session", "", "tmux session name")
	eventDoomLoopDetectedCmd.Flags().String("tool", "", "tool name that triggered detection")
	eventDoomLoopDetectedCmd.Flags().String("pattern", "", "normalised argument pattern that matched")
	eventDoomLoopDetectedCmd.Flags().Int("count", 5, "number of consecutive matching calls (default 5)")
	_ = eventDoomLoopDetectedCmd.MarkFlagRequired("session")
	_ = eventDoomLoopDetectedCmd.MarkFlagRequired("tool")
	_ = eventDoomLoopDetectedCmd.MarkFlagRequired("pattern")

	eventCmd.AddCommand(eventStateChangeCmd)
	eventCmd.AddCommand(eventPaneDiedCmd)
	eventCmd.AddCommand(eventTmuxSessionStartCmd)
	eventCmd.AddCommand(eventTmuxSessionEndCmd)
	eventCmd.AddCommand(eventCompactionCmd)
	eventCmd.AddCommand(eventErrorCmd)
	eventCmd.AddCommand(eventDoomLoopDetectedCmd)

	rootCmd.AddCommand(eventCmd)
}
