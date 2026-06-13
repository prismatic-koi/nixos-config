package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// findTriples returns all (flag, v1, v2) triples where args[i] == flag.
func findTriples(args []string, flag string) [][3]string {
	var out [][3]string
	for i := 0; i+2 < len(args); i++ {
		if args[i] == flag {
			out = append(out, [3]string{args[i+1], args[i+2], ""})
		}
	}
	return out
}

// hasArg returns true when args contains the given element.
func hasArg(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

// hasBind returns true when args contains --bind src src (same src and dst).
func hasBind(args []string, src string) bool {
	for _, t := range findTriples(args, "--bind") {
		if t[0] == src && t[1] == src {
			return true
		}
	}
	return false
}

// hasROBind returns true when args contains --ro-bind src src.
func hasROBind(args []string, src string) bool {
	for _, t := range findTriples(args, "--ro-bind") {
		if t[0] == src && t[1] == src {
			return true
		}
	}
	return false
}

// hasROBindSrcDst returns true when args contains --ro-bind src dst (where src
// and dst may differ — used to assert canonical path remappings).
func hasROBindSrcDst(args []string, src, dst string) bool {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" && args[i+1] == src && args[i+2] == dst {
			return true
		}
	}
	return false
}

// hasSetenv returns true when args contains --setenv key value.
func hasSetenv(args []string, key, value string) bool {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--setenv" && args[i+1] == key && args[i+2] == value {
			return true
		}
	}
	return false
}

// newBwrapManager creates a Manager and injects a bwrapIsolator, setting up
// the fake HOME so that conditional path checks can be controlled.
func newBwrapManager(cfg Config) *Manager {
	m := New(cfg)
	m.isolator = newBwrapIsolator(m.name)
	return m
}

// bwrapFixture creates a Manager with a temporary HOME directory and enough
// fake paths to exercise all branches of BuildArgs. The returned cleanup
// function removes all temporary state.
func bwrapFixture(t *testing.T, cfg Config) (m *Manager, fakeHome string, cleanup func()) {
	t.Helper()

	// Canonicalise the tempdir so it matches the production code, which resolves
	// home paths through filepath.EvalSymlinks before composing bind args. On
	// Darwin t.TempDir() returns a /var/folders/... path that is a symlink into
	// /private/var/folders/...; without this the bind-arg substring assertions
	// (which see the /private form) never match. On Linux this is a no-op.
	var err error
	fakeHome, err = filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}

	// Pre-create directories that BuildArgs expects unconditionally.
	// ~/.config/claude is conditional (OptionalIfMissing) but pre-created
	// here so the fixture exercises the bound path (issue #2243).
	dirs := []string{
		filepath.Join(fakeHome, ".config", "claude"),
		filepath.Join(fakeHome, ".mcp-auth"),
		filepath.Join(fakeHome, ".npm"),
		filepath.Join(fakeHome, ".cache", "nix"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("fixture: MkdirAll %q: %v", d, err)
		}
	}

	// Create fake SSH keys (regular files, so EvalSymlinks resolves them).
	sshDir := filepath.Join(fakeHome, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("fixture: MkdirAll ssh dir: %v", err)
	}
	sshFiles := []string{
		"prismatic-koi-ed25519",
		"prismatic-koi-ed25519-signingkey",
		"prismatic-koi-ed25519-signingkey.pub",
		"known_hosts",
	}
	for _, f := range sshFiles {
		p := filepath.Join(sshDir, f)
		if err := os.WriteFile(p, []byte("fake"), 0o600); err != nil {
			t.Fatalf("fixture: WriteFile %q: %v", p, err)
		}
	}

	// AWS readonly-config.
	awsDir := filepath.Join(fakeHome, ".config", "aws")
	if err := os.MkdirAll(awsDir, 0o755); err != nil {
		t.Fatalf("fixture: MkdirAll aws dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(awsDir, "readonly-config"), []byte("fake"), 0o600); err != nil {
		t.Fatalf("fixture: WriteFile aws readonly-config: %v", err)
	}

	// Kube agents-config.
	kubeDir := filepath.Join(fakeHome, ".config", "kube")
	if err := os.MkdirAll(kubeDir, 0o755); err != nil {
		t.Fatalf("fixture: MkdirAll kube dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kubeDir, "agents-config"), []byte("fake"), 0o600); err != nil {
		t.Fatalf("fixture: WriteFile kube agents-config: %v", err)
	}

	// Override HOME for the duration of this test.
	origHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", fakeHome); err != nil {
		t.Fatalf("fixture: Setenv HOME: %v", err)
	}

	m = newBwrapManager(cfg)

	// Write the SSH config and gitconfig temp files BuildArgs expects.
	if err := os.WriteFile(m.sshConfigFilePath(), []byte("fake ssh config"), 0o600); err != nil {
		t.Fatalf("fixture: WriteFile ssh config: %v", err)
	}
	if err := os.WriteFile(m.gitconfigFilePath(), []byte("fake gitconfig"), 0o600); err != nil {
		t.Fatalf("fixture: WriteFile gitconfig: %v", err)
	}

	cleanup = func() {
		_ = os.Setenv("HOME", origHome)
	}
	return m, fakeHome, cleanup
}

// ── Baseline namespace flags ─────────────────────────────────────────────────

func TestBwrapBuildArgs_BaselineFlags(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// The first baseline flags must appear in order, with --clearenv first.
	// --clearenv wipes the inherited environment so host-shell secrets (e.g.
	// PRISM_GITHUB_TOKEN_*) do not leak into the sandbox interior; every var
	// the sandbox needs is added back via explicit --setenv pairs.
	// Note: --unshare-ipc is intentionally absent — see issue #906.
	want := []string{
		"--clearenv",
		"--unshare-pid",
		"--unshare-uts",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--die-with-parent",
	}
	for i, w := range want {
		if i >= len(args) {
			t.Fatalf("args too short (%d): missing baseline flag %q", len(args), w)
		}
		if args[i] != w {
			t.Errorf("args[%d] = %q, want %q", i, args[i], w)
		}
	}
}

// TestBwrapBuildArgs_ClearenvBeforeAllSetenv verifies that --clearenv appears
// before the first --setenv pair. bwrap applies flags left-to-right, so
// --clearenv MUST come first — any --setenv emitted before it would be wiped.
func TestBwrapBuildArgs_ClearenvBeforeAllSetenv(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	clearenvIdx := -1
	firstSetenvIdx := -1
	for i, a := range args {
		if a == "--clearenv" && clearenvIdx == -1 {
			clearenvIdx = i
		}
		if a == "--setenv" && firstSetenvIdx == -1 {
			firstSetenvIdx = i
		}
	}

	if clearenvIdx == -1 {
		t.Fatalf("--clearenv missing from args: %v", args)
	}
	if firstSetenvIdx == -1 {
		t.Fatalf("expected at least one --setenv, got none: %v", args)
	}
	if clearenvIdx >= firstSetenvIdx {
		t.Errorf("--clearenv at index %d must precede first --setenv at index %d (otherwise the setenv is wiped)",
			clearenvIdx, firstSetenvIdx)
	}
}

// TestMinimalBwrapExecEnv_DropsSecrets verifies that minimalBwrapExecEnv
// strips every env var except the small allow-list bwrap itself needs. This
// is the second layer of the token-leak defence (the first being --clearenv
// on the bwrap command line): even the bwrap process's own /proc/<pid>/environ
// must not contain secrets that could be read by anyone with ptrace rights.
func TestMinimalBwrapExecEnv_DropsSecrets(t *testing.T) {
	input := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/ben",
		"USER=ben",
		"LOGNAME=ben",
		"TERM=tmux-256color",
		"COLORTERM=truecolor",
		"LANG=en_NZ.UTF-8",
		"LC_ALL=en_NZ.UTF-8",

		// All the secret shapes we've observed leaking.
		"GITHUB_TOKEN=github_pat_secret",
		"GITHUB_PACKAGES_TOKEN=ghp_secret",
		"PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR=github_pat_role",
		"PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER=github_pat_role",
		"ANTHROPIC_API_KEY=sk-anth",
		"OPENAI_API_KEY=sk-openai",
		"OPENROUTER_API_KEY=sk-or",

		// Arbitrary non-secret noise that should also be dropped.
		"PRISM_SESSION_NAME=leaky",
		"XDG_RUNTIME_DIR=/run/user/1000",
		"SSH_AUTH_SOCK=/tmp/ssh-xxx",
	}

	out := minimalBwrapExecEnv(input)

	wantPresent := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/ben",
		"USER=ben",
		"LOGNAME=ben",
		"TERM=tmux-256color",
		"COLORTERM=truecolor",
		"LANG=en_NZ.UTF-8",
		"LC_ALL=en_NZ.UTF-8",
	}
	gotSet := map[string]bool{}
	for _, kv := range out {
		gotSet[kv] = true
	}
	for _, kv := range wantPresent {
		if !gotSet[kv] {
			t.Errorf("allow-listed pair %q missing from output: %v", kv, out)
		}
	}

	// Every output pair must be on the allow-list — nothing else should leak.
	allowedKeys := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
		"TERM": true, "COLORTERM": true, "LANG": true, "LC_ALL": true,
	}
	for _, kv := range out {
		eq := -1
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				eq = i
				break
			}
		}
		if eq <= 0 {
			t.Errorf("output pair %q is malformed (no '=')", kv)
			continue
		}
		key := kv[:eq]
		if !allowedKeys[key] {
			t.Errorf("key %q leaked through minimalBwrapExecEnv (pair: %q)", key, kv)
		}
	}

	// Explicitly guard against the specific secret env var shapes.
	for _, kv := range out {
		for _, forbidden := range []string{
			"GITHUB_TOKEN",
			"GITHUB_PACKAGES_TOKEN",
			"PRISM_GITHUB_TOKEN_",
			"ANTHROPIC_API_KEY",
			"OPENAI_API_KEY",
			"OPENROUTER_API_KEY",
		} {
			if strings.HasPrefix(kv, forbidden) {
				t.Errorf("forbidden key %q leaked: %q", forbidden, kv)
			}
		}
	}
}

// TestMinimalBwrapExecEnv_IgnoresMalformed verifies that entries without '='
// (which should never appear in os.Environ() output but are possible in
// synthetic inputs) are silently skipped rather than panicking or producing
// bogus pairs.
func TestMinimalBwrapExecEnv_IgnoresMalformed(t *testing.T) {
	out := minimalBwrapExecEnv([]string{
		"PATH=/usr/bin",
		"malformed-no-equals",
		"=starts-with-equals",
		"HOME=/home/ben",
	})

	want := []string{"PATH=/usr/bin", "HOME=/home/ben"}
	if len(out) != len(want) {
		t.Fatalf("len(out) = %d, want %d: %v", len(out), len(want), out)
	}
	for i, w := range want {
		if out[i] != w {
			t.Errorf("out[%d] = %q, want %q", i, out[i], w)
		}
	}
}

// ── Read-write binds ─────────────────────────────────────────────────────────

func TestBwrapBuildArgs_WorktreeBound(t *testing.T) {
	worktree := t.TempDir()
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      worktree,
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasBind(args, worktree) {
		t.Errorf("worktree %q not found as --bind SRC SRC in args: %v", worktree, args)
	}
}

// TestBwrapBuildArgs_ClaudeConfigXDGDirBound verifies the claude config dir
// is RW-bound Dst==Src at the host XDG path (~/.config/claude) and that the
// former canonical ~/.claude bind is gone (issue #2243, Step 3c of #2132).
// claude-code reaches the dir via the CLAUDE_CONFIG_DIR env var; the bwrap
// namespace is additive from an empty root, so the Dst==Src bind delivers
// the host directory at the path the env var carries.
func TestBwrapBuildArgs_ClaudeConfigXDGDirBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	claudeXDGDir := filepath.Join(fakeHome, ".config", "claude")
	if !hasBind(args, claudeXDGDir) {
		t.Errorf("~/.config/claude %q not found as --bind SRC SRC in args: %v", claudeXDGDir, args)
	}

	// The canonical ~/.claude path must not appear anywhere in the args —
	// neither as a bind source nor as a destination (env-var route, #2243).
	canonical := filepath.Join(fakeHome, ".claude")
	if hasArg(args, canonical) {
		t.Errorf("canonical ~/.claude %q must not appear in args (XDG relocation, #2243): %v", canonical, args)
	}
}

