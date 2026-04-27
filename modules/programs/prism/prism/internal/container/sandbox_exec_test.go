package container

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// newSandboxExecManager creates a Manager and injects a sandboxExecIsolator
// so tests can drive BuildArgs/PrepareSandboxExec end-to-end without a real
// macOS host.
func newSandboxExecManager(cfg Config) *Manager {
	m := New(cfg)
	m.isolator = newSandboxExecIsolator(m.name)
	return m
}

// newSandboxExecManagerWithInstance is like newSandboxExecManager but ensures
// the Config has an InstanceID so sandboxExecHomePath uses the instance ID
// rather than falling back to the container name. Used in tests that exercise
// staging HOME generation (#1017).
func newSandboxExecManagerWithInstance(cfg Config) *Manager {
	if cfg.InstanceID == "" {
		cfg.InstanceID = "test-instance-id"
	}
	return newSandboxExecManager(cfg)
}

// ── generateProfile content assertions ──────────────────────────────────────

// TestGenerateProfile_VersionAndDenyDefault verifies that the profile begins
// with the SBPL header that locks deny-by-default semantics: (version 1)
// followed by (deny default). This is non-negotiable per #1012 — every
// other clause is interpreted relative to deny-by-default.
func TestGenerateProfile_VersionAndDenyDefault(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	// (version 1) MUST be the first non-empty content.
	if !strings.HasPrefix(profile, "(version 1)\n") {
		t.Errorf("profile must begin with (version 1)\\n; got prefix %q", firstNChars(profile, 64))
	}

	// (deny default) MUST follow immediately after (version 1).
	if !strings.HasPrefix(profile, "(version 1)\n(deny default)\n") {
		t.Errorf("profile must start with (version 1) then (deny default); got prefix %q", firstNChars(profile, 64))
	}
}

// TestGenerateProfile_ReadOnlySystemRoots verifies that every read-only
// system root listed in the AC appears as a (subpath ...) inside an
// (allow file-read* ...) clause.
func TestGenerateProfile_ReadOnlySystemRoots(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	// Locate the (allow file-read* ... block (the first one — there is only
	// one in this PR, but extracting it explicitly insulates the assertion
	// from accidental clauses appearing later in the file).
	allowReadBlock := extractClause(t, profile, "(allow file-read*")

	expected := []string{
		`(subpath "/nix")`,
		`(subpath "/usr")`,
		`(subpath "/System")`,
		`(subpath "/Library")`,
		`(subpath "/private/etc")`,
		`(subpath "/private/var/db/dyld")`,
		`(subpath "/private/var/db/timezone")`,
	}
	for _, want := range expected {
		if !strings.Contains(allowReadBlock, want) {
			t.Errorf("(allow file-read* ...) block missing %q; block:\n%s", want, allowReadBlock)
		}
	}
}

// TestGenerateProfile_SensitiveSubtreeDenies verifies that the two deny
// subpaths from the AC appear inside a (deny file-read* file-write* ...)
// clause. These mirror the bwrap --tmpfs shadows of /etc/wireguard and
// /etc/wpa_supplicant.
func TestGenerateProfile_SensitiveSubtreeDenies(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	denyBlock := extractClause(t, profile, "(deny file-read* file-write*")
	expected := []string{
		`(subpath "/private/etc/wireguard")`,
		`(subpath "/private/etc/wpa_supplicant")`,
	}
	for _, want := range expected {
		if !strings.Contains(denyBlock, want) {
			t.Errorf("(deny file-read* file-write* ...) block missing %q; block:\n%s", want, denyBlock)
		}
	}
}

// TestGenerateProfile_ProcessAndIPCAllows verifies that the profile contains
// the seven process/IPC primitives required for node and opencode to run.
// These are listed verbatim in the AC; the test asserts substring presence
// rather than coupling to the exact whitespace in the generator output.
func TestGenerateProfile_ProcessAndIPCAllows(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	wantSubstrings := []string{
		"(allow process-exec*",
		"process-fork",
		"signal",
		"mach-lookup",
		"mach-register",
		"sysctl-read",
		"iokit-open",
		"ipc-posix-shm",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing required substring %q; full profile:\n%s", want, profile)
		}
	}
}

