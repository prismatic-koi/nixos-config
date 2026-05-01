package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// ── PIInvocation ─────────────────────────────────────────────────────────────

func TestPIInvocation_BasicFlags(t *testing.T) {
	cfg := Config{
		PIProvider:                "anthropic",
		PIModel:                   "anthropic/claude-opus-4",
		PIThinking:                "high",
		PISystemPromptSandboxPath: "/tmp/prism-system-prompt.md",
		PIExtensionSandboxDir:     "/etc/prism/pi-extensions",
	}
	args := PIInvocation(cfg)

	if args[0] != "pi" {
		t.Errorf("expected args[0] == 'pi', got %q", args[0])
	}
	for _, pair := range [][2]string{
		{"--provider", "anthropic"},
		{"--model", "anthropic/claude-opus-4"},
		{"--thinking", "high"},
		{"--append-system-prompt", "/tmp/prism-system-prompt.md"},
		{"--extension", "/etc/prism/pi-extensions/prism.ts"},
	} {
		flag, val := pair[0], pair[1]
		if !hasPair(args, flag, val) {
			t.Errorf("expected %q %q in args; got %v", flag, val, args)
		}
	}
	if !hasArg(args, "--no-session") {
		t.Error("expected --no-session in args")
	}
}

func TestPIInvocation_NoOptionalFlags(t *testing.T) {
	// When Provider, Model, Thinking are empty, those flags must be omitted.
	cfg := Config{
		PISystemPromptSandboxPath: "/tmp/prism-system-prompt.md",
		PIExtensionSandboxDir:     "/etc/prism/pi-extensions",
	}
	args := PIInvocation(cfg)

	for _, flag := range []string{"--provider", "--model", "--thinking"} {
		if hasArg(args, flag) {
			t.Errorf("expected %q to be absent; got %v", flag, args)
		}
	}
}

func TestPIInvocation_DefaultSandboxPaths(t *testing.T) {
	// When sandbox path overrides are empty the defaults must be used.
	cfg := Config{}
	args := PIInvocation(cfg)

	if !hasPair(args, "--append-system-prompt", piSystemPromptSandboxDefault) {
		t.Errorf("expected default system-prompt sandbox path %q; got %v", piSystemPromptSandboxDefault, args)
	}
	expectedExt := filepath.Join(piExtensionSandboxDefault, piExtensionFilename)
	if !hasPair(args, "--extension", expectedExt) {
		t.Errorf("expected default extension path %q; got %v", expectedExt, args)
	}
}

func TestPIInvocation_InitialPrompt(t *testing.T) {
	cfg := Config{InitialPrompt: "do the thing"}
	args := PIInvocation(cfg)

	if !hasPair(args, "--prompt", "do the thing") {
		t.Errorf("expected --prompt 'do the thing'; got %v", args)
	}
}

func TestPIInvocation_NoInitialPrompt(t *testing.T) {
	cfg := Config{}
	args := PIInvocation(cfg)

	if hasArg(args, "--prompt") {
		t.Errorf("expected --prompt to be absent; got %v", args)
	}
}

// hasPair returns true when flag and val appear consecutively in args.
func hasPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

// ── WriteSystemPromptFile ────────────────────────────────────────────────────

func TestWriteSystemPromptFile_WritesContent(t *testing.T) {
	// Prepare a source system-prompt file.
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "agent-instructions.md")
	content := "# System Prompt\nDo great things."
	if err := os.WriteFile(srcFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}

	slot := config.RoleSlot{
		Provider:         "anthropic",
		Model:            "anthropic/claude-opus-4",
		SystemPromptPath: srcFile,
	}

	// Override XDG_STATE_HOME so the temp file lands in t.TempDir().
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	hostPath, sandboxPath, err := WriteSystemPromptFile(slot, "test-session@main")
	if err != nil {
		t.Fatalf("WriteSystemPromptFile: %v", err)
	}

	// Sandbox path must be the default.
	if sandboxPath != piSystemPromptSandboxDefault {
		t.Errorf("sandboxPath = %q, want %q", sandboxPath, piSystemPromptSandboxDefault)
	}

	// Host path must exist and contain the source content.
	got, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatalf("read host path %q: %v", hostPath, err)
	}
	if string(got) != content {
		t.Errorf("file content = %q, want %q", string(got), content)
	}

	// Host path filename must be piSystemPromptFileName.
	if filepath.Base(hostPath) != piSystemPromptFileName {
		t.Errorf("base name = %q, want %q", filepath.Base(hostPath), piSystemPromptFileName)
	}
}

func TestWriteSystemPromptFile_EmptySystemPromptPath(t *testing.T) {
	slot := config.RoleSlot{}
	_, _, err := WriteSystemPromptFile(slot, "test-session@main")
	if err == nil {
		t.Fatal("expected error for empty SystemPromptPath, got nil")
	}
	if !strings.Contains(err.Error(), "systemPromptPath") {
		t.Errorf("error message does not mention systemPromptPath: %v", err)
	}
}

func TestWriteSystemPromptFile_MissingSourceFile(t *testing.T) {
	slot := config.RoleSlot{
		SystemPromptPath: "/nonexistent/path/agent.md",
	}
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	_, _, err := WriteSystemPromptFile(slot, "test-session@main")
	if err == nil {
		t.Fatal("expected error for missing source file, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error message does not mention the missing path: %v", err)
	}
}

// ── ValidatePIExtensionDir ───────────────────────────────────────────────────

func TestValidatePIExtensionDir_OK(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, piExtensionFilename), []byte("// ext"), 0o644); err != nil {
		t.Fatalf("write ext: %v", err)
	}
	if err := ValidatePIExtensionDir(dir); err != nil {
		t.Errorf("ValidatePIExtensionDir: unexpected error: %v", err)
	}
}

func TestValidatePIExtensionDir_EmptyDir(t *testing.T) {
	err := ValidatePIExtensionDir("")
	if err == nil {
		t.Fatal("expected error for empty dir, got nil")
	}
}

func TestValidatePIExtensionDir_MissingExtFile(t *testing.T) {
	dir := t.TempDir()
	err := ValidatePIExtensionDir(dir)
	if err == nil {
		t.Fatal("expected error for missing prism.ts, got nil")
	}
	if !strings.Contains(err.Error(), "prism.ts") {
		t.Errorf("error message does not mention prism.ts: %v", err)
	}
}