// TestBwrapBuildArgs_ClaudeConfigDirAbsentNoBind verifies the claude XDG
// mount is OptionalIfMissing: a host without ~/.config/claude produces no
// bind args for it (bwrap aborts on missing --bind sources), and no
// canonical ~/.claude bind reappears (issue #2243).
func TestBwrapBuildArgs_ClaudeConfigDirAbsentNoBind(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	// Remove the fixture's pre-created ~/.config/claude.
	claudeXDGDir := filepath.Join(fakeHome, ".config", "claude")
	if err := os.RemoveAll(claudeXDGDir); err != nil {
		t.Fatalf("RemoveAll %q: %v", claudeXDGDir, err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if hasArg(args, claudeXDGDir) {
		t.Errorf("absent ~/.config/claude %q must not be referenced in args (OptionalIfMissing): %v", claudeXDGDir, args)
	}
	if hasArg(args, filepath.Join(fakeHome, ".claude")) {
		t.Errorf("canonical ~/.claude must not appear in args (XDG relocation, #2243): %v", args)
	}
}

func TestBwrapBuildArgs_McpAuthBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	mcpAuthDir := filepath.Join(fakeHome, ".mcp-auth")
	if !hasBind(args, mcpAuthDir) {
		t.Errorf("~/.mcp-auth %q not found as --bind SRC SRC in args: %v", mcpAuthDir, args)
	}
}

func TestBwrapBuildArgs_PiXDGDirNotBound(t *testing.T) {
	// ~/.local/share/pi is NOT bound into the bwrap sandbox. PI does not use
	// that XDG path (it lives at ~/.pi/agent/); the old unconditional --bind
	// was a dead mount from the opencode→pi rename that broke fresh installs
	// where the source directory did not exist (bwrap aborts on missing
	// --bind sources). Removed in #1622.
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	xdgDir := filepath.Join(fakeHome, ".local", "share", "pi")
	if hasBind(args, xdgDir) {
		t.Errorf("~/.local/share/pi %q should NOT be bound in bwrap args (dead mount removed in #1622), but was: %v", xdgDir, args)
	}
}

func TestBwrapBuildArgs_NixDaemonSocketDirBound(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasBind(args, "/nix/var/nix/daemon-socket") {
		t.Errorf("/nix/var/nix/daemon-socket not found as --bind SRC SRC in args: %v", args)
	}
}

func TestBwrapBuildArgs_NixCacheDirBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	nixCacheDir := filepath.Join(fakeHome, ".cache", "nix")
	if !hasBind(args, nixCacheDir) {
		t.Errorf("~/.cache/nix %q not found as --bind SRC SRC in args: %v", nixCacheDir, args)
	}
}

