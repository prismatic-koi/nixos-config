package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	// --no-session must NOT appear — PI must use its native OAuth session
	// persistence so that spawned sessions can authenticate with Anthropic.
	if hasArg(args, "--no-session") {
		t.Errorf("--no-session must not appear in PIInvocation args; got %v", args)
	}
	// --append-system-prompt must NOT appear — the role system prompt is
	// injected at runtime by the prism PI extension (before_agent_start), not
	// via a CLI flag or a staged file (design #2031).
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

// TestPIInvocation_OffThinkingPassedThrough verifies that thinking="off"
// (the zero value in profiles after #1299) is passed as --thinking off to
// PI — not translated or dropped.
func TestPIInvocation_OffThinkingPassedThrough(t *testing.T) {
	cfg := Config{
		PIThinking:            "off",
		PIExtensionSandboxDir: "/etc/prism/pi-extensions",
	}
	args := PIInvocation(cfg)
	if !hasPair(args, "--thinking", "off") {
		t.Errorf("expected --thinking off in PI invocation args; got %v", args)
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

// ── EnsurePIAgentConfigDir (post #2034 shared-mount layout) ─────────────────

func TestEnsurePIAgentConfigDir_ReturnsSharedHostAndCanonicalSandboxPath(t *testing.T) {
	// Design #2031 PR3 (#2034): the host dir is the user's ~/.pi/agent (shared
	// across all sessions), and the sandbox dir is the canonical default
	// /run/prism/pi-agent. EnsurePIAgentConfigDir must return exactly that.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	hostDir, sandboxDir, err := EnsurePIAgentConfigDir()
	if err != nil {
		t.Fatalf("EnsurePIAgentConfigDir: %v", err)
	}

	wantHost := filepath.Join(fakeHome, ".pi", "agent")
	if hostDir != wantHost {
		t.Errorf("hostDir = %q, want %q", hostDir, wantHost)
	}
	if sandboxDir != piAgentConfigSandboxDefault {
		t.Errorf("sandboxDir = %q, want %q", sandboxDir, piAgentConfigSandboxDefault)
	}

	// The host dir must exist after the call (created on a fresh install).
	if info, statErr := os.Stat(hostDir); statErr != nil {
		t.Errorf("shared agent dir %q does not exist after EnsurePIAgentConfigDir: %v", hostDir, statErr)
	} else if !info.IsDir() {
		t.Errorf("shared agent dir %q is not a directory", hostDir)
	}
}

func TestEnsurePIAgentConfigDir_SharedAcrossSessions(t *testing.T) {
	// AC: a single shared mount of ~/.pi/agent is used for every session/role.
	// Two calls must return the same hostDir (no per-session divergence).
	t.Setenv("HOME", t.TempDir())

	host1, _, err1 := EnsurePIAgentConfigDir()
	host2, _, err2 := EnsurePIAgentConfigDir()
	if err1 != nil || err2 != nil {
		t.Fatalf("EnsurePIAgentConfigDir errors: %v, %v", err1, err2)
	}
	if host1 != host2 {
		t.Errorf("expected same hostDir for repeated calls; got %q and %q", host1, host2)
	}
}

func TestEnsurePIAgentConfigDir_IdempotentWhenDirExists(t *testing.T) {
	// Pre-existing ~/.pi/agent must not be re-created or modified; the call
	// must succeed without error.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	preExisting := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(preExisting, 0o700); err != nil {
		t.Fatalf("prep: mkdir ~/.pi/agent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(preExisting, "auth.json"), []byte(`{"token":"keep-me"}`), 0o600); err != nil {
		t.Fatalf("prep: write auth.json: %v", err)
	}

	hostDir, _, err := EnsurePIAgentConfigDir()
	if err != nil {
		t.Fatalf("EnsurePIAgentConfigDir: %v", err)
	}
	if hostDir != preExisting {
		t.Errorf("hostDir = %q, want %q", hostDir, preExisting)
	}

	// auth.json content must be untouched.
	got, err := os.ReadFile(filepath.Join(preExisting, "auth.json"))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if string(got) != `{"token":"keep-me"}` {
		t.Errorf("auth.json modified by EnsurePIAgentConfigDir: got %q", string(got))
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

	// Post #2034: the agent config dir must be RW-bind-mounted (--bind, not
	// --ro-bind) at the sandbox path so OAuth proper-lockfile mkdir of
	// auth.json.lock on the parent dir succeeds. See pi_invocation.go
	// top-of-file doc for the full rationale.
	found = false
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && args[i+1] == agentConfigDir && args[i+2] == customSandboxDir {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --bind %q %q in args; got %v", agentConfigDir, customSandboxDir, args)
	}
	// The parent mount must NOT be --ro-bind — OAuth refresh would EPERM.
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" && args[i+1] == agentConfigDir && args[i+2] == customSandboxDir {
			t.Errorf("parent agent config dir mount must be --bind (RW) not --ro-bind; got %v", args)
		}
	}
}

func TestAppendPIBwrapMounts_NoPIAuthJSONEnvVar(t *testing.T) {
	// PI_AUTH_JSON does not exist in the pi source — appendPIBwrapMounts must
	// never emit --setenv PI_AUTH_JSON regardless of configuration.
	extDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(extDir, piExtensionFilename), []byte("// ext"), 0o644); err != nil {
		t.Fatalf("write ext: %v", err)
	}
	agentConfigDir := t.TempDir()
	customSandboxDir := "/run/prism/pi-agent"

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

	// PI_AUTH_JSON must NEVER be set — it is a fictional env var.
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--setenv" && args[i+1] == "PI_AUTH_JSON" {
			t.Errorf("PI_AUTH_JSON must never be set (fictional env var); got %v", args)
		}
	}
}

