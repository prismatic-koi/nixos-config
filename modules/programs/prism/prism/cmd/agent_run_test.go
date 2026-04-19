package cmd

// Unit tests for the PRISM_INITIAL_PROMPT env-var read in agent_run.go.
//
// These tests verify that applyInitialPromptEnvVar reads the env var and sets
// InitialPrompt on a container.Config correctly, without needing a real DB
// connection, bwrap binary, or syscall.Exec path.

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// TestApplyInitialPromptEnvVar_Set verifies that when PRISM_INITIAL_PROMPT is
// set in the environment, applyInitialPromptEnvVar assigns it to InitialPrompt.
func TestApplyInitialPromptEnvVar_Set(t *testing.T) {
	t.Setenv("PRISM_INITIAL_PROMPT", "foo")

	cfg := container.Config{}
	applyInitialPromptEnvVar(&cfg)

	if cfg.InitialPrompt != "foo" {
		t.Errorf("InitialPrompt = %q, want %q", cfg.InitialPrompt, "foo")
	}
}

// TestApplyInitialPromptEnvVar_Unset verifies that when PRISM_INITIAL_PROMPT
// is not set (empty string), InitialPrompt remains empty.
func TestApplyInitialPromptEnvVar_Unset(t *testing.T) {
	// Ensure the env var is absent/empty.
	t.Setenv("PRISM_INITIAL_PROMPT", "")

	cfg := container.Config{}
	applyInitialPromptEnvVar(&cfg)

	if cfg.InitialPrompt != "" {
		t.Errorf("InitialPrompt = %q, want empty string when env var is unset", cfg.InitialPrompt)
	}
}

// TestApplyInitialPromptEnvVar_SpecialChars verifies that prompts containing
// special characters (newlines, quotes, backticks, equals signs) are read
// verbatim — no shell interpretation occurs in the env-var pipeline.
func TestApplyInitialPromptEnvVar_SpecialChars(t *testing.T) {
	prompt := "line1\nline2 'single' \"double\" `backtick` KEY=value is part of the message"
	t.Setenv("PRISM_INITIAL_PROMPT", prompt)

	cfg := container.Config{}
	applyInitialPromptEnvVar(&cfg)

	if cfg.InitialPrompt != prompt {
		t.Errorf("InitialPrompt = %q, want %q", cfg.InitialPrompt, prompt)
	}
}