// TestBwrapBuildArgs_NixCacheDirAbsentNoBind pins the OptionalIfMissing
// semantics added in #2245 (Step 3e of #2132): when ~/.cache/nix does not
// exist on the host (fresh machine), no bind triple is emitted for it. The
// entry used to be unconditional, which emits a --bind with a missing
// source — and bwrap ABORTS on missing bind sources (the #2243 lesson), so
// the absent dir would have broken every session on a fresh host.
func TestBwrapBuildArgs_NixCacheDirAbsentNoBind(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	// The fixture pre-creates ~/.cache/nix; remove it to simulate the fresh
	// host before building args.
	nixCacheDir := filepath.Join(fakeHome, ".cache", "nix")
	if err := os.RemoveAll(nixCacheDir); err != nil {
		t.Fatalf("RemoveAll ~/.cache/nix: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if hasBind(args, nixCacheDir) {
		t.Errorf("missing ~/.cache/nix should be omitted (OptionalIfMissing, #2245) but found as --bind in args: %v", args)
	}
}

// TestBwrapBuildArgs_BunCacheDirBound pins the ~/.cache/bun RW bind after
// its convergence from the inline bwrap.go block into the
// StandardSandboxMounts walk (#2245, Step 3e of #2132). Behaviour must be
// identical to the former inline block: RW, Dst==Src, present when the host
// dir exists.
func TestBwrapBuildArgs_BunCacheDirBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	// The fixture does not pre-create ~/.cache/bun — create it so the
	// conditional mount fires.
	bunCacheDir := filepath.Join(fakeHome, ".cache", "bun")
	if err := os.MkdirAll(bunCacheDir, 0o755); err != nil {
		t.Fatalf("MkdirAll ~/.cache/bun: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasBind(args, bunCacheDir) {
		t.Errorf("~/.cache/bun %q not found as --bind SRC SRC in args: %v", bunCacheDir, args)
	}
	if hasROBind(args, bunCacheDir) {
		t.Errorf("~/.cache/bun %q must be RW (--bind), not RO (--ro-bind): %v", bunCacheDir, args)
	}
}

// TestBwrapBuildArgs_BunCacheDirAbsentNoBind pins the conditional half of
// the bun-cache convergence (#2245): when ~/.cache/bun does not exist on
// the host, no bind triple is emitted (OptionalIfMissing — bwrap aborts on
// missing bind sources).
func TestBwrapBuildArgs_BunCacheDirAbsentNoBind(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	bunCacheDir := filepath.Join(fakeHome, ".cache", "bun")
	if _, err := os.Stat(bunCacheDir); err == nil {
		t.Fatalf("fixture unexpectedly created ~/.cache/bun — update this test")
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if hasBind(args, bunCacheDir) {
		t.Errorf("missing ~/.cache/bun should be omitted but found as --bind in args: %v", args)
	}
	if hasROBind(args, bunCacheDir) {
		t.Errorf("missing ~/.cache/bun should be omitted but found as --ro-bind in args: %v", args)
	}
}

// TestBwrapBuildArgs_PrismProfilesJSONROBound pins the
// ~/.config/prism/profiles.json single-file RO mount (issue #2286). The CLI's
// `prism profile list` / `prism profile show` and the available_profiles
// section of `prism agent-context` open this file directly via
// internal/config/profiles.go::LoadProfiles; without this mount those
// commands fail from inside any bwrap-isolated session with a misleading
// "not found — run the system rebuild" error. Read-only at Dst==Src under
// $HOME/.config/prism/profiles.json (matching profilesFilePath()'s
// XDG_CONFIG_HOME-else-$HOME/.config resolution inside the sandbox).
func TestBwrapBuildArgs_PrismProfilesJSONROBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	// The fixture does not pre-create ~/.config/prism/profiles.json — plant
	// it as a regular file so the conditional (OptionalIfMissing) mount fires.
	profilesJSON := filepath.Join(fakeHome, ".config", "prism", "profiles.json")
	if err := os.MkdirAll(filepath.Dir(profilesJSON), 0o755); err != nil {
		t.Fatalf("MkdirAll ~/.config/prism: %v", err)
	}
	if err := os.WriteFile(profilesJSON, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile profiles.json: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasROBind(args, profilesJSON) {
		t.Errorf("profiles.json %q not found as --ro-bind SRC SRC in args: %v", profilesJSON, args)
	}
	// RO must not silently become RW — reject any --bind (the RW flag) of
	// the same path.
	if hasBind(args, profilesJSON) {
		t.Errorf("profiles.json %q must be RO (--ro-bind), not RW (--bind): %v", profilesJSON, args)
	}
}

// TestBwrapBuildArgs_PrismProfilesJSONAbsentNoBind covers the fresh-install
// edge case from issue #2286: when profiles.json does not exist on the host
// (before the first `nh switch`), no bind triple is emitted —
// OptionalIfMissing skips the mount, and bwrap (which aborts on missing
// bind sources) is never asked to bind a non-existent file. LoadProfiles
// inside the sandbox then sees ENOENT through its normal
// os.ReadFile/os.IsNotExist branch and emits the existing host-side error
// message verbatim — the change must not paper over the genuine missing
// case.
func TestBwrapBuildArgs_PrismProfilesJSONAbsentNoBind(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	profilesJSON := filepath.Join(fakeHome, ".config", "prism", "profiles.json")
	if _, err := os.Stat(profilesJSON); err == nil {
		t.Fatalf("fixture unexpectedly created profiles.json — update this test")
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if hasROBind(args, profilesJSON) {
		t.Errorf("missing profiles.json should be omitted but found as --ro-bind in args: %v", args)
	}
	if hasBind(args, profilesJSON) {
		t.Errorf("missing profiles.json should be omitted but found as --bind in args: %v", args)
	}
}

func TestBwrapBuildArgs_BareRepoBoundAtHostPath(t *testing.T) {
	// When BareRoot and WorktreeGitDir are set, the bare repo (.bare dir) and
	// worktree private git state are both bound at their host paths (Dst == Src),
	// with no canonical-path remapping.
	bareRoot := t.TempDir()
	bareDir := filepath.Join(bareRoot, ".bare")
	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		t.Fatalf("MkdirAll bareDir: %v", err)
	}

	worktreeGitDir := t.TempDir()

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:    "repo@feat",
		Worktree:       t.TempDir(),
		AllocatedPort:  14010,
		BareRoot:       bareRoot,
		WorktreeGitDir: worktreeGitDir,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// Bare dir is bound at its actual host path (Dst == Src).
	if !hasBind(args, bareDir) {
		t.Errorf("bare dir %q not found as --bind SRC SRC in args: %v", bareDir, args)
	}

	// Worktree private git state is also bound at its host path (Dst == Src).
	if !hasBind(args, worktreeGitDir) {
		t.Errorf("worktree git dir %q not found as --bind SRC SRC in args: %v", worktreeGitDir, args)
	}

	// Confirm /prism-git is NOT a destination (no remapping).
	for _, tri := range findTriples(args, "--bind") {
		if tri[1] == "/prism-git" {
			t.Errorf("unexpected --bind _ /prism-git found (bwrap must use Dst==Src): %v", args)
		}
	}
}

func TestBwrapBuildArgs_MissingBareRootOmitted(t *testing.T) {
	// When BareRoot is set but the .bare directory does not exist,
	// the bind should be omitted.
	bareRoot := t.TempDir()
	// Do NOT create bareRoot/.bare — it should be absent.

	worktreeGitDir := t.TempDir()

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:    "repo@feat",
		Worktree:       t.TempDir(),
		AllocatedPort:  14010,
		BareRoot:       bareRoot,
		WorktreeGitDir: worktreeGitDir,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	bareDir := filepath.Join(bareRoot, ".bare")
	if hasBind(args, bareDir) {
		t.Errorf("missing bareDir %q should be omitted but found as --bind in args: %v", bareDir, args)
	}
}

// ── Read-only binds ──────────────────────────────────────────────────────────

func TestBwrapBuildArgs_AWSConfigCanonicalBindsGone(t *testing.T) {
	// Issue #2234 (Step 3a of #2132, design decision §5.1): the aws
	// config/credentials canonical-path ($HOME/.aws/*) bind-mounts were
	// dropped from StandardSandboxMounts. The aws CLI reaches both files via
	// the AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE env vars at the host
	// XDG paths. Because the bwrap namespace is additive from an empty root,
	// the XDG paths themselves are bound Dst==Src (see
	// TestBwrapBuildArgs_AWSXDGPathROBound) — only the canonical ~/.aws
	// destinations must be gone.
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// No bind of any kind (--ro-bind or --bind, any src pairing) may deliver
	// anything at the canonical sandbox destinations.
	forbidden := []string{
		filepath.Join(fakeHome, ".aws", "config"),
		filepath.Join(fakeHome, ".aws", "credentials"),
	}
	for i, arg := range args {
		if arg != "--ro-bind" && arg != "--bind" {
			continue
		}
		if i+2 >= len(args) {
			continue
		}
		for _, p := range forbidden {
			if args[i+1] == p || args[i+2] == p {
				t.Errorf("aws config/credentials canonical-path bind found (%s %q %q) — dropped in #2234 (env-var route); args: %v",
					arg, args[i+1], args[i+2], args)
			}
		}
	}
}

func TestBwrapBuildArgs_AWSXDGPathROBound(t *testing.T) {
	// Issue #2234: the env-var route needs the aws config content delivered
	// INTO the bwrap namespace — bwrap builds its filesystem additively from
	// an empty root, so an env var pointing at an unbound path would resolve
	// to nothing in-sandbox. The XDG host paths are bound Dst==Src so
	// AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE (which carry exactly
	// these paths) resolve to readable files.
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	// Create the credentials file (bwrapFixture only creates readonly-config).
	credsPath := filepath.Join(fakeHome, ".config", "aws", "credentials")
	if err := os.WriteFile(credsPath, []byte("fake"), 0o600); err != nil {
		t.Fatalf("WriteFile aws credentials: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// SRC: resolved host file (EvalSymlinks pins the sops target inode).
	// DST: the same XDG path — the value the env vars carry.
	cfgPath := filepath.Join(fakeHome, ".config", "aws", "readonly-config")
	if !hasROBindSrcDst(args, cfgPath, cfgPath) {
		t.Errorf("AWS readonly-config: want --ro-bind %q %q (Dst==Src XDG delivery for the env-var route, #2234) in args: %v",
			cfgPath, cfgPath, args)
	}
	if !hasROBindSrcDst(args, credsPath, credsPath) {
		t.Errorf("AWS credentials: want --ro-bind %q %q (Dst==Src XDG delivery for the env-var route, #2234) in args: %v",
			credsPath, credsPath, args)
	}
}

func TestBwrapBuildArgs_KubeAgentsConfigXDGPathROBound(t *testing.T) {
	// Issue #2235: the env-var route needs the kube config content delivered
	// INTO the bwrap namespace — bwrap builds its filesystem additively from
	// an empty root, so KUBECONFIG pointing at an unbound path would resolve
	// to nothing in-sandbox. The XDG host path is bound Dst==Src so
	// KUBECONFIG (which carries exactly this path) resolves to a readable
	// file. The former canonical-path ($HOME/.kube/config) remap is gone.
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// SRC: resolved host file (EvalSymlinks pins the sops target inode).
	// DST: the same XDG path — the value KUBECONFIG carries.
	kubeSrc := filepath.Join(fakeHome, ".config", "kube", "agents-config")
	if !hasROBindSrcDst(args, kubeSrc, kubeSrc) {
		t.Errorf("kube agents-config: want --ro-bind %q %q (Dst==Src XDG delivery for the env-var route, #2235) in args: %v",
			kubeSrc, kubeSrc, args)
	}

	// The canonical-path remap must be gone (issue #2235, bwrap convergence
	// per #2132 design decision §5.1).
	kubeCanonicalDst := filepath.Join(fakeHome, ".kube", "config")
	for i, arg := range args {
		if arg != "--ro-bind" && arg != "--bind" {
			continue
		}
		if i+2 >= len(args) {
			continue
		}
		if args[i+1] == kubeCanonicalDst || args[i+2] == kubeCanonicalDst {
			t.Errorf("kube canonical-path bind found (%s %q %q) — dropped in #2235 (env-var route); args: %v",
				arg, args[i+1], args[i+2], args)
		}
	}
}

// TestBwrapBuildArgs_KubeCacheDirEnvInjected verifies the bwrap equivalent
// of the sandbox-exec KUBECACHEDIR redirect (issue #2235): kubectl's cache
// is pointed at /tmp/kube-cache on the per-session tmpfs (--tmpfs /tmp), so
// cache writes are ephemeral and never reach the host. The injection is
// unconditional — it must not depend on the kube config existing on the
// host (kubectl creates the dir on first use).
func TestBwrapBuildArgs_KubeCacheDirEnvInjected(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	// Remove the kube config to prove the env injection is unconditional.
	if err := os.Remove(filepath.Join(fakeHome, ".config", "kube", "agents-config")); err != nil {
		t.Fatalf("Remove kube agents-config: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasSetenv(args, "KUBECACHEDIR", bwrapKubeCacheDir) {
		t.Errorf("--setenv KUBECACHEDIR %s not found in args: %v", bwrapKubeCacheDir, args)
	}

	// The value must live on the per-session /tmp tmpfs — never a host path.
	if !strings.HasPrefix(bwrapKubeCacheDir, "/tmp/") {
		t.Errorf("bwrapKubeCacheDir = %q, want a /tmp/ tmpfs path (ephemeral, never host-visible)", bwrapKubeCacheDir)
	}
}

func TestBwrapBuildArgs_SSHAccessKeyRemapped(t *testing.T) {
	// The access key is mounted at the host path but exposed inside the
	// sandbox at the canonical generic path $HOME/.ssh/access-key so that
	// the generated ~/.ssh/config's IdentityFile line resolves correctly.
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	accessKeySrc := filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519")
	accessKeyDst := filepath.Join(fakeHome, ".ssh", "access-key")
	if !hasROBindSrcDst(args, accessKeySrc, accessKeyDst) {
		t.Errorf("SSH access key: want --ro-bind %q %q in args: %v", accessKeySrc, accessKeyDst, args)
	}
	// Must NOT be bound at the host-name path (Dst==Src form was replaced
	// by the canonical-name remap in the signing-key parity fix).
	if hasROBindSrcDst(args, accessKeySrc, accessKeySrc) {
		t.Errorf("SSH access key: --ro-bind %q %q (Dst==Src) should not be emitted; args: %v",
			accessKeySrc, accessKeySrc, args)
	}
}

func TestBwrapBuildArgs_SSHSigningKeyRemapped(t *testing.T) {
	// Both halves of the signing key pair are remapped to canonical generic
	// paths under $HOME/.ssh/ so the generated ~/.gitconfig's signingKey
	// line resolves to the same path the agent sees at runtime.
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	signingKeySrc := filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519-signingkey")
	signingKeyDst := filepath.Join(fakeHome, ".ssh", "signing-key")
	signingKeyPubSrc := filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519-signingkey.pub")
	signingKeyPubDst := filepath.Join(fakeHome, ".ssh", "signing-key.pub")

	if !hasROBindSrcDst(args, signingKeySrc, signingKeyDst) {
		t.Errorf("SSH signing key (private): want --ro-bind %q %q in args: %v",
			signingKeySrc, signingKeyDst, args)
	}
	if !hasROBindSrcDst(args, signingKeyPubSrc, signingKeyPubDst) {
		t.Errorf("SSH signing key (public): want --ro-bind %q %q in args: %v",
			signingKeyPubSrc, signingKeyPubDst, args)
	}
	// Must NOT be bound at the host-name paths.
	if hasROBindSrcDst(args, signingKeySrc, signingKeySrc) {
		t.Errorf("SSH signing key (private): --ro-bind %q %q (Dst==Src) should not be emitted",
			signingKeySrc, signingKeySrc)
	}
	if hasROBindSrcDst(args, signingKeyPubSrc, signingKeyPubSrc) {
		t.Errorf("SSH signing key (public): --ro-bind %q %q (Dst==Src) should not be emitted",
			signingKeyPubSrc, signingKeyPubSrc)
	}
}

func TestBwrapBuildArgs_AllowedSignersRemapped(t *testing.T) {
	// When writeGitconfig has successfully written the allowed_signers file,
	// BuildArgs must mount it at the canonical $HOME/.ssh/allowed_signers
	// path so that `git verify-commit` (which reads the path from gitconfig)
	// resolves it inside the sandbox.
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		GitUserName:   "test-user",
		GitUserEmail:  "test@example.com",
	})
	defer cleanup()

	// writeGitconfig(isolationBwrap) both writes the gitconfig and, as a
	// side effect, writes the allowed_signers file and sets
	// allowedSignersReady — which is what gates the mount below.
	if err := m.writeGitconfig(isolationBwrap); err != nil {
		t.Fatalf("writeGitconfig: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(m.gitconfigFilePath())
		_ = os.Remove(m.allowedSignersFilePath())
	})

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	allowedSrc := m.allowedSignersFilePath()
	allowedDst := filepath.Join(fakeHome, ".ssh", "allowed_signers")
	if !hasROBindSrcDst(args, allowedSrc, allowedDst) {
		t.Errorf("allowed_signers: want --ro-bind %q %q in args: %v",
			allowedSrc, allowedDst, args)
	}
}

func TestBwrapBuildArgs_KnownHostsRemapped(t *testing.T) {
	// known_hosts is mounted with Dst = $HOME/.ssh/known_hosts, matching the
	// canonical-name pattern used by the other SSH artefacts. This is
	// functionally identical to Dst==Src because host sshDir already equals
	// $HOME/.ssh, but the explicit form reads uniformly with the siblings.
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	knownHostsSrc := filepath.Join(fakeHome, ".ssh", "known_hosts")
	knownHostsDst := filepath.Join(fakeHome, ".ssh", "known_hosts")
	if !hasROBindSrcDst(args, knownHostsSrc, knownHostsDst) {
		t.Errorf("known_hosts: want --ro-bind %q %q in args: %v",
			knownHostsSrc, knownHostsDst, args)
	}
}

func TestBwrapBuildArgs_GeneratedSSHConfigROBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// SRC: generated temp file (the SSH config with absolute key paths)
	// DST: sandbox $HOME/.ssh/config (canonical path SSH reads by default)
	sshSrc := m.sshConfigFilePath()
	sshDst := filepath.Join(fakeHome, ".ssh", "config")
	if !hasROBindSrcDst(args, sshSrc, sshDst) {
		t.Errorf("generated SSH config: want --ro-bind %q %q in args: %v", sshSrc, sshDst, args)
	}
}

func TestBwrapBuildArgs_GeneratedGitconfigROBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// SRC: generated temp file (identity, signing config, convenience settings)
	// DST: sandbox $HOME/.gitconfig (canonical path git reads for user config)
	gitSrc := m.gitconfigFilePath()
	gitDst := filepath.Join(fakeHome, ".gitconfig")
	if !hasROBindSrcDst(args, gitSrc, gitDst) {
		t.Errorf("generated gitconfig: want --ro-bind %q %q in args: %v", gitSrc, gitDst, args)
	}
}

// ── Env vars (--setenv K V) ──────────────────────────────────────────────────

func TestBwrapBuildArgs_EnvVarsTranslated(t *testing.T) {
	// Set a known env var that credentialEnvVars() will pick up.
	t.Setenv("ANTHROPIC_API_KEY", "test-anthropic-key")

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasSetenv(args, "ANTHROPIC_API_KEY", "test-anthropic-key") {
		t.Errorf("--setenv ANTHROPIC_API_KEY test-anthropic-key not found in args: %v", args)
	}
}

func TestBwrapBuildArgs_NixConfigSetenv(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasSetenv(args, "NIX_CONFIG", "store = daemon") {
		t.Errorf("--setenv NIX_CONFIG 'store = daemon' not found in args: %v", args)
	}
}

// TestBwrapBuildArgs_TermPassthrough verifies that the host TERM value is
// passed through verbatim into the sandbox (e.g. tmux-256color). bwrap mode
// bind-mounts the host terminfo tree so any host TERM entry is resolvable
// inside the sandbox.
func TestBwrapBuildArgs_TermPassthrough(t *testing.T) {
	t.Setenv("TERM", "tmux-256color")

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasSetenv(args, "TERM", "tmux-256color") {
		t.Errorf("--setenv TERM tmux-256color not found in args: %v", args)
	}
}

// TestBwrapBuildArgs_TermFallbackWhenUnset verifies that when TERM is unset on
// the host, the sandbox receives the safe fallback value xterm-256color.
func TestBwrapBuildArgs_TermFallbackWhenUnset(t *testing.T) {
	t.Setenv("TERM", "")

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasSetenv(args, "TERM", "xterm-256color") {
		t.Errorf("--setenv TERM xterm-256color (fallback) not found when TERM is empty, args: %v", args)
	}
}

// TestBwrapBuildArgs_TermUnusualValue verifies that unusual TERM values
// (e.g. alacritty-direct) are passed through verbatim without any shell
// interpretation, since bwrap --setenv takes KEY and VALUE as distinct argv
// elements.
func TestBwrapBuildArgs_TermUnusualValue(t *testing.T) {
	t.Setenv("TERM", "alacritty-direct")

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasSetenv(args, "TERM", "alacritty-direct") {
		t.Errorf("--setenv TERM alacritty-direct not found in args: %v", args)
	}
}

// ── Standard sandbox env vars (PATH, HOME, USER, etc.) ──────────────────────

// TestStandardSandboxEnvArgs_PathFallbackWhenUnset verifies that when PATH is
// not set, the fallback chain is emitted.
func TestStandardSandboxEnvArgs_PathFallbackWhenUnset(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("USER", "")
	args := standardSandboxEnvArgs()
	fp := fallbackPATH()
	if !hasSetenv(args, "PATH", fp) {
		t.Errorf("expected --setenv PATH %q when PATH is empty, got args: %v", fp, args)
	}
}

// TestStandardSandboxEnvArgs_PathFromHostWhenSet verifies that when PATH is
// set on the host, that exact value is forwarded.
func TestStandardSandboxEnvArgs_PathFromHostWhenSet(t *testing.T) {
	hostPath := "/custom/bin:/usr/local/bin:/usr/bin"
	t.Setenv("PATH", hostPath)
	args := standardSandboxEnvArgs()
	if !hasSetenv(args, "PATH", hostPath) {
		t.Errorf("expected --setenv PATH %q, got args: %v", hostPath, args)
	}
}

// TestStandardSandboxEnvArgs_OptionalVarsPresentWhenSet verifies that HOME,
// USER, LOGNAME, LANG, and LC_ALL are forwarded when set on the host.
func TestStandardSandboxEnvArgs_OptionalVarsPresentWhenSet(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	t.Setenv("USER", "testuser")
	t.Setenv("LOGNAME", "testuser")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "en_US.UTF-8")

	args := standardSandboxEnvArgs()

	cases := [][2]string{
		{"HOME", "/home/testuser"},
		{"USER", "testuser"},
		{"LOGNAME", "testuser"},
		{"LANG", "en_US.UTF-8"},
		{"LC_ALL", "en_US.UTF-8"},
	}
	for _, c := range cases {
		if !hasSetenv(args, c[0], c[1]) {
			t.Errorf("expected --setenv %s %q in args: %v", c[0], c[1], args)
		}
	}
}

