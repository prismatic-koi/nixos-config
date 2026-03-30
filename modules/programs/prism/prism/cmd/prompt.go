package cmd

// prism prompt — send a follow-up prompt to a running or finished agent session.
//
// Usage:
//
//	prism prompt <session> --prompt <text>
//	prism prompt <session> --prompt - < /tmp/prompt.txt
//	prism prompt <session> --prompt-file /tmp/prompt.txt

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/tmux"
)

var promptCmd = &cobra.Command{
	Use:   "prompt <session>",
	Short: "Send a follow-up prompt to a running agent session",
	Long: `Send a follow-up message to the opencode agent running in the named tmux
session. The session must already exist and have an agent window.

The prompt is delivered after a short delay (500 ms) to ensure opencode is
idle and accepting input before the keys are sent.`,
	Args: cobra.ExactArgs(1),
	RunE: runPrompt,
}

func init() {
	addPromptFlags(promptCmd)
	rootCmd.AddCommand(promptCmd)
}

func runPrompt(cmd *cobra.Command, args []string) error {
	session := args[0]

	promptText, err := requirePromptInput(cmd)
	if err != nil {
		return err
	}

	if !tmux.HasSession(session) {
		return fmt.Errorf("session %q not found\nrun `prism list-sessions` to see available sessions", session)
	}

	// Refuse to inject a prompt into a session that is waiting for the user.
	// This check is done before CapturePaneScreen (which blocks for ~500 ms)
	// so we fail fast without doing any expensive work.
	// Sending keys while opencode is in waiting state would corrupt the input
	// field rather than delivering a proper follow-up message.
	if state := tmux.AgentStateOf(session); state == "waiting" {
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

	target := session + ":agent"

	// Verify the agent window exists by attempting a dry capture.
	if _, err := tmux.CapturePaneScreen(session, tmux.DefaultCaptureHeight); err != nil {
		return fmt.Errorf("session %q has no agent window: %w", session, err)
	}

	// Send the prompt with a short delay — opencode is already running, we
	// just need a moment for it to finish any current operation before the
	// keys arrive.
	const delayMs = 500
	if err := tmux.SendKeysDelayed(target, promptText, delayMs); err != nil {
		return fmt.Errorf("send prompt to %s: %w", session, err)
	}

	fmt.Printf("prompt sent to %s\n", session)
	return nil
}
