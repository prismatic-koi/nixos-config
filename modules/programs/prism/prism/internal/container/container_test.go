package container

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── containerName tests ──────────────────────────────────────────────────────

func TestContainerName_ReplacesAt(t *testing.T) {
	name := containerName("nixos-config@feature")
	if !strings.HasPrefix(name, "prism-") {
		t.Errorf("container name %q should start with prism-", name)
	}
	if strings.Contains(name, "@") {
		t.Errorf("container name %q should not contain @", name)
	}
}

func TestContainerName_ReplacesSlash(t *testing.T) {
	name := containerName("repo@feat/sub")
	if strings.Contains(name, "/") {
		t.Errorf("container name %q should not contain /", name)
	}
}

func TestContainerName_ReplacesDot(t *testing.T) {
	name := containerName("repo.git@main")
	if strings.Contains(name, ".") {
		t.Errorf("container name %q should not contain .", name)
	}
}

func TestContainerName_Deterministic(t *testing.T) {
	a := containerName("nixos-config@main")
	b := containerName("nixos-config@main")
	if a != b {
		t.Errorf("containerName is not deterministic: %q != %q", a, b)
	}
}

// ── NameForSession tests ─────────────────────────────────────────────────────

func TestNameForSession_ReplacesAt(t *testing.T) {
	name := NameForSession("nixos-config@feature")
	want := "prism-nixos-config-feature"
	if name != want {
		t.Errorf("NameForSession(%q) = %q, want %q", "nixos-config@feature", name, want)
	}
}

func TestNameForSession_ReplacesSlash(t *testing.T) {
	name := NameForSession("repo@feat/sub")
	want := "prism-repo-feat-sub"
	if name != want {
		t.Errorf("NameForSession(%q) = %q, want %q", "repo@feat/sub", name, want)
	}
}

func TestNameForSession_ReplacesDot(t *testing.T) {
	name := NameForSession("repo.git@main")
	want := "prism-repo-git-main"
	if name != want {
		t.Errorf("NameForSession(%q) = %q, want %q", "repo.git@main", name, want)
	}
}

func TestNameForSession_MatchesContainerName(t *testing.T) {
	sessions := []string{
		"nixos-config@main",
		"repo@feat/sub",
		"repo.git@main",
		"a@b/c.d",
	}
	for _, s := range sessions {
		exported := NameForSession(s)
		unexported := containerName(s)
		if exported != unexported {
			t.Errorf("NameForSession(%q) = %q, containerName(%q) = %q — must be identical",
				s, exported, s, unexported)
		}
	}
}

// ── New tests ────────────────────────────────────────────────────────────────

func TestNew_SetsName(t *testing.T) {
	m := New(Config{
		SessionName:   "nixos-config@feature",
		AllocatedPort: 14001,
	})
	want := containerName("nixos-config@feature")
	if m.Name() != want {
		t.Errorf("Name() = %q, want %q", m.Name(), want)
	}
}

func TestNew_DefaultsHTTPClient(t *testing.T) {
	m := New(Config{SessionName: "repo@branch", AllocatedPort: 14000})
	if m.httpClient == nil {
		t.Error("httpClient should not be nil after New()")
	}
}

func TestNew_UsesProvidedHTTPClient(t *testing.T) {
	custom := &http.Client{Timeout: 99 * time.Second}
	m := New(Config{
		SessionName:   "repo@branch",
		AllocatedPort: 14000,
		HTTPClient:    custom,
	})
	if m.httpClient != custom {
		t.Error("expected provided HTTPClient to be used")
	}
}

// ── buildRunArgs tests ───────────────────────────────────────────────────────

func TestBuildRunArgs_ContainerPortLocalhostOnly(t *testing.T) {
	m := New(Config{
		SessionName:   "my-repo@feat",
		AllocatedPort: 14005,
	})
	args := m.buildRunArgs()

	// Find the --publish argument value.
	var portArg string
	for i, arg := range args {
		if arg == "--publish" && i+1 < len(args) {
			portArg = args[i+1]
			break
		}
	}
	if portArg == "" {
		t.Fatal("--publish flag not found in podman run args")
	}
	if !strings.HasPrefix(portArg, "127.0.0.1:14005:") {
		t.Errorf("port binding %q should start with 127.0.0.1:14005:", portArg)
	}
	if strings.HasPrefix(portArg, "0.0.0.0") {
		t.Errorf("port binding %q must not bind to 0.0.0.0", portArg)
	}
}

func TestBuildRunArgs_ContainerInnerPort4096(t *testing.T) {
	m := New(Config{
		SessionName:   "my-repo@feat",
		AllocatedPort: 14005,
	})
	args := m.buildRunArgs()

	var portArg string
	for i, arg := range args {
		if arg == "--publish" && i+1 < len(args) {
			portArg = args[i+1]
			break
		}
	}
	if !strings.HasSuffix(portArg, ":4096") {
		t.Errorf("port binding %q should end with :4096 (container port)", portArg)
	}
}

func TestBuildRunArgs_WorktreeMountedAtWorkspace(t *testing.T) {
	m := New(Config{
		SessionName:   "my-repo@feat",
		Worktree:      "/home/user/code/my-repo/feat",
		AllocatedPort: 14000,
	})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "/home/user/code/my-repo/feat:/workspace") {
				found = true
				// Must have :Z SELinux label (AC-2).
				if !strings.HasSuffix(v, ":Z") {
					t.Errorf("worktree mount %q is missing :Z SELinux label", v)
				}
				break
			}
		}
	}
	if !found {
		t.Errorf("worktree volume mount not found in args: %v", args)
	}
}

func TestBuildRunArgs_OpencodeStateMountedAtContainerPath(t *testing.T) {
	// AC-3: opencode state mounted read-write into /root/.local/share/opencode
	// with :Z SELinux label.
	m := New(Config{
		SessionName:   "my-repo@feat",
		AllocatedPort: 14000,
	})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, ":/root/.local/share/opencode") {
				found = true
				if !strings.HasSuffix(v, ":Z") {
					t.Errorf("opencode state mount %q should have :Z SELinux label (AC-3)", v)
				}
				// Must be read-write (no :ro).
				if strings.Contains(v, ":ro") {
					t.Errorf("opencode state mount %q should be read-write, not :ro (AC-3)", v)
				}
				break
			}
		}
	}
	if !found {
		t.Errorf("opencode state volume mount not found in args: %v", args)
	}
}

func TestBuildRunArgs_WorkdirIsWorkspace(t *testing.T) {
	m := New(Config{
		SessionName:   "my-repo@feat",
		Worktree:      "/home/user/code/my-repo/feat",
		AllocatedPort: 14000,
	})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--workdir" && i+1 < len(args) && args[i+1] == "/workspace" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("--workdir /workspace not found in args: %v", args)
	}
}

func TestBuildRunArgs_OpencodeServeCommand(t *testing.T) {
	m := New(Config{SessionName: "repo@main", AllocatedPort: 14000})
	args := m.buildRunArgs()

	// The last elements should be:
	// <image> opencode serve --port 4096 --hostname 0.0.0.0
	// That is 7 elements from the end.
	n := len(args)
	if n < 7 {
		t.Fatalf("too few args (%d): %v", n, args)
	}
	if args[n-7] != Image {
		t.Errorf("expected image %q at args[n-7], got %q (all args: %v)", Image, args[n-7], args)
	}
	if args[n-6] != "opencode" || args[n-5] != "serve" {
		t.Errorf("expected 'opencode serve', got %q %q", args[n-6], args[n-5])
	}
	if args[n-4] != "--port" || args[n-3] != "4096" {
		t.Errorf("expected '--port 4096', got %q %q", args[n-4], args[n-3])
	}
	if args[n-2] != "--hostname" || args[n-1] != "0.0.0.0" {
		t.Errorf("expected '--hostname 0.0.0.0', got %q %q", args[n-2], args[n-1])
	}
}

