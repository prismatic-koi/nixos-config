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
		PIBinaryPath:          "/nix/store/abc-pi/bin/pi",
		PIProvider:            "anthropic",
		PIModel:               "anthropic/claude-opus-4",
		PIThinking:            "high",
		PIExtensionSandboxDir: "/etc/prism/pi-extensions",
	}
	args := PIInvocation(cfg)

	if args[0] != "/nix/store/abc-pi/bin/pi" {
		t.Errorf("expected args[0] == '/nix/store/abc-pi/bin/pi', got %q", args[0])
	}
	for _, pair := range [][2]string{
		{"--provider", "anthropic"},
		{"--model", "anthropic/claude-opus-4"},
		{"--thinking", "high"},
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
	// --append-system-prompt must NOT appear — system prompt is delivered via
	// PI_CODING_AGENT_DIR / APPEND_SYSTEM.md, not via a CLI flag.
	if hasArg(args, "--append-system-prompt") {
		t.Errorf("--append-system-prompt must not appear in PIInvocation args; got %v", args)
	}
}

func TestPIInvocation_NoOptionalFlags(t *testing.T) {
	// When Provider, Model, Thinking are empty, those flags must be omitted.
	cfg := Config{
		PIExtensionSandboxDir: "/etc/prism/pi-extensions",
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

	// --append-system-prompt must be absent (system prompt via PI_CODING_AGENT_DIR).
	if hasArg(args, "--append-system-prompt") {
		t.Errorf("--append-system-prompt must not appear in PIInvocation args; got %v", args)
	}
	expectedExt := filepath.Join(piExtensionSandboxDefault, piExtensionFilename)
	if !hasPair(args, "--extension", expectedExt) {
		t.Errorf("expected default extension path %q; got %v", expectedExt, args)
	}
}

func TestPIInvocation_InitialPrompt(t *testing.T) {
	cfg := Config{InitialPrompt: "do the thing"}
	args := PIInvocation(cfg)

	// The prompt must appear as a bare positional argument (last element),
	// not as --prompt <text>.
	if hasArg(args, "--prompt") {
		t.Errorf("--prompt flag must not appear; pi takes the message as a positional arg; got %v", args)
	}
	if len(args) == 0 || args[len(args)-1] != "do the thing" {
		t.Errorf("expected 'do the thing' as last positional arg; got %v", args)
	}
}

func TestPIInvocation_NoInitialPrompt(t *testing.T) {
	cfg := Config{}
	args := PIInvocation(cfg)

	if hasArg(args, "--prompt") {
		t.Errorf("expected --prompt to be absent; got %v", args)
	}
	// When no prompt, the last arg must not be an empty string positional arg.
	if len(args) > 0 && args[len(args)-1] == "" {
		t.Errorf("expected no empty positional arg appended; got %v", args)
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

// hasTriple returns true when flag, val1, val2 appear consecutively in args.
func hasTriple(args []string, flag, val1, val2 string) bool {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == flag && args[i+1] == val1 && args[i+2] == val2 {
			return true
		}
	}
	return false
}

// ── StagePIAgentConfigDir ─────────────────────────────────────────────────────

func TestStagePIAgentConfigDir_WritesAppendSystem(t *testing.T) {
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

	// Override XDG_STATE_HOME so staging lands in t.TempDir().
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	hostDir, sandboxDir, err := StagePIAgentConfigDir(slot, "test-session@main")
	if err != nil {
		t.Fatalf("StagePIAgentConfigDir: %v", err)
	}

	// Sandbox dir must be the default.
	if sandboxDir != piAgentConfigSandboxDefault {
		t.Errorf("sandboxDir = %q, want %q", sandboxDir, piAgentConfigSandboxDefault)
	}

	// APPEND_SYSTEM.md must exist and contain the source content.
	appendSystemPath := filepath.Join(hostDir, piAppendSystemFilename)
	got, err := os.ReadFile(appendSystemPath)
	if err != nil {
		t.Fatalf("read APPEND_SYSTEM.md at %q: %v", appendSystemPath, err)
	}
	if string(got) != content {
		t.Errorf("APPEND_SYSTEM.md content = %q, want %q", string(got), content)
	}

	// hostDir must be a subdirectory named pi-agent under the run dir.
	if filepath.Base(hostDir) != piAgentConfigSubdir {
		t.Errorf("hostDir base = %q, want %q", filepath.Base(hostDir), piAgentConfigSubdir)
	}
}

func TestStagePIAgentConfigDir_EmptySystemPromptPath(t *testing.T) {
	// When SystemPromptPath is empty, staging dir is created but APPEND_SYSTEM.md
	// is omitted — no error (edge-case AC).
	slot := config.RoleSlot{}
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	hostDir, sandboxDir, err := StagePIAgentConfigDir(slot, "test-session@main")
	if err != nil {
		t.Fatalf("expected no error for empty SystemPromptPath; got %v", err)
	}
	if hostDir == "" || sandboxDir == "" {
		t.Fatalf("expected non-empty hostDir and sandboxDir; got %q, %q", hostDir, sandboxDir)
	}
	// Staging dir must exist.
	if _, statErr := os.Stat(hostDir); statErr != nil {
		t.Errorf("staging dir %q does not exist: %v", hostDir, statErr)
	}
	// APPEND_SYSTEM.md must NOT exist.
	appendSystemPath := filepath.Join(hostDir, piAppendSystemFilename)
	if _, statErr := os.Stat(appendSystemPath); statErr == nil {
		t.Errorf("APPEND_SYSTEM.md should not exist when SystemPromptPath is empty")
	}
}

func TestStagePIAgentConfigDir_MissingSourceFile(t *testing.T) {
	// When SystemPromptPath points to a missing file, staging dir is created
	// but APPEND_SYSTEM.md is omitted — no error (edge-case AC: non-fatal).
	slot := config.RoleSlot{
		SystemPromptPath: "/nonexistent/path/agent.md",
	}
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	hostDir, _, err := StagePIAgentConfigDir(slot, "test-session@main")
	if err != nil {
		t.Fatalf("expected no error for missing source file; got %v", err)
	}
	// Staging dir must still exist.
	if _, statErr := os.Stat(hostDir); statErr != nil {
		t.Errorf("staging dir %q does not exist: %v", hostDir, statErr)
	}
	// APPEND_SYSTEM.md must NOT exist.
	appendSystemPath := filepath.Join(hostDir, piAppendSystemFilename)
	if _, statErr := os.Stat(appendSystemPath); statErr == nil {
		t.Errorf("APPEND_SYSTEM.md should not exist when source file is missing")
	}
}

func TestStagePIAgentConfigDir_IsolatedPerSession(t *testing.T) {
	// Two concurrent spawns for different session names must use different
	// staging dirs — no cross-session contamination (edge-case AC).
	srcFile := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(srcFile, []byte("role prompt"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	slot := config.RoleSlot{SystemPromptPath: srcFile}
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	dir1, _, err1 := StagePIAgentConfigDir(slot, "nixos-config@feature-a")
	dir2, _, err2 := StagePIAgentConfigDir(slot, "nixos-config@feature-b")
	if err1 != nil || err2 != nil {
		t.Fatalf("StagePIAgentConfigDir errors: %v, %v", err1, err2)
	}
	if dir1 == dir2 {
		t.Errorf("expected different staging dirs for different sessions; both got %q", dir1)
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

func TestAppendPIBwrapMounts_EmptyPIBinaryPathReturnsError(t *testing.T) {
	// An empty PIBinaryPath must return a clear error, not silently fall back
	// to a bare name that would fail inside the bwrap sandbox with ENOENT.
	extDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(extDir, piExtensionFilename), []byte("// ext"), 0o644); err != nil {
		t.Fatalf("write ext: %v", err)
	}

	cfg := Config{
		PIBinaryPath:       "", // intentionally empty
		PIExtensionHostDir: extDir,
	}

	_, err := appendPIBwrapMounts(nil, cfg)
	if err == nil {
		t.Fatal("expected an error for empty PIBinaryPath, got nil")
	}
	if !strings.Contains(err.Error(), "PIBinaryPath") {
		t.Errorf("error should mention PIBinaryPath; got: %v", err)
	}
}

func TestAppendPIBwrapMounts_EmitsParentDirUnconditionally(t *testing.T) {
	// Regression test for the bug where --dir was skipped for /etc-prefixed
	// parent paths. /etc/prism does not exist on the host, so bwrap would
	// fail at runtime if --dir /etc/prism is omitted.
	extDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(extDir, piExtensionFilename), []byte("// ext"), 0o644); err != nil {
		t.Fatalf("write ext: %v", err)
	}
	agentConfigDir := t.TempDir()

	// Create a fake pi binary for the test.
	fakePI := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(fakePI, []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatalf("write fake pi binary: %v", err)
	}

	cfg := Config{
		PIBinaryPath:         fakePI,
		PIAgentConfigHostDir: agentConfigDir,
		PIExtensionHostDir:   extDir,
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

	// Must set PI_CODING_AGENT_DIR to the default sandbox agent config dir.
	if !hasPair(args, "--setenv", "PI_CODING_AGENT_DIR") {
		t.Errorf("expected --setenv PI_CODING_AGENT_DIR in args; got %v", args)
	}

	// Must ro-bind-mount the PI binary so it is accessible inside the sandbox.
	if !hasTriple(args, "--ro-bind", fakePI, fakePI) {
		t.Errorf("expected --ro-bind %q %q for PI binary in args; got %v", fakePI, fakePI, args)
	}
}

func TestAppendPIBwrapMounts_SetsAgentConfigDirEnv(t *testing.T) {
	extDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(extDir, piExtensionFilename), []byte("// ext"), 0o644); err != nil {
		t.Fatalf("write ext: %v", err)
	}
	agentConfigDir := t.TempDir()
	customSandboxDir := "/run/prism/custom-agent"

	fakePI := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(fakePI, []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatalf("write fake pi binary: %v", err)
	}

	cfg := Config{
		PIBinaryPath:            fakePI,
		PIAgentConfigHostDir:    agentConfigDir,
		PIAgentConfigSandboxDir: customSandboxDir,
		PIExtensionHostDir:      extDir,
	}

	args, err := appendPIBwrapMounts(nil, cfg)
	if err != nil {
		t.Fatalf("appendPIBwrapMounts: %v", err)
	}

	// PI_CODING_AGENT_DIR must be set to the custom sandbox dir.
	found := false
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--setenv" && args[i+1] == "PI_CODING_AGENT_DIR" && args[i+2] == customSandboxDir {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --setenv PI_CODING_AGENT_DIR %q in args; got %v", customSandboxDir, args)
	}

	// The agent config dir must be ro-bind-mounted.
	found = false
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" && args[i+1] == agentConfigDir && args[i+2] == customSandboxDir {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --ro-bind %q %q in args; got %v", agentConfigDir, customSandboxDir, args)
	}
}