// TestStandardSandboxEnvArgs_OptionalVarsOmittedWhenUnset verifies that
// optional vars (HOME, USER, LOGNAME, LANG, LC_ALL) are NOT emitted when
// they are not set on the host. SHELL is not in this list because it is
// forced to /bin/sh unconditionally (see TestStandardSandboxEnvArgs_ShellPinnedToBinSh).
func TestStandardSandboxEnvArgs_OptionalVarsOmittedWhenUnset(t *testing.T) {
	for _, key := range []string{"HOME", "USER", "LOGNAME", "LANG", "LC_ALL"} {
		t.Setenv(key, "")
	}
	args := standardSandboxEnvArgs()

	for _, key := range []string{"HOME", "USER", "LOGNAME", "LANG", "LC_ALL"} {
		for i := 0; i+2 < len(args); i++ {
			if args[i] == "--setenv" && args[i+1] == key {
				t.Errorf("optional var %s should be omitted when unset but found in args: %v", key, args)
			}
		}
	}
}

// TestStandardSandboxEnvArgs_ShellPinnedToBinSh verifies that SHELL is always
// set to /bin/sh, regardless of the host value. On NixOS /bin/sh is a symlink
// to bash; using /bin/sh (not zsh) avoids NixOS' /etc/zshenv sourcing
// set-environment, which cats sops paths that don't exist inside the sandbox
// and silently overwrites GITHUB_TOKEN with an empty string. bash -c does
// not source /etc/profile or /etc/bashrc for non-interactive invocations, so
// injected env vars survive intact. We pin /bin/sh (not /bin/bash) because
// /bin/ inside the sandbox only contains the NixOS /bin/sh symlink — bash
// itself lives in the Nix store at a hash-prefixed path.
func TestStandardSandboxEnvArgs_ShellPinnedToBinSh(t *testing.T) {
	for _, hostShell := range []string{
		"", // unset host SHELL
		"/run/current-system/sw/bin/zsh",
		"/usr/bin/fish",
		"/bin/bash",
	} {
		t.Run("host_SHELL="+hostShell, func(t *testing.T) {
			t.Setenv("SHELL", hostShell)
			args := standardSandboxEnvArgs()
			if !hasSetenv(args, "SHELL", "/bin/sh") {
				t.Errorf("expected --setenv SHELL /bin/sh regardless of host value %q; got args: %v",
					hostShell, args)
			}
		})
	}
}

// TestStandardSandboxEnvArgs_PathFallbackExactNoUser verifies the fallback PATH
// when USER is empty is exactly the base string without a per-user prefix.
func TestStandardSandboxEnvArgs_PathFallbackExactNoUser(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("USER", "")
	args := standardSandboxEnvArgs()
	want := "/run/current-system/sw/bin:/nix/var/nix/profiles/default/bin:/usr/bin:/bin"
	if !hasSetenv(args, "PATH", want) {
		t.Errorf("fallback PATH (no USER) = unexpected value; want exactly %q, got args: %v", want, args)
	}
}

// TestBwrapBuildArgs_PathSetenvPresentInBuildArgs verifies that BuildArgs
// emits --setenv PATH (via standardSandboxEnvArgs) before the -- terminator.
func TestBwrapBuildArgs_PathSetenvPresentInBuildArgs(t *testing.T) {
	hostPath := "/nix/store/test/bin:/run/current-system/sw/bin"
	t.Setenv("PATH", hostPath)

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// Confirm --setenv PATH appears before the -- separator.
	sepIdx := -1
	for i, a := range args {
		if a == "--" {
			sepIdx = i
			break
		}
	}
	if sepIdx < 0 {
		t.Fatalf("-- separator not found in args: %v", args)
	}

	pre := args[:sepIdx]
	if !hasSetenv(pre, "PATH", hostPath) {
		t.Errorf("--setenv PATH %q not found before -- in args: %v", hostPath, args)
	}
}

// TestBwrapBuildArgs_PathFallbackInBuildArgs verifies that when PATH is empty
// on the host, BuildArgs emits the fallback PATH via standardSandboxEnvArgs.
func TestBwrapBuildArgs_PathFallbackInBuildArgs(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("USER", "")

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	fp := fallbackPATH()
	if !hasSetenv(args, "PATH", fp) {
		t.Errorf("expected fallback --setenv PATH %q when PATH empty, got args: %v", fp, args)
	}
}

// TestBwrapBuildArgs_TermRegression guards that TERM is always injected into
// the sandbox. When TERM is set on the host it is passed through; when unset
// it falls back to xterm-256color (the previous hardcoded value, now a safe
// default rather than an override).
func TestBwrapBuildArgs_TermRegression(t *testing.T) {
	// Simulate a normal tmux pane with TERM set.
	t.Setenv("TERM", "tmux-256color")

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasSetenv(args, "TERM", "tmux-256color") {
		t.Errorf("regression: --setenv TERM tmux-256color missing from args: %v", args)
	}
}

func TestBwrapBuildArgs_PrismSessionNameSetenv(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "myrepo@feat",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasSetenv(args, "PRISM_SESSION_NAME", "myrepo@feat") {
		t.Errorf("--setenv PRISM_SESSION_NAME myrepo@feat not found in args: %v", args)
	}
}

// ── COLORTERM pass-through ────────────────────────────────────────────────────

// TestBwrapBuildArgs_ColortermPassthrough verifies that when COLORTERM is set
// on the host, it is injected into the sandbox so TUI libraries receive the
// truecolor signal.
func TestBwrapBuildArgs_ColortermPassthrough(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasSetenv(args, "COLORTERM", "truecolor") {
		t.Errorf("--setenv COLORTERM truecolor not found in args: %v", args)
	}
}

// TestBwrapBuildArgs_ColortermOmittedWhenUnset verifies that when COLORTERM is
// not set on the host, no COLORTERM is injected into the sandbox (no fabricated
// value).
func TestBwrapBuildArgs_ColortermOmittedWhenUnset(t *testing.T) {
	t.Setenv("COLORTERM", "")

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--setenv" && args[i+1] == "COLORTERM" {
			t.Errorf("COLORTERM should be omitted when unset but found --setenv COLORTERM %q in args: %v", args[i+2], args)
		}
	}
}

// ── AgentEnvVars (--setenv K V) ───────────────────────────────────────────────

// TestBwrapBuildArgs_AgentEnvVarsInjected verifies that every entry in
// Config.AgentEnvVars is emitted as --setenv K V in the bwrap arg list.
// KUBECONFIG flows through since issue #2235 (Step 3b of #2132) — the
// canonical-path ($HOME/.kube/config) kube bind was dropped and kubectl
// resolves the config via KUBECONFIG at the host XDG path. AWS_CONFIG_FILE
// and AWS_SHARED_CREDENTIALS_FILE flow through since issue #2234 (Step 3a)
// on the same pattern.
func TestBwrapBuildArgs_AgentEnvVarsInjected(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		AgentEnvVars: map[string]string{
			"GIT_EDITOR":                  "true",
			"KUBECONFIG":                  "/home/ben/.config/kube/agents-config",
			"AWS_CONFIG_FILE":             "/home/ben/.config/aws/readonly-config",
			"AWS_SHARED_CREDENTIALS_FILE": "/home/ben/.config/aws/credentials",
			"CLAUDE_CONFIG_DIR":           "/home/ben/.config/claude",
		},
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// GIT_EDITOR must be injected.
	if !hasSetenv(args, "GIT_EDITOR", "true") {
		t.Errorf("--setenv GIT_EDITOR true not found in args: %v", args)
	}

	// AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE MUST be injected
	// (issue #2234) — the canonical-path bind-mounts are gone and the aws
	// CLI resolves the files via these env vars at the host XDG paths.
	if !hasSetenv(args, "AWS_CONFIG_FILE", "/home/ben/.config/aws/readonly-config") {
		t.Errorf("--setenv AWS_CONFIG_FILE not found in args (must flow since #2234): %v", args)
	}
	if !hasSetenv(args, "AWS_SHARED_CREDENTIALS_FILE", "/home/ben/.config/aws/credentials") {
		t.Errorf("--setenv AWS_SHARED_CREDENTIALS_FILE not found in args (must flow since #2234): %v", args)
	}

	// KUBECONFIG MUST be injected (issue #2235) — the canonical-path kube
	// bind is gone and kubectl resolves the config via this env var at the
	// host XDG path.
	if !hasSetenv(args, "KUBECONFIG", "/home/ben/.config/kube/agents-config") {
		t.Errorf("--setenv KUBECONFIG not found in args (must flow since #2235): %v", args)
	}

	// CLAUDE_CONFIG_DIR MUST be injected (issue #2243) — the canonical-path
	// ~/.claude bind is gone and claude-code resolves its config dir via
	// this env var at the host XDG path.
	if !hasSetenv(args, "CLAUDE_CONFIG_DIR", "/home/ben/.config/claude") {
		t.Errorf("--setenv CLAUDE_CONFIG_DIR not found in args (must flow since #2243): %v", args)
	}
}

// TestBwrapBuildArgs_KubeconfigInjectedEvenWhenFileAbsent verifies that
// KUBECONFIG is injected even when ~/.config/kube/agents-config does not
// exist on the host, and that the absent file produces no bind args (the
// XDG Dst==Src mount is EvalSymlinks-conditional). The env layer is a plain
// value pass-through (issue #2235); kubectl tolerates a missing config file,
// so injection must not depend on file existence — mirrors the AWS shape
// from #2234.
func TestBwrapBuildArgs_KubeconfigInjectedEvenWhenFileAbsent(t *testing.T) {
	fakeHome := t.TempDir()
	// Do NOT create kube dir — file absent.
	origHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", fakeHome); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Minimal fixture: create the dirs/files required by BuildArgs.
	for _, d := range []string{
		filepath.Join(fakeHome, ".config", "claude"),
		filepath.Join(fakeHome, ".mcp-auth"),
		filepath.Join(fakeHome, ".local", "share", "pi"),
		filepath.Join(fakeHome, ".cache", "nix"),
		filepath.Join(fakeHome, ".ssh"),
		filepath.Join(fakeHome, ".config", "aws"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll %q: %v", d, err)
		}
	}
	for _, f := range []string{
		filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519"),
		filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519-signingkey"),
		filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519-signingkey.pub"),
		filepath.Join(fakeHome, ".ssh", "known_hosts"),
		filepath.Join(fakeHome, ".config", "aws", "readonly-config"),
	} {
		if err := os.WriteFile(f, []byte("fake"), 0o600); err != nil {
			t.Fatalf("WriteFile %q: %v", f, err)
		}
	}

	m := newBwrapManager(Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		AgentEnvVars:  map[string]string{"KUBECONFIG": "/home/ben/.config/kube/agents-config"},
	})
	if err := os.WriteFile(m.sshConfigFilePath(), []byte("fake"), 0o600); err != nil {
		t.Fatalf("WriteFile ssh config: %v", err)
	}
	if err := os.WriteFile(m.gitconfigFilePath(), []byte("fake"), 0o600); err != nil {
		t.Fatalf("WriteFile gitconfig: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasSetenv(args, "KUBECONFIG", "/home/ben/.config/kube/agents-config") {
		t.Errorf("--setenv KUBECONFIG must be injected even when the host file is absent (#2235); args: %v", args)
	}

	// Absent file → no XDG bind emitted (EvalSymlinks silent skip).
	kubeSrc := filepath.Join(fakeHome, ".config", "kube", "agents-config")
	if hasROBindSrcDst(args, kubeSrc, kubeSrc) {
		t.Errorf("absent kube file %q must not produce a bind; args: %v", kubeSrc, args)
	}
}