func TestBuildRunArgs_ContainerNameSet(t *testing.T) {
	m := New(Config{SessionName: "my-repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--name" && i+1 < len(args) {
			if args[i+1] == m.Name() {
				found = true
			}
			break
		}
	}
	if !found {
		t.Errorf("--name %q not found in args: %v", m.Name(), args)
	}
}

func TestBuildRunArgs_NetworkPasta(t *testing.T) {
	// AC-12: explicit network mode for isolation.
	m := New(Config{SessionName: "repo@br", AllocatedPort: 14000})
	args := m.buildRunArgs()
	found := false
	for i, arg := range args {
		if arg == "--network" && i+1 < len(args) && args[i+1] == "pasta" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("--network pasta not found in args: %v", args)
	}
}

func TestBuildRunArgs_DetachedFlag(t *testing.T) {
	m := New(Config{SessionName: "repo@br", AllocatedPort: 14000})
	args := m.buildRunArgs()
	found := false
	for _, arg := range args {
		if arg == "--detach" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("--detach not found in args: %v", args)
	}
}

func TestBuildRunArgs_PluginsDirMountedViaAllowlist(t *testing.T) {
	// The plugins/ directory is mounted read-write (not via the ro allowlist)
	// so plugin packages can write back runtime state.
	fakeHome := t.TempDir()
	pluginsDir := filepath.Join(fakeHome, ".config", "opencode", "plugins")
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll pluginsDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginsDir, "prism-hooks.ts"), []byte("stub"), 0o644); err != nil {
		t.Fatalf("WriteFile prism-hooks.ts: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
	})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--mount" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "dst=/root/.config/opencode/plugins") {
				found = true
				// Must be read-write — plugin packages write runtime state.
				if strings.Contains(v, ",ro") {
					t.Errorf("plugins dir mount %q must be read-write (no ,ro)", v)
				}
				break
			}
		}
	}
	if !found {
		t.Errorf("plugins dir not mounted; args: %v", args)
	}
}

func TestBuildRunArgs_GitMountsWhenBareRootSet(t *testing.T) {
	m := New(Config{
		SessionName:    "repo@feat",
		Worktree:       "/home/user/code/my-repo/feat",
		AllocatedPort:  14000,
		BareRoot:       "/home/user/code/my-repo",
		WorktreeGitDir: "/home/user/code/my-repo/.bare/worktrees/feat",
	})
	args := m.buildRunArgs()
	var mounts []string
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			mounts = append(mounts, args[i+1])
		}
	}
	foundBareRepo := false
	for _, v := range mounts {
		if strings.Contains(v, "/home/user/code/my-repo/.bare:/prism-git:") {
			foundBareRepo = true
			if !strings.HasSuffix(v, ":Z") {
				t.Errorf("bare repo mount %q should have :Z label", v)
			}
			if strings.Contains(v, ":ro") {
				t.Errorf("bare repo mount %q must be read-write (not :ro)", v)
			}
			break
		}
	}
	if !foundBareRepo {
		t.Errorf("bare repo mount not found; mounts: %v", mounts)
	}
	foundGitDir := false
	for _, v := range mounts {
		if strings.Contains(v, "/home/user/code/my-repo/.bare/worktrees/feat:/prism-git/worktrees/feat:") {
			foundGitDir = true
			if !strings.HasSuffix(v, ":Z") {
				t.Errorf("worktree git dir mount %q should have :Z label", v)
			}
			break
		}
	}
	if !foundGitDir {
		t.Errorf("worktree git dir mount not found; mounts: %v", mounts)
	}

	// Corrected .git pointer file mounted over /workspace/.git (read-only).
	foundGitdirFile := false
	for _, v := range mounts {
		if strings.HasSuffix(v, ":/workspace/.git:ro") {
			foundGitdirFile = true
			break
		}
	}
	if !foundGitdirFile {
		t.Errorf("corrected .git pointer file mount not found; mounts: %v", mounts)
	}

	// Corrected worktree back-pointer mounted over /prism-git/worktrees/<branch>/gitdir (read-only).
	// This ensures nix/libgit2 resolves the worktree chain correctly inside the container.
	foundWtGitdir := false
	for _, v := range mounts {
		if strings.HasSuffix(v, ":/prism-git/worktrees/feat/gitdir:ro") {
			foundWtGitdir = true
			break
		}
	}
	if !foundWtGitdir {
		t.Errorf("worktree back-pointer (gitdir) mount not found; mounts: %v", mounts)
	}
}

func TestBuildRunArgs_NoGitMountsWhenBareRootEmpty(t *testing.T) {
	m := New(Config{
		SessionName:   "repo@feat",
		Worktree:      "/home/user/code/my-repo/feat",
		AllocatedPort: 14000,
	})
	args := m.buildRunArgs()
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			// Check that no mount targets a /prism-git container path.
			// Note: the volume spec format is "host-path:container-path[:opts]",
			// so we check the container-side path portion (after the first colon).
			if idx := strings.Index(v, ":"); idx >= 0 {
				containerPath := v[idx:]
				if strings.Contains(containerPath, "/prism-git") {
					t.Errorf("unexpected git mount when BareRoot is empty: %q", v)
				}
			}
			if strings.HasSuffix(v, ":/workspace/.git:ro") {
				t.Errorf("unexpected .git file mount when BareRoot is empty: %q", v)
			}
		}
	}
}

func TestBuildRunArgs_HostAPISockMountAndEnvWhenSet(t *testing.T) {
	// AC-1, AC-2: when HostAPISockPath is non-empty, buildRunArgs must include
	// the socket directory volume mount and the PRISM_HOST_API env var.
	m := New(Config{
		SessionName:     "repo@feat",
		AllocatedPort:   14000,
		HostAPISockPath: "/home/user/.local/state/prism/run/repo@feat-hostapi.sock",
	})
	args := m.buildRunArgs()

	// AC-1: --volume <host-dir>:/var/run/prism-host:Z must be present.
	foundMount := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if v == "/home/user/.local/state/prism/run:/var/run/prism-host:Z" {
				foundMount = true
				break
			}
		}
	}
	if !foundMount {
		t.Errorf("host-API socket volume mount not found in args: %v", args)
	}

	// AC-2: --env PRISM_HOST_API=unix:///var/run/prism-host/<sockfilename> must be present.
	foundEnv := false
	for i, arg := range args {
		if arg == "--env" && i+1 < len(args) {
			if args[i+1] == "PRISM_HOST_API=unix:///var/run/prism-host/repo@feat-hostapi.sock" {
				foundEnv = true
				break
			}
		}
	}
	if !foundEnv {
		t.Errorf("PRISM_HOST_API env var not found in args: %v", args)
	}
}