func TestAppendPIBwrapMounts_BindsAuthJSONWhenExists(t *testing.T) {
	// When ~/.pi/agent/auth.json exists on the host, appendPIBwrapMounts must
	// emit --bind <authPath> <authPath> (read-write) so that OAuth token
	// refreshes inside the bwrap sandbox are persisted back to the host file.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	piAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(piAgentDir, 0o700); err != nil {
		t.Fatalf("mkdir ~/.pi/agent: %v", err)
	}
	authPath := filepath.Join(piAgentDir, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
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

	// Must contain --bind authPath authPath (read-write, not --ro-bind).
	if !hasTriple(args, "--bind", authPath, authPath) {
		t.Errorf("expected --bind %q %q in args; got %v", authPath, authPath, args)
	}
	// Must NOT use --ro-bind for auth.json (OAuth refreshes need write access).
	if hasTriple(args, "--ro-bind", authPath, authPath) {
		t.Errorf("--ro-bind must not be used for auth.json (need write access for token refresh); got %v", args)
	}
}

func TestAppendPIBwrapMounts_NoBindWhenAuthJSONAbsent(t *testing.T) {
	// When ~/.pi/agent/auth.json does not exist on the host, appendPIBwrapMounts
	// must NOT add any bind mount for it — pi prompts for login instead.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// Deliberately do NOT create ~/.pi/agent/auth.json.

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

	// Must NOT contain any bind mount for a .pi/agent/auth.json path.
	piAuthSuffix := filepath.Join(".pi", "agent", "auth.json")
	for i := 0; i+2 < len(args); i++ {
		if (args[i] == "--bind" || args[i] == "--ro-bind") &&
			strings.HasSuffix(args[i+1], piAuthSuffix) {
			t.Errorf("auth.json bind mount must be absent when file does not exist; got %v", args)
		}
	}
}