// TestBwrapBuildArgs_AwsEnvVarsInjectedEvenWhenFilesAbsent verifies that
// AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE are injected even when
// ~/.config/aws/readonly-config and ~/.config/aws/credentials do not exist
// on the host, and that the absent files produce no bind args (the XDG
// Dst==Src mounts are EvalSymlinks-conditional). The env layer is a plain
// value pass-through (issue #2234); the aws CLI tolerates a missing
// credentials file (config-only operation — the credentials file is
// deliberately absent on the current host) and a missing config file, so
// injection must not depend on file existence.
func TestBwrapBuildArgs_AwsEnvVarsInjectedEvenWhenFilesAbsent(t *testing.T) {
	fakeHome := t.TempDir()
	// Do NOT create aws dir — file absent.
	origHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", fakeHome); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	defer func() { _ = os.Setenv("HOME", origHome) }()

	for _, d := range []string{
		filepath.Join(fakeHome, ".config", "claude"),
		filepath.Join(fakeHome, ".mcp-auth"),
		filepath.Join(fakeHome, ".local", "share", "pi"),
		filepath.Join(fakeHome, ".cache", "nix"),
		filepath.Join(fakeHome, ".ssh"),
		filepath.Join(fakeHome, ".config", "kube"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll %q: %v", d, err)
		}
	}
	for _, f := range []string{
		filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519"),
		filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519-signingkey"),
		filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519-signingkey.pub"),
		filepath.Join(fakeHome, ".ssh", "known_hosts"),
		filepath.Join(fakeHome, ".config", "kube", "agents-config"),
	} {
		if err := os.WriteFile(f, []byte("fake"), 0o600); err != nil {
			t.Fatalf("WriteFile %q: %v", f, err)
		}
	}

	m := newBwrapManager(Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		AgentEnvVars: map[string]string{
			"AWS_CONFIG_FILE":             "/home/ben/.config/aws/readonly-config",
			"AWS_SHARED_CREDENTIALS_FILE": "/home/ben/.config/aws/credentials",
		},
	})
	if err := os.WriteFile(m.sshConfigFilePath(), []byte("fake"), 0o600); err != nil {
		t.Fatalf("WriteFile ssh config: %v", err)
	}
	if err := os.WriteFile(m.gitconfigFilePath(), []byte("fake"), 0o600); err != nil {
		t.Fatalf("WriteFile gitconfig: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasSetenv(args, "AWS_CONFIG_FILE", "/home/ben/.config/aws/readonly-config") {
		t.Errorf("--setenv AWS_CONFIG_FILE must be injected even when the host file is absent (#2234); args: %v", args)
	}
	if !hasSetenv(args, "AWS_SHARED_CREDENTIALS_FILE", "/home/ben/.config/aws/credentials") {
		t.Errorf("--setenv AWS_SHARED_CREDENTIALS_FILE must be injected even when the host file is absent (#2234); args: %v", args)
	}

	// Absent files → no XDG bind emitted (EvalSymlinks silent skip).
	for _, p := range []string{
		filepath.Join(fakeHome, ".config", "aws", "readonly-config"),
		filepath.Join(fakeHome, ".config", "aws", "credentials"),
	} {
		if hasROBindSrcDst(args, p, p) {
			t.Errorf("absent aws file %q must not produce a bind; args: %v", p, args)
		}
	}
}

// TestBwrapBuildArgs_AgentEnvVarsEmptyNoExtra verifies that an empty
// AgentEnvVars map produces no extra --setenv flags.
func TestBwrapBuildArgs_AgentEnvVarsEmptyNoExtra(t *testing.T) {
	// Collect baseline arg count with nil AgentEnvVars.
	mNil, _, cleanupNil := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		AgentEnvVars:  nil,
	})
	defer cleanupNil()
	bNil := &bwrapIsolator{name: mNil.name}
	argsNil := bNil.BuildArgs(mNil)

	// Empty map should produce the same args as nil.
	mEmpty, _, cleanupEmpty := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		AgentEnvVars:  map[string]string{},
	})
	defer cleanupEmpty()
	bEmpty := &bwrapIsolator{name: mEmpty.name}
	argsEmpty := bEmpty.BuildArgs(mEmpty)

	if len(argsEmpty) != len(argsNil) {
		t.Errorf("empty AgentEnvVars should produce same arg count as nil: nil=%d, empty=%d", len(argsNil), len(argsEmpty))
	}
}

// TestBwrapBuildArgs_AgentEnvVarsSpecialChars verifies that values containing
// spaces and single quotes are passed through verbatim, since bwrap --setenv
// takes VALUE as a distinct argv element and does no shell interpretation.
func TestBwrapBuildArgs_AgentEnvVarsSpecialChars(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		AgentEnvVars: map[string]string{
			"TRICKY_VAR": "value with spaces and 'quotes'",
		},
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasSetenv(args, "TRICKY_VAR", "value with spaces and 'quotes'") {
		t.Errorf("--setenv TRICKY_VAR with special chars not found verbatim in args: %v", args)
	}
}

// ── --chdir points at the worktree source path ───────────────────────────────

func TestBwrapBuildArgs_ChdirIsWorktree(t *testing.T) {
	worktree := t.TempDir()
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      worktree,
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	found := false
	for i, a := range args {
		if a == "--chdir" && i+1 < len(args) {
			if args[i+1] == worktree {
				found = true
			} else {
				t.Errorf("--chdir value = %q, want %q", args[i+1], worktree)
			}
			break
		}
	}
	if !found {
		t.Errorf("--chdir %q not found in args: %v", worktree, args)
	}
}

func TestBwrapBuildArgs_ChdirIsNotSlashWorkspace(t *testing.T) {
	// The bwrap path must NOT remap to /workspace.
	worktree := t.TempDir()
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      worktree,
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	for i, a := range args {
		if a == "--chdir" && i+1 < len(args) {
			if args[i+1] == "/workspace" {
				t.Errorf("--chdir must not be /workspace in bwrap mode; got %q", args[i+1])
			}
		}
	}
}

// ── Hostname is 127.0.0.1 (not 0.0.0.0) ─────────────────────────────────────
// ── Edge case: missing host path omitted ─────────────────────────────────────

