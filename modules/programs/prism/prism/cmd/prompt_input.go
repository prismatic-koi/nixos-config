package cmd

// resolvePromptInput — shared helper for reading a prompt from --prompt,
// --prompt-file, or stdin (when --prompt is "-").
//
// All three commands (prompt, spawn, pr) call this to avoid duplicating the
// logic for the three input sources.

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// addPromptFlags registers --prompt and --prompt-file on cmd, with the
// standard help text that documents the shell-escaping convention.
// Commands where a prompt is mandatory should call requirePromptInput in their
// RunE; optional-prompt commands call resolvePrompt directly.
func addPromptFlags(cmd *cobra.Command) {
	cmd.Flags().String(
		"prompt", "",
		"Text to send to the agent.\n"+
			"    Wrap values containing shell metacharacters in single quotes:\n"+
			"      --prompt 'run `gh pr view 42` and review the diff'\n"+
			"    Use --prompt - to read from stdin (safe for any content):\n"+
			"      echo 'my prompt' | prism ... --prompt -\n"+
			"    Or use --prompt-file to read from a file (safest for complex prompts).",
	)
	cmd.Flags().String(
		"prompt-file", "",
		"Path to a file containing the prompt text.\n"+
			"    Mutually exclusive with --prompt. Newlines are preserved.",
	)
}

// requirePromptInput is used by commands where a prompt is mandatory (prism
// prompt). It calls resolvePrompt and then errors if the result is empty.
func requirePromptInput(cmd *cobra.Command) (string, error) {
	text, err := resolvePrompt(cmd)
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", fmt.Errorf(
			"a prompt is required — supply one of:\n" +
				"  --prompt <text>\n" +
				"  --prompt - (read from stdin)\n" +
				"  --prompt-file <path>",
		)
	}
	return text, nil
}

// resolvePrompt reads the prompt text from whichever source was provided:
//   - --prompt-file <path>  → read file contents
//   - --prompt -            → read from stdin
//   - --prompt <text>       → use text directly
//
// Returns an error if both --prompt and --prompt-file are specified, or if
// reading the file/stdin fails.
func resolvePrompt(cmd *cobra.Command) (string, error) {
	promptFile, _ := cmd.Flags().GetString("prompt-file")
	promptText, _ := cmd.Flags().GetString("prompt")

	if promptFile != "" && promptText != "" {
		return "", fmt.Errorf("--prompt and --prompt-file are mutually exclusive")
	}

	if promptFile != "" {
		data, err := os.ReadFile(promptFile)
		if err != nil {
			return "", fmt.Errorf("read prompt file %q: %w", promptFile, err)
		}
		return strings.TrimRight(string(data), "\n"), nil
	}

	if promptText == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read prompt from stdin: %w", err)
		}
		return strings.TrimRight(string(data), "\n"), nil
	}

	return promptText, nil
}