func TestBuildRunArgs_HostAPISockVolumeBeforeEnv(t *testing.T) {
	// AC-3: the --volume for the socket directory must appear before the --env PRISM_HOST_API.
	m := New(Config{
		SessionName:     "repo@feat",
		AllocatedPort:   14000,
		HostAPISockPath: "/run/prism/test-hostapi.sock",
	})
	args := m.buildRunArgs()

	mountIdx := -1
	envIdx := -1
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) &&
			strings.Contains(args[i+1], "/var/run/prism-host") {
			mountIdx = i
		}
		if arg == "--env" && i+1 < len(args) &&
			args[i+1] == "PRISM_HOST_API=unix:///var/run/prism-host/test-hostapi.sock" {
			envIdx = i
		}
	}
	if mountIdx == -1 {
		t.Fatal("host-API socket volume mount not found")
	}
	if envIdx == -1 {
		t.Fatal("PRISM_HOST_API env var not found")
	}
	if mountIdx >= envIdx {
		t.Errorf("expected --volume (idx %d) to appear before --env PRISM_HOST_API (idx %d)", mountIdx, envIdx)
	}
}

func TestBuildRunArgs_HostAPISockAfterPrismBareRoot(t *testing.T) {
	// AC-3: both host-API args must appear after PRISM_BARE_ROOT injection.
	m := New(Config{
		SessionName:     "repo@feat",
		AllocatedPort:   14000,
		HostAPISockPath: "/run/prism/test-hostapi.sock",
	})
	args := m.buildRunArgs()

	bareRootIdx := -1
	hostAPIVolumeIdx := -1
	for i, arg := range args {
		if arg == "--env" && i+1 < len(args) && args[i+1] == "PRISM_BARE_ROOT=/prism-git" {
			bareRootIdx = i
		}
		if arg == "--volume" && i+1 < len(args) &&
			strings.Contains(args[i+1], "/var/run/prism-host") {
			hostAPIVolumeIdx = i
		}
	}
	if bareRootIdx == -1 {
		t.Fatal("PRISM_BARE_ROOT env injection not found")
	}
	if hostAPIVolumeIdx == -1 {
		t.Fatal("host-API socket volume mount not found")
	}
	if hostAPIVolumeIdx <= bareRootIdx {
		t.Errorf("host-API volume (idx %d) should appear after PRISM_BARE_ROOT (idx %d)", hostAPIVolumeIdx, bareRootIdx)
	}
}

func TestBuildRunArgs_NoHostAPISockWhenEmpty(t *testing.T) {
	// AC-5: when HostAPISockPath is empty, no socket mount or PRISM_HOST_API env var
	// should appear in the args.
	m := New(Config{
		SessionName:     "repo@feat",
		AllocatedPort:   14000,
		HostAPISockPath: "",
	})
	args := m.buildRunArgs()

	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			if strings.Contains(args[i+1], "prism-host") {
				t.Errorf("unexpected host-API socket mount when HostAPISockPath is empty: %q", args[i+1])
			}
		}
		if arg == "--env" && i+1 < len(args) {
			if strings.HasPrefix(args[i+1], "PRISM_HOST_API=") {
				t.Errorf("unexpected PRISM_HOST_API env var when HostAPISockPath is empty: %q", args[i+1])
			}
		}
	}
}

// ── credentialEnvVars tests ──────────────────────────────────────────────────

func TestCredentialEnvVars_LLMKeysForwarded(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("OPENAI_API_KEY", "")

	m := New(Config{SessionName: "repo@main", AllocatedPort: 14000, AgentRole: "worker"})
	vars := m.credentialEnvVars()

	found := false
	for _, kv := range vars {
		if kv == "ANTHROPIC_API_KEY=sk-ant-test" {
			found = true
		}
		if strings.HasPrefix(kv, "OPENAI_API_KEY=") {
			t.Errorf("OPENAI_API_KEY should not be forwarded when empty; got %q", kv)
		}
	}
	if !found {
		t.Errorf("ANTHROPIC_API_KEY not forwarded; vars = %v", vars)
	}
}

func TestCredentialEnvVars_WorkerGetsWorkerToken(t *testing.T) {
	// With the new 4-PAT architecture, tokens are keyed by account+role.
	// When BareRoot is empty (cannot derive account), fall back to GITHUB_TOKEN.
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER", "worker-tok-123")
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR", "coord-tok-456")
	t.Setenv("GITHUB_TOKEN", "")

	// BareRoot empty — cannot derive account — fallback applies.
	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000, AgentRole: "worker"})
	vars := m.credentialEnvVars()

	// With empty BareRoot, no account-specific token can be selected.
	// Verify GITHUB_TOKEN is not injected (it's also empty here).
	for _, kv := range vars {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			t.Errorf("expected no GITHUB_TOKEN injection when BareRoot is empty and GITHUB_TOKEN unset; got %q", kv)
		}
	}
}

// makeBareRootWithRemote creates a minimal bare git repo under dir/.bare with
// the given remoteURL set as the "origin" remote, and returns dir as the
// bareRoot. It calls t.Fatal on any setup error.
func makeBareRootWithRemote(t *testing.T, remoteURL string) string {
	t.Helper()
	bareRoot := t.TempDir()
	bareDir := filepath.Join(bareRoot, ".bare")
	out, err := exec.Command("git", "init", "--bare", bareDir).CombinedOutput()
	if err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	out, err = exec.Command("git", "--git-dir", bareDir, "remote", "add", "origin", remoteURL).CombinedOutput()
	if err != nil {
		t.Fatalf("git remote add origin: %v\n%s", err, out)
	}
	return bareRoot
}

func TestCredentialEnvVars_AccountRoleTokenSelection(t *testing.T) {
	// Integration test: verifies that credentialEnvVars selects the correct
	// PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE> env var based on the git remote URL
	// and agent role, and injects it as GITHUB_TOKEN into the container env.
	cases := []struct {
		name        string
		remoteURL   string
		agentRole   string
		tokenEnvVar string
		tokenValue  string
		wantGHToken string
	}{
		{
			name:        "prismatic-koi worker (SSH remote)",
			remoteURL:   "git@github.com:prismatic-koi/nixos-config.git",
			agentRole:   "worker",
			tokenEnvVar: "PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER",
			tokenValue:  "worker-tok-pk",
			wantGHToken: "worker-tok-pk",
		},
		{
			name:        "prismatic-koi coordinator (SSH remote)",
			remoteURL:   "git@github.com:prismatic-koi/nixos-config.git",
			agentRole:   "coordinator",
			tokenEnvVar: "PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR",
			tokenValue:  "coord-tok-pk",
			wantGHToken: "coord-tok-pk",
		},
		{
			name:        "thankyou-payroll worker (HTTPS remote)",
			remoteURL:   "https://github.com/thankyou-payroll/some-repo.git",
			agentRole:   "worker",
			tokenEnvVar: "PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_WORKER",
			tokenValue:  "worker-tok-tp",
			wantGHToken: "worker-tok-tp",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bareRoot := makeBareRootWithRemote(t, tc.remoteURL)
			t.Setenv(tc.tokenEnvVar, tc.tokenValue)
			// Ensure no other token var leaks across sub-tests.
			t.Setenv("GITHUB_TOKEN", "")

			m := New(Config{
				SessionName:   "repo@feat",
				AllocatedPort: 14000,
				AgentRole:     tc.agentRole,
				BareRoot:      bareRoot,
			})
			vars := m.credentialEnvVars()

			found := false
			for _, kv := range vars {
				if kv == "GITHUB_TOKEN="+tc.wantGHToken {
					found = true
				}
			}
			if !found {
				t.Errorf("expected GITHUB_TOKEN=%s in vars; got: %v", tc.wantGHToken, vars)
			}
		})
	}
}