func TestAppendPIBwrapMounts_NoPiAgentBindMount(t *testing.T) {
	// ~/.pi/agent is no longer bind-mounted — files are copied into the staging
	// dir by StagePIAgentConfigDir instead. Verify that even when ~/.pi/agent
	// exists, appendPIBwrapMounts does NOT add a --ro-bind for it.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	piAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(piAgentDir, 0o700); err != nil {
		t.Fatalf("mkdir ~/.pi/agent: %v", err)
	}

	extDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(extDir, piExtensionFilename), []byte("// ext"), 0o644); err != nil {
		t.Fatalf("write ext: %v", err)
	}
	fakePI := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(fakePI, []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatalf("write fake pi binary: %v", err)
	}

	cfg := Config{
		PIBinaryPath:       fakePI,
		PIExtensionHostDir: extDir,
	}

	args, err := appendPIBwrapMounts(nil, cfg)
	if err != nil {
		t.Fatalf("appendPIBwrapMounts: %v", err)
	}

	// Must NOT contain any --ro-bind involving ~/.pi/agent.
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" && args[i+1] == piAgentDir {
			t.Errorf("unexpected --ro-bind for ~/.pi/agent in args; got %v", args)
		}
	}
}

// ── StagePIAgentConfigDir: host file copies ──────────────────────────────────