func TestBwrapBuildArgs_MissingMountOmitted(t *testing.T) {
	// Create a fixture with all the standard paths present, then remove the
	// kube agents-config to verify it is omitted from the arg list.
	// (The vehicle was the AWS readonly-config until #2234 dropped its
	// canonical mount — kube exercises the same EvalSymlinks silent-skip
	// semantics in StandardSandboxMounts, now on the Dst==Src XDG bind
	// from #2235.)
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	// Remove kube agents-config — should be absent from bwrap args.
	kubeSrc := filepath.Join(fakeHome, ".config", "kube", "agents-config")
	if err := os.Remove(kubeSrc); err != nil {
		t.Fatalf("Remove kube agents-config: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// Neither the Dst==Src XDG form (#2235) nor the old canonical remap
	// should appear.
	if hasROBindSrcDst(args, kubeSrc, kubeSrc) {
		t.Errorf("missing kube agents-config should be omitted but found as --ro-bind %q %q in args: %v", kubeSrc, kubeSrc, args)
	}
	kubeCanonicalDst := filepath.Join(fakeHome, ".kube", "config")
	if hasROBindSrcDst(args, kubeSrc, kubeCanonicalDst) {
		t.Errorf("missing kube agents-config should be omitted but found as --ro-bind %q %q in args: %v", kubeSrc, kubeCanonicalDst, args)
	}
}

func TestBwrapBuildArgs_MissingMcpAuthOmitted(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	mcpAuthDir := filepath.Join(fakeHome, ".mcp-auth")
	if err := os.RemoveAll(mcpAuthDir); err != nil {
		t.Fatalf("RemoveAll mcp-auth: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if hasBind(args, mcpAuthDir) {
		t.Errorf("missing ~/.mcp-auth should be omitted but found as --bind in args: %v", args)
	}
}

// TestBwrapBuildArgs_NpmCacheBound pins the ~/.npm RW bind-mount when the
// host directory exists. npx (used by mcp-remote and similar npx-fetched
// tools) caches downloaded packages under ~/.npm/_npx/; without this mount
// the sandbox cache-misses and re-downloads, which then fails under the
// sandbox's network policy. Parity with sandbox-exec's §5e RW (subpath ~/.npm)
// grant in generateProfile (sandbox_exec.go). See issue #2127.
func TestBwrapBuildArgs_NpmCacheBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	npmDir := filepath.Join(fakeHome, ".npm")
	if !hasBind(args, npmDir) {
		t.Errorf("~/.npm %q not found as --bind SRC SRC in args: %v", npmDir, args)
	}
	// And it must NOT be read-only — npx writes to its cache on first use.
	if hasROBind(args, npmDir) {
		t.Errorf("~/.npm %q must be RW (--bind), not RO (--ro-bind): %v", npmDir, args)
	}
}

// TestBwrapBuildArgs_MissingNpmCacheOmitted pins the conditional-on-existence
// semantics: when ~/.npm does not exist on the host, the mount is omitted
// silently and the sandbox starts cleanly. Mirrors
// TestBwrapBuildArgs_MissingMcpAuthOmitted. See issue #2127.
func TestBwrapBuildArgs_MissingNpmCacheOmitted(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	npmDir := filepath.Join(fakeHome, ".npm")
	if err := os.RemoveAll(npmDir); err != nil {
		t.Fatalf("RemoveAll npm: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if hasBind(args, npmDir) {
		t.Errorf("missing ~/.npm should be omitted but found as --bind in args: %v", args)
	}
	if hasROBind(args, npmDir) {
		t.Errorf("missing ~/.npm should be omitted but found as --ro-bind in args: %v", args)
	}
}

func TestBwrapBuildArgs_MissingKubeConfigOmitted(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	kubeSrc := filepath.Join(fakeHome, ".config", "kube", "agents-config")
	if err := os.Remove(kubeSrc); err != nil {
		t.Fatalf("Remove kube agents-config: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// Neither the Dst==Src XDG form (#2235) nor the old canonical remap
	// should appear.
	if hasROBindSrcDst(args, kubeSrc, kubeSrc) {
		t.Errorf("missing kube agents-config should be omitted but found as --ro-bind %q %q in args: %v", kubeSrc, kubeSrc, args)
	}
	kubeCanonicalDst := filepath.Join(fakeHome, ".kube", "config")
	if hasROBindSrcDst(args, kubeSrc, kubeCanonicalDst) {
		t.Errorf("missing kube agents-config should be omitted but found as --ro-bind %q %q in args: %v", kubeSrc, kubeCanonicalDst, args)
	}
}

// ── Canonical path remaps (SRC != DST assertions) ────────────────────────────

// TestBwrapBuildArgs_AWSCredentialsNotRemapped verifies that the AWS
// credentials file is NOT mounted at the canonical $HOME/.aws/credentials
// path even when the XDG source exists — the canonical-path remap was
// dropped in issue #2234; the aws CLI reads the file via
// AWS_SHARED_CREDENTIALS_FILE at the host XDG path, which is bound Dst==Src
// (see TestBwrapBuildArgs_AWSXDGPathROBound).
func TestBwrapBuildArgs_AWSCredentialsNotRemapped(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	// Create the credentials file (bwrapFixture only creates readonly-config).
	awsDir := filepath.Join(fakeHome, ".config", "aws")
	credsSrc := filepath.Join(awsDir, "credentials")
	if err := os.WriteFile(credsSrc, []byte("fake"), 0o600); err != nil {
		t.Fatalf("WriteFile aws credentials: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	credsDst := filepath.Join(fakeHome, ".aws", "credentials")
	if hasROBindSrcDst(args, credsSrc, credsDst) {
		t.Errorf("AWS credentials: --ro-bind %q %q must NOT be emitted (env-var route since #2234); args: %v",
			credsSrc, credsDst, args)
	}
}

// TestBwrapBuildArgs_AllRemapsHaveCorrectDestinations is a comprehensive table
// test that asserts each canonical path remap produces the exact (SRC, DST) pair
// expected. This guards against regressions where a future change accidentally
// restores the old Dst==Src behaviour for any remapped mount.
func TestBwrapBuildArgs_AllRemapsHaveCorrectDestinations(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		ConfigContent: `{"model":"claude-3-5-sonnet"}`,
	})
	defer cleanup()

	// Write the harness config temp file as Create() would.
	if err := os.WriteFile(m.harnessConfigFilePath(), []byte(`{"model":"claude-3-5-sonnet"}`), 0o600); err != nil {
		t.Fatalf("WriteFile harness config: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// Note (#2234/#2235): the AWS readonly-config, AWS credentials, and kube
	// agents-config remaps are gone — the CLIs read those files via env vars
	// at the host XDG paths (bound Dst==Src). See
	// TestBwrapBuildArgs_AWSConfigCanonicalBindsGone and
	// TestBwrapBuildArgs_KubeAgentsConfigXDGPathROBound for the negative
	// assertions.
	cases := []struct {
		name string
		src  string
		dst  string
	}{
		{
			name: "generated SSH config → $HOME/.ssh/config",
			src:  m.sshConfigFilePath(),
			dst:  filepath.Join(fakeHome, ".ssh", "config"),
		},
		{
			name: "generated gitconfig → $HOME/.gitconfig",
			src:  m.gitconfigFilePath(),
			dst:  filepath.Join(fakeHome, ".gitconfig"),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if !hasROBindSrcDst(args, tc.src, tc.dst) {
				t.Errorf("remap %q: want --ro-bind %q %q in args: %v", tc.name, tc.src, tc.dst, args)
			}
		})
	}
}

// ── Isolator interface compliance ────────────────────────────────────────────

func TestBwrapIsolator_ImplementsIsolator(t *testing.T) {
	// Compile-time check: bwrapIsolator must implement Isolator.
	var _ Isolator = &bwrapIsolator{}
}

func TestBwrapIsolator_BuildRunArgsReturnsNil(t *testing.T) {
	// BuildRunArgs() is a stub on bwrapIsolator — it returns nil.
	b := &bwrapIsolator{name: "test"}
	if got := b.BuildRunArgs(); got != nil {
		t.Errorf("BuildRunArgs() = %v, want nil", got)
	}
}

func TestBwrapIsolator_HasExitedBeforeRun(t *testing.T) {
	// Before Run is called, HasExited must return (false, 0).
	b := &bwrapIsolator{name: "test"}
	exited, code := b.HasExited()
	if exited {
		t.Errorf("HasExited() = (true, %d) before Run, want (false, 0)", code)
	}
	if code != 0 {
		t.Errorf("HasExited() code = %d before Run, want 0", code)
	}
}

// ── newBwrapIsolator returns an Isolator ─────────────────────────────────────

func TestNewBwrapIsolator_ReturnsIsolator(t *testing.T) {
	var _ Isolator = newBwrapIsolator("test")
}

// ── PrepareBwrap uses bwrapIsolator via container.go (wired in #877) ─────────
// The old guard test (TestBwrapIsolator_NotUsedInContainerGo) is removed: the
// wiring PR (#877) intentionally references bwrapIsolator from container.go
// via Manager.PrepareBwrap(). The structural guarantee from #876 is now met
// by the AC that bwrap is NOT wired into Manager.Create() (the sidecar path).

func TestPrepareBwrap_WritesSSHConfigAndGitconfig(t *testing.T) {
	// PrepareBwrap must write the SSH config and gitconfig temp files and
	// return a non-empty arg list. It must NOT write gitdir fixup files.
	worktree := t.TempDir()
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@feat",
		Worktree:      worktree,
		AllocatedPort: 14020,
		AgentRole:     "worker",
		GitUserName:   "Test User",
		GitUserEmail:  "test@example.com",
	})
	defer cleanup()

	args, err := m.PrepareBwrap()
	if err != nil {
		t.Fatalf("PrepareBwrap: %v", err)
	}
	if len(args) == 0 {
		t.Fatalf("PrepareBwrap returned empty arg list")
	}

	// SSH config must exist.
	if _, err := os.Stat(m.SshConfigFilePath()); err != nil {
		t.Errorf("SSH config temp file not written: %v", err)
	}

	// Gitconfig must exist.
	if _, err := os.Stat(m.GitconfigFilePath()); err != nil {
		t.Errorf("Gitconfig temp file not written: %v", err)
	}

	// Gitdir fixup files must NOT exist (they belonged to the removed
	// legacy container path).
	if _, err := os.Stat(m.GitdirFilePath()); err == nil {
		t.Errorf("gitdir fixup file should not be written by PrepareBwrap")
	}
	if _, err := os.Stat(m.WorktreeGitdirFilePath()); err == nil {
		t.Errorf("worktree gitdir fixup file should not be written by PrepareBwrap")
	}
}

// ── Representative full-fixture test ─────────────────────────────────────────

// TestBwrapBuildArgs_FullFixture exercises a representative Manager with
// multiple config fields set and asserts the key structural properties of the
// generated arg list.
func TestBwrapBuildArgs_FullFixture(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("GITHUB_TOKEN", "ghp_test")

	worktree := t.TempDir()

	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "nixos-config@feature",
		Worktree:      worktree,
		AllocatedPort: 14020,
		AgentRole:     "worker",
		InitialPrompt: "implement the feature",
		ConfigContent: `{"model":"claude-3-5-sonnet"}`,
		GitUserName:   "Test User",
		GitUserEmail:  "test@example.com",
	})
	defer cleanup()

	// Write the harness config temp file.
	if err := os.WriteFile(m.harnessConfigFilePath(), []byte(m.cfg.ConfigContent), 0o600); err != nil {
		t.Fatalf("WriteFile harness config: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	t.Logf("full bwrap args (%d): %v", len(args), args)

	// Baseline flags present. Note: --unshare-ipc is intentionally absent (see
	// issue #906 — it breaks SQLite WAL mmap coherency between concurrent sessions).
	for _, flag := range []string{"--clearenv", "--unshare-pid", "--unshare-uts", "--die-with-parent"} {
		if !hasArg(args, flag) {
			t.Errorf("baseline flag %q missing from args", flag)
		}
	}
	// Explicitly assert --unshare-ipc is NOT present.
	if hasArg(args, "--unshare-ipc") {
		t.Errorf("--unshare-ipc must NOT be present in args (breaks SQLite WAL concurrency — see issue #906)")
	}
	for _, pair := range [][2]string{
		{"--proc", "/proc"},
		{"--dev", "/dev"},
		{"--tmpfs", "/tmp"},
	} {
		found := false
		for i, a := range args {
			if a == pair[0] && i+1 < len(args) && args[i+1] == pair[1] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s %s missing from args", pair[0], pair[1])
		}
	}

	// Worktree bound read-write.
	if !hasBind(args, worktree) {
		t.Errorf("worktree %q not found as --bind in args", worktree)
	}

	// --chdir points at worktree.
	found := false
	for i, a := range args {
		if a == "--chdir" && i+1 < len(args) && args[i+1] == worktree {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("--chdir %q not found in args", worktree)
	}

	// env vars translated.
	if !hasSetenv(args, "ANTHROPIC_API_KEY", "sk-ant-test") {
		t.Errorf("--setenv ANTHROPIC_API_KEY not found in args")
	}
	if !hasSetenv(args, "GITHUB_TOKEN", "ghp_test") {
		t.Errorf("--setenv GITHUB_TOKEN not found in args")
	}
	if !hasSetenv(args, "PRISM_SESSION_NAME", "nixos-config@feature") {
		t.Errorf("--setenv PRISM_SESSION_NAME not found in args")
	}

	// -- separator present.
	if !hasArg(args, "--") {
		t.Errorf("-- separator missing from args")
	}

	// Tail: pi --extension <extensionPath> --agent <role> <prompt>
	// PIInvocation emits the prompt as a bare positional arg, not --prompt.
	// Issue #2064 added --agent <role> between --extension and the prompt
	// so the prism PI extension's pi.getFlag("agent") path resolves
	// synchronously — see TestPIInvocation_AgentFlag in this package.
	n := len(args)
	if args[n-1] != "implement the feature" {
		t.Errorf("expected prompt as last positional arg, got %q", args[n-1])
	}
	if args[n-2] != "worker" || args[n-3] != "--agent" {
		t.Errorf("expected --agent worker before the prompt, got [..., %q, %q, %q]", args[n-3], args[n-2], args[n-1])
	}
	if args[n-5] != "--extension" {
		t.Errorf("expected --extension flag near end (before --agent), got %q at [n-5]", args[n-5])
	}

	// The canonical-path remaps are gone: AWS readonly-config since #2234,
	// kube agents-config since #2235 (env-var route) — assert both stay
	// gone, and that the kube XDG Dst==Src bind is present instead.
	if hasROBindSrcDst(args,
		filepath.Join(fakeHome, ".config", "aws", "readonly-config"),
		filepath.Join(fakeHome, ".aws", "config"),
	) {
		t.Errorf("AWS readonly-config: --ro-bind src $HOME/.aws/config must NOT be emitted (env-var route since #2234)")
	}
	if hasROBindSrcDst(args,
		filepath.Join(fakeHome, ".config", "kube", "agents-config"),
		filepath.Join(fakeHome, ".kube", "config"),
	) {
		t.Errorf("kube agents-config: --ro-bind src $HOME/.kube/config must NOT be emitted (env-var route since #2235)")
	}
	kubeXDG := filepath.Join(fakeHome, ".config", "kube", "agents-config")
	if !hasROBindSrcDst(args, kubeXDG, kubeXDG) {
		t.Errorf("kube agents-config: want --ro-bind %q %q (Dst==Src XDG delivery, #2235)", kubeXDG, kubeXDG)
	}

	// Claude config dir: the canonical ~/.claude bind is gone since #2243
	// (env-var route via CLAUDE_CONFIG_DIR); the XDG path is RW-bound
	// Dst==Src instead.
	if hasArg(args, filepath.Join(fakeHome, ".claude")) {
		t.Errorf("canonical ~/.claude must NOT appear in args (XDG relocation, #2243)")
	}
	claudeXDG := filepath.Join(fakeHome, ".config", "claude")
	if !hasBind(args, claudeXDG) {
		t.Errorf("claude config dir: want --bind %q %q (Dst==Src XDG delivery, #2243)", claudeXDG, claudeXDG)
	}

	// KUBECACHEDIR redirects kubectl's cache to the per-session /tmp tmpfs
	// (issue #2235).
	if !hasSetenv(args, "KUBECACHEDIR", bwrapKubeCacheDir) {
		t.Errorf("--setenv KUBECACHEDIR %s not found in args", bwrapKubeCacheDir)
	}
}

// ── System binary roots (unconditional --ro-bind entries) ────────────────────

// TestBwrapBuildArgs_SystemRootsPresent verifies that BuildArgs always emits
// --ro-bind for the five fixed NixOS system binary roots, regardless of
// Manager configuration.
func TestBwrapBuildArgs_SystemRootsPresent(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	for _, root := range []string{
		"/nix",
		"/etc",
		"/run/current-system",
		"/bin",
		"/run/wrappers",
	} {
		if !hasROBind(args, root) {
			t.Errorf("system root %q not found as --ro-bind SRC SRC in args: %v", root, args)
		}
	}
}

// TestBwrapBuildArgs_SystemRootsBeforeWorktree verifies the system binary
// roots appear before the worktree mount — i.e. immediately after the baseline
// namespace flags — so the ordering described in BuildArgs is maintained.
func TestBwrapBuildArgs_SystemRootsBeforeWorktree(t *testing.T) {
	worktree := t.TempDir()
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      worktree,
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// Find index of first system root ro-bind and first worktree bind.
	nixIdx := -1
	worktreeIdx := -1
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" && args[i+1] == "/nix" && nixIdx < 0 {
			nixIdx = i
		}
		if args[i] == "--bind" && args[i+1] == worktree && worktreeIdx < 0 {
			worktreeIdx = i
		}
	}
	if nixIdx < 0 {
		t.Fatalf("--ro-bind /nix /nix not found in args: %v", args)
	}
	if worktreeIdx < 0 {
		t.Fatalf("--bind %q %q not found in args: %v", worktree, worktree, args)
	}
	if nixIdx >= worktreeIdx {
		t.Errorf("system root /nix (idx %d) should appear before worktree bind (idx %d)", nixIdx, worktreeIdx)
	}
}

// ── Security: sensitive /etc subtree shadowing ───────────────────────────────

// TestBwrapBuildArgs_SensitiveEtcSubtreesShadowed verifies that when
// /etc/wireguard, /etc/wpa_supplicant, and /etc/ssh exist on the host,
// BuildArgs emits --tmpfs for each AFTER the /etc ro-bind-mount. The ordering
// is critical: bwrap applies mounts left-to-right, so the tmpfs must come
// after the ro-bind to shadow the subtree rather than being overwritten by it.
func TestBwrapBuildArgs_SensitiveEtcSubtreesShadowed(t *testing.T) {
	// Create fake /etc/wireguard and /etc/wpa_supplicant directories in a temp
	// location, then temporarily point the os.Stat checks at them by creating
	// them at the actual paths used by BuildArgs. Since BuildArgs hard-codes
	// /etc/wireguard and /etc/wpa_supplicant, we create the real directories
	// if they don't exist, run the test, then clean up only if we created them.
	type tempDir struct {
		path    string
		created bool
	}
	sensitives := []tempDir{
		{path: "/etc/wireguard"},
		{path: "/etc/wpa_supplicant"},
		{path: "/etc/ssh"},
	}
	for i := range sensitives {
		if _, err := os.Stat(sensitives[i].path); os.IsNotExist(err) {
			if mkErr := os.MkdirAll(sensitives[i].path, 0o755); mkErr != nil {
				t.Skipf("cannot create %s for test (insufficient permissions): %v", sensitives[i].path, mkErr)
			}
			sensitives[i].created = true
		}
	}
	defer func() {
		for _, d := range sensitives {
			if d.created {
				_ = os.Remove(d.path)
			}
		}
	}()

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// Find the index of the /etc ro-bind-mount.
	etcROBindIdx := -1
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" && args[i+1] == "/etc" && args[i+2] == "/etc" {
			etcROBindIdx = i
			break
		}
	}
	if etcROBindIdx < 0 {
		t.Fatalf("--ro-bind /etc /etc not found in args: %v", args)
	}

	// Assert each sensitive directory is shadowed by --tmpfs AFTER the
	// /etc ro-bind.
	for _, d := range sensitives {
		tmpfsIdx := -1
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--tmpfs" && args[i+1] == d.path {
				tmpfsIdx = i
				break
			}
		}
		if tmpfsIdx < 0 {
			t.Errorf("--tmpfs %s not found in args — sensitive subtree is not shadowed: %v", d.path, args)
			continue
		}
		if tmpfsIdx <= etcROBindIdx {
			t.Errorf("--tmpfs %s (idx %d) must appear AFTER --ro-bind /etc /etc (idx %d); "+
				"bwrap applies mounts left-to-right so the ordering is security-critical",
				d.path, tmpfsIdx, etcROBindIdx)
		}
	}
}

