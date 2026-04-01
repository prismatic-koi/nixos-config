package cmd

// prism save — snapshot the current set of tmux sessions to disk so that
// prism restore can recreate them after a reboot.
//
// The snapshot is written atomically to:
//
//	$XDG_STATE_HOME/prism/sessions.json   (typically ~/.local/state/prism/)
//
// Each entry records the session name, the working directory of its agent
// window (or the session root for non-agent sessions), and the bare-repo root
// where applicable.  This is enough for prism restore to call
// ensureAndSwitchSession with the same arguments that created each session.
//
// Infrastructure sessions (scratchpad, prism-dashboard) are recorded but
// recreated as bare sessions without windows — they self-configure on first use.
//
// prism save is safe to call frequently (e.g. every status-interval from the
// tmux after-refresh-client hook) because it is a pure write with no side
// effects on the running server.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/opencode"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// SavedSession is one entry in the sessions snapshot file.
type SavedSession struct {
	// Name is the tmux session name, e.g. "nixos-config@20260330T1932".
	Name string `json:"name"`
	// Dir is the working directory to use when recreating the session.
	// For worktree sessions this is the worktree path; for scratchpad it is
	// the user's home directory; for prism-dashboard it is empty.
	Dir string `json:"dir,omitempty"`
	// BareRoot is the parent bare-repo directory, e.g. "/home/ben/code/nixos-config".
	// Empty for sessions that are not inside a prism bare repo.
	BareRoot string `json:"bare_root,omitempty"`
	// OpenCodeSession is the opencode session ID most recently active in this
	// worktree, as queried from the opencode database at save time.
	// Empty for scratchpad, prism-dashboard, and sessions with no opencode history.
	OpenCodeSession string `json:"opencode_session,omitempty"`
}

// saveStatePath returns the path to the sessions snapshot file.
func saveStatePath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", "sessions.json")
}

var saveCmd = &cobra.Command{
	Use:   "save",
	Short: "Snapshot current tmux sessions to disk for later restore",
	RunE:  runSave,
}

func init() {
	rootCmd.AddCommand(saveCmd)
}

func runSave(_ *cobra.Command, _ []string) error {
	sessions, err := tmux.Sessions()
	if err != nil {
		// No tmux server — nothing to save, not an error.
		return nil
	}

	saved := []SavedSession{}
	for _, s := range sessions {
		entry := sessionToSaved(s)
		saved = append(saved, entry)
	}

	return writeSessions(saved)
}

// sessionToSaved converts a live tmux.Session to a SavedSession record.
func sessionToSaved(s tmux.Session) SavedSession {
	switch s.Name {
	case "scratchpad":
		home, _ := os.UserHomeDir()
		return SavedSession{Name: s.Name, Dir: home}
	case "prism-dashboard":
		return SavedSession{Name: s.Name}
	}

	// For worktree sessions use the agent window's working directory.
	// Fall back to the session name itself — not ideal but better than nothing.
	dir := s.AgentPath
	if dir == "" {
		return SavedSession{Name: s.Name}
	}

	bareRoot := git.BareRoot(dir)
	return SavedSession{
		Name:            s.Name,
		Dir:             dir,
		BareRoot:        bareRoot,
		OpenCodeSession: opencode.LatestSessionForDir(dir),
	}
}

// writeSessions atomically writes the session list to the snapshot path.
func writeSessions(sessions []SavedSession) error {
	path := saveStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}

	data, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sessions: %w", err)
	}

	// Write to a per-PID temp file then rename for atomicity.
	// Using the PID avoids a race when multiple clients trigger after-refresh-client
	// concurrently: each process writes its own .tmp file, and the last rename wins
	// cleanly without any process clobbering another's in-progress write.
	tmp := path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