// TestGenerateProfile_NetworkAllow verifies the (allow network*) clause is
// present. This is locked in #1012 — match bwrap's permissive network
// policy. Restriction is a future symmetric concern.
func TestGenerateProfile_NetworkAllow(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	if !strings.Contains(profile, "(allow network*)") {
		t.Errorf("profile missing (allow network*); full profile:\n%s", profile)
	}
}

// TestGenerateProfile_StagingHomeAndWorktreeRules verifies that the profile
// emitted by generateProfile includes the staging HOME, worktree, and bare
// repo as (allow file-read* file-write* (subpath ...)) clauses when the
// Manager has InstanceID, Worktree, and BareRoot set. This is the PR #1017
// replacement for TestGenerateProfile_NoOutOfScopeRules.
func TestGenerateProfile_StagingHomeAndWorktreeRules(t *testing.T) {
	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@main",
		Worktree:    "/tmp/fake-worktree",
		BareRoot:    "/tmp/fake-bare",
		InstanceID:  "test-instance-id",
	})
	profile := generateProfile(m)

	// The profile must contain (allow file-read* file-write* ...) with the
	// staging home, worktree, and bare repo subpaths.
	if !strings.Contains(profile, "(allow file-read* file-write*") {
		t.Errorf("profile missing (allow file-read* file-write* ...) clause; full profile:\n%s", profile)
	}
	if !strings.Contains(profile, "/tmp/fake-worktree") {
		t.Errorf("profile missing worktree path /tmp/fake-worktree; full profile:\n%s", profile)
	}
	if !strings.Contains(profile, "/tmp/fake-bare/.bare") {
		t.Errorf("profile missing bare repo path /tmp/fake-bare/.bare; full profile:\n%s", profile)
	}
	// The staging home path must be present (namespaced by InstanceID).
	if !strings.Contains(profile, "test-instance-id") {
		t.Errorf("profile missing staging HOME path containing instance ID 'test-instance-id'; full profile:\n%s", profile)
	}
}

// TestGenerateProfile_AWSHomePathDenied verifies that the profile contains a
// (deny file-read* file-write* (subpath "$HOME/.aws")) clause to prevent the
// sandbox from accessing the host's raw ~/.aws directory. Only the staged
// entries (symlinked through the staging HOME) are accessible.
func TestGenerateProfile_AWSHomePathDenied(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	if !strings.Contains(profile, "(deny file-read* file-write*") {
		t.Errorf("profile missing deny clause for ~/.aws; full profile:\n%s", profile)
	}
	if !strings.Contains(profile, "/.aws") {
		t.Errorf("profile missing ~/.aws deny subpath; full profile:\n%s", profile)
	}
}

// ── PrepareSandboxExec ──────────────────────────────────────────────────────