// TestBwrapBuildArgs_SensitiveEtcSubtreesAbsentWhenMissing verifies that when
// /etc/wireguard, /etc/wpa_supplicant, and /etc/ssh do not exist on the host,
// BuildArgs does NOT emit --tmpfs for them. On a machine without wgnord enabled
// and without the directories pre-created, the mounts must be omitted to avoid
// failing with EROFS when bwrap tries to create the mount-point inside the
// read-only /etc namespace.
func TestBwrapBuildArgs_SensitiveEtcSubtreesAbsentWhenMissing(t *testing.T) {
	// This test only runs meaningfully when the directories are absent.
	// If they all exist (e.g. this is the navi machine with impermanence),
	// there's nothing to verify here — skip gracefully.
	for _, p := range []string{"/etc/wireguard", "/etc/wpa_supplicant", "/etc/ssh"} {
		if _, err := os.Stat(p); err == nil {
			t.Skipf("%s exists on this host — cannot test the absent-path branch", p)
		}
	}

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	for _, p := range []string{"/etc/wireguard", "/etc/wpa_supplicant", "/etc/ssh"} {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--tmpfs" && args[i+1] == p {
				t.Errorf("--tmpfs %s should NOT appear in args when the directory does not exist on the host, but it does: %v", p, args)
			}
		}
	}
}

// ── Per-user nix profile mounts (conditional) ────────────────────────────────

// TestBwrapBuildArgs_NixProfilePresentWhenExists verifies that when
// $HOME/.nix-profile exists, BuildArgs emits --ro-bind for it.
func TestBwrapBuildArgs_NixProfilePresentWhenExists(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	// Create the nix-profile dir under the fake HOME.
	nixProfile := filepath.Join(fakeHome, ".nix-profile")
	if err := os.MkdirAll(nixProfile, 0o755); err != nil {
		t.Fatalf("MkdirAll nix-profile: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasROBind(args, nixProfile) {
		t.Errorf("~/.nix-profile %q not found as --ro-bind SRC SRC when it exists: %v", nixProfile, args)
	}
}

// TestBwrapBuildArgs_NixProfileAbsentWhenMissing verifies that when
// $HOME/.nix-profile does not exist, BuildArgs does not emit --ro-bind for it.
func TestBwrapBuildArgs_NixProfileAbsentWhenMissing(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	nixProfile := filepath.Join(fakeHome, ".nix-profile")
	// Do NOT create nixProfile — it should be absent.

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if hasROBind(args, nixProfile) {
		t.Errorf("~/.nix-profile should be omitted when absent, but found as --ro-bind in args: %v", args)
	}
}

// TestBwrapBuildArgs_LocalStateNixProfilePresentWhenExists verifies that when
// $HOME/.local/state/nix/profile exists, BuildArgs emits --ro-bind for it.
func TestBwrapBuildArgs_LocalStateNixProfilePresentWhenExists(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	localStateProfile := filepath.Join(fakeHome, ".local", "state", "nix", "profile")
	if err := os.MkdirAll(localStateProfile, 0o755); err != nil {
		t.Fatalf("MkdirAll local state nix profile: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasROBind(args, localStateProfile) {
		t.Errorf("~/.local/state/nix/profile %q not found as --ro-bind SRC SRC when it exists: %v", localStateProfile, args)
	}
}

// TestBwrapBuildArgs_LocalStateNixProfileAbsentWhenMissing verifies that when
// $HOME/.local/state/nix/profile does not exist, BuildArgs does not emit
// --ro-bind for it.
func TestBwrapBuildArgs_LocalStateNixProfileAbsentWhenMissing(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	localStateProfile := filepath.Join(fakeHome, ".local", "state", "nix", "profile")
	// Do NOT create localStateProfile — it should be absent.

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if hasROBind(args, localStateProfile) {
		t.Errorf("~/.local/state/nix/profile should be omitted when absent, but found as --ro-bind in args: %v", args)
	}
}

// ── fallbackPATH() function tests ────────────────────────────────────────────

// TestFallbackPATH_BeginsWithPerUserWhenUserSet verifies that when USER is
// non-empty, fallbackPATH() returns a value that begins with the per-user
// profile bin path.
func TestFallbackPATH_BeginsWithPerUserWhenUserSet(t *testing.T) {
	t.Setenv("USER", "ben")
	fp := fallbackPATH()
	wantPrefix := "/etc/profiles/per-user/ben/bin"
	if !strings.HasPrefix(fp, wantPrefix) {
		t.Errorf("fallbackPATH() = %q; want it to begin with %q when USER=ben", fp, wantPrefix)
	}
}

// TestFallbackPATH_NoPerUserWhenUserEmpty verifies that when USER is empty,
// fallbackPATH() does not include /etc/profiles/per-user//bin (no empty user
// path component).
func TestFallbackPATH_NoPerUserWhenUserEmpty(t *testing.T) {
	t.Setenv("USER", "")
	fp := fallbackPATH()
	badEntry := "/etc/profiles/per-user//bin"
	if strings.Contains(fp, badEntry) {
		t.Errorf("fallbackPATH() = %q; must not contain %q when USER is empty", fp, badEntry)
	}
	// Also verify no per-user entry at all.
	perUserPrefix := "/etc/profiles/per-user/"
	if strings.Contains(fp, perUserPrefix) {
		t.Errorf("fallbackPATH() = %q; must not contain any per-user entry when USER is empty", fp)
	}
}

// TestFallbackPATH_ContainsBaseEntriesAlways verifies that the base NixOS
// paths are always present in fallbackPATH() regardless of USER.
func TestFallbackPATH_ContainsBaseEntriesAlways(t *testing.T) {
	for _, user := range []string{"", "testuser"} {
		t.Run("USER="+user, func(t *testing.T) {
			t.Setenv("USER", user)
			fp := fallbackPATH()
			for _, entry := range []string{
				"/run/current-system/sw/bin",
				"/nix/var/nix/profiles/default/bin",
				"/usr/bin",
				"/bin",
			} {
				if !strings.Contains(fp, entry) {
					t.Errorf("fallbackPATH() = %q; must contain %q", fp, entry)
				}
			}
		})
	}
}

// TestStandardSandboxEnvArgs_FallbackPathWithUser verifies that when PATH is
// empty and USER is set, the fallback includes the per-user profile entry.
func TestStandardSandboxEnvArgs_FallbackPathWithUser(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("USER", "alice")
	args := standardSandboxEnvArgs()
	fp := fallbackPATH()
	if !hasSetenv(args, "PATH", fp) {
		t.Errorf("expected --setenv PATH %q (with per-user entry) when USER=alice and PATH empty, got args: %v", fp, args)
	}
	if !strings.HasPrefix(fp, "/etc/profiles/per-user/alice/bin") {
		t.Errorf("fallbackPATH with USER=alice should begin with per-user entry, got %q", fp)
	}
}

// ── Host-API socket isolation (security fix #960) ────────────────────────────

// TestBwrapBuildArgs_HostAPISockPerSessionDirBindNotSharedDir verifies that when
// HostAPISockPath is set, BuildArgs binds only the session's per-session socket
// DIRECTORY (not the shared run/ directory). Security fix #960: each session's
// socket is placed in run/<session>/hostapi.sock so binding only that directory
// prevents the sandbox from seeing other sessions' sockets. A directory bind
// (not file bind) is used so the socket file appears inside the sandbox after
// the sidecar calls net.Listen (file bind would pin the inode).
func TestBwrapBuildArgs_HostAPISockPerSessionDirBindNotSharedDir(t *testing.T) {
	// Use the new per-session directory format (run/<session>/hostapi.sock)
	// matching what SidecarHostAPIPath now returns.
	sockDir := filepath.Join(t.TempDir(), "run", "repo@feat")
	sockPath := filepath.Join(sockDir, "hostapi.sock")
	// prepareVolumeDirs pre-creates this directory in production; simulate that here.
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatalf("MkdirAll sockDir: %v", err)
	}
	// The shared run/ directory (parent of the per-session dir) should NOT be mounted.
	sharedRunDir := filepath.Dir(sockDir)

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:     "repo@feat",
		Worktree:        t.TempDir(),
		AllocatedPort:   14010,
		HostAPISockPath: sockPath,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// The per-session socket DIRECTORY must be bind-mounted at its own path (SRC == DST).
	if !hasBind(args, sockDir) {
		t.Errorf("per-session socket dir %q not found as --bind SRC SRC in args: %v", sockDir, args)
	}

	// The socket FILE must NOT be bind-mounted directly (we bind the directory).
	for _, tri := range findTriples(args, "--bind") {
		if tri[0] == sockPath && tri[1] == sockPath {
			t.Errorf("socket FILE %q must not be --bind SRC SRC (bind the directory instead); args: %v", sockPath, args)
		}
	}

	// The shared run/ directory must NOT be bind-mounted (security fix #960).
	if hasBind(args, sharedRunDir) {
		t.Errorf("shared run/ DIRECTORY %q must not be mounted (security fix #960); found as --bind in args: %v", sharedRunDir, args)
	}
}

// TestBwrapBuildArgs_HostAPISockEnvVarSet verifies that PRISM_HOST_API is set
// to the unix:// path of the session's own socket when HostAPISockPath is set.
func TestBwrapBuildArgs_HostAPISockEnvVarSet(t *testing.T) {
	sockDir := filepath.Join(t.TempDir(), "run", "repo@feat")
	sockPath := filepath.Join(sockDir, "hostapi.sock")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatalf("MkdirAll sockDir: %v", err)
	}
	if err := os.WriteFile(sockPath, []byte{}, 0o600); err != nil {
		t.Fatalf("WriteFile socket: %v", err)
	}

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:     "repo@feat",
		Worktree:        t.TempDir(),
		AllocatedPort:   14010,
		HostAPISockPath: sockPath,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	wantVal := "unix://" + sockPath
	if !hasSetenv(args, "PRISM_HOST_API", wantVal) {
		t.Errorf("--setenv PRISM_HOST_API %q not found in args: %v", wantVal, args)
	}
}

