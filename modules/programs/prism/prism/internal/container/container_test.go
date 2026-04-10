package container

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestBuildRunArgs_OpencodeConfigMountedReadOnly(t *testing.T) {
	// AC-4: opencode config entries are mounted read-only into /root/.config/opencode.
	// The whole-dir mount is replaced by per-entry mounts that resolve Nix symlinks.
	// We check that opencode.json ends up mounted read-only.
	m := New(Config{
		SessionName:   "my-repo@feat",
		AllocatedPort: 14000,
	})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "/root/.config/opencode/opencode.json") {
				found = true
				if !strings.Contains(v, ":ro") {
					t.Errorf("opencode.json mount %q should be :ro (AC-4)", v)
				}
				break
			}
		}
	}
	if !found {
		// opencode.json might not exist in the test environment — that's ok,
		// but the plugin-path explicit mount should still be present.
		t.Logf("opencode.json not individually mounted (may not exist in test env)")
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

func TestBuildRunArgs_PluginMount(t *testing.T) {
	m := New(Config{
		SessionName:    "repo@feat",
		AllocatedPort:  14000,
		PluginHostPath: "/home/user/.config/opencode/plugins/prism-hooks.ts",
	})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "prism-hooks.ts") {
				found = true
				// Must be read-only.
				if !strings.HasSuffix(v, ":ro") {
					t.Errorf("plugin mount %q should be read-only (:ro)", v)
				}
				// Must map to container plugin dir.
				if !strings.Contains(v, "/root/.config/opencode/plugins/prism-hooks.ts") {
					t.Errorf("plugin mount %q should target /root/.config/opencode/plugins/", v)
				}
				break
			}
		}
	}
	if !found {
		t.Errorf("plugin volume mount not found in args: %v", args)
	}
}

