package main

// prompt_test.go — unit tests for `iris prompt` flag parsing and the
// resolveIrisPromptInput helper. Wire-level integration is covered in
// prompt_integration_test.go.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// withFakeStdin replaces os.Stdin with a pipe whose read end is preloaded
// with `body`. It restores the original stdin on test cleanup.
func withFakeStdin(t *testing.T, body string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		_ = r.Close()
		_ = w.Close()
		t.Fatalf("write stdin: %v", err)
	}
	if err := w.Close(); err != nil {
		_ = r.Close()
		t.Fatalf("close stdin writer: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		_ = r.Close()
	})
}

// newPromptCmdForTest builds a fresh cobra.Command with the same flag set as
// promptCmd, so each test gets a clean flag state without depending on
// process-global state.
func newPromptCmdForTest() *cobra.Command {
	c := &cobra.Command{
		Use:  "prompt <session>",
		Args: cobra.ExactArgs(1),
		// No RunE — the tests inspect flags/resolveIrisPromptInput directly.
	}
	c.Flags().String("prompt", "", "")
	c.Flags().String("prompt-file", "", "")
	c.Flags().String("socket", "", "")
	c.MarkFlagsMutuallyExclusive("prompt", "prompt-file")
	return c
}

func TestResolveIrisPromptInput_InlinePrompt(t *testing.T) {
	cmd := newPromptCmdForTest()
	if err := cmd.ParseFlags([]string{"--prompt", "hello world"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got, err := resolveIrisPromptInput(cmd)
	if err != nil {
		t.Fatalf("resolveIrisPromptInput: %v", err)
	}
	if got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestResolveIrisPromptInput_PromptFile_TrimsSingleTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.txt")
	// One trailing newline — should be stripped to match prism's behaviour.
	if err := os.WriteFile(path, []byte("multi\nline body\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := newPromptCmdForTest()
	if err := cmd.ParseFlags([]string{"--prompt-file", path}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got, err := resolveIrisPromptInput(cmd)
	if err != nil {
		t.Fatalf("resolveIrisPromptInput: %v", err)
	}
	const want = "multi\nline body"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveIrisPromptInput_PromptFile_PreservesInternalAndExtraTrailingNewlines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.txt")
	// Two trailing newlines — only ONE is stripped (matches prism: a single
	// editor-inserted trailing newline is removed; deliberate blank lines
	// at the end of a body are preserved).
	if err := os.WriteFile(path, []byte("body\n\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := newPromptCmdForTest()
	if err := cmd.ParseFlags([]string{"--prompt-file", path}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got, err := resolveIrisPromptInput(cmd)
	if err != nil {
		t.Fatalf("resolveIrisPromptInput: %v", err)
	}
	if got != "body\n" {
		t.Errorf("got %q, want %q", got, "body\n")
	}
}

func TestResolveIrisPromptInput_PromptFile_NotFound(t *testing.T) {
	cmd := newPromptCmdForTest()
	if err := cmd.ParseFlags([]string{"--prompt-file", "/no/such/file/iris-prompt-test"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	_, err := resolveIrisPromptInput(cmd)
	if err == nil {
		t.Fatalf("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "read prompt file") {
		t.Errorf("error missing 'read prompt file' wording: %q", err.Error())
	}
}

func TestResolveIrisPromptInput_Stdin(t *testing.T) {
	withFakeStdin(t, "from stdin\n")
	cmd := newPromptCmdForTest()
	if err := cmd.ParseFlags([]string{"--prompt", "-"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got, err := resolveIrisPromptInput(cmd)
	if err != nil {
		t.Fatalf("resolveIrisPromptInput: %v", err)
	}
	if got != "from stdin" {
		t.Errorf("got %q, want %q", got, "from stdin")
	}
}

func TestResolveIrisPromptInput_NoFlags_ReturnsEmpty(t *testing.T) {
	cmd := newPromptCmdForTest()
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got, err := resolveIrisPromptInput(cmd)
	if err != nil {
		t.Fatalf("resolveIrisPromptInput: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestPromptCmd_MutualExclusion confirms that cobra enforces the mutual
// exclusion of --prompt and --prompt-file. We build a synthetic root that
// hosts an exact-copy `prompt` subcommand (same MarkFlagsMutuallyExclusive
// call) so we can drive Execute() without booting the real rootCmd's
// daemon startup path.
func TestPromptCmd_MutualExclusion(t *testing.T) {
	root := &cobra.Command{Use: "iris", SilenceUsage: true, SilenceErrors: true}
	sub := &cobra.Command{
		Use:           "prompt <session>",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(*cobra.Command, []string) error { return nil },
	}
	sub.Flags().String("prompt", "", "")
	sub.Flags().String("prompt-file", "", "")
	sub.MarkFlagsMutuallyExclusive("prompt", "prompt-file")
	root.AddCommand(sub)

	var stderr bytes.Buffer
	root.SetErr(&stderr)
	root.SetOut(&stderr)
	root.SetArgs([]string{"prompt", "some-session", "--prompt", "x", "--prompt-file", "/tmp/y"})

	err := root.Execute()
	if err == nil {
		t.Fatalf("expected mutual-exclusion error, got nil (stderr=%q)", stderr.String())
	}
	// Cobra's actual wording is: "if any flags in the group [prompt prompt-file]
	// are set none of the others can be". We accept either that or the
	// shorthand "exclusive" wording in case cobra revises the phrasing.
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "exclusive") &&
		!(strings.Contains(low, "prompt-file") && strings.Contains(low, "none of the others")) {
		t.Errorf("error did not mention mutual exclusion: %q", err.Error())
	}
}