func TestGithubAccountFromURL_SSHFormat(t *testing.T) {
	tests := []struct {
		url     string
		account string
	}{
		{"git@github.com:prismatic-koi/nixos-config.git", "prismatic-koi"},
		{"git@github.com:thankyou-payroll/some-repo.git", "thankyou-payroll"},
		{"git@github.com:myorg/repo", "myorg"},
		{"git@gitlab.com:user/repo.git", ""},
		{"", ""},
	}
	for _, tc := range tests {
		got := githubAccountFromURL(tc.url)
		if got != tc.account {
			t.Errorf("githubAccountFromURL(%q) = %q, want %q", tc.url, got, tc.account)
		}
	}
}

func TestGithubAccountFromURL_HTTPSFormat(t *testing.T) {
	tests := []struct {
		url     string
		account string
	}{
		{"https://github.com/prismatic-koi/nixos-config.git", "prismatic-koi"},
		{"https://github.com/thankyou-payroll/some-repo", "thankyou-payroll"},
		{"https://x-access-token:TOKEN@github.com/myorg/repo.git", "myorg"},
		{"https://gitlab.com/user/repo.git", ""},
	}
	for _, tc := range tests {
		got := githubAccountFromURL(tc.url)
		if got != tc.account {
			t.Errorf("githubAccountFromURL(%q) = %q, want %q", tc.url, got, tc.account)
		}
	}
}

func TestCredentialEnvVars_CoordinatorGetsCoordinatorToken(t *testing.T) {
	// Verify the env var name for coordinator role is correctly constructed.
	// Full integration (with actual BareRoot pointing at a git repo) is
	// exercised by TestGithubAccountFromURL_* above + the env var selection logic.
	tokenVar := "PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR"
	t.Setenv(tokenVar, "coord-tok-456")
	t.Setenv("GITHUB_TOKEN", "fallback")

	// When BareRoot is empty, the account cannot be derived and we fall back.
	m := New(Config{SessionName: "repo@main", AllocatedPort: 14000, AgentRole: "coordinator"})
	vars := m.credentialEnvVars()

	found := false
	for _, kv := range vars {
		if kv == "GITHUB_TOKEN=fallback" {
			found = true
		}
	}
	if !found {
		t.Errorf("fallback GITHUB_TOKEN not injected when BareRoot is empty; vars=%v", vars)
	}
}

func TestCredentialEnvVars_FallbackToGitHubToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "fallback-tok")

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000, AgentRole: "worker"})
	vars := m.credentialEnvVars()

	found := false
	for _, kv := range vars {
		if kv == "GITHUB_TOKEN=fallback-tok" {
			found = true
		}
	}
	if !found {
		t.Errorf("fallback GITHUB_TOKEN not forwarded; vars=%v", vars)
	}
}

func TestCredentialEnvVars_NoGitDirEnvVars(t *testing.T) {
	// GIT_DIR and GIT_COMMON_DIR must not be injected — the corrected .git
	// pointer file (bind-mounted by Create) makes them unnecessary (#492).
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"bare root set", Config{
			SessionName:    "repo@feat",
			AllocatedPort:  14000,
			BareRoot:       "/home/user/code/my-repo",
			WorktreeGitDir: "/home/user/code/my-repo/.bare/worktrees/feat",
		}},
		{"bare root empty", Config{
			SessionName:   "repo@feat",
			AllocatedPort: 14000,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars := New(tc.cfg).credentialEnvVars()
			for _, kv := range vars {
				if strings.HasPrefix(kv, "GIT_DIR=") {
					t.Errorf("GIT_DIR should not be injected; got %q", kv)
				}
				if strings.HasPrefix(kv, "GIT_COMMON_DIR=") {
					t.Errorf("GIT_COMMON_DIR should not be injected; got %q", kv)
				}
			}
		})
	}
}

// ── isHealthy / WaitHealthy tests ────────────────────────────────────────────

func TestWaitHealthy_SucceedsWhenServerResponds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Extract port from test server URL.
	var port int
	if _, err := fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port); err != nil {
		if _, err2 := fmt.Sscanf(srv.URL, "http://localhost:%d", &port); err2 != nil {
			t.Fatalf("parse test server URL %q: %v", srv.URL, err2)
		}
	}

	m := New(Config{
		SessionName:        "repo@feat",
		AllocatedPort:      port,
		HealthCheckTimeout: 5 * time.Second,
		HTTPClient:         srv.Client(),
	})
	// Override URL to use test server.
	m.healthCheckURL = srv.URL + "/"

	ctx := context.Background()
	if err := m.WaitHealthy(ctx); err != nil {
		t.Errorf("WaitHealthy returned error: %v", err)
	}
}

func TestWaitHealthy_TimesOutWhenNoServer(t *testing.T) {
	m := New(Config{
		SessionName:        "repo@feat",
		AllocatedPort:      14099,
		HealthCheckTimeout: 200 * time.Millisecond,
	})
	// Point at a port that nothing is listening on.
	m.healthCheckURL = "http://127.0.0.1:14099/"

	ctx := context.Background()
	err := m.WaitHealthy(ctx)
	if err == nil {
		t.Error("WaitHealthy should return error when timeout expires")
	}
}

func TestWaitHealthy_ReturnsOnContextCancel(t *testing.T) {
	m := New(Config{
		SessionName:        "repo@feat",
		AllocatedPort:      14098,
		HealthCheckTimeout: 30 * time.Second,
	})
	m.healthCheckURL = "http://127.0.0.1:14098/"

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately.
	cancel()

	err := m.WaitHealthy(ctx)
	if err == nil {
		t.Error("WaitHealthy should return error when context is cancelled")
	}
}

// ── isNoSuchContainer tests ──────────────────────────────────────────────────

func TestIsNoSuchContainer(t *testing.T) {
	cases := []struct {
		output string
		want   bool
	}{
		{"Error: no such container: foo", true},
		{"No such container: bar", true},
		{"no such container blah", true},
		{"Error response from daemon: No such container: xyz", true},
		{"something else entirely", false},
		{"", false},
	}
	for _, tc := range cases {
		got := IsNoSuchContainerError(tc.output)
		if got != tc.want {
			t.Errorf("IsNoSuchContainerError(%q) = %v, want %v", tc.output, got, tc.want)
		}
	}
}

// ── redactArgs tests ─────────────────────────────────────────────────────────

func TestRedactArgs_EnvValueRedacted(t *testing.T) {
	args := []string{"run", "--env", "ANTHROPIC_API_KEY=sk-ant-abc123"}
	got := redactArgs(args)
	if got[2] != "ANTHROPIC_API_KEY=***" {
		t.Errorf("expected ANTHROPIC_API_KEY=***, got %q", got[2])
	}
	// Original must be unchanged.
	if args[2] != "ANTHROPIC_API_KEY=sk-ant-abc123" {
		t.Errorf("redactArgs mutated original slice")
	}
}

func TestRedactArgs_NonEnvArgsUntouched(t *testing.T) {
	args := []string{"run", "--name", "prism-foo", "--detach"}
	got := redactArgs(args)
	for i, want := range args {
		if got[i] != want {
			t.Errorf("arg[%d]: got %q, want %q", i, got[i], want)
		}
	}
}

func TestRedactArgs_EnvAsLastArgNoPanic(t *testing.T) {
	// --env at the last position — should not panic.
	args := []string{"run", "--env"}
	got := redactArgs(args)
	if len(got) != 2 || got[1] != "--env" {
		t.Errorf("unexpected result for trailing --env: %v", got)
	}
}

// ── opencode config mount tests ──────────────────────────────────────────────

