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

	fakeHome = t.TempDir()

	// Pre-create directories that BuildArgs expects unconditionally.
	dirs := []string{
		filepath.Join(fakeHome, ".claude"),
		filepath.Join(fakeHome, ".mcp-auth"),
		// Shared opencode data dir — bound directly into the bwrap sandbox.
		filepath.Join(fakeHome, ".local", "share", "opencode"),
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

func TestBwrapBuildArgs_ClaudeDirBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	claudeDir := filepath.Join(fakeHome, ".claude")
	if !hasBind(args, claudeDir) {
		t.Errorf("~/.claude %q not found as --bind SRC SRC in args: %v", claudeDir, args)
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

func TestBwrapBuildArgs_OpencodeSharedDirBound(t *testing.T) {
	// bwrap mode binds the shared host opencode data dir
	// (~/.local/share/opencode/) directly into the sandbox so all sessions
	// share a single SQLite DB. The per-session prism-sessions/<name>/
	// isolation used by the podman path is not needed on Linux (no virtiofs
	// WAL-mode locking issue) and is intentionally omitted here.
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	sharedDir := filepath.Join(fakeHome, ".local", "share", "opencode")
	if !hasBind(args, sharedDir) {
		t.Errorf("opencode shared data dir %q not found as --bind SRC SRC in args: %v", sharedDir, args)
	}

	// Confirm the per-session prism-sessions path is NOT bound — bwrap
	// must use the shared dir, not the per-session one.
	perSessionDir := filepath.Join(fakeHome, ".local", "share", "opencode", "prism-sessions", m.name)
	if hasBind(args, perSessionDir) {
		t.Errorf("per-session opencode dir %q should NOT be bound in bwrap mode, but was: %v", perSessionDir, args)
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

func TestBwrapBuildArgs_BareRepoBoundAtHostPath(t *testing.T) {
	// When BareRoot and WorktreeGitDir are set, the bare repo (.bare dir) and
	// worktree private git state are both bound at their host paths (Dst == Src),
	// not remapped to /prism-git as in the podman path.
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

func TestBwrapBuildArgs_AWSReadonlyConfigROBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// SRC: host ~/.config/aws/readonly-config (the XDG-compliant location managed by sops-nix)
	// DST: sandbox $HOME/.aws/config (canonical path the AWS CLI reads by default)
	awsSrc := filepath.Join(fakeHome, ".config", "aws", "readonly-config")
	awsDst := filepath.Join(fakeHome, ".aws", "config")
	if !hasROBindSrcDst(args, awsSrc, awsDst) {
		t.Errorf("AWS readonly-config: want --ro-bind %q %q in args: %v", awsSrc, awsDst, args)
	}
}

func TestBwrapBuildArgs_KubeAgentsConfigROBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// SRC: host ~/.config/kube/agents-config (XDG-compliant, managed by sops-nix)
	// DST: sandbox $HOME/.kube/config (canonical path kubectl reads by default)
	kubeSrc := filepath.Join(fakeHome, ".config", "kube", "agents-config")
	kubeDst := filepath.Join(fakeHome, ".kube", "config")
	if !hasROBindSrcDst(args, kubeSrc, kubeDst) {
		t.Errorf("kube agents-config: want --ro-bind %q %q in args: %v", kubeSrc, kubeDst, args)
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

func TestBwrapBuildArgs_OpencodeJSONROBound(t *testing.T) {
	// The mount is emitted when the temp file exists on disk, regardless of
	// whether cfg.ConfigContent is set (file-existence check, not string check).
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		// ConfigContent is intentionally NOT set here to verify that the mount
		// is driven by file existence (os.Stat) rather than cfg.ConfigContent.
	})
	defer cleanup()

	// Write the opencode config temp file as cmd/spawn.go does at spawn time.
	if err := os.WriteFile(m.opencodeConfigFilePath(), []byte(`{"model":"claude-3-5-sonnet"}`), 0o644); err != nil {
		t.Fatalf("WriteFile opencode config: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(m.opencodeConfigFilePath()) })

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// SRC: generated temp file (role-specific opencode.json with model, agent, providers)
	// DST: sandbox $HOME/.config/opencode/opencode.json (canonical path opencode reads)
	configSrc := m.opencodeConfigFilePath()
	configDst := filepath.Join(fakeHome, ".config", "opencode", "opencode.json")
	if !hasROBindSrcDst(args, configSrc, configDst) {
		t.Errorf("opencode.json: want --ro-bind %q %q in args: %v", configSrc, configDst, args)
	}
}

// TestBwrapBuildArgs_OpencodeJSONNotMountedWhenAbsent asserts that the
// opencode.json --ro-bind is omitted when the temp file does not exist on disk.
func TestBwrapBuildArgs_OpencodeJSONNotMountedWhenAbsent(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		// ConfigContent deliberately set to verify it does NOT trigger the mount
		// when the file is absent (the check is file-existence, not string).
		ConfigContent: `{"model":"claude-3-5-sonnet"}`,
	})
	defer cleanup()

	// Ensure the temp file does NOT exist (do not write it).
	_ = os.Remove(m.opencodeConfigFilePath())

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	configSrc := m.opencodeConfigFilePath()
	configDst := filepath.Join(fakeHome, ".config", "opencode", "opencode.json")
	if hasROBindSrcDst(args, configSrc, configDst) {
		t.Errorf("opencode.json --ro-bind should be absent when temp file does not exist, but found in args: %v", args)
	}
}

// ── opencode config allowlist entries ────────────────────────────────────────

// TestBwrapBuildArgs_OpencodeAllowlistNewEntries verifies that the entries
// added to bring the bwrap allowlist to parity with the podman allowlist are
// emitted when the corresponding paths exist on the host.
func TestBwrapBuildArgs_OpencodeAllowlistNewEntries(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	opencodeConfigDir := filepath.Join(fakeHome, ".config", "opencode")

	// Create each new allowlist entry so the conditional stat() succeeds.
	newEntries := []string{
		"command",
		"tui.json",
		".gitignore",
		"mcp-atlassian-slim-proxy.mjs",
	}
	for _, entry := range newEntries {
		p := filepath.Join(opencodeConfigDir, entry)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll for %q: %v", entry, err)
		}
		if err := os.WriteFile(p, []byte("fake"), 0o644); err != nil {
			t.Fatalf("WriteFile %q: %v", entry, err)
		}
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	for _, entry := range newEntries {
		p := filepath.Join(opencodeConfigDir, entry)
		if !hasROBind(args, p) {
			t.Errorf("opencode allowlist entry %q not found as --ro-bind SRC SRC in args: %v", entry, args)
		}
	}
}

// TestBwrapBuildArgs_OpencodeAllowlistNewEntriesOmittedWhenAbsent verifies
// that new allowlist entries are omitted when they do not exist on the host.
func TestBwrapBuildArgs_OpencodeAllowlistNewEntriesOmittedWhenAbsent(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	opencodeConfigDir := filepath.Join(fakeHome, ".config", "opencode")

	// Do NOT create the new entries — they should be absent from args.
	newEntries := []string{
		"command",
		"tui.json",
		".gitignore",
		"mcp-atlassian-slim-proxy.mjs",
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	for _, entry := range newEntries {
		p := filepath.Join(opencodeConfigDir, entry)
		if hasROBind(args, p) {
			t.Errorf("absent opencode allowlist entry %q should be omitted but found as --ro-bind in args: %v", entry, args)
		}
	}
}

// TestBwrapBuildArgs_AgentsDirMountedForNonReview verifies that agents/ is
// included in the allowlist (and therefore bound) when AgentRole is empty,
// "worker", or "coordinator" — i.e. any non-review-* value.
func TestBwrapBuildArgs_AgentsDirMountedForNonReview(t *testing.T) {
	for _, role := range []string{"", "worker", "coordinator"} {
		t.Run("AgentRole="+role, func(t *testing.T) {
			m, fakeHome, cleanup := bwrapFixture(t, Config{
				SessionName:   "repo@main",
				Worktree:      t.TempDir(),
				AllocatedPort: 14010,
				AgentRole:     role,
			})
			defer cleanup()

			// Create agents/ so the stat() succeeds.
			agentsDir := filepath.Join(fakeHome, ".config", "opencode", "agents")
			if err := os.MkdirAll(agentsDir, 0o755); err != nil {
				t.Fatalf("MkdirAll agents: %v", err)
			}

			b := &bwrapIsolator{name: m.name}
			args := b.BuildArgs(m)

			if !hasROBind(args, agentsDir) {
				t.Errorf("AgentRole=%q: agents/ %q not found as --ro-bind SRC SRC in args: %v", role, agentsDir, args)
			}
		})
	}
}

// TestBwrapBuildArgs_AgentsDirNotMountedForReview verifies that agents/ is
// NOT bound when AgentRole starts with "review-" (e.g. "review-code",
// "review-qa", "review-security", "review-goal", "review-context").
func TestBwrapBuildArgs_AgentsDirNotMountedForReview(t *testing.T) {
	for _, role := range []string{"review-code", "review-qa", "review-security", "review-goal", "review-context"} {
		t.Run("AgentRole="+role, func(t *testing.T) {
			m, fakeHome, cleanup := bwrapFixture(t, Config{
				SessionName:   "repo@main",
				Worktree:      t.TempDir(),
				AllocatedPort: 14010,
				AgentRole:     role,
			})
			defer cleanup()

			// Create agents/ so the stat() would succeed if the code were wrong.
			agentsDir := filepath.Join(fakeHome, ".config", "opencode", "agents")
			if err := os.MkdirAll(agentsDir, 0o755); err != nil {
				t.Fatalf("MkdirAll agents: %v", err)
			}

			b := &bwrapIsolator{name: m.name}
			args := b.BuildArgs(m)

			if hasROBind(args, agentsDir) {
				t.Errorf("AgentRole=%q: agents/ should NOT be bound for review containers, but found as --ro-bind in args: %v", role, args)
			}
		})
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

// TestBwrapBuildArgs_AgentEnvVarsInjected verifies that entries in
// Config.AgentEnvVars (e.g. GIT_EDITOR, KUBECONFIG, AWS_CONFIG_FILE) are
// emitted as --setenv K V in the bwrap arg list.
func TestBwrapBuildArgs_AgentEnvVarsInjected(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		AgentEnvVars: map[string]string{
			"GIT_EDITOR":      "true",
			"KUBECONFIG":      "/home/ben/.config/kube/agents-config",
			"AWS_CONFIG_FILE": "/home/ben/.config/aws/readonly-config",
		},
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	cases := [][2]string{
		{"GIT_EDITOR", "true"},
		{"KUBECONFIG", "/home/ben/.config/kube/agents-config"},
		{"AWS_CONFIG_FILE", "/home/ben/.config/aws/readonly-config"},
	}
	for _, c := range cases {
		if !hasSetenv(args, c[0], c[1]) {
			t.Errorf("--setenv %s %q not found in args: %v", c[0], c[1], args)
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

// ── Terminator: -- opencode --port <port> --hostname 127.0.0.1 ───────────────

func TestBwrapBuildArgs_Terminator(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// Find the -- separator.
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

	tail := args[sepIdx+1:]
	if len(tail) < 5 {
		t.Fatalf("tail after -- too short (%d): %v", len(tail), tail)
	}
	if tail[0] != "opencode" {
		t.Errorf("tail[0] = %q, want opencode", tail[0])
	}
	if tail[1] != "--port" {
		t.Errorf("tail[1] = %q, want --port", tail[1])
	}
	// bwrap binds directly to the per-session AllocatedPort (shared host
	// network namespace — no --publish remap). The fixture sets AllocatedPort
	// = 14010 above; asserting that value confirms the opencode --port flag
	// tracks it rather than the fixed ContainerPort (which would cause every
	// second bwrap session to fail with EADDRINUSE).
	wantPort := "14010"
	if tail[2] != wantPort {
		t.Errorf("tail[2] = %q, want %q", tail[2], wantPort)
	}
	if tail[3] != "--hostname" {
		t.Errorf("tail[3] = %q, want --hostname", tail[3])
	}
	if tail[4] != "127.0.0.1" {
		t.Errorf("tail[4] = %q, want 127.0.0.1", tail[4])
	}
}

// TestBwrapBuildArgs_PortTracksAllocatedPort verifies that the opencode --port
// flag in the bwrap argv reflects cfg.AllocatedPort rather than being
// hardcoded. Two distinct AllocatedPort values must produce two distinct --port
// values in the resulting argv. Regression guard for the bug where every bwrap
// session bound the fixed ContainerPort and the second session silently failed
// with EADDRINUSE (because bwrap shares the host network namespace).
func TestBwrapBuildArgs_PortTracksAllocatedPort(t *testing.T) {
	portOf := func(t *testing.T, allocated int) string {
		t.Helper()
		m, _, cleanup := bwrapFixture(t, Config{
			SessionName:   "repo@main",
			Worktree:      t.TempDir(),
			AllocatedPort: allocated,
		})
		defer cleanup()
		b := &bwrapIsolator{name: m.name}
		args := b.BuildArgs(m)
		// Locate the terminator and return the --port value.
		for i, a := range args {
			if a == "--" && i+3 < len(args) && args[i+1] == "opencode" && args[i+2] == "--port" {
				return args[i+3]
			}
		}
		t.Fatalf("did not find '-- opencode --port <value>' in args: %v", args)
		return ""
	}

	if got := portOf(t, 14010); got != "14010" {
		t.Errorf("AllocatedPort=14010: --port = %q, want \"14010\"", got)
	}
	if got := portOf(t, 14020); got != "14020" {
		t.Errorf("AllocatedPort=14020: --port = %q, want \"14020\"", got)
	}
}

// TestBwrapBuildArgs_PortFallbackWhenUnset verifies that when AllocatedPort is
// 0 (unset — e.g. a malformed session row with no opencode_port in the DB), the
// argv falls back to ContainerPort rather than emitting "--port 0" which would
// make opencode bind to an arbitrary ephemeral port the sidecar can't locate.
func TestBwrapBuildArgs_PortFallbackWhenUnset(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName: "repo@main",
		Worktree:    t.TempDir(),
		// AllocatedPort intentionally left as zero-value.
	})
	defer cleanup()
	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	want := "4096" // ContainerPort — keep in sync with container.ContainerPort.
	for i, a := range args {
		if a == "--" && i+3 < len(args) && args[i+1] == "opencode" && args[i+2] == "--port" {
			if args[i+3] != want {
				t.Errorf("fallback --port = %q, want %q (ContainerPort)", args[i+3], want)
			}
			return
		}
	}
	t.Fatalf("did not find '-- opencode --port <value>' in args: %v", args)
}

func TestBwrapBuildArgs_AgentRoleAndPromptInTail(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		AgentRole:     "worker",
		InitialPrompt: "fix the bug",
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	n := len(args)
	if n < 4 {
		t.Fatalf("too few args (%d): %v", n, args)
	}
	if args[n-4] != "--agent" || args[n-3] != "worker" {
		t.Errorf("expected --agent worker near end, got %q %q", args[n-4], args[n-3])
	}
	if args[n-2] != "--prompt" || args[n-1] != "fix the bug" {
		t.Errorf("expected --prompt 'fix the bug' near end, got %q %q", args[n-2], args[n-1])
	}
}

func TestBwrapBuildArgs_NoAgentNorPromptWhenEmpty(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if hasArg(args, "--agent") {
		t.Errorf("--agent unexpectedly present when AgentRole is empty: %v", args)
	}
	if hasArg(args, "--prompt") {
		t.Errorf("--prompt unexpectedly present when InitialPrompt is empty: %v", args)
	}
}

// ── Hostname is 127.0.0.1 (not 0.0.0.0) ─────────────────────────────────────

func TestBwrapBuildArgs_HostnameIs127(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	for i, a := range args {
		if a == "--hostname" && i+1 < len(args) {
			if args[i+1] != "127.0.0.1" {
				t.Errorf("--hostname = %q, want 127.0.0.1", args[i+1])
			}
		}
	}
}

// ── Edge case: missing host path omitted ─────────────────────────────────────

func TestBwrapBuildArgs_MissingMountOmitted(t *testing.T) {
	// Create a fixture with all the standard paths present, then remove the
	// AWS readonly-config to verify it is omitted from the arg list.
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	// Remove AWS readonly-config — should be absent from bwrap args.
	awsSrc := filepath.Join(fakeHome, ".config", "aws", "readonly-config")
	if err := os.Remove(awsSrc); err != nil {
		t.Fatalf("Remove aws config: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// Neither the src (old Dst==Src form) nor the remapped dst should appear.
	awsDst := filepath.Join(fakeHome, ".aws", "config")
	if hasROBindSrcDst(args, awsSrc, awsDst) {
		t.Errorf("missing AWS readonly-config should be omitted but found as --ro-bind %q %q in args: %v", awsSrc, awsDst, args)
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

	// Neither the src nor the remapped dst should appear.
	kubeDst := filepath.Join(fakeHome, ".kube", "config")
	if hasROBindSrcDst(args, kubeSrc, kubeDst) {
		t.Errorf("missing kube agents-config should be omitted but found as --ro-bind %q %q in args: %v", kubeSrc, kubeDst, args)
	}
}

// ── Canonical path remaps (SRC != DST assertions) ────────────────────────────

// TestBwrapBuildArgs_AWSCredentialsRemapped verifies that the AWS credentials
// file is mounted with the canonical path remap:
// SRC: ~/.config/aws/credentials → DST: $HOME/.aws/credentials
func TestBwrapBuildArgs_AWSCredentialsRemapped(t *testing.T) {
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
	if !hasROBindSrcDst(args, credsSrc, credsDst) {
		t.Errorf("AWS credentials: want --ro-bind %q %q in args: %v", credsSrc, credsDst, args)
	}
}

// TestBwrapBuildArgs_AWSCredentialsMissingOmitted verifies that when
// ~/.config/aws/credentials does not exist, no --ro-bind for it is emitted.
func TestBwrapBuildArgs_AWSCredentialsMissingOmitted(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	// Ensure credentials does NOT exist (bwrapFixture doesn't create it).
	credsSrc := filepath.Join(fakeHome, ".config", "aws", "credentials")
	_ = os.Remove(credsSrc) // ignore error if already absent

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	credsDst := filepath.Join(fakeHome, ".aws", "credentials")
	if hasROBindSrcDst(args, credsSrc, credsDst) {
		t.Errorf("missing AWS credentials should be omitted but found as --ro-bind %q %q in args: %v", credsSrc, credsDst, args)
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

	// Write the opencode config temp file as Create() would.
	if err := os.WriteFile(m.opencodeConfigFilePath(), []byte(`{"model":"claude-3-5-sonnet"}`), 0o600); err != nil {
		t.Fatalf("WriteFile opencode config: %v", err)
	}

	// Create AWS credentials so the conditional bind fires.
	credsSrc := filepath.Join(fakeHome, ".config", "aws", "credentials")
	if err := os.WriteFile(credsSrc, []byte("fake"), 0o600); err != nil {
		t.Fatalf("WriteFile aws credentials: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

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
		{
			name: "opencode.json → $HOME/.config/opencode/opencode.json",
			src:  m.opencodeConfigFilePath(),
			dst:  filepath.Join(fakeHome, ".config", "opencode", "opencode.json"),
		},
		{
			name: "kube agents-config → $HOME/.kube/config",
			src:  filepath.Join(fakeHome, ".config", "kube", "agents-config"),
			dst:  filepath.Join(fakeHome, ".kube", "config"),
		},
		{
			name: "AWS readonly-config → $HOME/.aws/config",
			src:  filepath.Join(fakeHome, ".config", "aws", "readonly-config"),
			dst:  filepath.Join(fakeHome, ".aws", "config"),
		},
		{
			name: "AWS credentials → $HOME/.aws/credentials",
			src:  credsSrc,
			dst:  filepath.Join(fakeHome, ".aws", "credentials"),
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

	// Gitdir fixup files must NOT exist (they are podman-only).
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

	// Write the opencode config temp file.
	if err := os.WriteFile(m.opencodeConfigFilePath(), []byte(m.cfg.ConfigContent), 0o600); err != nil {
		t.Fatalf("WriteFile opencode config: %v", err)
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

	// Tail: opencode --port <AllocatedPort> --hostname 127.0.0.1 --agent worker --prompt ...
	n := len(args)
	if args[n-4] != "--agent" || args[n-3] != "worker" {
		t.Errorf("expected --agent worker near end, got %q %q", args[n-4], args[n-3])
	}
	if args[n-2] != "--prompt" || args[n-1] != "implement the feature" {
		t.Errorf("expected --prompt 'implement the feature' near end, got %q %q", args[n-2], args[n-1])
	}

	// Kube and AWS ro-binds present with correct remapped destinations.
	if !hasROBindSrcDst(args,
		filepath.Join(fakeHome, ".config", "aws", "readonly-config"),
		filepath.Join(fakeHome, ".aws", "config"),
	) {
		t.Errorf("AWS readonly-config: want --ro-bind src $HOME/.aws/config")
	}
	if !hasROBindSrcDst(args,
		filepath.Join(fakeHome, ".config", "kube", "agents-config"),
		filepath.Join(fakeHome, ".kube", "config"),
	) {
		t.Errorf("kube agents-config: want --ro-bind src $HOME/.kube/config")
	}
	if !hasROBindSrcDst(args,
		m.opencodeConfigFilePath(),
		filepath.Join(fakeHome, ".config", "opencode", "opencode.json"),
	) {
		t.Errorf("opencode.json: want --ro-bind src $HOME/.config/opencode/opencode.json")
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
