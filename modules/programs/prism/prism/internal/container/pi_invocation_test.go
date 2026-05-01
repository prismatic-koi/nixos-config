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

// ── appendPIBwrapMounts ──────────────────────────────────────────────────────

func TestAppendPIBwrapMounts_EmitsParentDirUnconditionally(t *testing.T) {
	// Regression test for the bug where --dir was skipped for /etc-prefixed
	// parent paths. /etc/prism does not exist on the host, so bwrap would
	// fail at runtime if --dir /etc/prism is omitted.
	extDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(extDir, piExtensionFilename), []byte("// ext"), 0o644); err != nil {
		t.Fatalf("write ext: %v", err)
	}
	promptFile := filepath.Join(t.TempDir(), "system-prompt.md")
	if err := os.WriteFile(promptFile, []byte("# prompt"), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	cfg := Config{
		PISystemPromptHostPath: promptFile,
		PIExtensionHostDir:     extDir,
		// Use default sandbox paths so /etc/prism/pi-extensions is the target.
	}

	args, err := appendPIBwrapMounts(nil, cfg)
	if err != nil {
		t.Fatalf("appendPIBwrapMounts: %v", err)
	}

	// Must contain --dir /etc/prism (the parent of the default sandbox ext dir).
	expectedParent := filepath.Dir(piExtensionSandboxDefault) // /etc/prism
	if !hasPair(args, "--dir", expectedParent) {
		t.Errorf("expected --dir %q in args (parent dir); got %v", expectedParent, args)
	}
	// Must also contain --dir /etc/prism/pi-extensions (the target dir itself).
	if !hasPair(args, "--dir", piExtensionSandboxDefault) {
		t.Errorf("expected --dir %q in args (target dir); got %v", piExtensionSandboxDefault, args)
	}
}

// ── piHarnessPipePath ────────────────────────────────────────────────────────

func TestPIHarnessPipePath_UsesXDGStateHome(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	path := piHarnessPipePath("test-session@main")

	if !strings.HasSuffix(path, "pipe.sock") {
		t.Errorf("expected path to end with pipe.sock, got %q", path)
	}
	if !strings.HasPrefix(path, stateHome) {
		t.Errorf("expected path to start with %q, got %q", stateHome, path)
	}
	// Must contain the prism/run/<hash>/ component.
	if !strings.Contains(path, "prism/run/") {
		t.Errorf("expected prism/run/ in path, got %q", path)
	}
}

func TestPIHarnessPipePath_DeterministicForSameName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p1 := piHarnessPipePath("nixos-config@feature")
	p2 := piHarnessPipePath("nixos-config@feature")
	if p1 != p2 {
		t.Errorf("expected same path for same session name, got %q and %q", p1, p2)
	}
}

func TestPIHarnessPipePath_DifferentForDifferentNames(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p1 := piHarnessPipePath("nixos-config@feature-a")
	p2 := piHarnessPipePath("nixos-config@feature-b")
	if p1 == p2 {
		t.Errorf("expected different paths for different session names, got same: %q", p1)
	}
}