func TestBuildRunArgs_OpencodeJsonSkippedInItemByItemMount(t *testing.T) {
	// opencode.json must never be mounted — agent identity and permissions are
	// set authoritatively via OPENCODE_CONFIG_CONTENT (injected as --env).
	// Any on-disk opencode.json would be superseded anyway, but skipping the
	// mount is belt-and-suspenders and avoids polluting the container.
	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		AgentRole:     "worker",
	})
	args := m.buildRunArgs()

	for i, arg := range args {
		if (arg == "--volume" || arg == "--mount") && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "opencode.json") {
				t.Errorf("opencode.json must not be mounted into the container, got: %q", v)
			}
		}
	}
}

func TestBuildRunArgs_NoSingleWholeDirConfigMount(t *testing.T) {
	// The old behaviour mounted a single pre-built Nix derivation at
	// /root/.config/opencode. The new design always uses item-by-item mounts
	// (skipping opencode.json) so the whole-dir bind mount must never appear.
	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		AgentRole:     "worker",
	})
	args := m.buildRunArgs()

	for i, arg := range args {
		if arg == "--mount" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "dst=/root/.config/opencode,") ||
				strings.HasSuffix(v, "dst=/root/.config/opencode") {
				t.Errorf("unexpected single whole-dir config mount: %q", v)
			}
		}
	}
}

func TestBuildRunArgs_PluginsWholeDir(t *testing.T) {
	// The whole plugins/ directory is mounted read-write (not via the ro
	// allowlist) so that plugin packages can write back runtime state.
	fakeHome := t.TempDir()
	pluginsDir := filepath.Join(fakeHome, ".config", "opencode", "plugins")
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll pluginsDir: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		AgentRole:     "worker",
	})
	args := m.buildRunArgs()

	foundDir := false
	for i, arg := range args {
		if arg == "--mount" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "dst=/root/.config/opencode/plugins") {
				foundDir = true
				// Must NOT be read-only — plugins write runtime state.
				if strings.Contains(v, ",ro") {
					t.Errorf("plugins dir mount %q must be read-write (no ,ro)", v)
				}
				break
			}
		}
	}
	if !foundDir {
		t.Errorf("plugins dir not mounted; args: %v", args)
	}
}

func TestBuildRunArgs_ConfigMountAllowlist(t *testing.T) {
	// Verifies that the config mount block uses an explicit allowlist:
	// - All 7 allowlisted entries present on disk ARE mounted read-only.
	// - plugins/ is mounted separately as read-write (not in the ro allowlist).
	// - Bun ecosystem files (package.json, bun.lock, package-lock.json,
	//   node_modules/) are NOT mounted.
	// - opencode.json is NOT mounted even when present on disk.
	//
	// Previous tests (TestBuildRunArgs_OpencodeJsonSkippedInItemByItemMount,
	// TestBuildRunArgs_NoSingleWholeDirConfigMount) ran against an empty home dir,
	// so filepath.EvalSymlinks always failed and the config block produced zero
	// mount args — the allowlist was never actually exercised. This test populates
	// the config dir so the allowlist logic is fully covered.

	fakeHome := t.TempDir()
	configDir := filepath.Join(fakeHome, ".config", "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll configDir: %v", err)
	}

	// Allowlisted files/dirs that should be mounted read-only (7 entries).
	allowlisted := []struct {
		name  string
		isDir bool
	}{
		{"AGENTS.md", false},
		{"tui.json", false},
		{"mcp-atlassian-slim-proxy.mjs", false},
		{".gitignore", false},
		{"agents", true},
		{"skills", true},
		{"command", true},
	}
	for _, e := range allowlisted {
		p := filepath.Join(configDir, e.name)
		if e.isDir {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatalf("MkdirAll %s: %v", e.name, err)
			}
		} else {
			if err := os.WriteFile(p, []byte("stub"), 0o644); err != nil {
				t.Fatalf("WriteFile %s: %v", e.name, err)
			}
		}
	}

	// plugins/ should be mounted read-write separately.
	if err := os.MkdirAll(filepath.Join(configDir, "plugins"), 0o755); err != nil {
		t.Fatalf("MkdirAll plugins: %v", err)
	}

	// Files that must NOT be mounted — bun ecosystem files and opencode.json.
	excluded := []string{"package.json", "bun.lock", "package-lock.json", "opencode.json"}
	for _, name := range excluded {
		if err := os.WriteFile(filepath.Join(configDir, name), []byte("stub"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	// node_modules/ as a directory must also not be mounted.
	if err := os.MkdirAll(filepath.Join(configDir, "node_modules"), 0o755); err != nil {
		t.Fatalf("MkdirAll node_modules: %v", err)
	}

	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		AgentRole:     "worker",
	})
	args := m.buildRunArgs()

	// Collect all mount/volume values for inspection.
	var mountValues []string
	for i, arg := range args {
		if (arg == "--volume" || arg == "--mount") && i+1 < len(args) {
			mountValues = append(mountValues, args[i+1])
		}
	}

	// Assert allowlisted entries ARE present as read-only mounts.
	for _, e := range allowlisted {
		found := false
		containerPath := "/root/.config/opencode/" + e.name
		for _, v := range mountValues {
			if strings.Contains(v, containerPath) {
				if !strings.Contains(v, ":ro") && !strings.Contains(v, ",ro") {
					t.Errorf("mount for %q is not read-only: %q", e.name, v)
				}
				found = true
				break
			}
		}
		if !found {
			t.Errorf("allowlisted entry %q was not mounted; mount values: %v", e.name, mountValues)
		}
	}

	// Assert plugins/ IS mounted and is NOT read-only.
	foundPlugins := false
	for _, v := range mountValues {
		if strings.Contains(v, "/root/.config/opencode/plugins") {
			foundPlugins = true
			if strings.Contains(v, ":ro") || strings.Contains(v, ",ro") {
				t.Errorf("plugins mount %q must be read-write (no :ro/,ro)", v)
			}
			break
		}
	}
	if !foundPlugins {
		t.Errorf("plugins dir not mounted; mount values: %v", mountValues)
	}

	// Assert excluded entries are NOT present.
	excluded = append(excluded, "node_modules") // dir also covered
	for _, name := range excluded {
		containerPath := "/root/.config/opencode/" + name
		for _, v := range mountValues {
			if strings.Contains(v, containerPath) {
				t.Errorf("excluded entry %q must not be mounted, but found: %q", name, v)
			}
		}
	}
}

func TestRedactArgs_MultipleEnvVarsAllRedacted(t *testing.T) {
	args := []string{
		"run",
		"--env", "ANTHROPIC_API_KEY=sk-ant-secret",
		"--env", "GITHUB_TOKEN=ghp_supersecret",
		"--env", "OPENAI_API_KEY=sk-openai-xyz",
	}
	got := redactArgs(args)
	for i, arg := range got {
		if strings.Contains(arg, "secret") || strings.Contains(arg, "supersecret") || strings.Contains(arg, "xyz") {
			t.Errorf("got[%d] = %q still contains plaintext secret", i, arg)
		}
	}
	// Keys must still be present.
	if got[2] != "ANTHROPIC_API_KEY=***" {
		t.Errorf("expected ANTHROPIC_API_KEY=***, got %q", got[2])
	}
	if got[4] != "GITHUB_TOKEN=***" {
		t.Errorf("expected GITHUB_TOKEN=***, got %q", got[4])
	}
	if got[6] != "OPENAI_API_KEY=***" {
		t.Errorf("expected OPENAI_API_KEY=***, got %q", got[6])
	}
}

