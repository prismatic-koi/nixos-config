package cmd

// prism prompt — send a follow-up prompt to a running or finished agent session.
//
// Usage:
//
//	prism prompt <session> --prompt <text>
//	prism prompt <session> --prompt - < /tmp/prompt.txt
//	prism prompt <session> --prompt-file /tmp/prompt.txt
//	prism prompt <session> --urgent --prompt <text>

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

var promptCmd = &cobra.Command{
	Use:   "prompt <session>",
	Short: "Send a follow-up prompt to a running agent session",
	Long: `Send a follow-up message to the opencode agent running in the named tmux
session. The session must already exist and have an agent window.

The prompt is written to the message bus (bus_messages table) and delivered
by the opencode plugin on the next idle event (normal urgency) or within
~2 seconds (interrupt urgency, with --urgent).`,
	Args: cobra.ExactArgs(1),
	RunE: runPrompt,
}

func init() {
	addPromptFlags(promptCmd)
	promptCmd.Flags().Bool("urgent", false, "Deliver within ~2 seconds via interrupt polling (urgency='interrupt')")
	rootCmd.AddCommand(promptCmd)
}

func runPrompt(cmd *cobra.Command, args []string) error {
	session := args[0]

	promptText, err := requirePromptInput(cmd)
	if err != nil {
		return err
	}

	urgent, _ := cmd.Flags().GetBool("urgent")

	// Open DB.
	database, err := openDB()
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	// Check target session status.
	status, err := database.CurrentStatus(session)
	if err != nil {
		return fmt.Errorf("check session status: %w", err)
	}

	if status != nil && status.EndedAt != nil {
		return fmt.Errorf(
			"session %q has ended — escalate to user to restart if needed",
			session,
		)
	}

	if status != nil && status.State == "waiting" {
		return fmt.Errorf(
			"session %q is waiting for user input\n\n"+
				"The agent has paused and is expecting a direct response from the user.\n"+
				"Please switch to that session and respond there, or escalate to the user\n"+
				"so they can address it directly.\n\n"+
				"  prism checkin %s   — inspect the current state\n"+
				"  (C-f or C-w)       — switch to the session in tmux",
			session, session,
		)
	}

	// Derive from_session from the current process CWD using the .bare walk.
	fromSession := ""
	if cwd, err := os.Getwd(); err == nil {
		fromSession = deriveSessionNameFromCWD(cwd)
	}

	// Derive repo from the target session's agent_status, fallback to from_session.
	repo := ""
	if status != nil {
		repo = status.Repo
	}
	if repo == "" && fromSession != "" {
		// best-effort: extract repo component from from_session (format: "repo@branch")
		for i, c := range fromSession {
			if c == '@' {
				repo = fromSession[:i]
				break
			}
		}
	}

	urgency := "normal"
	if urgent {
		urgency = "interrupt"
	}

	msg := db.BusMessage{
		ID:          uuid.New().String(),
		FromSession: fromSession,
		ToSession:   session,
		Repo:        repo,
		Text:        promptText,
		Urgency:     urgency,
		SentAt:      time.Now(),
	}

	if err := database.WriteBusMessage(msg); err != nil {
		return fmt.Errorf("write bus message: %w", err)
	}

	// Touch sentinel file so the Stage 8 dashboard watcher can react.
	if err := touchBusSentinel(session); err != nil {
		// Non-fatal: DB write succeeded; sentinel is best-effort.
		fmt.Fprintf(os.Stderr, "warning: could not touch bus sentinel: %v\n", err)
	}

	fmt.Printf("prompt queued for %s\n", session)
	return nil
}

// deriveSessionNameFromCWD walks up from cwd to find a .bare marker and
// derives the session name using the same logic as cmd/switch.go.
// Returns empty string if no .bare marker is found.
func deriveSessionNameFromCWD(cwd string) string {
	bareRoot := deriveBareRoot(cwd)
	if bareRoot == "" {
		return ""
	}
	return sessionNameFor(cwd, bareRoot)
}

// touchBusSentinel creates/updates the sentinel file at
// $XDG_STATE_HOME/prism/bus/<session>.signal. The directory is created if
// it does not exist.
func touchBusSentinel(sessionName string) error {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("home dir: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	busDir := filepath.Join(stateHome, "prism", "bus")
	if err := os.MkdirAll(busDir, 0o755); err != nil {
		return fmt.Errorf("mkdir bus: %w", err)
	}
	sentinelPath := filepath.Join(busDir, sessionName+".signal")
	f, err := os.OpenFile(sentinelPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	now := time.Now()
	_ = os.Chtimes(sentinelPath, now, now)
	return f.Close()
}