func TestAppendPIBwrapMounts_BindsAtlassianOAuthWhenExists(t *testing.T) {
	// When ~/.pi/agent/atlassian-mcp-oauth.json already exists on the host,
	// appendPIBwrapMounts must emit --bind <path> <path> (read-write) so that
	// OAuth token refreshes inside the bwrap sandbox are persisted to the host.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	piAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(piAgentDir, 0o700); err != nil {
		t.Fatalf("mkdir ~/.pi/agent: %v", err)
	}
	atlasPath := filepath.Join(piAgentDir, "atlassian-mcp-oauth.json")
	if err := os.WriteFile(atlasPath, []byte(`{"accessToken":"tok"}`), 0o600); err != nil {
		t.Fatalf("write atlassian-mcp-oauth.json: %v", err)
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

	// Must contain --bind atlasPath atlasPath (read-write, not --ro-bind).
	if !hasTriple(args, "--bind", atlasPath, atlasPath) {
		t.Errorf("expected --bind %q %q in args; got %v", atlasPath, atlasPath, args)
	}
	// Must NOT use --ro-bind (OAuth refreshes need write access).
	if hasTriple(args, "--ro-bind", atlasPath, atlasPath) {
		t.Errorf("--ro-bind must not be used for atlassian-mcp-oauth.json; got %v", args)
	}
}

func TestAppendPIBwrapMounts_CreatesAndBindsAtlassianOAuthWhenAbsent(t *testing.T) {
	// When ~/.pi/agent/atlassian-mcp-oauth.json does not exist, appendPIBwrapMounts
	// must create an empty placeholder file (so bwrap can bind-mount it) and
	// then emit --bind <path> <path>. This ensures that /login-atlassian inside
	// the bwrap session writes tokens to the host path rather than the ephemeral
	// sandbox home.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	piAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(piAgentDir, 0o700); err != nil {
		t.Fatalf("mkdir ~/.pi/agent: %v", err)
	}
	// Deliberately do NOT create atlassian-mcp-oauth.json.

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

	atlasPath := filepath.Join(piAgentDir, "atlassian-mcp-oauth.json")

	// The placeholder file must have been created on the host.
	if _, statErr := os.Stat(atlasPath); statErr != nil {
		t.Errorf("expected placeholder atlassian-mcp-oauth.json to be created on host; got stat error: %v", statErr)
	}

	// Must contain --bind atlasPath atlasPath.
	if !hasTriple(args, "--bind", atlasPath, atlasPath) {
		t.Errorf("expected --bind %q %q in args after placeholder creation; got %v", atlasPath, atlasPath, args)
	}
}

func TestAppendPIBwrapMounts_SharedPiAgentRwBindAtSandboxPath(t *testing.T) {
	// Design #2031 PR3 (#2034): the shared host ~/.pi/agent directory must be
	// bind-mounted READ-WRITE into the sandbox at the canonical path
	// /run/prism/pi-agent (or the configured PIAgentConfigSandboxDir). RW is
	// required because pi-coding-agent's proper-lockfile auth.json refresh
	// mkdir's auth.json.lock on the PARENT directory — see pi_invocation.go
	// top-of-file doc for the full rationale. [security] AC.
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
		PIBinaryPath:         fakePI,
		PIAgentConfigHostDir: piAgentDir,
		PIExtensionHostDir:   extDir,
		// Default sandbox dir (piAgentConfigSandboxDefault).
	}

	args, err := appendPIBwrapMounts(nil, cfg)
	if err != nil {
		t.Fatalf("appendPIBwrapMounts: %v", err)
	}

	// Must contain --bind <host ~/.pi/agent> /run/prism/pi-agent (RW).
	if !hasTriple(args, "--bind", piAgentDir, piAgentConfigSandboxDefault) {
		t.Errorf("expected --bind %q %q (RW shared mount) in args; got %v",
			piAgentDir, piAgentConfigSandboxDefault, args)
	}
	// Must NOT use --ro-bind for the parent mount — proper-lockfile mkdir
	// needs write access to the parent dir for OAuth refresh to succeed.
	if hasTriple(args, "--ro-bind", piAgentDir, piAgentConfigSandboxDefault) {
		t.Errorf("parent ~/.pi/agent mount must be --bind (RW) not --ro-bind "+
			"— OAuth proper-lockfile needs to mkdir auth.json.lock in the parent dir; got %v", args)
	}
	// PI_CODING_AGENT_DIR must point at the canonical sandbox path.
	if !hasTriple(args, "--setenv", "PI_CODING_AGENT_DIR", piAgentConfigSandboxDefault) {
		t.Errorf("expected --setenv PI_CODING_AGENT_DIR %q; got %v",
			piAgentConfigSandboxDefault, args)
	}
}