// ── SSH key simplification tests (AC-5, AC-9) ────────────────────────────────

func TestBuildRunArgs_SSHConfigIsMounted(t *testing.T) {
	// Verifies that a generated SSH config file is mounted at /root/.ssh/config:ro.
	// The content of the SSH config (including the IdentityFile = access-key reference
	// required by AC-9) is tested via the Create() path which writes the config file.
	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
	})
	args := m.buildRunArgs()
	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			if strings.HasSuffix(args[i+1], ":/root/.ssh/config:ro") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("SSH config mount not found at /root/.ssh/config:ro in args: %v", args)
	}
}

func TestBuildRunArgs_GitconfigMountedAtRootGitconfig(t *testing.T) {
	// AC-4: The generated .gitconfig must be mounted at /root/.gitconfig:ro.
	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
	})
	args := m.buildRunArgs()
	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			if strings.HasSuffix(args[i+1], ":/root/.gitconfig:ro") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("gitconfig mount not found at /root/.gitconfig:ro in args: %v", args)
	}
}

func TestBuildRunArgs_NoWholeDirSSHMount(t *testing.T) {
	// AC-15: The whole ~/.ssh directory must NOT be mounted.
	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
	})
	args := m.buildRunArgs()
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			// A whole-dir ssh mount has the container path :/root/.ssh (without a
			// file suffix), not :/root/.ssh/<filename>.
			if v == ":/root/.ssh:ro" || strings.HasSuffix(v, "/.ssh:/root/.ssh:ro") {
				t.Errorf("whole ~/.ssh directory is mounted (must use individual key files): %q", v)
			}
		}
	}
}

// ── credentialEnvVars git identity removal tests (AC-11) ────────────────────

func TestCredentialEnvVars_NoGitIdentityEnvVars(t *testing.T) {
	// AC-11: GIT_AUTHOR_NAME, GIT_AUTHOR_EMAIL, GIT_COMMITTER_NAME,
	// and GIT_COMMITTER_EMAIL must NOT be injected — the container now has
	// a generated .gitconfig with [user] section.
	t.Setenv("GIT_AUTHOR_NAME", "Test User")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test User")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000, AgentRole: "worker"})
	vars := m.credentialEnvVars()

	for _, kv := range vars {
		if strings.HasPrefix(kv, "GIT_AUTHOR_NAME=") {
			t.Errorf("GIT_AUTHOR_NAME must not be injected (gitconfig handles identity); got %q", kv)
		}
		if strings.HasPrefix(kv, "GIT_AUTHOR_EMAIL=") {
			t.Errorf("GIT_AUTHOR_EMAIL must not be injected (gitconfig handles identity); got %q", kv)
		}
		if strings.HasPrefix(kv, "GIT_COMMITTER_NAME=") {
			t.Errorf("GIT_COMMITTER_NAME must not be injected (gitconfig handles identity); got %q", kv)
		}
		if strings.HasPrefix(kv, "GIT_COMMITTER_EMAIL=") {
			t.Errorf("GIT_COMMITTER_EMAIL must not be injected (gitconfig handles identity); got %q", kv)
		}
	}
}

// ── writeGitconfig tests (AC-1, AC-13, AC-14) ────────────────────────────────

func TestWriteGitconfig_IncludesPushAndInit(t *testing.T) {
	// AC-1: generated gitconfig must always include [push] and [init] sections.
	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
	})

	if err := m.writeGitconfig(); err != nil {
		t.Fatalf("writeGitconfig returned error: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(m.gitconfigFilePath()) })

	data, err := os.ReadFile(m.gitconfigFilePath())
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "[push]") {
		t.Errorf("gitconfig missing [push] section; content:\n%s", content)
	}
	if !strings.Contains(content, "autoSetupRemote = true") {
		t.Errorf("gitconfig missing autoSetupRemote; content:\n%s", content)
	}
	if !strings.Contains(content, "[init]") {
		t.Errorf("gitconfig missing [init] section; content:\n%s", content)
	}
	if !strings.Contains(content, "defaultBranch = main") {
		t.Errorf("gitconfig missing defaultBranch; content:\n%s", content)
	}
}

func TestWriteGitconfig_NoSigningSectionsWhenKeysUnavailable(t *testing.T) {
	// AC-13: when signing keys are not resolvable, the generated gitconfig must
	// NOT contain [commit], [gpg], or signingKey.
	//
	// We redirect HOME to a temp dir with no .ssh/ directory so that
	// filepath.EvalSymlinks cannot find the signing keys, regardless of
	// what is present on the host running the test.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		GitUserName:   "test-user",
		GitUserEmail:  "test@example.com",
	})

	if err := m.writeGitconfig(); err != nil {
		t.Fatalf("writeGitconfig returned error: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(m.gitconfigFilePath()) })

	data, err := os.ReadFile(m.gitconfigFilePath())
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	content := string(data)

	// These sections must be absent when signing keys can't be resolved.
	if strings.Contains(content, "[commit]") {
		t.Errorf("gitconfig must not include [commit] when signing keys are absent; content:\n%s", content)
	}
	if strings.Contains(content, "[gpg]") {
		t.Errorf("gitconfig must not include [gpg] when signing keys are absent; content:\n%s", content)
	}
	if strings.Contains(content, "signingKey") {
		t.Errorf("gitconfig must not include signingKey when signing keys are absent; content:\n%s", content)
	}
}

func TestWriteGitconfig_SigningSectionsWhenKeysAndIdentityPresent(t *testing.T) {
	// AC-10: when signing keys are resolvable AND identity is set, the generated
	// gitconfig must contain [commit] gpgsign=true, [gpg] format=ssh, and
	// user.signingKey = /root/.ssh/signing-key.pub.
	fakeHome := t.TempDir()
	sshDir := fakeHome + "/.ssh"
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("MkdirAll .ssh: %v", err)
	}
	for _, name := range []string{"prismatic-koi-ed25519-signingkey", "prismatic-koi-ed25519-signingkey.pub"} {
		if err := os.WriteFile(sshDir+"/"+name, []byte("stub"), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		GitUserName:   "test-user",
		GitUserEmail:  "test@example.com",
	})

	if err := m.writeGitconfig(); err != nil {
		t.Fatalf("writeGitconfig returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(m.gitconfigFilePath())
		_ = os.Remove(m.allowedSignersFilePath())
	})

	data, err := os.ReadFile(m.gitconfigFilePath())
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "[commit]") {
		t.Errorf("gitconfig missing [commit] when signing keys and identity are present; content:\n%s", content)
	}
	if !strings.Contains(content, "gpgsign = true") {
		t.Errorf("gitconfig missing gpgsign = true; content:\n%s", content)
	}
	if !strings.Contains(content, "[gpg]") {
		t.Errorf("gitconfig missing [gpg] when signing keys and identity are present; content:\n%s", content)
	}
	if !strings.Contains(content, "format = ssh") {
		t.Errorf("gitconfig missing format = ssh; content:\n%s", content)
	}
	if !strings.Contains(content, "signingKey = /root/.ssh/signing-key.pub") {
		t.Errorf("gitconfig missing signingKey = /root/.ssh/signing-key.pub; content:\n%s", content)
	}
}

