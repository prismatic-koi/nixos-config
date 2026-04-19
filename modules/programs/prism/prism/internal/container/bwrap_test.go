package container

import (
	"fmt"
	"os"
	"path/filepath"
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
		filepath.Join(fakeHome, ".local", "share", "opencode", "prism-sessions"),
		filepath.Join(fakeHome, ".cache", "nix"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("fixture: MkdirAll %q: %v", d, err)
		}
	}

	// Ensure the opencode session dir exists (named after the container).
	containerN := containerName(cfg.SessionName)
	sessionDir := filepath.Join(fakeHome, ".local", "share", "opencode", "prism-sessions", containerN)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("fixture: MkdirAll session dir: %v", err)
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

	// The first 8 elements must be the baseline namespace flags in order.
	want := []string{
		"--unshare-pid",
		"--unshare-ipc",
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

func TestBwrapBuildArgs_OpencodeSessionDirBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	sessionDir := filepath.Join(fakeHome, ".local", "share", "opencode", "prism-sessions", m.name)
	if !hasBind(args, sessionDir) {
		t.Errorf("opencode session dir %q not found as --bind SRC SRC in args: %v", sessionDir, args)
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

	awsConfig := filepath.Join(fakeHome, ".config", "aws", "readonly-config")
	if !hasROBind(args, awsConfig) {
		t.Errorf("AWS readonly-config %q not found as --ro-bind SRC SRC in args: %v", awsConfig, args)
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

	kubeConfig := filepath.Join(fakeHome, ".config", "kube", "agents-config")
	if !hasROBind(args, kubeConfig) {
		t.Errorf("kube agents-config %q not found as --ro-bind SRC SRC in args: %v", kubeConfig, args)
	}
}

func TestBwrapBuildArgs_SSHAccessKeyROBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	accessKey := filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519")
	if !hasROBind(args, accessKey) {
		t.Errorf("SSH access key %q not found as --ro-bind SRC SRC in args: %v", accessKey, args)
	}
}

func TestBwrapBuildArgs_SSHSigningKeyROBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	signingKey := filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519-signingkey")
	signingKeyPub := filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519-signingkey.pub")
	if !hasROBind(args, signingKey) {
		t.Errorf("SSH signing key (private) %q not found as --ro-bind SRC SRC in args: %v", signingKey, args)
	}
	if !hasROBind(args, signingKeyPub) {
		t.Errorf("SSH signing key (public) %q not found as --ro-bind SRC SRC in args: %v", signingKeyPub, args)
	}
}

func TestBwrapBuildArgs_KnownHostsROBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	knownHosts := filepath.Join(fakeHome, ".ssh", "known_hosts")
	if !hasROBind(args, knownHosts) {
		t.Errorf("known_hosts %q not found as --ro-bind SRC SRC in args: %v", knownHosts, args)
	}
}

func TestBwrapBuildArgs_GeneratedSSHConfigROBound(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	sshConfigPath := m.sshConfigFilePath()
	if !hasROBind(args, sshConfigPath) {
		t.Errorf("generated SSH config %q not found as --ro-bind SRC SRC in args: %v", sshConfigPath, args)
	}
}

func TestBwrapBuildArgs_GeneratedGitconfigROBound(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	gitconfigPath := m.gitconfigFilePath()
	if !hasROBind(args, gitconfigPath) {
		t.Errorf("generated gitconfig %q not found as --ro-bind SRC SRC in args: %v", gitconfigPath, args)
	}
}

func TestBwrapBuildArgs_OpencodeJSONROBound(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		ConfigContent: `{"model":"claude-3-5-sonnet"}`,
	})
	defer cleanup()

	// Write the opencode config temp file as Create() would.
	if err := os.WriteFile(m.opencodeConfigFilePath(), []byte(m.cfg.ConfigContent), 0o600); err != nil {
		t.Fatalf("WriteFile opencode config: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	configPath := m.opencodeConfigFilePath()
	if !hasROBind(args, configPath) {
		t.Errorf("opencode.json %q not found as --ro-bind SRC SRC in args: %v", configPath, args)
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

func TestBwrapBuildArgs_TermSetenv(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasSetenv(args, "TERM", "xterm-256color") {
		t.Errorf("--setenv TERM xterm-256color not found in args: %v", args)
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
	wantPort := fmt.Sprintf("%d", ContainerPort)
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
	awsConfig := filepath.Join(fakeHome, ".config", "aws", "readonly-config")
	if err := os.Remove(awsConfig); err != nil {
		t.Fatalf("Remove aws config: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if hasROBind(args, awsConfig) {
		t.Errorf("missing AWS readonly-config should be omitted but found as --ro-bind in args: %v", args)
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

	kubeConfig := filepath.Join(fakeHome, ".config", "kube", "agents-config")
	if err := os.Remove(kubeConfig); err != nil {
		t.Fatalf("Remove kube agents-config: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if hasROBind(args, kubeConfig) {
		t.Errorf("missing kube agents-config should be omitted but found as --ro-bind in args: %v", args)
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

	// Baseline flags present.
	for _, flag := range []string{"--unshare-pid", "--unshare-ipc", "--unshare-uts", "--die-with-parent"} {
		if !hasArg(args, flag) {
			t.Errorf("baseline flag %q missing from args", flag)
		}
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

	// Tail: opencode --port 4096 --hostname 127.0.0.1 --agent worker --prompt ...
	n := len(args)
	if args[n-4] != "--agent" || args[n-3] != "worker" {
		t.Errorf("expected --agent worker near end, got %q %q", args[n-4], args[n-3])
	}
	if args[n-2] != "--prompt" || args[n-1] != "implement the feature" {
		t.Errorf("expected --prompt 'implement the feature' near end, got %q %q", args[n-2], args[n-1])
	}

	// Kube and AWS ro-binds present.
	if !hasROBind(args, filepath.Join(fakeHome, ".config", "aws", "readonly-config")) {
		t.Errorf("AWS readonly-config --ro-bind missing")
	}
	if !hasROBind(args, filepath.Join(fakeHome, ".config", "kube", "agents-config")) {
		t.Errorf("kube agents-config --ro-bind missing")
	}
	if !hasROBind(args, m.opencodeConfigFilePath()) {
		t.Errorf("opencode.json --ro-bind missing")
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