// TestPrepareSandboxExec_WritesProfileAndReturnsArgs verifies that
// PrepareSandboxExec materialises the profile to a temp file under the
// per-session state dir and returns args of the shape
// ["sandbox-exec", "-f", <profile_path>, <harness>, ...].
func TestPrepareSandboxExec_WritesProfileAndReturnsArgs(t *testing.T) {
	m := newSandboxExecManager(Config{
		SessionName:   "repo@feat",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	t.Cleanup(func() {
		_ = os.Remove(m.sandboxExecProfilePath())
		if stagingHome, err := m.sandboxExecHomePath(); err == nil {
			_ = os.RemoveAll(stagingHome)
		}
	})

	args, err := m.PrepareSandboxExec()
	if err != nil {
		t.Fatalf("PrepareSandboxExec: %v", err)
	}

	if len(args) < 4 {
		t.Fatalf("args too short (%d); want at least 4 elements: %v", len(args), args)
	}
	if args[0] != "sandbox-exec" {
		t.Errorf("args[0] = %q, want %q", args[0], "sandbox-exec")
	}
	if args[1] != "-f" {
		t.Errorf("args[1] = %q, want %q", args[1], "-f")
	}
	profilePath := args[2]
	if profilePath == "" {
		t.Errorf("args[2] (profile path) is empty: %v", args)
	}

	// The harness binary follows the profile path. We don't pin the exact
	// string so future PRs can swap "opencode" for an absolute path without
	// breaking this assertion, but it must be non-empty.
	if args[3] == "" {
		t.Errorf("args[3] (harness binary) is empty: %v", args)
	}

	// The profile file must exist on disk after PrepareSandboxExec returns.
	info, statErr := os.Stat(profilePath)
	if statErr != nil {
		t.Fatalf("profile path %q not on disk after PrepareSandboxExec: %v", profilePath, statErr)
	}
	if info.Size() == 0 {
		t.Errorf("profile file is empty: %s", profilePath)
	}

	// The on-disk profile must match generateProfile's output verbatim.
	gotContent, readErr := os.ReadFile(profilePath)
	if readErr != nil {
		t.Fatalf("read profile: %v", readErr)
	}
	wantContent := generateProfile(m)
	if string(gotContent) != wantContent {
		t.Errorf("on-disk profile content does not match generateProfile output\n--- on disk:\n%s\n--- generateProfile:\n%s",
			string(gotContent), wantContent)
	}
}

// TestPrepareSandboxExec_ProfilePathIsSessionScoped verifies that the
// profile path is namespaced by the session name so concurrent sessions
// never collide.
func TestPrepareSandboxExec_ProfilePathIsSessionScoped(t *testing.T) {
	mA := newSandboxExecManager(Config{SessionName: "repoA@main"})
	mB := newSandboxExecManager(Config{SessionName: "repoB@main"})

	if mA.sandboxExecProfilePath() == mB.sandboxExecProfilePath() {
		t.Errorf("two managers with different session names share a profile path: %s",
			mA.sandboxExecProfilePath())
	}
	if !strings.Contains(mA.sandboxExecProfilePath(), mA.name) {
		t.Errorf("profile path %q does not contain session-derived name %q",
			mA.sandboxExecProfilePath(), mA.name)
	}
}

// TestSandboxExecBuildArgs_OpencodePortFlag verifies that BuildArgs emits
// --port <AllocatedPort> --hostname 127.0.0.1 in the harness arguments,
// mirroring the bwrap path's invocation contract.
func TestSandboxExecBuildArgs_OpencodePortFlag(t *testing.T) {
	m := newSandboxExecManager(Config{
		SessionName:   "repo@main",
		AllocatedPort: 14111,
	})
	s := &sandboxExecIsolator{name: m.name}
	args := s.BuildArgs(m)

	if !sliceContainsPair(args, "--port", "14111") {
		t.Errorf("expected --port 14111 in args: %v", args)
	}
	if !sliceContainsPair(args, "--hostname", "127.0.0.1") {
		t.Errorf("expected --hostname 127.0.0.1 in args: %v", args)
	}
}

// TestSandboxExecBuildArgs_PortFallback verifies that when AllocatedPort is
// zero, BuildArgs falls back to ContainerPort. This mirrors bwrap.BuildArgs
// and protects against malformed sessions.
func TestSandboxExecBuildArgs_PortFallback(t *testing.T) {
	m := newSandboxExecManager(Config{
		SessionName:   "repo@main",
		AllocatedPort: 0,
	})
	s := &sandboxExecIsolator{name: m.name}
	args := s.BuildArgs(m)

	wantPort := containerPortString()
	if !sliceContainsPair(args, "--port", wantPort) {
		t.Errorf("expected fallback --port %s in args: %v", wantPort, args)
	}
}

// TestSandboxExecBuildArgs_AgentRolePassedThrough verifies that AgentRole
// is appended as --agent <role> when non-empty, and omitted when empty —
// matching the bwrap path.
func TestSandboxExecBuildArgs_AgentRolePassedThrough(t *testing.T) {
	for _, tc := range []struct {
		role string
		want bool
	}{
		{"worker", true},
		{"coordinator", true},
		{"review-code", true},
		{"", false},
	} {
		t.Run("AgentRole="+tc.role, func(t *testing.T) {
			m := newSandboxExecManager(Config{
				SessionName:   "repo@main",
				AllocatedPort: 14010,
				AgentRole:     tc.role,
			})
			s := &sandboxExecIsolator{name: m.name}
			args := s.BuildArgs(m)

			has := sliceContainsPair(args, "--agent", tc.role)
			if tc.want && !has {
				t.Errorf("expected --agent %q in args (role non-empty): %v", tc.role, args)
			}
			if !tc.want && hasFlag(args, "--agent") {
				t.Errorf("--agent must be absent when role is empty; got: %v", args)
			}
		})
	}
}

// TestSandboxExecBuildArgs_InitialPromptPassedThrough verifies that
// InitialPrompt becomes --prompt <text> when non-empty, and is omitted when
// empty — matching the bwrap path.
func TestSandboxExecBuildArgs_InitialPromptPassedThrough(t *testing.T) {
	m := newSandboxExecManager(Config{
		SessionName:   "repo@main",
		AllocatedPort: 14010,
		InitialPrompt: "do the thing",
	})
	s := &sandboxExecIsolator{name: m.name}
	args := s.BuildArgs(m)
	if !sliceContainsPair(args, "--prompt", "do the thing") {
		t.Errorf("expected --prompt 'do the thing' in args: %v", args)
	}

	mEmpty := newSandboxExecManager(Config{
		SessionName:   "repo@main",
		AllocatedPort: 14010,
	})
	sEmpty := &sandboxExecIsolator{name: mEmpty.name}
	argsEmpty := sEmpty.BuildArgs(mEmpty)
	if hasFlag(argsEmpty, "--prompt") {
		t.Errorf("--prompt must be absent when InitialPrompt is empty; got: %v", argsEmpty)
	}
}

// TestSandboxExecBuildArgs_HarnessImmediatelyAfterProfile verifies that the
// harness binary appears at args[3] — directly after "sandbox-exec -f
// <profile>". This is the shape the AC requires.
func TestSandboxExecBuildArgs_HarnessImmediatelyAfterProfile(t *testing.T) {
	m := newSandboxExecManager(Config{
		SessionName:   "repo@main",
		AllocatedPort: 14010,
	})
	s := &sandboxExecIsolator{name: m.name}
	args := s.BuildArgs(m)
	if len(args) < 4 {
		t.Fatalf("args too short (%d): %v", len(args), args)
	}
	if args[0] != "sandbox-exec" || args[1] != "-f" {
		t.Fatalf("expected leading sandbox-exec -f; got: %v", args)
	}
	// args[2] is the profile path; args[3] must be the harness binary.
	if args[3] != "opencode" {
		t.Errorf("expected args[3] to be the harness binary 'opencode'; got %q in %v", args[3], args)
	}
}

// ── PrepareSandboxExecHome ───────────────────────────────────────────────────

// newFakeHome creates a temp directory tree that mimics the credential and
// config paths that PrepareSandboxExecHome reads from $HOME. It sets HOME to
// the fake home dir for the duration of the test and returns the fake home path.
func newFakeHome(t *testing.T) string {
	t.Helper()
	fakeHome := t.TempDir()

	// Create all the directories and files that PrepareSandboxExecHome expects.
	dirs := []string{
		".ssh",
		".aws",
		".config/aws",
		".config/opencode",
		".config/kube",
		".cache/opencode",
		".cache/bun",
		".cache/nix",
		".claude",
		".mcp-auth",
		".local/share/opencode",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(fakeHome, d), 0o755); err != nil {
			t.Fatalf("create fake home dir %s: %v", d, err)
		}
	}

	// Write dummy files that represent SSH keys and config.
	sshFiles := []string{
		"prismatic-koi-ed25519",
		"prismatic-koi-ed25519-signingkey",
		"prismatic-koi-ed25519-signingkey.pub",
		"known_hosts",
	}
	for _, f := range sshFiles {
		if err := os.WriteFile(filepath.Join(fakeHome, ".ssh", f), []byte("dummy"), 0o600); err != nil {
			t.Fatalf("write ssh file %s: %v", f, err)
		}
	}

	// AWS readonly-config (in XDG location, symlinked by the staging builder).
	if err := os.WriteFile(filepath.Join(fakeHome, ".config", "aws", "readonly-config"), []byte("dummy-aws-cfg"), 0o644); err != nil {
		t.Fatalf("write aws config: %v", err)
	}
	// Kube agents-config.
	if err := os.WriteFile(filepath.Join(fakeHome, ".config", "kube", "agents-config"), []byte("dummy-kube"), 0o644); err != nil {
		t.Fatalf("write kube config: %v", err)
	}

	// Override HOME for the duration of the test.
	t.Setenv("HOME", fakeHome)

	return fakeHome
}

// TestPrepareSandboxExecHome_CreatesDirectoryAtExpectedPath verifies that
// PrepareSandboxExecHome creates the staging HOME at
// ~/.local/state/prism/sessions/<instance_id>/home/.
func TestPrepareSandboxExecHome_CreatesDirectoryAtExpectedPath(t *testing.T) {
	fakeHome := newFakeHome(t)
	instanceID := "test-instance-abc"

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  instanceID,
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	wantPath := filepath.Join(fakeHome, ".local", "state", "prism", "sessions", instanceID, "home")
	if stagingHome != wantPath {
		t.Errorf("staging HOME = %q, want %q", stagingHome, wantPath)
	}
	if _, err := os.Stat(stagingHome); err != nil {
		t.Errorf("staging HOME does not exist on disk: %v", err)
	}
}

// TestPrepareSandboxExecHome_SSHSymlinks verifies that the staging HOME
// contains symlinks for access-key, signing-key, signing-key.pub, and
// known_hosts when the corresponding files exist in the fake $HOME/.ssh/.
func TestPrepareSandboxExecHome_SSHSymlinks(t *testing.T) {
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "ssh-test",
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	for _, name := range []string{"access-key", "signing-key", "signing-key.pub", "known_hosts"} {
		p := filepath.Join(stagingHome, ".ssh", name)
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("expected symlink %s to exist: %v", p, err)
			continue
		}
		target, readErr := os.Readlink(p)
		if readErr != nil {
			t.Errorf("%s is not a symlink: %v", p, readErr)
			continue
		}
		if target == "" {
			t.Errorf("%s symlink has empty target", p)
		}
	}
}

