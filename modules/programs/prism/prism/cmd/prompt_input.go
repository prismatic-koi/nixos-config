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
// The two flags are declared mutually exclusive via cobra.
// Commands where a prompt is mandatory should call requirePromptInput in their
// RunE; optional-prompt commands call resolvePrompt directly.
func addPromptFlags(cmd *cobra.Command) {
	cmd.Flags().String(
		"prompt", "",
		"Text to send to the agent. Use --prompt-file for complex strings or to avoid shell-escaping issues.",
	)
	cmd.Flags().String(
		"prompt-file", "",
		"Path to a file containing the prompt text. Mutually exclusive with --prompt.",
	)
	cmd.MarkFlagsMutuallyExclusive("prompt", "prompt-file")
}

// requirePromptInput is used by commands where a prompt is mandatory (prism
// prompt). It calls resolvePromptWithSource and then errors if the result is empty.
func requirePromptInput(cmd *cobra.Command) (string, error) {
	text, _, err := resolvePromptWithSource(cmd)
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

// resolvePrompt reads the prompt text from whichever source was provided.
// It is a thin wrapper around resolvePromptWithSource for callers that do not
// need the source discriminator.
func resolvePrompt(cmd *cobra.Command) (string, error) {
	text, _, err := resolvePromptWithSource(cmd)
	return text, err
}

// resolvePromptWithSource reads the prompt text from whichever source was
// provided and also returns the prompt_source discriminator for C.4.SRC:
//
//   - --prompt-file <path>  → (text, "cli-positional", nil)
//   - --prompt -            → (text, "cli-stdin", nil)
//   - --prompt <text>       → (text, "cli-positional", nil)
//   - no prompt flag        → ("", "", nil)
//
// Note: cobra enforces mutual exclusion of --prompt and --prompt-file before
// RunE is called, so no manual check is needed here.
// A single trailing newline is stripped from file/stdin input to match the
// behaviour of most Unix text tools (editors append a final newline that is
// not part of the intended content).
func resolvePromptWithSource(cmd *cobra.Command) (text, source string, err error) {
	promptFile, _ := cmd.Flags().GetString("prompt-file")
	promptText, _ := cmd.Flags().GetString("prompt")

	if promptFile != "" {
		data, readErr := os.ReadFile(promptFile)
		if readErr != nil {
			return "", "", fmt.Errorf("read prompt file %q: %w", promptFile, readErr)
		}
		return strings.TrimSuffix(string(data), "\n"), "cli-positional", nil
	}

	if promptText == "-" {
		data, readErr := io.ReadAll(os.Stdin)
		if readErr != nil {
			return "", "", fmt.Errorf("read prompt from stdin: %w", readErr)
		}
		return strings.TrimSuffix(string(data), "\n"), "cli-stdin", nil
	}

	if promptText != "" {
		return promptText, "cli-positional", nil
	}

	return "", "", nil
}