func TestStagePIAgentConfigDir_CopiesAuthAndSettings(t *testing.T) {
	// When ~/.pi/agent/{auth,settings}.json exist, they must be copied into
	// the staging dir.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	piAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(piAgentDir, 0o700); err != nil {
		t.Fatalf("mkdir ~/.pi/agent: %v", err)
	}
	authContent := `{"token":"secret"}`
	settingsContent := `{"theme":"dark"}`
	if err := os.WriteFile(filepath.Join(piAgentDir, "auth.json"), []byte(authContent), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(piAgentDir, "settings.json"), []byte(settingsContent), 0o600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}

	hostDir, _, err := StagePIAgentConfigDir(config.RoleSlot{}, "test-session@copy-test")
	if err != nil {
		t.Fatalf("StagePIAgentConfigDir: %v", err)
	}

	for name, want := range map[string]string{
		"auth.json":     authContent,
		"settings.json": settingsContent,
	} {
		got, readErr := os.ReadFile(filepath.Join(hostDir, name))
		if readErr != nil {
			t.Errorf("read %s: %v", name, readErr)
			continue
		}
		if string(got) != want {
			t.Errorf("%s content = %q, want %q", name, string(got), want)
		}
	}
}

func TestStagePIAgentConfigDir_CopiesThemesDir(t *testing.T) {
	// When ~/.pi/agent/themes/ exists, its contents must be copied recursively
	// into the staging dir.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	themesDir := filepath.Join(fakeHome, ".pi", "agent", "themes")
	if err := os.MkdirAll(themesDir, 0o700); err != nil {
		t.Fatalf("mkdir themes: %v", err)
	}
	themeContent := `{"name":"cool"}`
	if err := os.WriteFile(filepath.Join(themesDir, "cool.json"), []byte(themeContent), 0o600); err != nil {
		t.Fatalf("write theme file: %v", err)
	}

	hostDir, _, err := StagePIAgentConfigDir(config.RoleSlot{}, "test-session@themes-test")
	if err != nil {
		t.Fatalf("StagePIAgentConfigDir: %v", err)
	}

	got, readErr := os.ReadFile(filepath.Join(hostDir, "themes", "cool.json"))
	if readErr != nil {
		t.Fatalf("read themes/cool.json: %v", readErr)
	}
	if string(got) != themeContent {
		t.Errorf("themes/cool.json content = %q, want %q", string(got), themeContent)
	}
}

func TestStagePIAgentConfigDir_MissingHostFiles(t *testing.T) {
	// When ~/.pi/agent does not exist at all, staging succeeds without error
	// and the optional files are simply absent from the staging dir.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// Deliberately do NOT create ~/.pi/agent.

	hostDir, _, err := StagePIAgentConfigDir(config.RoleSlot{}, "test-session@missing-pi")
	if err != nil {
		t.Fatalf("StagePIAgentConfigDir: %v", err)
	}

	for _, name := range []string{"auth.json", "settings.json"} {
		if _, statErr := os.Stat(filepath.Join(hostDir, name)); statErr == nil {
			t.Errorf("%s must not exist when source is absent", name)
		}
	}
	if _, statErr := os.Stat(filepath.Join(hostDir, "themes")); statErr == nil {
		t.Errorf("themes/ must not exist when source is absent")
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
