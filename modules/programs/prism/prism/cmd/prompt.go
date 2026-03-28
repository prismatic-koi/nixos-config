package cmd

// prism prompt — send a follow-up prompt to a running or finished agent session.
//
// Usage:
//
//	prism prompt <session> --prompt <text>

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
	promptCmd.Flags().String("prompt", "", "Text to send to the agent (required)")
	_ = promptCmd.MarkFlagRequired("prompt")
	rootCmd.AddCommand(promptCmd)
}

func runPrompt(cmd *cobra.Command, args []string) error {
	session := args[0]
	promptText, _ := cmd.Flags().GetString("prompt")

	if !tmux.HasSession(session) {
		return fmt.Errorf("session %q not found\nrun `prism list-sessions` to see available sessions", session)
	}

	target := session + ":agent"

	// Verify the agent window exists by attempting a dry capture.
	if _, err := tmux.CapturePaneScreen(session); err != nil {
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