// TestBwrapBuildArgs_HostAPITCPPortUsesHTTPNotSocket verifies that when
// HostAPITCPPort is set (Darwin path), PRISM_HOST_API is set to the TCP URL
// and no socket bind-mount is emitted.
func TestBwrapBuildArgs_HostAPITCPPortUsesHTTPNotSocket(t *testing.T) {
	const tcpPort = 51234

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:     "repo@feat",
		Worktree:        t.TempDir(),
		AllocatedPort:   14010,
		HostAPITCPPort:  tcpPort,
		HostAPISockPath: "/home/user/.local/state/prism/run/repo@feat-hostapi.sock",
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// Must have TCP-based PRISM_HOST_API.
	wantVal := "http://host.containers.internal:51234"
	if !hasSetenv(args, "PRISM_HOST_API", wantVal) {
		t.Errorf("--setenv PRISM_HOST_API %q not found in args: %v", wantVal, args)
	}

	// Must NOT have unix:// PRISM_HOST_API.
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--setenv" && args[i+1] == "PRISM_HOST_API" {
			if strings.HasPrefix(args[i+2], "unix://") {
				t.Errorf("unexpected unix:// PRISM_HOST_API when HostAPITCPPort is set: %q", args[i+2])
			}
		}
	}

	// Must NOT mount the socket file or directory.
	sockPath := "/home/user/.local/state/prism/run/repo@feat-hostapi.sock"
	sockDir := filepath.Dir(sockPath)
	if hasBind(args, sockPath) {
		t.Errorf("socket file %q must not be mounted when HostAPITCPPort is set: %v", sockPath, args)
	}
	if hasBind(args, sockDir) {
		t.Errorf("socket dir %q must not be mounted when HostAPITCPPort is set: %v", sockDir, args)
	}
}

// TestBwrapBuildArgs_NoHostAPIWhenSockPathEmpty verifies that when both
// HostAPISockPath and HostAPITCPPort are zero/empty, no PRISM_HOST_API
// setenv or socket bind is emitted.
func TestBwrapBuildArgs_NoHostAPIWhenSockPathEmpty(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@feat",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		// HostAPISockPath and HostAPITCPPort intentionally left empty.
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--setenv" && args[i+1] == "PRISM_HOST_API" {
			t.Errorf("unexpected --setenv PRISM_HOST_API when HostAPISockPath is empty: %q", args[i+2])
		}
	}
}

// ── findPairs/findTriples smoke tests ────────────────────────────────────────

func TestHelpers_FindTriples(t *testing.T) {
	args := []string{"--bind", "/a", "/a", "--ro-bind", "/b", "/b", "--bind", "/c", "/c"}
	triples := findTriples(args, "--bind")
	if len(triples) != 2 {
		t.Errorf("expected 2 --bind triples, got %d: %v", len(triples), triples)
	}
}

func TestHelpers_HasSetenv(t *testing.T) {
	args := []string{"--setenv", "FOO", "bar", "--setenv", "BAZ", "qux"}
	if !hasSetenv(args, "FOO", "bar") {
		t.Error("hasSetenv(FOO, bar) should return true")
	}
	if hasSetenv(args, "FOO", "wrong") {
		t.Error("hasSetenv(FOO, wrong) should return false")
	}
}

// ── PI session persistence: global per-cwd history overlay (#1985) ───────────
//
// The host's ~/.pi/agent/sessions/ directory is overlay-mounted onto
// $PI_CODING_AGENT_DIR/sessions/ inside the sandbox by appendPIBwrapMounts
// (called from BuildArgs when Harness == "pi"). The tests below pin that
// contract: the overlay --bind must appear in the arg list, must use the
// in-sandbox sessions path as its destination (NOT the host path), must come
// AFTER the staging-dir bind (so bwrap applies it as an overlay on top), and
// must appear before the "--" terminator.
//
// Pre-#1985 these tests verified a separate --bind of
// ~/.pi/agent/sessions onto its own host path, motivated by an OAuth-tokens
// comment. That mount was dead code in the bwrap path (pi inside the sandbox
// only writes to PI_CODING_AGENT_DIR, never to the host home path), so it was
// removed during the #1985 consolidation.

// bwrapPIFixture extends bwrapFixture with the minimum PI-specific config
// needed to exercise the pi harness code path in BuildArgs. It creates a
// fake PI binary, extension directory, AND the shared host ~/.pi/agent
// directory at <fakeHome>/.pi/agent (mirroring what production
// EnsurePIAgentConfigDir returns post-#2034), then returns a Config with
// Harness="pi" + PIAgentConfigHostDir pointing at that shared dir so the
// pi-mount block in appendPIBwrapMounts is exercised end-to-end.
func bwrapPIFixture(t *testing.T) (m *Manager, fakeHome string, cleanup func()) {
	t.Helper()

	// Create the fake PI binary and extension directory.
	fakePIBin := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(fakePIBin, []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatalf("bwrapPIFixture: write fake pi binary: %v", err)
	}
	extDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(extDir, "prism.ts"), []byte("// ext"), 0o644); err != nil {
		t.Fatalf("bwrapPIFixture: write pi extension: %v", err)
	}

	// Construct Manager via bwrapFixture which sets HOME to a fake.
	m, fakeHome, cleanup = bwrapFixture(t, Config{
		SessionName:             "repo@pi",
		Worktree:                t.TempDir(),
		AllocatedPort:           14010,
		Harness:                 "pi",
		PIBinaryPath:            fakePIBin,
		PIExtensionHostDir:      extDir,
		PIAgentConfigHostDir:    filepath.Join("", ".pi", "agent"), // placeholder, overridden below
		PIAgentConfigSandboxDir: "/run/prism/pi-agent",
	})

	// Now that fakeHome is known, point PIAgentConfigHostDir at the canonical
	// shared mount source <fakeHome>/.pi/agent (matching production behaviour
	// after #2034). Create it on disk so bwrap has a valid bind source.
	agentCfgDir := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(agentCfgDir, 0o700); err != nil {
		t.Fatalf("bwrapPIFixture: create ~/.pi/agent: %v", err)
	}
	m.cfg.PIAgentConfigHostDir = agentCfgDir
	return m, fakeHome, cleanup
}

// findBindPairs returns every (src, dst) --bind pair in args, in order.
func findBindPairs(args []string) [][2]string {
	var pairs [][2]string
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" {
			pairs = append(pairs, [2]string{args[i+1], args[i+2]})
		}
	}
	return pairs
}

// TestBwrapBuildArgs_PISessions_ReachableViaSharedMount verifies the #1985
// AC under the post-#2034 design: the host's ~/.pi/agent/sessions/ directory
// is reachable at $PI_CODING_AGENT_DIR/sessions/ inside the sandbox via the
// shared ~/.pi/agent RW bind — no dedicated sessions-overlay bind is
// emitted because the parent mount itself IS the host ~/.pi/agent.
func TestBwrapBuildArgs_PISessions_ReachableViaSharedMount(t *testing.T) {
	m, fakeHome, cleanup := bwrapPIFixture(t)
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	piAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	sandboxAgentDir := "/run/prism/pi-agent"

	// The parent RW bind must be present — it's how the sandbox reaches
	// $PI_CODING_AGENT_DIR/sessions/ and writes back to the host.
	found := false
	for _, p := range findBindPairs(args) {
		if p[0] == piAgentDir && p[1] == sandboxAgentDir {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --bind %q %q (RW shared mount of host ~/.pi/agent); args=%v",
			piAgentDir, sandboxAgentDir, args)
	}

	// The host sessions dir must exist after BuildArgs (created as a
	// side-effect so pi can write JSONL transcripts to it).
	hostSessionsDir := filepath.Join(fakeHome, ".pi", "agent", "sessions")
	if info, err := os.Stat(hostSessionsDir); err != nil {
		t.Errorf("expected BuildArgs to create %q; stat err=%v", hostSessionsDir, err)
	} else if !info.IsDir() {
		t.Errorf("expected %q to be a directory; got mode=%v", hostSessionsDir, info.Mode())
	}

	// There must NOT be a redundant standalone --bind of the sessions dir
	// onto $PI_CODING_AGENT_DIR/sessions/ — post-#2034 that overlay is gone
	// because the parent mount covers it.
	sandboxSessionsDir := sandboxAgentDir + "/sessions"
	for _, p := range findBindPairs(args) {
		if p[0] == hostSessionsDir && p[1] == sandboxSessionsDir {
			t.Errorf("unexpected dedicated sessions overlay --bind %q %q — "+
				"the shared parent mount of ~/.pi/agent covers sessions/ directly (post #2034); got args=%v",
				p[0], p[1], args)
		}
	}
}

// TestBwrapBuildArgs_PISessionsOverlay_CreatesHostDirIfMissing verifies AC8
// edge case: the spawn does not fail when ~/.pi/agent/ does not yet exist on
// the host. The overlay code creates the host directory before emitting the
// bind, so pi can write to it from inside the sandbox.
func TestBwrapBuildArgs_PISessionsOverlay_CreatesHostDirIfMissing(t *testing.T) {
	m, fakeHome, cleanup := bwrapPIFixture(t)
	defer cleanup()

	hostSessionsDir := filepath.Join(fakeHome, ".pi", "agent", "sessions")
	// Pre-condition: directory does NOT exist (the fixture does not create it).
	if _, err := os.Stat(hostSessionsDir); !os.IsNotExist(err) {
		t.Fatalf("pre-condition: %q must not exist (stat err=%v)", hostSessionsDir, err)
	}

	b := &bwrapIsolator{name: m.name}
	_ = b.BuildArgs(m)

	// Post-condition: BuildArgs created the host sessions dir as a side-effect
	// of preparing the overlay bind, so the subsequent bwrap exec will succeed
	// (bwrap requires the bind source to exist).
	if info, err := os.Stat(hostSessionsDir); err != nil {
		t.Errorf("expected BuildArgs to create %q; stat err=%v", hostSessionsDir, err)
	} else if !info.IsDir() {
		t.Errorf("expected %q to be a directory; got mode=%v", hostSessionsDir, info.Mode())
	}
}

// TestBwrapBuildArgs_PISharedMount_BeforeTerminator verifies that the
// shared ~/.pi/agent --bind appears before the "--" terminator. bwrap
// requires all namespace flags to precede the separator. (Replaces the
// pre-#2034 sessions-overlay-specific check; the parent bind subsumes it.)
func TestBwrapBuildArgs_PISharedMount_BeforeTerminator(t *testing.T) {
	m, fakeHome, cleanup := bwrapPIFixture(t)
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	sepIdx := -1
	for i, a := range args {
		if a == "--" {
			sepIdx = i
			break
		}
	}
	if sepIdx < 0 {
		t.Fatalf("-- separator not found in args: %v", args)
	}

	wantSrc := filepath.Join(fakeHome, ".pi", "agent")
	wantDst := "/run/prism/pi-agent"
	found := false
	for i := 0; i+2 < sepIdx; i++ {
		if args[i] == "--bind" && args[i+1] == wantSrc && args[i+2] == wantDst {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("shared --bind %q %q not found before -- terminator in args: %v",
			wantSrc, wantDst, args)
	}
}

// TestBwrapBuildArgs_PISharedMount_OmittedForNonPI verifies that the entire
// PI bind-mount block (including the shared ~/.pi/agent mount) is gated on
// cfg.Harness == "pi" — non-pi harnesses must not see the host pi-agent
// directory inside their sandbox.
func TestBwrapBuildArgs_PISharedMount_OmittedForNonPI(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		Harness:       "anthropic",
	})
	defer cleanup()

	// Even with the host dir present, the non-pi path must not bind it.
	piAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	hostSessionsDir := filepath.Join(piAgentDir, "sessions")
	if err := os.MkdirAll(hostSessionsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll pi sessions dir: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	for _, p := range findBindPairs(args) {
		if p[0] == piAgentDir || p[0] == hostSessionsDir {
			t.Errorf("non-pi harness must not --bind %q; got pair %v in args=%v",
				p[0], p, args)
		}
	}
}