func TestWriteGitconfig_UserSectionWhenIdentityPresent(t *testing.T) {
	// AC-2, AC-14: [user] section present only when both name and email are set.
	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		GitUserName:   "test-user",
		GitUserEmail:  "test@example.com",
	})

	if err := m.writeGitconfig(); err != nil {
		t.Fatalf("writeGitconfig returned error: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(m.gitconfigFilePath()) })

	data, err := os.ReadFile(m.gitconfigFilePath())
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "[user]") {
		t.Errorf("gitconfig missing [user] section when identity is set; content:\n%s", content)
	}
	if !strings.Contains(content, "name = test-user") {
		t.Errorf("gitconfig missing name in [user]; content:\n%s", content)
	}
	if !strings.Contains(content, "email = test@example.com") {
		t.Errorf("gitconfig missing email in [user]; content:\n%s", content)
	}
}

func TestWriteGitconfig_NoSigningWithoutIdentity(t *testing.T) {
	// Bug regression: when signing keys are present but identity (name/email)
	// is empty, the [commit] and [gpg] sections must NOT be written.
	// Previously only hasSigning was checked, which would produce gpgsign=true
	// with no signingKey set (since signingKey lives in [user] which requires
	// identity), causing every git commit to fail.
	//
	// We set up a fakeHome with real signing key files so hasSigning=true,
	// but leave GitUserName/GitUserEmail empty.
	fakeHome := t.TempDir()
	sshDir := fakeHome + "/.ssh"
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("MkdirAll .ssh: %v", err)
	}
	// Create non-empty stub files (EvalSymlinks just needs them to exist on disk).
	for _, name := range []string{"prismatic-koi-ed25519-signingkey", "prismatic-koi-ed25519-signingkey.pub"} {
		if err := os.WriteFile(sshDir+"/"+name, []byte("stub"), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		// GitUserName and GitUserEmail intentionally left empty.
	})

	if err := m.writeGitconfig(); err != nil {
		t.Fatalf("writeGitconfig returned error: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(m.gitconfigFilePath()) })

	data, err := os.ReadFile(m.gitconfigFilePath())
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	content := string(data)

	// Signing sections must be absent — identity is missing so signingKey
	// would never be set, and gpgsign=true without a key breaks git commit.
	if strings.Contains(content, "[commit]") {
		t.Errorf("gitconfig must not include [commit] when identity is absent; content:\n%s", content)
	}
	if strings.Contains(content, "[gpg]") {
		t.Errorf("gitconfig must not include [gpg] when identity is absent; content:\n%s", content)
	}
	if strings.Contains(content, "gpgsign") {
		t.Errorf("gitconfig must not include gpgsign when identity is absent; content:\n%s", content)
	}
}

func TestWriteGitconfig_NoUserSectionWhenIdentityMissing(t *testing.T) {
	// AC-14: [user] section omitted when GitUserName or GitUserEmail is empty.
	for _, tc := range []struct {
		name  string
		uname string
		email string
	}{
		{"both empty", "", ""},
		{"name only", "test-user", ""},
		{"email only", "", "test@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Config{
				SessionName:   "repo@feat",
				AllocatedPort: 14000,
				GitUserName:   tc.uname,
				GitUserEmail:  tc.email,
			})

			if err := m.writeGitconfig(); err != nil {
				t.Fatalf("writeGitconfig returned error: %v", err)
			}
			t.Cleanup(func() { _ = os.Remove(m.gitconfigFilePath()) })

			data, err := os.ReadFile(m.gitconfigFilePath())
			if err != nil {
				t.Fatalf("read gitconfig: %v", err)
			}
			content := string(data)

			if strings.Contains(content, "[user]") {
				t.Errorf("[user] section present but identity incomplete (name=%q, email=%q); content:\n%s",
					tc.uname, tc.email, content)
			}
		})
	}
}

// ── SSH key name configurability tests ──────────────────────────────────────

func TestBuildRunArgs_CustomAccessKeyName(t *testing.T) {
	// When SshAccessKeyName is set, buildRunArgs should resolve and mount that
	// file at /root/.ssh/access-key:ro rather than the hardcoded default.
	fakeHome := t.TempDir()
	sshDir := fakeHome + "/.ssh"
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("MkdirAll .ssh: %v", err)
	}
	// Create only the custom-named key (not the default name).
	customKey := sshDir + "/my-custom-access-key"
	if err := os.WriteFile(customKey, []byte("stub"), 0o600); err != nil {
		t.Fatalf("WriteFile custom access key: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:      "repo@feat",
		AllocatedPort:    14000,
		SshAccessKeyName: "my-custom-access-key",
	})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.HasSuffix(v, ":/root/.ssh/access-key:ro") {
				found = true
				// The source path must be the resolved real path of the custom key.
				if !strings.Contains(v, customKey) {
					t.Errorf("access-key mount source %q does not contain custom key path %q", v, customKey)
				}
				break
			}
		}
	}
	if !found {
		t.Errorf("access-key volume mount at /root/.ssh/access-key:ro not found in args: %v", args)
	}
}

func TestBuildRunArgs_FallbackWhenAccessKeyNameEmpty(t *testing.T) {
	// When SshAccessKeyName is empty, buildRunArgs falls back to the default
	// name "prismatic-koi-ed25519".
	fakeHome := t.TempDir()
	sshDir := fakeHome + "/.ssh"
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("MkdirAll .ssh: %v", err)
	}
	// Create the default-named key.
	defaultKey := sshDir + "/prismatic-koi-ed25519"
	if err := os.WriteFile(defaultKey, []byte("stub"), 0o600); err != nil {
		t.Fatalf("WriteFile default access key: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		// SshAccessKeyName intentionally left empty — must fall back to default.
	})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.HasSuffix(v, ":/root/.ssh/access-key:ro") {
				found = true
				if !strings.Contains(v, defaultKey) {
					t.Errorf("access-key mount source %q does not contain default key path %q", v, defaultKey)
				}
				break
			}
		}
	}
	if !found {
		t.Errorf("access-key volume mount at /root/.ssh/access-key:ro not found in args with default key: %v", args)
	}
}

func TestWriteGitconfig_CustomSigningKeyName(t *testing.T) {
	// When SshSigningKeyName is set, writeGitconfig should resolve that filename
	// rather than the hardcoded default.
	fakeHome := t.TempDir()
	sshDir := fakeHome + "/.ssh"
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("MkdirAll .ssh: %v", err)
	}
	// Create files under the custom name, NOT the default name.
	for _, name := range []string{"my-custom-signingkey", "my-custom-signingkey.pub"} {
		if err := os.WriteFile(sshDir+"/"+name, []byte("stub"), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:       "repo@feat",
		AllocatedPort:     14000,
		GitUserName:       "test-user",
		GitUserEmail:      "test@example.com",
		SshSigningKeyName: "my-custom-signingkey",
	})

	if err := m.writeGitconfig(); err != nil {
		t.Fatalf("writeGitconfig returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(m.gitconfigFilePath())
		_ = os.Remove(m.allowedSignersFilePath())
	})

	data, err := os.ReadFile(m.gitconfigFilePath())
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	content := string(data)

	// Signing sections must be present because the custom-named keys exist.
	if !strings.Contains(content, "[commit]") {
		t.Errorf("gitconfig missing [commit] for custom signing key; content:\n%s", content)
	}
	if !strings.Contains(content, "signingKey = /root/.ssh/signing-key.pub") {
		t.Errorf("gitconfig missing signingKey; content:\n%s", content)
	}
}