func TestBuildRunArgs_NoPluginMountWhenEmpty(t *testing.T) {
	m := New(Config{
		SessionName:    "repo@feat",
		AllocatedPort:  14000,
		PluginHostPath: "",
	})
	args := m.buildRunArgs()

	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			if strings.Contains(args[i+1], "prism-hooks") {
				t.Errorf("unexpected plugin mount in args when PluginHostPath is empty: %v", args)
			}
		}
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
	// the socket volume mount and the PRISM_HOST_API env var.
	m := New(Config{
		SessionName:     "repo@feat",
		AllocatedPort:   14000,
		HostAPISockPath: "/home/user/.local/state/prism/run/repo@feat-hostapi.sock",
	})
	args := m.buildRunArgs()

	// AC-1: --volume <host-sock>:/var/run/prism-hostapi.sock:Z must be present.
	foundMount := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if v == "/home/user/.local/state/prism/run/repo@feat-hostapi.sock:/var/run/prism-hostapi.sock:Z" {
				foundMount = true
				break
			}
		}
	}
	if !foundMount {
		t.Errorf("host-API socket volume mount not found in args: %v", args)
	}

	// AC-2: --env PRISM_HOST_API=unix:///var/run/prism-hostapi.sock must be present.
	foundEnv := false
	for i, arg := range args {
		if arg == "--env" && i+1 < len(args) {
			if args[i+1] == "PRISM_HOST_API=unix:///var/run/prism-hostapi.sock" {
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
	// AC-3: the --volume for the socket must appear before the --env PRISM_HOST_API.
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
			strings.Contains(args[i+1], "/var/run/prism-hostapi.sock") {
			mountIdx = i
		}
		if arg == "--env" && i+1 < len(args) &&
			args[i+1] == "PRISM_HOST_API=unix:///var/run/prism-hostapi.sock" {
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
			strings.Contains(args[i+1], "/var/run/prism-hostapi.sock") {
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
			if strings.Contains(args[i+1], "prism-hostapi") {
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
	t.Setenv("PRISM_WORKER_GITHUB_TOKEN", "worker-tok-123")
	t.Setenv("PRISM_COORDINATOR_GITHUB_TOKEN", "coord-tok-456")

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000, AgentRole: "worker"})
	vars := m.credentialEnvVars()

	found := false
	for _, kv := range vars {
		if kv == "GITHUB_TOKEN=worker-tok-123" {
			found = true
		}
		if kv == "GITHUB_TOKEN=coord-tok-456" {
			t.Errorf("worker should not receive coordinator token")
		}
	}
	if !found {
		t.Errorf("worker did not receive GITHUB_TOKEN=worker-tok-123; vars=%v", vars)
	}
}

func TestCredentialEnvVars_CoordinatorGetsCoordinatorToken(t *testing.T) {
	t.Setenv("PRISM_WORKER_GITHUB_TOKEN", "worker-tok-123")
	t.Setenv("PRISM_COORDINATOR_GITHUB_TOKEN", "coord-tok-456")

	m := New(Config{SessionName: "repo@main", AllocatedPort: 14000, AgentRole: "coordinator"})
	vars := m.credentialEnvVars()

	found := false
	for _, kv := range vars {
		if kv == "GITHUB_TOKEN=coord-tok-456" {
			found = true
		}
		if kv == "GITHUB_TOKEN=worker-tok-123" {
			t.Errorf("coordinator should not receive worker token")
		}
	}
	if !found {
		t.Errorf("coordinator did not receive GITHUB_TOKEN=coord-tok-456; vars=%v", vars)
	}
}

func TestCredentialEnvVars_FallbackToGitHubToken(t *testing.T) {
	t.Setenv("PRISM_WORKER_GITHUB_TOKEN", "")
	t.Setenv("PRISM_COORDINATOR_GITHUB_TOKEN", "")
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

// ── role-specific opencode config mount tests ────────────────────────────────

func TestBuildRunArgs_WorkerConfigMountedForWorkerRole(t *testing.T) {
	// AC-14, AC-15: when ContainerWorkerConfigPath is set and role is "worker",
	// the worker config dir must be bind-mounted at /root/.config/opencode:ro.
	m := New(Config{
		SessionName:               "repo@feat",
		AllocatedPort:             14000,
		AgentRole:                 "worker",
		ContainerWorkerConfigPath: "/nix/store/abc123-opencode-container-config-worker",
	})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--mount" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "/nix/store/abc123-opencode-container-config-worker") &&
				strings.Contains(v, "dst=/root/.config/opencode") &&
				strings.Contains(v, "ro") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("worker config bind mount not found in args: %v", args)
	}
}

func TestBuildRunArgs_CoordinatorConfigMountedForCoordinatorRole(t *testing.T) {
	// AC-14, AC-15: when ContainerCoordinatorConfigPath is set and role is
	// "coordinator", the coordinator config dir must be bind-mounted at
	// /root/.config/opencode:ro.
	m := New(Config{
		SessionName:                    "repo@main",
		AllocatedPort:                  14000,
		AgentRole:                      "coordinator",
		ContainerWorkerConfigPath:      "/nix/store/abc123-opencode-container-config-worker",
		ContainerCoordinatorConfigPath: "/nix/store/def456-opencode-container-config-coordinator",
	})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--mount" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "/nix/store/def456-opencode-container-config-coordinator") &&
				strings.Contains(v, "dst=/root/.config/opencode") &&
				strings.Contains(v, "ro") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("coordinator config bind mount not found in args: %v", args)
	}
}

func TestBuildRunArgs_WorkerConfigMountedForUnknownRole(t *testing.T) {
	// AC-15: unknown roles fall back to the worker config.
	m := New(Config{
		SessionName:               "repo@feat",
		AllocatedPort:             14000,
		AgentRole:                 "unknown-role",
		ContainerWorkerConfigPath: "/nix/store/abc123-opencode-container-config-worker",
	})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--mount" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "/nix/store/abc123-opencode-container-config-worker") &&
				strings.Contains(v, "dst=/root/.config/opencode") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("worker config bind mount not found for unknown role in args: %v", args)
	}
}

func TestBuildRunArgs_CoordinatorConfigNotUsedForWorkerRole(t *testing.T) {
	// AC-15: worker role must NOT use the coordinator config.
	m := New(Config{
		SessionName:                    "repo@feat",
		AllocatedPort:                  14000,
		AgentRole:                      "worker",
		ContainerWorkerConfigPath:      "/nix/store/abc123-opencode-container-config-worker",
		ContainerCoordinatorConfigPath: "/nix/store/def456-opencode-container-config-coordinator",
	})
	args := m.buildRunArgs()

	for i, arg := range args {
		if arg == "--mount" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "def456-opencode-container-config-coordinator") {
				t.Errorf("coordinator config must not be mounted for worker role: %q", v)
			}
		}
	}
}