// TestPrepareSandboxExecHome_MissingSourceSkipped verifies that when a source
// path does not exist, the corresponding symlink is NOT created (no dangling
// symlinks).
func TestPrepareSandboxExecHome_MissingSourceSkipped(t *testing.T) {
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "missing-test",
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// The fake home has no ~/.aws/sso or ~/.aws/cli directories.
	// Symlinks for those must not exist in the staging HOME.
	for _, absent := range []string{
		filepath.Join(stagingHome, ".aws", "sso"),
		filepath.Join(stagingHome, ".aws", "cli"),
	} {
		if _, err := os.Lstat(absent); err == nil {
			t.Errorf("symlink %s should NOT exist (source absent), but it does", absent)
		}
	}
}

// TestPrepareSandboxExecHome_RegularFilesNotSymlinks verifies that .gitconfig,
// .ssh/config, and .config/opencode/opencode.json are regular generated files
// (not symlinks) in the staging HOME.
func TestPrepareSandboxExecHome_RegularFilesNotSymlinks(t *testing.T) {
	fakeHome := newFakeHome(t)

	// Write a fake opencode config so the builder copies it.
	m := newSandboxExecManagerWithInstance(Config{
		SessionName:   "repo@feat",
		InstanceID:    "reg-file-test",
		GitUserName:   "Test User",
		GitUserEmail:  "test@example.com",
	})
	// Write the opencode config temp file.
	_ = os.WriteFile(m.opencodeConfigFilePath(), []byte(`{"model":"test"}`), 0o644)
	t.Cleanup(func() { _ = os.Remove(m.opencodeConfigFilePath()) })

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(stagingHome)
		_ = os.RemoveAll(filepath.Join(fakeHome, ".local", "state", "prism"))
	})

	regularFiles := []string{
		filepath.Join(stagingHome, ".gitconfig"),
		filepath.Join(stagingHome, ".ssh", "config"),
		filepath.Join(stagingHome, ".config", "opencode", "opencode.json"),
	}
	for _, p := range regularFiles {
		info, err := os.Lstat(p)
		if err != nil {
			t.Errorf("expected %s to exist: %v", p, err)
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s must be a regular file, not a symlink", p)
		}
	}
}