func TestAppendPIBwrapMounts_AuthJSONReachableRwViaSharedMount(t *testing.T) {
	// Post #2034: the parent mount of ~/.pi/agent is RW, so auth.json is
	// automatically writable at $PI_CODING_AGENT_DIR/auth.json without a
	// dedicated overlay bind — the host file IS the file backing that
	// in-sandbox path. The host-path RW bind is retained so $HOME-relative
	// access works too. [security] AC.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	piAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(piAgentDir, 0o700); err != nil {
		t.Fatalf("mkdir ~/.pi/agent: %v", err)
	}
	authPath := filepath.Join(piAgentDir, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
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
		PIBinaryPath:         fakePI,
		PIAgentConfigHostDir: piAgentDir,
		PIExtensionHostDir:   extDir,
	}

	args, err := appendPIBwrapMounts(nil, cfg)
	if err != nil {
		t.Fatalf("appendPIBwrapMounts: %v", err)
	}

	// Parent mount must be RW so writes to $PI_CODING_AGENT_DIR/auth.json
	// (the path pi actually reads via PI_CODING_AGENT_DIR) reach the host.
	// This is the primary write-through mechanism for OAuth token refresh.
	if !hasTriple(args, "--bind", piAgentDir, piAgentConfigSandboxDefault) {
		t.Errorf("expected RW --bind of parent ~/.pi/agent at sandbox path; got %v", args)
	}
	// Host-path RW bind retained for $HOME-relative access.
	if !hasTriple(args, "--bind", authPath, authPath) {
		t.Errorf("expected --bind %q %q (host-path access) in args; got %v", authPath, authPath, args)
	}
	// Must NOT use --ro-bind for auth.json or its parent.
	if hasTriple(args, "--ro-bind", authPath, authPath) ||
		hasTriple(args, "--ro-bind", piAgentDir, piAgentConfigSandboxDefault) {
		t.Errorf("auth.json / parent dir must not be --ro-bind; got %v", args)
	}
}

func TestAppendPIBwrapMounts_AtlassianOAuthReachableRwViaSharedMount(t *testing.T) {
	// Same as auth.json: the RW parent mount makes atlassian-mcp-oauth.json
	// writable at $PI_CODING_AGENT_DIR/atlassian-mcp-oauth.json. Host-path
	// bind retained.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	piAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(piAgentDir, 0o700); err != nil {
		t.Fatalf("mkdir ~/.pi/agent: %v", err)
	}
	atlasPath := filepath.Join(piAgentDir, "atlassian-mcp-oauth.json")
	if err := os.WriteFile(atlasPath, []byte(`{"accessToken":"tok"}`), 0o600); err != nil {
		t.Fatalf("write atlassian-mcp-oauth.json: %v", err)
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
		PIBinaryPath:         fakePI,
		PIAgentConfigHostDir: piAgentDir,
		PIExtensionHostDir:   extDir,
	}

	args, err := appendPIBwrapMounts(nil, cfg)
	if err != nil {
		t.Fatalf("appendPIBwrapMounts: %v", err)
	}

	// Parent is RW.
	if !hasTriple(args, "--bind", piAgentDir, piAgentConfigSandboxDefault) {
		t.Errorf("expected RW --bind of parent ~/.pi/agent at sandbox path; got %v", args)
	}
	// Host-path bind retained.
	if !hasTriple(args, "--bind", atlasPath, atlasPath) {
		t.Errorf("expected --bind %q %q (host-path access) in args; got %v", atlasPath, atlasPath, args)
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