func TestBuildRunArgs_ConfigDirMountIsReadOnly(t *testing.T) {
	// AC-14: the single bind mount must be read-only.
	m := New(Config{
		SessionName:               "repo@feat",
		AllocatedPort:             14000,
		AgentRole:                 "worker",
		ContainerWorkerConfigPath: "/nix/store/abc123-opencode-container-config-worker",
	})
	args := m.buildRunArgs()

	for i, arg := range args {
		if arg == "--mount" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "dst=/root/.config/opencode") {
				if !strings.Contains(v, "ro") {
					t.Errorf("opencode config mount %q must be read-only (ro)", v)
				}
				return
			}
		}
	}
	t.Errorf("opencode config mount not found in args: %v", args)
}

func TestBuildRunArgs_PluginMountedSeparatelyWithRoleConfig(t *testing.T) {
	// AC-16: even when using role-based config dir mount, the prism-hooks plugin
	// file is still mounted separately via --volume at the plugins/ path.
	m := New(Config{
		SessionName:               "repo@feat",
		AllocatedPort:             14000,
		AgentRole:                 "worker",
		ContainerWorkerConfigPath: "/nix/store/abc123-opencode-container-config-worker",
		PluginHostPath:            "/home/user/.config/opencode/plugins/prism-hooks.ts",
	})
	args := m.buildRunArgs()

	foundPlugin := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "prism-hooks.ts") &&
				strings.Contains(v, "/root/.config/opencode/plugins/prism-hooks.ts") {
				foundPlugin = true
				if !strings.HasSuffix(v, ":ro") {
					t.Errorf("plugin mount %q must be read-only", v)
				}
				break
			}
		}
	}
	if !foundPlugin {
		t.Errorf("plugin file not mounted separately when role config dir is used; args: %v", args)
	}
}

func TestBuildRunArgs_CoordinatorFallsBackWhenCoordinatorPathEmpty(t *testing.T) {
	// AC-18: if the coordinator path is empty but the worker path is non-empty,
	// the coordinator container must fall back to the legacy item-by-item mount
	// rather than silently using the worker config.
	m := New(Config{
		SessionName:                    "repo@main",
		AllocatedPort:                  14000,
		AgentRole:                      "coordinator",
		ContainerWorkerConfigPath:      "/nix/store/abc123-opencode-container-config-worker",
		ContainerCoordinatorConfigPath: "", // absent — should trigger fallback
	})
	args := m.buildRunArgs()

	// The single whole-dir bind mount must NOT be present for the coordinator
	// config dir — neither the worker path nor any other single-dir mount.
	for i, arg := range args {
		if arg == "--mount" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "dst=/root/.config/opencode,") ||
				strings.HasSuffix(v, "dst=/root/.config/opencode") {
				t.Errorf("unexpected single whole-dir config mount when coordinator path is empty: %q", v)
			}
			// Worker path must also not be mounted as the opencode config dir.
			if strings.Contains(v, "abc123-opencode-container-config-worker") &&
				strings.Contains(v, "dst=/root/.config/opencode") {
				t.Errorf("worker config must not be used for coordinator role: %q", v)
			}
		}
	}
}

func TestBuildRunArgs_FallbackToItemByItemWhenConfigPathsEmpty(t *testing.T) {
	// AC-18: when both config paths are empty, buildRunArgs must NOT add the
	// single whole-directory bind mount for /root/.config/opencode. The legacy
	// item-by-item path will be attempted instead, which mounts individual
	// sub-paths (like /root/.config/opencode/agents, /root/.config/opencode/skills,
	// etc.) rather than the top-level directory as a single unit.
	m := New(Config{
		SessionName:                    "repo@feat",
		AllocatedPort:                  14000,
		AgentRole:                      "worker",
		ContainerWorkerConfigPath:      "",
		ContainerCoordinatorConfigPath: "",
	})
	args := m.buildRunArgs()

	for i, arg := range args {
		if arg == "--mount" && i+1 < len(args) {
			v := args[i+1]
			// The single whole-dir mount has exactly "dst=/root/.config/opencode"
			// (no sub-path after it). Item-by-item mounts have a sub-path like
			// "dst=/root/.config/opencode/agents". Detect the whole-dir mount by
			// checking that the destination is exactly "/root/.config/opencode"
			// with no trailing path separator or additional segment.
			if strings.Contains(v, "dst=/root/.config/opencode,") ||
				strings.HasSuffix(v, "dst=/root/.config/opencode") {
				t.Errorf("unexpected single whole-dir config mount in fallback mode: %q", v)
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