func TestWriteGitconfig_FallbackWhenSigningKeyNameEmpty(t *testing.T) {
	// When SshSigningKeyName is empty, writeGitconfig falls back to the
	// default name "prismatic-koi-ed25519-signingkey".
	fakeHome := t.TempDir()
	sshDir := fakeHome + "/.ssh"
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("MkdirAll .ssh: %v", err)
	}
	// Only the default-named files exist.
	for _, name := range []string{"prismatic-koi-ed25519-signingkey", "prismatic-koi-ed25519-signingkey.pub"} {
		if err := os.WriteFile(sshDir+"/"+name, []byte("stub"), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		GitUserName:   "test-user",
		GitUserEmail:  "test@example.com",
		// SshSigningKeyName intentionally left empty — must fall back to default.
	})

	if err := m.writeGitconfig(); err != nil {
		t.Fatalf("writeGitconfig returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(m.gitconfigFilePath())
		_ = os.Remove(m.allowedSignersFilePath())
	})

	data, err := os.ReadFile(m.gitconfigFilePath())
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	content := string(data)

	// Fallback to default name means signing keys are found and sections present.
	if !strings.Contains(content, "[commit]") {
		t.Errorf("gitconfig missing [commit] with default signing key fallback; content:\n%s", content)
	}
	if !strings.Contains(content, "signingKey = /root/.ssh/signing-key.pub") {
		t.Errorf("gitconfig missing signingKey with default signing key fallback; content:\n%s", content)
	}
}

// ── allowed_signers tests (AC-7, AC-8, AC-9, AC-10, AC-11) ───────────────────

func TestWriteGitconfig_AllowedSignersFileInGitconfigWhenSigningAvailable(t *testing.T) {
	// AC-7: generated gitconfig must contain allowedSignersFile = /root/.ssh/allowed_signers
	// in the [gpg "ssh"] section when signing keys and identity are present.
	fakeHome := t.TempDir()
	sshDir := fakeHome + "/.ssh"
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("MkdirAll .ssh: %v", err)
	}
	pubKeyData := "sk-ssh-ed25519@openssh.com AAAA1234 test@example.com"
	for _, name := range []string{"prismatic-koi-ed25519-signingkey", "prismatic-koi-ed25519-signingkey.pub"} {
		content := "stub"
		if name == "prismatic-koi-ed25519-signingkey.pub" {
			content = pubKeyData
		}
		if err := os.WriteFile(sshDir+"/"+name, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		GitUserName:   "test-user",
		GitUserEmail:  "test@example.com",
	})

	if err := m.writeGitconfig(); err != nil {
		t.Fatalf("writeGitconfig returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(m.gitconfigFilePath())
		_ = os.Remove(m.allowedSignersFilePath())
	})

	data, err := os.ReadFile(m.gitconfigFilePath())
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, `[gpg "ssh"]`) {
		t.Errorf("gitconfig missing [gpg \"ssh\"] section; content:\n%s", content)
	}
	if !strings.Contains(content, "allowedSignersFile = /root/.ssh/allowed_signers") {
		t.Errorf("gitconfig missing allowedSignersFile = /root/.ssh/allowed_signers; content:\n%s", content)
	}
}

func TestWriteGitconfig_AllowedSignersAbsentWhenSigningUnavailable(t *testing.T) {
	// AC-8: allowedSignersFile must NOT appear in the gitconfig when signing
	// keys are not resolvable.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		GitUserName:   "test-user",
		GitUserEmail:  "test@example.com",
	})

	if err := m.writeGitconfig(); err != nil {
		t.Fatalf("writeGitconfig returned error: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(m.gitconfigFilePath()) })

	data, err := os.ReadFile(m.gitconfigFilePath())
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	content := string(data)

	if strings.Contains(content, "allowedSignersFile") {
		t.Errorf("gitconfig must not contain allowedSignersFile when signing keys are absent; content:\n%s", content)
	}
	if strings.Contains(content, `[gpg "ssh"]`) {
		t.Errorf("gitconfig must not contain [gpg \"ssh\"] section when signing keys are absent; content:\n%s", content)
	}
}

func TestWriteGitconfig_AllowedSignersFileContent(t *testing.T) {
	// AC-9: the allowed_signers file must be written with correct
	// "<email> <pubkey-contents>" content.
	fakeHome := t.TempDir()
	sshDir := fakeHome + "/.ssh"
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("MkdirAll .ssh: %v", err)
	}
	pubKeyData := "sk-ssh-ed25519@openssh.com AAAA5678 user@host"
	if err := os.WriteFile(sshDir+"/prismatic-koi-ed25519-signingkey", []byte("stub"), 0o600); err != nil {
		t.Fatalf("WriteFile signingkey: %v", err)
	}
	if err := os.WriteFile(sshDir+"/prismatic-koi-ed25519-signingkey.pub", []byte(pubKeyData), 0o600); err != nil {
		t.Fatalf("WriteFile signingkey.pub: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		GitUserName:   "test-user",
		GitUserEmail:  "test@example.com",
	})

	if err := m.writeGitconfig(); err != nil {
		t.Fatalf("writeGitconfig returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(m.gitconfigFilePath())
		_ = os.Remove(m.allowedSignersFilePath())
	})

	asData, err := os.ReadFile(m.allowedSignersFilePath())
	if err != nil {
		t.Fatalf("read allowed_signers file: %v", err)
	}
	asContent := string(asData)

	expectedLine := "test@example.com " + pubKeyData
	if !strings.Contains(asContent, expectedLine) {
		t.Errorf("allowed_signers content %q does not contain expected line %q", asContent, expectedLine)
	}
}

func TestBuildRunArgs_AllowedSignersMountedWhenSigningAvailable(t *testing.T) {
	// AC-10: buildRunArgs must include a bind-mount for the allowed_signers file
	// at /root/.ssh/allowed_signers:ro when signing keys and identity are present.
	// writeGitconfig must be called first (as Create() does) to set allowedSignersReady.
	fakeHome := t.TempDir()
	sshDir := fakeHome + "/.ssh"
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("MkdirAll .ssh: %v", err)
	}
	for _, name := range []string{"prismatic-koi-ed25519", "prismatic-koi-ed25519-signingkey", "prismatic-koi-ed25519-signingkey.pub"} {
		content := "stub"
		if name == "prismatic-koi-ed25519-signingkey.pub" {
			content = "sk-ssh-ed25519@openssh.com AAAA1234"
		}
		if err := os.WriteFile(sshDir+"/"+name, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		GitUserName:   "test-user",
		GitUserEmail:  "test@example.com",
	})

	// writeGitconfig must be called first — it sets allowedSignersReady which
	// buildRunArgs uses to gate the bind-mount.
	if err := m.writeGitconfig(); err != nil {
		t.Fatalf("writeGitconfig: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(m.gitconfigFilePath())
		_ = os.Remove(m.allowedSignersFilePath())
	})

	args := m.buildRunArgs()

	wantMount := m.allowedSignersFilePath() + ":/root/.ssh/allowed_signers:ro"
	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			if args[i+1] == wantMount {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("allowed_signers mount %q not found in args; args: %v", wantMount, args)
	}
}

func TestBuildRunArgs_AllowedSignersNotMountedWhenSigningUnavailable(t *testing.T) {
	// AC-11: buildRunArgs must NOT include the allowed_signers bind-mount when
	// signing keys are not resolvable.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		GitUserName:   "test-user",
		GitUserEmail:  "test@example.com",
	})
	args := m.buildRunArgs()

	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			if strings.HasSuffix(args[i+1], ":/root/.ssh/allowed_signers:ro") {
				t.Errorf("allowed_signers mount must not be present when signing keys are unavailable; found %q", args[i+1])
			}
		}
	}
}