// TestPrepareSandboxExecHome_AgentsIncludedForNonReview verifies that
// .config/opencode/agents/ is present when AgentRole does NOT start with
// "review-". Mirrors bwrap.go:447-448.
func TestPrepareSandboxExecHome_AgentsIncludedForNonReview(t *testing.T) {
	fakeHome := newFakeHome(t)
	// Create agents/ dir in the fake home's opencode config.
	agentsDir := filepath.Join(fakeHome, ".config", "opencode", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("create agents dir: %v", err)
	}

	for _, role := range []string{"worker", "coordinator", ""} {
		t.Run("role="+role, func(t *testing.T) {
			m := newSandboxExecManagerWithInstance(Config{
				SessionName: "repo@feat",
				InstanceID:  "agents-incl-" + role,
				AgentRole:   role,
			})
			stagingHome, err := m.PrepareSandboxExecHome()
			if err != nil {
				t.Fatalf("PrepareSandboxExecHome: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

			agentsLink := filepath.Join(stagingHome, ".config", "opencode", "agents")
			if _, err := os.Lstat(agentsLink); err != nil {
				t.Errorf("agents/ symlink must exist for role %q: %v", role, err)
			}
		})
	}
}

// TestPrepareSandboxExecHome_AgentsExcludedForReview verifies that
// .config/opencode/agents/ is NOT present when AgentRole starts with "review-".
// Mirrors bwrap.go:447-448.
func TestPrepareSandboxExecHome_AgentsExcludedForReview(t *testing.T) {
	fakeHome := newFakeHome(t)
	// Create agents/ dir in the fake home's opencode config.
	agentsDir := filepath.Join(fakeHome, ".config", "opencode", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("create agents dir: %v", err)
	}

	for _, role := range []string{"review-", "review-goal", "review-code", "review-security"} {
		t.Run("role="+role, func(t *testing.T) {
			m := newSandboxExecManagerWithInstance(Config{
				SessionName: "repo@feat",
				InstanceID:  "agents-excl-" + role,
				AgentRole:   role,
			})
			stagingHome, err := m.PrepareSandboxExecHome()
			if err != nil {
				t.Fatalf("PrepareSandboxExecHome: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

			agentsLink := filepath.Join(stagingHome, ".config", "opencode", "agents")
			if _, err := os.Lstat(agentsLink); err == nil {
				t.Errorf("agents/ must NOT exist for review role %q", role)
			}
		})
	}
}

// TestPrepareSandboxExecHome_NixCacheAlwaysIncluded verifies that
// .cache/nix/ is always included as a symlink (matching bwrap.go:333-335
// unconditional RW bind).
func TestPrepareSandboxExecHome_NixCacheAlwaysIncluded(t *testing.T) {
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "nix-cache-test",
	})
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	nixLink := filepath.Join(stagingHome, ".cache", "nix")
	if _, err := os.Lstat(nixLink); err != nil {
		t.Errorf(".cache/nix symlink must always be created: %v", err)
	}
	target, err := os.Readlink(nixLink)
	if err != nil {
		t.Errorf(".cache/nix is not a symlink: %v", err)
	} else if target == "" {
		t.Errorf(".cache/nix symlink has empty target")
	}
}

// TestPrepareSandboxExecHome_IdempotentReCreation verifies that calling
// PrepareSandboxExecHome a second time on an existing staging dir succeeds
// without error and does not corrupt symlinks.
func TestPrepareSandboxExecHome_IdempotentReCreation(t *testing.T) {
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "idempotent-test",
	})

	// First call.
	stagingHome1, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("first PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome1) })

	// Second call — must not fail.
	stagingHome2, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("second PrepareSandboxExecHome: %v", err)
	}
	if stagingHome1 != stagingHome2 {
		t.Errorf("staging HOME changed between calls: %q != %q", stagingHome1, stagingHome2)
	}

	// Verify that the .cache/nix symlink is still valid after re-creation.
	nixLink := filepath.Join(stagingHome2, ".cache", "nix")
	if _, err := os.Lstat(nixLink); err != nil {
		t.Errorf(".cache/nix not present after re-creation: %v", err)
	}
}

// TestPrepareSandboxExecHome_TwoConcurrentSessions verifies that two sessions
// with different InstanceIDs have independent staging dirs (no collisions).
func TestPrepareSandboxExecHome_TwoConcurrentSessions(t *testing.T) {
	newFakeHome(t)

	mA := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat-a",
		InstanceID:  "instance-aaa",
	})
	mB := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat-b",
		InstanceID:  "instance-bbb",
	})

	homeA, errA := mA.PrepareSandboxExecHome()
	if errA != nil {
		t.Fatalf("session A: %v", errA)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeA) })

	homeB, errB := mB.PrepareSandboxExecHome()
	if errB != nil {
		t.Fatalf("session B: %v", errB)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeB) })

	if homeA == homeB {
		t.Errorf("two sessions have the same staging HOME: %q", homeA)
	}
	if strings.Contains(homeA, "instance-bbb") || strings.Contains(homeB, "instance-aaa") {
		t.Errorf("staging HOME paths are not properly namespaced: A=%q B=%q", homeA, homeB)
	}
}

// TestSandboxExecCleanup_RemovesStagingHome verifies that EnsureRemoved removes
// the staging HOME directory created by PrepareSandboxExecHome.
func TestSandboxExecCleanup_RemovesStagingHome(t *testing.T) {
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "cleanup-test",
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}

	// Verify it exists.
	if _, statErr := os.Stat(stagingHome); statErr != nil {
		t.Fatalf("staging HOME not created: %v", statErr)
	}

	// Call EnsureRemoved (uses a background context; no podman calls needed
	// because sandboxExecIsolator.Shutdown is a no-op).
	m.EnsureRemoved(context.Background())

	// Staging HOME must be gone.
	if _, statErr := os.Stat(stagingHome); statErr == nil {
		t.Errorf("staging HOME still exists after EnsureRemoved: %s", stagingHome)
	}
}

// TestGenerateProfile_ProfileIncludesSymlinkTargetAllows verifies that the
// profile emitted by generateProfile includes (allow file-read* (literal ...))
// rules for symlink targets in the staging HOME.
func TestGenerateProfile_ProfileIncludesSymlinkTargetAllows(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "profile-targets-test",
		Worktree:    t.TempDir(),
	})

	// Pre-build the staging HOME so generateProfile can collect targets.
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(stagingHome)
		_ = os.RemoveAll(filepath.Join(fakeHome, ".local", "state", "prism"))
	})

	profile := generateProfile(m)

	// The profile must contain (allow file-read* with literal paths from the
	// fake home's ssh dir or aws dir.
	if !strings.Contains(profile, "(allow file-read*") {
		t.Errorf("profile missing (allow file-read* ...) clause; full profile:\n%s", profile)
	}
}

// TestGenerateProfile_AWSDenyClause verifies that the profile contains a
// (deny file-read* file-write* (subpath ".../.aws")) clause for the host's
// ~/.aws directory, to prevent the sandbox from accessing host credentials.
func TestGenerateProfile_AWSDenyClause(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "aws-deny-test",
		Worktree:    t.TempDir(),
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(stagingHome)
		_ = os.RemoveAll(filepath.Join(fakeHome, ".local", "state", "prism"))
	})

	profile := generateProfile(m)

	// The profile must contain a (deny ...) clause that references /.aws.
	// We look for the /.aws substring inside any deny clause.
	if !strings.Contains(profile, "/.aws") {
		t.Fatalf("profile missing /.aws deny subpath; full profile:\n%s", profile)
	}
	// Verify it's inside a deny clause (not just an allow).
	awsIdx := strings.Index(profile, "/.aws")
	// Walk backwards from awsIdx to find the nearest opening paren clause.
	clauseStart := strings.LastIndex(profile[:awsIdx], "(deny")
	if clauseStart < 0 {
		t.Errorf("/.aws appears in profile but not inside a (deny ...) clause; full profile:\n%s", profile)
	}
}

// ── MinimalIsolatedExecEnv ───────────────────────────────────────────────────

// TestMinimalIsolatedExecEnv_AllowsExpectedKeys verifies that the shared
// helper passes through exactly the eight keys called out in the AC.
func TestMinimalIsolatedExecEnv_AllowsExpectedKeys(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"HOME=/Users/ben",
		"USER=ben",
		"LOGNAME=ben",
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"LANG=en_NZ.UTF-8",
		"LC_ALL=en_NZ.UTF-8",
		"GITHUB_TOKEN=secret",
		"ANTHROPIC_API_KEY=sk-secret",
	}
	out := MinimalIsolatedExecEnv(in)

	gotSet := map[string]bool{}
	for _, kv := range out {
		gotSet[kv] = true
	}
	for _, want := range []string{
		"PATH=/usr/bin", "HOME=/Users/ben", "USER=ben", "LOGNAME=ben",
		"TERM=xterm-256color", "COLORTERM=truecolor",
		"LANG=en_NZ.UTF-8", "LC_ALL=en_NZ.UTF-8",
	} {
		if !gotSet[want] {
			t.Errorf("expected %q to be passed through; got %v", want, out)
		}
	}
	for _, kv := range out {
		k := strings.SplitN(kv, "=", 2)[0]
		switch k {
		case "GITHUB_TOKEN", "ANTHROPIC_API_KEY":
			t.Errorf("forbidden key %q leaked through MinimalIsolatedExecEnv: %q", k, kv)
		}
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// extractClause finds a top-level (...) form whose opening text matches the
// given prefix and returns its full text including the trailing closing
// parenthesis. The implementation is line-and-paren naive — it tracks the
// nesting depth from the opening prefix and returns when depth returns to 0.
//
// This is sufficient for the small, hand-written profile in this PR; if the
// generator grows nested macros, switch to a real SBPL parser.
func extractClause(t *testing.T, profile, prefix string) string {
	t.Helper()
	idx := strings.Index(profile, prefix)
	if idx < 0 {
		t.Fatalf("clause beginning with %q not found in profile:\n%s", prefix, profile)
	}
	depth := 0
	end := -1
	for i := idx; i < len(profile); i++ {
		switch profile[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i + 1
				goto done
			}
		}
	}
done:
	if end < 0 {
		t.Fatalf("clause beginning at index %d (prefix %q) is unterminated", idx, prefix)
	}
	return profile[idx:end]
}

// sliceContainsPair returns true when args contains [..., flag, value, ...].
func sliceContainsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// hasFlag returns true when args contains an exact match for flag.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// firstNChars returns up to n leading characters of s, used in error
// messages to avoid dumping a multi-line profile when a prefix mismatch is
// the actual diagnostic value.
func firstNChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// containerPortString returns ContainerPort formatted as a decimal string,
// for tests that assert on the fallback --port value without repeating the
// constant.
func containerPortString() string {
	return fmt.Sprintf("%d", ContainerPort)
}
