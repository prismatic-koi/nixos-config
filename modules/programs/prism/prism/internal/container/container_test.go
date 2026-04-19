package container

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestNameForSession_ReplacesTilde(t *testing.T) {
	// Review agent session names contain "~" (e.g. "nixos-config@feature~review-1~review-code").
	// Podman rejects "~" in container names (allowed: [a-zA-Z0-9][a-zA-Z0-9_.-]*).
	name := NameForSession("nixos-config@feature~review-1~review-code")
	want := "prism-nixos-config-feature-review-1-review-code"
	if name != want {
		t.Errorf("NameForSession(%q) = %q, want %q", "nixos-config@feature~review-1~review-code", name, want)
	}
}

func TestNameForSession_MatchesContainerName(t *testing.T) {
	sessions := []string{
		"nixos-config@main",
		"repo@feat/sub",
		"repo.git@main",
		"a@b/c.d",
		"nixos-config@feature~review-1~review-code",
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
	// AC-2: opencode state is per-session, mounted read-write into
	// /root/.local/share/opencode with :Z SELinux label.
	// AC-7: source path must contain prism-sessions/<container-name>/.
	m := New(Config{
		SessionName:   "my-repo@feat",
		AllocatedPort: 14000,
	})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, ":/root/.local/share/opencode:") {
				found = true
				if !strings.HasSuffix(v, ":Z") {
					t.Errorf("opencode state mount %q should have :Z SELinux label (AC-2)", v)
				}
				// Must be read-write (no :ro).
				if strings.Contains(v, ":ro") {
					t.Errorf("opencode state mount %q should be read-write, not :ro (AC-2)", v)
				}
				// Source path must contain prism-sessions/<container-name> (AC-7).
				wantSubpath := "prism-sessions/" + m.Name()
				if !strings.Contains(v, wantSubpath) {
					t.Errorf("opencode state mount source %q should contain %q (AC-7)", v, wantSubpath)
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

func TestBuildRunArgs_OpencodeCommand(t *testing.T) {
	m := New(Config{SessionName: "repo@main", AllocatedPort: 14000})
	args := m.buildRunArgs()

	// The last elements should be:
	// <image> opencode --port 4096 --hostname 0.0.0.0
	// That is 6 elements from the end (no "serve" subcommand any more).
	n := len(args)
	if n < 6 {
		t.Fatalf("too few args (%d): %v", n, args)
	}
	if args[n-6] != Image {
		t.Errorf("expected image %q at args[n-6], got %q (all args: %v)", Image, args[n-6], args)
	}
	if args[n-5] != "opencode" {
		t.Errorf("expected 'opencode', got %q", args[n-5])
	}
	// Verify there is no "serve" subcommand — opencode runs in combined TUI + HTTP mode.
	if args[n-5] == "opencode" && len(args) > n-4 && args[n-4] == "serve" {
		t.Errorf("unexpected 'serve' subcommand: opencode should run in combined TUI + HTTP mode (RFC #691)")
	}
	if args[n-4] != "--port" || args[n-3] != "4096" {
		t.Errorf("expected '--port 4096', got %q %q", args[n-4], args[n-3])
	}
	if args[n-2] != "--hostname" || args[n-1] != "0.0.0.0" {
		t.Errorf("expected '--hostname 0.0.0.0', got %q %q", args[n-2], args[n-1])
	}
}

func TestBuildRunArgs_AgentRoleAndPromptAppended(t *testing.T) {
	// When both AgentRole and InitialPrompt are set, --agent and --prompt
	// should both appear at the end of the args.
	m := New(Config{
		SessionName:   "repo@main",
		AllocatedPort: 14000,
		AgentRole:     "worker",
		InitialPrompt: "fix the login bug",
	})
	args := m.buildRunArgs()

	n := len(args)
	// Last four args should be: --agent worker --prompt "fix the login bug"
	if n < 4 {
		t.Fatalf("too few args: %v", args)
	}
	if args[n-4] != "--agent" || args[n-3] != "worker" {
		t.Errorf("expected '--agent worker', got %q %q", args[n-4], args[n-3])
	}
	if args[n-2] != "--prompt" || args[n-1] != "fix the login bug" {
		t.Errorf("expected '--prompt fix the login bug', got %q %q", args[n-2], args[n-1])
	}
}

func TestBuildRunArgs_AgentRoleWithoutPrompt(t *testing.T) {
	// When AgentRole is set but InitialPrompt is empty, --agent should still
	// be appended so opencode does not default to the wrong agent type.
	// This is the review-agent case: role is set (e.g. "review-code") but
	// there is no prompt at container launch time.
	m := New(Config{
		SessionName:   "repo@main",
		AllocatedPort: 14000,
		AgentRole:     "review-code",
		InitialPrompt: "",
	})
	args := m.buildRunArgs()

	n := len(args)
	// Last two args should be: --agent review-code
	if n < 2 {
		t.Fatalf("too few args: %v", args)
	}
	if args[n-2] != "--agent" || args[n-1] != "review-code" {
		t.Errorf("expected '--agent review-code', got %q %q", args[n-2], args[n-1])
	}
	// --prompt must NOT be present.
	for _, arg := range args {
		if arg == "--prompt" {
			t.Errorf("unexpected --prompt in args when InitialPrompt is empty: %v", args)
		}
	}
}

func TestBuildRunArgs_NoAgentRoleNoPromptNoExtraArgs(t *testing.T) {
	// When neither AgentRole nor InitialPrompt are set, no extra args.
	m := New(Config{
		SessionName:   "repo@main",
		AllocatedPort: 14000,
		AgentRole:     "",
		InitialPrompt: "",
	})
	args := m.buildRunArgs()

	for _, arg := range args {
		if arg == "--agent" {
			t.Errorf("unexpected --agent in args when AgentRole is empty: %v", args)
		}
		if arg == "--prompt" {
			t.Errorf("unexpected --prompt in args when InitialPrompt is empty: %v", args)
		}
	}
}

func TestBuildRunArgs_PromptWithoutAgentRole(t *testing.T) {
	// When InitialPrompt is set but AgentRole is empty, --prompt is appended
	// without --agent (opencode will use its own default agent).
	m := New(Config{
		SessionName:   "repo@main",
		AllocatedPort: 14000,
		AgentRole:     "",
		InitialPrompt: "do the thing",
	})
	args := m.buildRunArgs()

	n := len(args)
	if n < 2 {
		t.Fatalf("too few args: %v", args)
	}
	if args[n-2] != "--prompt" || args[n-1] != "do the thing" {
		t.Errorf("expected '--prompt do the thing', got %q %q", args[n-2], args[n-1])
	}
	// --agent must NOT be present when AgentRole is empty.
	for i, arg := range args {
		if arg == "--agent" {
			t.Errorf("unexpected --agent at index %d when AgentRole is empty: %v", i, args)
		}
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

func TestBuildRunArgs_PluginDirMountedViaAllowlist(t *testing.T) {
	// plugins/ is a directory on disk — it should be mounted read-only via
	// the config allowlist (--mount type=bind,ro), not as a single file.
	fakeHome := t.TempDir()
	pluginDir := filepath.Join(fakeHome, ".config", "opencode", "plugins")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("MkdirAll plugins: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "my-plugin.ts"), []byte("stub"), 0o644); err != nil {
		t.Fatalf("WriteFile my-plugin.ts: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
	})
	args := m.buildRunArgs()

	// Must be mounted via --mount type=bind (directory path), read-only.
	found := false
	for i, arg := range args {
		if arg == "--mount" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "/root/.config/opencode/plugins") && strings.Contains(v, ",ro") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("plugins/ not mounted via --mount type=bind,ro; args: %v", args)
	}

	// Must NOT appear as an individual --volume file mount.
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, "plugins/my-plugin.ts") {
				t.Errorf("plugin should not be mounted as individual file: %q", v)
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

// ── HostAPITCPPort / Darwin TCP path tests ───────────────────────────────────

// TestBuildRunArgs_HostAPITCPPortPublishAndEnv asserts that when HostAPITCPPort
// is non-zero, buildRunArgs sets PRISM_HOST_API=http://host.containers.internal:<port>
// and does NOT emit a --publish flag for the host-API port or a unix:// env var
// or socket-directory volume mount.
func TestBuildRunArgs_HostAPITCPPortPublishAndEnv(t *testing.T) {
	const hostPort = 51234
	m := New(Config{
		SessionName:     "repo@feat",
		AllocatedPort:   14000,
		HostAPISockPath: "/home/user/.local/state/prism/run/repo@feat-hostapi.sock",
		HostAPITCPPort:  hostPort,
	})
	args := m.buildRunArgs()

	// Must NOT have --publish referencing the host-API port (no port forwarding
	// needed; the container reaches the sidecar via host.containers.internal).
	// AllocatedPort (14000) != hostPort (51234), so the opencode --publish is
	// unambiguously distinct and needs no special carve-out.
	for i, arg := range args {
		if arg == "--publish" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, fmt.Sprintf(":%d", hostPort)) || strings.Contains(v, fmt.Sprintf("%d:", hostPort)) {
				t.Errorf("unexpected --publish referencing host-API TCP port %d: %q", hostPort, v)
			}
		}
	}

	// Must have PRISM_HOST_API=http://host.containers.internal:<hostPort>.
	wantEnv := fmt.Sprintf("PRISM_HOST_API=http://host.containers.internal:%d", hostPort)
	foundEnv := false
	for i, arg := range args {
		if arg == "--env" && i+1 < len(args) && args[i+1] == wantEnv {
			foundEnv = true
			break
		}
	}
	if !foundEnv {
		t.Errorf("expected --env %s in args, not found: %v", wantEnv, args)
	}

	// Must NOT have unix:// env var.
	for i, arg := range args {
		if arg == "--env" && i+1 < len(args) {
			if strings.Contains(args[i+1], "unix://") {
				t.Errorf("unexpected unix:// in PRISM_HOST_API when HostAPITCPPort is set: %q", args[i+1])
			}
		}
	}

	// Must NOT have socket-directory volume mount (/var/run/prism-host).
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			if strings.Contains(args[i+1], "prism-host") {
				t.Errorf("unexpected socket-directory volume mount when HostAPITCPPort is set: %q", args[i+1])
			}
		}
	}
}

// TestBuildRunArgs_HostAPISockUnixPathLinux asserts that when HostAPITCPPort is
// zero and HostAPISockPath is non-empty (Linux path), the args still include the
// unix:// env var and socket-directory volume, and no --publish flag for port
// 4097 is present.
func TestBuildRunArgs_HostAPISockUnixPathLinux(t *testing.T) {
	const sockPath = "/home/user/.local/state/prism/run/repo@feat-hostapi.sock"
	m := New(Config{
		SessionName:     "repo@feat",
		AllocatedPort:   14000,
		HostAPISockPath: sockPath,
		HostAPITCPPort:  0, // Linux path
	})
	args := m.buildRunArgs()

	// Must have unix:// env var.
	wantEnv := "PRISM_HOST_API=unix:///var/run/prism-host/repo@feat-hostapi.sock"
	foundEnv := false
	for i, arg := range args {
		if arg == "--env" && i+1 < len(args) && args[i+1] == wantEnv {
			foundEnv = true
			break
		}
	}
	if !foundEnv {
		t.Errorf("expected --env %s in args, not found: %v", wantEnv, args)
	}

	// Must have socket-directory volume mount.
	wantMount := "/home/user/.local/state/prism/run:/var/run/prism-host:Z"
	foundMount := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) && args[i+1] == wantMount {
			foundMount = true
			break
		}
	}
	if !foundMount {
		t.Errorf("expected --volume %s in args, not found: %v", wantMount, args)
	}

	// Must NOT have --publish for a host-API port (the former HostAPIContainerPort
	// 4097 is now gone; the Linux path never emits --publish for host-API).
	for i, arg := range args {
		if arg == "--publish" && i+1 < len(args) {
			v := args[i+1]
			// The only --publish allowed is the opencode container port (4096).
			if !strings.HasSuffix(v, ":4096") {
				t.Errorf("unexpected --publish when HostAPITCPPort is zero: %q", v)
			}
		}
	}

	// Must NOT have http:// in PRISM_HOST_API.
	for i, arg := range args {
		if arg == "--env" && i+1 < len(args) {
			if strings.HasPrefix(args[i+1], "PRISM_HOST_API=http://") {
				t.Errorf("unexpected http:// PRISM_HOST_API when HostAPITCPPort is zero: %q", args[i+1])
			}
		}
	}
}

// TestBuildRunArgs_HostAPITCPPort_LoopbackOnly asserts that when HostAPITCPPort
// is set, no --publish flag is emitted for the host-API port (the container
// reaches the sidecar via host.containers.internal, not port forwarding).
func TestBuildRunArgs_HostAPITCPPort_LoopbackOnly(t *testing.T) {
	const hostPort = 51234
	m := New(Config{
		SessionName:    "repo@feat",
		AllocatedPort:  14000,
		HostAPITCPPort: hostPort,
	})
	args := m.buildRunArgs()

	// No --publish should reference the host-API TCP port.
	for i, arg := range args {
		if arg == "--publish" && i+1 < len(args) {
			v := args[i+1]
			// The only allowed --publish is the opencode container port (AllocatedPort:4096).
			if strings.Contains(v, fmt.Sprintf("%d:", hostPort)) || strings.HasSuffix(v, fmt.Sprintf(":%d", hostPort)) {
				t.Errorf("unexpected --publish referencing host-API TCP port %d: %q", hostPort, v)
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

func TestBuildRunArgs_HostOpencodeJsonNotMountedFromAllowlist(t *testing.T) {
	// The host's opencode.json must NOT be mounted via the config allowlist.
	// When ConfigContent is empty, no opencode.json should appear at all.
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
				t.Errorf("opencode.json must not be mounted when ConfigContent is empty, got: %q", v)
			}
		}
	}
}

func TestBuildRunArgs_OpencodeJsonMountedWhenConfigContentSet(t *testing.T) {
	// When ConfigContent is set, the generated temp file must be mounted
	// at /root/.config/opencode/opencode.json:ro.
	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		AgentRole:     "worker",
		ConfigContent: `{"model":"anthropic/claude-sonnet-4-6"}`,
	})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.HasSuffix(v, ":/root/.config/opencode/opencode.json:ro") {
				found = true
				// Source must be the temp file path.
				if !strings.Contains(v, "prism-opencode-config-") {
					t.Errorf("opencode.json mount source should be temp file, got: %q", v)
				}
				break
			}
		}
	}
	if !found {
		t.Errorf("opencode.json mount not found when ConfigContent is set; args: %v", args)
	}
}

func TestBuildRunArgs_NoConfigContentEnvVar(t *testing.T) {
	// OPENCODE_CONFIG_CONTENT must NOT be injected as an env var — the config
	// is now delivered via a mounted opencode.json file.
	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		AgentRole:     "worker",
		ConfigContent: `{"model":"anthropic/claude-sonnet-4-6"}`,
	})
	args := m.buildRunArgs()

	for i, arg := range args {
		if arg == "--env" && i+1 < len(args) {
			if strings.HasPrefix(args[i+1], "OPENCODE_CONFIG_CONTENT=") {
				t.Errorf("OPENCODE_CONFIG_CONTENT must not be injected as env var, got: %q", args[i+1])
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

func TestBuildRunArgs_ConfigMountAllowlist(t *testing.T) {
	// Verifies that the config mount block uses an explicit allowlist:
	// - All 8 allowlisted entries present on disk ARE mounted read-only.
	// - Bun ecosystem files (package.json, bun.lock, package-lock.json,
	//   node_modules/) are NOT mounted.
	// - The host's opencode.json is NOT mounted via the allowlist (it is
	//   delivered separately via the ConfigContent temp file mechanism).
	//
	// Previous tests (TestBuildRunArgs_HostOpencodeJsonNotMountedFromAllowlist,
	// TestBuildRunArgs_NoSingleWholeDirConfigMount) ran against an empty home dir,
	// so filepath.EvalSymlinks always failed and the config block produced zero
	// mount args — the allowlist was never actually exercised. This test populates
	// the config dir so the allowlist logic is fully covered.

	fakeHome := t.TempDir()
	configDir := filepath.Join(fakeHome, ".config", "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll configDir: %v", err)
	}

	// Allowlisted files/dirs that should be mounted (all 8 entries from configAllowlist).
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
		{"plugins", true},
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

	// Assert allowlisted entries ARE present (as :ro mounts).
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

	if err := m.writeGitconfig(isolationPodman); err != nil {
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

	if err := m.writeGitconfig(isolationPodman); err != nil {
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

	if err := m.writeGitconfig(isolationPodman); err != nil {
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

// TestWriteGitconfig_BwrapMode_UsesHostHomePaths verifies that writeGitconfig
// in bwrap mode writes signingKey and allowedSignersFile paths rooted at the
// host user's $HOME (not /root). The bwrap sandbox runs as the host user, so
// the canonical $HOME/.ssh/<generic-name> paths resolve to the host user's
// home — not the image root — and must match what bwrap.go mounts.
func TestWriteGitconfig_BwrapMode_UsesHostHomePaths(t *testing.T) {
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

	if err := m.writeGitconfig(isolationBwrap); err != nil {
		t.Fatalf("writeGitconfig(isolationBwrap) returned error: %v", err)
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

	wantSigning := "signingKey = " + fakeHome + "/.ssh/signing-key.pub"
	if !strings.Contains(content, wantSigning) {
		t.Errorf("bwrap gitconfig missing %q; content:\n%s", wantSigning, content)
	}
	wantAllowed := "allowedSignersFile = " + fakeHome + "/.ssh/allowed_signers"
	if !strings.Contains(content, wantAllowed) {
		t.Errorf("bwrap gitconfig missing %q; content:\n%s", wantAllowed, content)
	}
	// Must NOT contain the podman-only /root/.ssh/... paths.
	if strings.Contains(content, "/root/.ssh/signing-key.pub") {
		t.Errorf("bwrap gitconfig must not contain /root/.ssh/signing-key.pub; content:\n%s", content)
	}
	if strings.Contains(content, "/root/.ssh/allowed_signers") {
		t.Errorf("bwrap gitconfig must not contain /root/.ssh/allowed_signers; content:\n%s", content)
	}
}

// TestWriteSshConfig_PodmanMode_UsesRootPath verifies that writeSshConfig in
// podman mode embeds IdentityFile = /root/.ssh/access-key (the path where
// buildRunArgs mounts the access key inside the podman container).
func TestWriteSshConfig_PodmanMode_UsesRootPath(t *testing.T) {
	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
	})

	if err := m.writeSshConfig(isolationPodman); err != nil {
		t.Fatalf("writeSshConfig(isolationPodman): %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(m.sshConfigFilePath()) })

	data, err := os.ReadFile(m.sshConfigFilePath())
	if err != nil {
		t.Fatalf("read ssh config: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "IdentityFile /root/.ssh/access-key") {
		t.Errorf("podman ssh config missing IdentityFile /root/.ssh/access-key; content:\n%s", content)
	}
}

// TestWriteSshConfig_BwrapMode_UsesHostHomePath verifies that writeSshConfig
// in bwrap mode embeds IdentityFile = <hostHome>/.ssh/access-key rather than
// /root/.ssh/access-key. bwrap mounts the access key at the host user's
// $HOME/.ssh/ so the generated SSH config must reference the same path.
func TestWriteSshConfig_BwrapMode_UsesHostHomePath(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
	})

	if err := m.writeSshConfig(isolationBwrap); err != nil {
		t.Fatalf("writeSshConfig(isolationBwrap): %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(m.sshConfigFilePath()) })

	data, err := os.ReadFile(m.sshConfigFilePath())
	if err != nil {
		t.Fatalf("read ssh config: %v", err)
	}
	content := string(data)
	want := "IdentityFile " + fakeHome + "/.ssh/access-key"
	if !strings.Contains(content, want) {
		t.Errorf("bwrap ssh config missing %q; content:\n%s", want, content)
	}
	if strings.Contains(content, "IdentityFile /root/.ssh/access-key") {
		t.Errorf("bwrap ssh config must not contain /root/.ssh/access-key; content:\n%s", content)
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

	if err := m.writeGitconfig(isolationPodman); err != nil {
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

	if err := m.writeGitconfig(isolationPodman); err != nil {
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

			if err := m.writeGitconfig(isolationPodman); err != nil {
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

	if err := m.writeGitconfig(isolationPodman); err != nil {
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

	if err := m.writeGitconfig(isolationPodman); err != nil {
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

	if err := m.writeGitconfig(isolationPodman); err != nil {
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

	if err := m.writeGitconfig(isolationPodman); err != nil {
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

	if err := m.writeGitconfig(isolationPodman); err != nil {
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
	if err := m.writeGitconfig(isolationPodman); err != nil {
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

// ── writeClaudeCredentials / Darwin Keychain tests ───────────────────────────

// TestWriteClaudeCredentials_NoopOnLinux verifies that writeClaudeCredentials
// is a no-op on non-Darwin platforms: claudeCredentialsReady stays false and
// no temp file is written. On Darwin this test still passes because it only
// asserts the invariant that claudeCredentialsReady accurately tracks whether
// the file was written — the Keychain call may succeed or fail, but the
// ready flag must match.
func TestWriteClaudeCredentials_NoopOnLinux(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("skipping Linux-specific no-op check on Darwin")
	}
	m := New(Config{SessionName: "repo@feat", AllocatedPort: 9999})
	m.writeClaudeCredentials()
	if m.claudeCredentialsReady {
		t.Error("claudeCredentialsReady should be false on non-Darwin")
	}
	if _, err := os.Stat(m.claudeCredentialsFilePath()); !os.IsNotExist(err) {
		t.Errorf("credentials temp file should not exist on non-Darwin, got: %v", err)
	}
}

// TestBuildRunArgs_ClaudeCredentialsMountedWhenReady verifies that when
// claudeCredentialsReady is true (simulating a successful Keychain extraction),
// buildRunArgs includes the bind-mount at /root/.claude/.credentials.json.
func TestBuildRunArgs_ClaudeCredentialsMountedWhenReady(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14001})

	// Simulate a successful writeClaudeCredentials by writing a fake file and
	// setting the flag directly (avoids requiring actual Keychain access in CI).
	fakeCredsContent := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-test"}}`
	if err := os.WriteFile(m.claudeCredentialsFilePath(), []byte(fakeCredsContent), 0o600); err != nil {
		t.Fatalf("write fake creds: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(m.claudeCredentialsFilePath()) })
	m.claudeCredentialsReady = true

	args := m.buildRunArgs()

	wantDst := ":/root/.claude/.credentials.json:ro"
	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) && strings.HasSuffix(args[i+1], wantDst) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("buildRunArgs missing Claude credentials mount ending in %q", wantDst)
	}
}

// TestBuildRunArgs_ClaudeCredentialsNotMountedWhenNotReady verifies that when
// claudeCredentialsReady is false (e.g. not logged in, or Linux), buildRunArgs
// does NOT include the /root/.claude/.credentials.json bind-mount.
func TestBuildRunArgs_ClaudeCredentialsNotMountedWhenNotReady(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14002})
	// claudeCredentialsReady defaults to false — do not set it.

	args := m.buildRunArgs()

	wantDst := ":/root/.claude/.credentials.json:ro"
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) && strings.HasSuffix(args[i+1], wantDst) {
			t.Errorf("Claude credentials mount must not be present when claudeCredentialsReady=false; found %q", args[i+1])
		}
	}
}

// ── AWS config mount tests ───────────────────────────────────────────────────

// TestBuildRunArgs_AWSReadonlyConfigMounted verifies that
// ~/.config/aws/readonly-config is mounted at /root/.aws/config:ro when present.
func TestBuildRunArgs_AWSReadonlyConfigMounted(t *testing.T) {
	fakeHome := t.TempDir()
	awsDir := filepath.Join(fakeHome, ".config", "aws")
	if err := os.MkdirAll(awsDir, 0o700); err != nil {
		t.Fatalf("MkdirAll aws dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(awsDir, "readonly-config"), []byte("stub"), 0o600); err != nil {
		t.Fatalf("WriteFile readonly-config: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	wantDst := ":/root/.aws/config:ro"
	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) && strings.HasSuffix(args[i+1], wantDst) {
			found = true
			if !strings.Contains(args[i+1], "readonly-config") {
				t.Errorf("AWS config mount source should be readonly-config, got: %q", args[i+1])
			}
			break
		}
	}
	if !found {
		t.Errorf("AWS readonly-config mount at /root/.aws/config:ro not found in args: %v", args)
	}
}

// TestBuildRunArgs_AWSCredentialsMounted verifies that
// ~/.config/aws/credentials is mounted at /root/.aws/credentials:ro when present.
func TestBuildRunArgs_AWSCredentialsMounted(t *testing.T) {
	fakeHome := t.TempDir()
	awsDir := filepath.Join(fakeHome, ".config", "aws")
	if err := os.MkdirAll(awsDir, 0o700); err != nil {
		t.Fatalf("MkdirAll aws dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(awsDir, "credentials"), []byte("stub"), 0o600); err != nil {
		t.Fatalf("WriteFile credentials: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	wantDst := ":/root/.aws/credentials:ro"
	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) && strings.HasSuffix(args[i+1], wantDst) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("AWS credentials mount at /root/.aws/credentials:ro not found in args: %v", args)
	}
}

// TestBuildRunArgs_AWSSSOCacheMounted verifies that ~/.aws/sso is mounted at
// /root/.aws/sso:ro when present, so SSO tokens from `aws sso login` are
// available inside the container.
func TestBuildRunArgs_AWSSSOCacheMounted(t *testing.T) {
	fakeHome := t.TempDir()
	ssoDir := filepath.Join(fakeHome, ".aws", "sso")
	if err := os.MkdirAll(ssoDir, 0o700); err != nil {
		t.Fatalf("MkdirAll sso dir: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	wantMount := ssoDir + ":/root/.aws/sso:ro"
	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) && args[i+1] == wantMount {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("AWS SSO cache mount %q not found in args: %v", wantMount, args)
	}
}

// TestBuildRunArgs_AWSCLICacheMounted verifies that ~/.aws/cli is mounted at
// /root/.aws/cli:ro when present.
func TestBuildRunArgs_AWSCLICacheMounted(t *testing.T) {
	fakeHome := t.TempDir()
	cliDir := filepath.Join(fakeHome, ".aws", "cli")
	if err := os.MkdirAll(cliDir, 0o700); err != nil {
		t.Fatalf("MkdirAll cli dir: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	wantMount := cliDir + ":/root/.aws/cli:ro"
	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) && args[i+1] == wantMount {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("AWS CLI cache mount %q not found in args: %v", wantMount, args)
	}
}

// TestBuildRunArgs_AWSAdminConfigNotMounted verifies that the admin AWS config
// (config, not readonly-config) is never mounted — only readonly-config is used.
func TestBuildRunArgs_AWSAdminConfigNotMounted(t *testing.T) {
	fakeHome := t.TempDir()
	awsDir := filepath.Join(fakeHome, ".config", "aws")
	if err := os.MkdirAll(awsDir, 0o700); err != nil {
		t.Fatalf("MkdirAll aws dir: %v", err)
	}
	// Create both the admin config and the readonly config.
	for _, name := range []string{"config", "readonly-config"} {
		if err := os.WriteFile(filepath.Join(awsDir, name), []byte("stub"), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	// The whole aws dir must not be mounted.
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			// Whole-dir mount would look like .../aws:/root/.aws or .../aws:/root/.aws:ro
			if strings.Contains(v, "/.config/aws:/root/.aws") {
				t.Errorf("whole AWS config dir must not be mounted; found %q", v)
			}
			// Allow readonly-config → /root/.aws/config:ro, but not the admin
			// config → /root/.aws/config:ro. Check the source (before the first
			// colon) to distinguish them.
			if strings.HasSuffix(v, ":/root/.aws/config:ro") {
				parts := strings.SplitN(v, ":", 2)
				if !strings.HasSuffix(parts[0], "readonly-config") {
					t.Errorf("admin AWS config must not be mounted at /root/.aws/config; found %q", v)
				}
			}
		}
	}
}

// TestBuildRunArgs_AWSNotMountedWhenAbsent verifies that when neither
// ~/.config/aws nor ~/.aws exist, no AWS mounts are added.
func TestBuildRunArgs_AWSNotMountedWhenAbsent(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			if strings.Contains(args[i+1], "/root/.aws") {
				t.Errorf("AWS mount must not be present when no AWS files exist; found %q", args[i+1])
			}
		}
	}
}

// ── Kube config mount tests ──────────────────────────────────────────────────

// TestBuildRunArgs_KubeAgentsConfigMounted verifies that
// ~/.config/kube/agents-config is mounted at /root/.kube/config:ro when present.
func TestBuildRunArgs_KubeAgentsConfigMounted(t *testing.T) {
	fakeHome := t.TempDir()
	kubeDir := filepath.Join(fakeHome, ".config", "kube")
	if err := os.MkdirAll(kubeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll kube dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kubeDir, "agents-config"), []byte("stub"), 0o600); err != nil {
		t.Fatalf("WriteFile agents-config: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	wantDst := ":/root/.kube/config:ro"
	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) && strings.HasSuffix(args[i+1], wantDst) {
			found = true
			if !strings.Contains(args[i+1], "agents-config") {
				t.Errorf("kube mount source should be agents-config, got: %q", args[i+1])
			}
			break
		}
	}
	if !found {
		t.Errorf("kube agents-config mount at /root/.kube/config:ro not found in args: %v", args)
	}

	// No KUBECONFIG env var should be injected — the default path suffices.
	for i, arg := range args {
		if arg == "--env" && i+1 < len(args) && strings.HasPrefix(args[i+1], "KUBECONFIG=") {
			t.Errorf("KUBECONFIG must not be injected when mounting at default path; got %q", args[i+1])
		}
	}
}

// TestBuildRunArgs_KubeAdminConfigNotMounted verifies that admin kubeconfig
// files (config, config-home) are never mounted — only agents-config is exposed.
func TestBuildRunArgs_KubeAdminConfigNotMounted(t *testing.T) {
	fakeHome := t.TempDir()
	kubeDir := filepath.Join(fakeHome, ".config", "kube")
	if err := os.MkdirAll(kubeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll kube dir: %v", err)
	}
	for _, name := range []string{"config", "config-home", "agents-config"} {
		if err := os.WriteFile(filepath.Join(kubeDir, name), []byte("stub"), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			// Whole-dir mount must not be present.
			if strings.Contains(v, "/.config/kube:/root/.kube") {
				t.Errorf("whole kube config dir must not be mounted; found %q", v)
			}
			// Admin config files must not be the source of any /root/.kube mount.
			if strings.HasSuffix(v, ":/root/.kube/config:ro") {
				parts := strings.SplitN(v, ":", 2)
				if !strings.HasSuffix(parts[0], "agents-config") {
					t.Errorf("admin kube config must not be mounted at /root/.kube/config; found %q", v)
				}
			}
		}
	}
}

// TestBuildRunArgs_KubeNotMountedWhenAbsent verifies that when
// ~/.config/kube/agents-config does not exist, no kube mounts are added.
func TestBuildRunArgs_KubeNotMountedWhenAbsent(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			if strings.Contains(args[i+1], "/root/.kube") {
				t.Errorf("kube mount must not be present when agents-config is absent; found %q", args[i+1])
			}
		}
	}
}

// ── Terminal environment tests ───────────────────────────────────────────────

// TestBuildRunArgs_TermEnvSet verifies that TERM=xterm-256color is passed as
// an --env flag in the podman run arguments. Without this, podman defaults to
// TERM=xterm (plain) when --tty is used, which breaks mouse events and SGR
// mouse protocol selection inside the opencode TUI (issue #737).
func TestBuildRunArgs_TermEnvSet(t *testing.T) {
	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--env" && i+1 < len(args) && args[i+1] == "TERM=xterm-256color" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("TERM=xterm-256color not found in buildRunArgs output; args: %v", args)
	}
}

// ── MCP auth mount tests ─────────────────────────────────────────────────────

// TestBuildRunArgs_McpAuthMountedWhenPresent verifies that when ~/.mcp-auth
// exists on the host, it is bind-mounted read-write at /root/.mcp-auth inside
// the container so OAuth tokens written by mcp-remote persist back to the host.
func TestBuildRunArgs_McpAuthMountedWhenPresent(t *testing.T) {
	fakeHome := t.TempDir()
	mcpAuthDir := filepath.Join(fakeHome, ".mcp-auth")
	if err := os.MkdirAll(mcpAuthDir, 0o700); err != nil {
		t.Fatalf("MkdirAll .mcp-auth: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.HasSuffix(v, ":/root/.mcp-auth") {
				found = true
				// Must be read-write (no :ro suffix).
				if strings.Contains(v, ":ro") {
					t.Errorf("mcp-auth mount %q must be read-write (no :ro)", v)
				}
				// Source must be the actual mcpAuthDir path.
				if !strings.HasPrefix(v, mcpAuthDir+":") {
					t.Errorf("mcp-auth mount source %q should be %q", v, mcpAuthDir)
				}
				break
			}
		}
	}
	if !found {
		t.Errorf("mcp-auth volume mount at /root/.mcp-auth not found in args: %v", args)
	}
}

// TestBuildRunArgs_McpAuthNotMountedWhenAbsent verifies that when ~/.mcp-auth
// does not exist on the host, no mcp-auth mount is added and podman run
// succeeds without error (skipped entirely).
func TestBuildRunArgs_McpAuthNotMountedWhenAbsent(t *testing.T) {
	fakeHome := t.TempDir()
	// Intentionally do NOT create ~/.mcp-auth.
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			if strings.Contains(args[i+1], "/root/.mcp-auth") {
				t.Errorf("mcp-auth mount must not be present when ~/.mcp-auth is absent; found %q", args[i+1])
			}
		}
	}
}

// ── prepareVolumeDirs tests (AC-1, AC-4) ─────────────────────────────────────

// TestPrepareVolumeDirs_CreatesSessionDir verifies that prepareVolumeDirs creates
// the per-session opencode state directory, cache directories, and the clipboard
// staging directory under the given HOME, so that buildRunArgs() remains a pure
// argument builder.
func TestPrepareVolumeDirs_CreatesSessionDir(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "my-repo@feat", AllocatedPort: 14000})
	if err := m.prepareVolumeDirs(true); err != nil {
		t.Fatalf("prepareVolumeDirs: %v", err)
	}

	wantSession := filepath.Join(fakeHome, ".local", "share", "opencode", "prism-sessions", m.Name())
	wantOpencode := filepath.Join(fakeHome, ".cache", "opencode")
	wantBun := filepath.Join(fakeHome, ".cache", "bun")
	// Clipboard staging dir is pre-created so that the bind-mount in buildRunArgs()
	// is always active, even on the first ever paste operation.
	wantClipboard := filepath.Join(fakeHome, ".cache", "prism", "clipboard")

	for _, dir := range []string{wantSession, wantOpencode, wantBun, wantClipboard} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("expected directory to exist: %s (err: %v)", dir, err)
		}
	}
}

// TestPrepareVolumeDirs_BwrapSkipsSessionDir verifies that when called with
// perSessionOpencode=false (the bwrap path), the per-session opencode state
// directory under prism-sessions/ is NOT created. bwrap mode shares the host's
// ~/.local/share/opencode/ directly, so a per-session subdir would just be
// dead state on disk.
func TestPrepareVolumeDirs_BwrapSkipsSessionDir(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "my-repo@feat", AllocatedPort: 14000})
	if err := m.prepareVolumeDirs(false); err != nil {
		t.Fatalf("prepareVolumeDirs(false): %v", err)
	}

	// Per-session dir must NOT exist.
	perSession := filepath.Join(fakeHome, ".local", "share", "opencode", "prism-sessions", m.Name())
	if _, err := os.Stat(perSession); !os.IsNotExist(err) {
		t.Errorf("per-session dir %q should not exist for bwrap mode (stat err: %v)", perSession, err)
	}

	// Other shared dirs that bwrap also relies on should still be created.
	for _, dir := range []string{
		filepath.Join(fakeHome, ".cache", "opencode"),
		filepath.Join(fakeHome, ".cache", "bun"),
		filepath.Join(fakeHome, ".cache", "prism", "clipboard"),
	} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("expected directory to exist: %s (err: %v)", dir, err)
		}
	}
}

// ── auth.json overlay tests (AC-3) ───────────────────────────────────────────

// TestBuildRunArgs_AuthJsonOverlayMountedWhenExists verifies that when
// ~/.local/share/opencode/auth.json exists on the host, a --volume arg is
// added overlaying it at /root/.local/share/opencode/auth.json inside the
// container so OAuth tokens are shared across per-session state directories.
func TestBuildRunArgs_AuthJsonOverlayMountedWhenExists(t *testing.T) {
	fakeHome := t.TempDir()
	opencodeDir := filepath.Join(fakeHome, ".local", "share", "opencode")
	if err := os.MkdirAll(opencodeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll opencode dir: %v", err)
	}
	authJSON := filepath.Join(opencodeDir, "auth.json")
	if err := os.WriteFile(authJSON, []byte(`{"token":"stub"}`), 0o600); err != nil {
		t.Fatalf("WriteFile auth.json: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	// The mount must be read-write (:Z, not :ro,Z): the opencode-claude-auth
	// plugin calls writeFileSync on auth.json on every load, so a :ro mount
	// causes EROFS and breaks Anthropic auth inside the container.
	wantDst := ":/root/.local/share/opencode/auth.json:Z"
	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.HasSuffix(v, wantDst) {
				found = true
				// Source must be the host auth.json path.
				if !strings.HasPrefix(v, authJSON+":") {
					t.Errorf("auth.json overlay source %q should be %q", v, authJSON)
				}
				// Must NOT contain :ro — the plugin writes to this file.
				if strings.Contains(v, ":ro") {
					t.Errorf("auth.json overlay mount %q must not be read-only (:ro causes EROFS)", v)
				}
				break
			}
		}
	}
	if !found {
		t.Errorf("auth.json overlay --volume not found in args; args: %v", args)
	}
}

// TestBuildRunArgs_AuthJsonOverlaySkippedWhenMissing verifies that when
// ~/.local/share/opencode/auth.json does not exist on the host, no auth.json
// overlay --volume arg is added and the container starts successfully without it.
func TestBuildRunArgs_AuthJsonOverlaySkippedWhenMissing(t *testing.T) {
	fakeHome := t.TempDir()
	// Intentionally do NOT create auth.json — not even the parent directory.
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	wantDstSubstr := ":/root/.local/share/opencode/auth.json"
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			if strings.Contains(args[i+1], wantDstSubstr) {
				t.Errorf("auth.json overlay must not be present when auth.json is absent; found %q", args[i+1])
			}
		}
	}
}

// ── Clipboard staging directory mount tests ──────────────────────────────────

// TestBuildRunArgs_ClipboardStagingDirMountedWhenPresent verifies that when
// ~/.cache/prism/clipboard/ exists on the host, buildRunArgs adds a read-only
// bind-mount at the identical absolute path inside the container.
// This allows opencode's drag-drop handler to stat() the path verbatim and
// read the staged image bytes without any path translation.
func TestBuildRunArgs_ClipboardStagingDirMountedWhenPresent(t *testing.T) {
	fakeHome := t.TempDir()
	clipboardDir := filepath.Join(fakeHome, ".cache", "prism", "clipboard")
	if err := os.MkdirAll(clipboardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll clipboard dir: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	// The mount must be: <clipboardDir>:<clipboardDir>:ro
	// (identical host and container path, read-only).
	wantMount := clipboardDir + ":" + clipboardDir + ":ro"
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
		t.Errorf("clipboard staging dir mount %q not found in args: %v", wantMount, args)
	}
}

// TestBuildRunArgs_ClipboardStagingDirNotMountedWhenAbsent verifies that when
// ~/.cache/prism/clipboard/ does not exist at container spawn time, buildRunArgs
// skips the bind-mount (consistent with the conditional-mount pattern used for
// AWS/MCP-auth/kube config). The container starts normally without error.
func TestBuildRunArgs_ClipboardStagingDirNotMountedWhenAbsent(t *testing.T) {
	fakeHome := t.TempDir()
	// Intentionally do NOT create ~/.cache/prism/clipboard/.
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	// No mount should reference the clipboard staging directory.
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, filepath.Join("prism", "clipboard")) {
				t.Errorf("clipboard staging dir must not be mounted when absent; found %q", v)
			}
		}
	}
}

// TestBuildRunArgs_ClipboardMountIsReadOnly verifies that the clipboard staging
// directory bind-mount uses read-only access (:ro) from the container's
// perspective. The container reads staged image files but must not write to the
// host's staging directory.
func TestBuildRunArgs_ClipboardMountIsReadOnly(t *testing.T) {
	fakeHome := t.TempDir()
	clipboardDir := filepath.Join(fakeHome, ".cache", "prism", "clipboard")
	if err := os.MkdirAll(clipboardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll clipboard dir: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, filepath.Join("prism", "clipboard")) {
				if !strings.HasSuffix(v, ":ro") {
					t.Errorf("clipboard staging dir mount must be read-only, got: %q", v)
				}
				break
			}
		}
	}
}

// TestBuildRunArgs_ClipboardMountSamePathBothSides verifies that the clipboard
// staging directory is bind-mounted at the identical absolute path on both the
// host side and the container side. opencode's stat() call uses the host path
// (printed by `prism clipboard paste-image`) without any translation.
func TestBuildRunArgs_ClipboardMountSamePathBothSides(t *testing.T) {
	fakeHome := t.TempDir()
	clipboardDir := filepath.Join(fakeHome, ".cache", "prism", "clipboard")
	if err := os.MkdirAll(clipboardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll clipboard dir: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, filepath.Join("prism", "clipboard")) {
				// Volume spec: host-path:container-path[:opts]
				// Both sides must be the same absolute path.
				parts := strings.SplitN(v, ":", 3)
				if len(parts) < 2 {
					t.Errorf("malformed volume spec: %q", v)
					break
				}
				hostPath := parts[0]
				containerPath := parts[1]
				if hostPath != containerPath {
					t.Errorf("clipboard mount host path %q != container path %q (must be identical)", hostPath, containerPath)
				}
				break
			}
		}
	}
}

// TestBuildRunArgs_ClipboardMountNoWaylandOrX11Sockets verifies that no
// Wayland socket, X11 socket, dbus socket, pipewire, or pulseaudio socket is
// added alongside the clipboard staging directory mount. Clipboard read occurs
// entirely host-side; no clipboard access mechanism is exposed inside the container.
func TestBuildRunArgs_ClipboardMountNoWaylandOrX11Sockets(t *testing.T) {
	fakeHome := t.TempDir()
	clipboardDir := filepath.Join(fakeHome, ".cache", "prism", "clipboard")
	if err := os.MkdirAll(clipboardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll clipboard dir: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	args := m.buildRunArgs()

	// Specific paths that must NOT appear in volume mounts or env vars.
	// These are security-sensitive clipboard access mechanisms that must remain
	// entirely host-side. We check specific known paths/patterns rather than
	// substring matching to avoid false positives from temp dir names.
	forbiddenContainerPaths := []string{
		"/run/user",        // XDG_RUNTIME_DIR (contains Wayland sockets)
		".X11-unix",        // X11 socket directory
		"wayland-",         // Wayland socket files (wayland-0, wayland-1, etc.)
		"/run/dbus",        // dbus socket
		"/run/pipewire",    // pipewire socket
		"/run/pulseaudio",  // pulseaudio socket
		"xdg_runtime_dir",  // env var exposing the runtime dir
		"wayland_display=", // env var exposing Wayland socket name
		"display=:",        // env var exposing X11 display
	}
	for i, arg := range args {
		if (arg == "--volume" || arg == "--mount" || arg == "--env") && i+1 < len(args) {
			v := strings.ToLower(args[i+1])
			for _, f := range forbiddenContainerPaths {
				if strings.Contains(v, strings.ToLower(f)) {
					t.Errorf("forbidden mount/env pattern %q found in args (security: clipboard must be read host-side only): %q", f, args[i+1])
				}
			}
		}
	}
}

// ── agents/ mount conditional tests ─────────────────────────────────────────

// hasAgentsDirMount returns true when the args list contains a --mount or
// --volume argument whose value references /root/.config/opencode/agents as
// the destination. This is the canonical way to detect whether the agents/
// bind-mount was added to a container's run args.
func hasAgentsDirMount(args []string) bool {
	const dest = "/root/.config/opencode/agents"
	for i, arg := range args {
		if i+1 >= len(args) {
			continue
		}
		switch arg {
		case "--mount":
			// --mount type=bind,src=...,dst=/root/.config/opencode/agents,...
			if strings.Contains(args[i+1], "dst="+dest) {
				return true
			}
		case "--volume":
			// --volume /nix/store/...-agents:/root/.config/opencode/agents:ro
			// The destination is the second colon-separated field.
			v := args[i+1]
			parts := strings.SplitN(v, ":", 3)
			if len(parts) >= 2 && parts[1] == dest {
				return true
			}
		}
	}
	return false
}

// TestBuildRunArgs_ReviewContainerHasNoAgentsMount asserts that for a
// review-role container (AgentRole starting with "review-"), no --mount or
// --volume arg with destination /root/.config/opencode/agents appears in the
// generated args. Review containers embed the role prompt inline via the
// opencode.json "prompt" field; mounting the host agents/ directory would
// cause opencode to read the "mode: subagent" front-matter from the review-*.md
// files and override the "mode: primary" declaration in opencode.json.
func TestBuildRunArgs_ReviewContainerHasNoAgentsMount(t *testing.T) {
	reviewRoles := []string{
		"review-goal",
		"review-code",
		"review-security",
		"review-qa",
		"review-context",
	}
	for _, role := range reviewRoles {
		t.Run(role, func(t *testing.T) {
			fakeHome := t.TempDir()
			// Create a fake agents/ directory on the host so that if the
			// mount were (incorrectly) attempted, EvalSymlinks would succeed.
			agentsDir := filepath.Join(fakeHome, ".config", "opencode", "agents")
			if err := os.MkdirAll(agentsDir, 0o755); err != nil {
				t.Fatalf("MkdirAll agents dir: %v", err)
			}
			t.Setenv("HOME", fakeHome)

			m := New(Config{
				SessionName:   "repo@feat",
				AllocatedPort: 14000,
				AgentRole:     role,
			})
			args := m.buildRunArgs()

			if hasAgentsDirMount(args) {
				t.Errorf("review container with AgentRole=%q must not have agents/ mount at /root/.config/opencode/agents, but it does; args: %v", role, args)
			}
		})
	}
}

// TestBuildRunArgs_WorkerContainerHasAgentsMount asserts that for a worker
// container, an agents/ bind-mount with destination /root/.config/opencode/agents
// does appear in the generated args. This ensures no regression on non-review
// container mounts.
func TestBuildRunArgs_WorkerContainerHasAgentsMount(t *testing.T) {
	fakeHome := t.TempDir()
	// Create a real agents/ directory so EvalSymlinks succeeds and the mount
	// is actually added (the allowlist skips entries that don't exist).
	agentsDir := filepath.Join(fakeHome, ".config", "opencode", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll agents dir: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		AgentRole:     "worker",
	})
	args := m.buildRunArgs()

	if !hasAgentsDirMount(args) {
		t.Errorf("worker container must have agents/ mount at /root/.config/opencode/agents, but it does not; args: %v", args)
	}
}

// ── Resource cap tests (issue #868) ──────────────────────────────────────────

// TestBuildRunArgs_ResourceCapsPresentWhenSet asserts that when MemoryMax,
// MemorySwapMax, and PidsLimit are set on Config, the corresponding podman flags
// (--memory, --memory-swap, --pids-limit) appear in the buildRunArgs output.
func TestBuildRunArgs_ResourceCapsPresentWhenSet(t *testing.T) {
	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		MemoryMax:     "8g",
		MemorySwapMax: "8g",
		PidsLimit:     4096,
	})
	args := m.buildRunArgs()

	wantMemory := "--memory=8g"
	wantSwap := "--memory-swap=8g"
	wantPids := "--pids-limit=4096"

	foundMemory, foundSwap, foundPids := false, false, false
	for _, arg := range args {
		switch arg {
		case wantMemory:
			foundMemory = true
		case wantSwap:
			foundSwap = true
		case wantPids:
			foundPids = true
		}
	}
	if !foundMemory {
		t.Errorf("%q not found in args: %v", wantMemory, args)
	}
	if !foundSwap {
		t.Errorf("%q not found in args: %v", wantSwap, args)
	}
	if !foundPids {
		t.Errorf("%q not found in args: %v", wantPids, args)
	}
}

// TestBuildRunArgs_ResourceCapsAbsentWhenUnset asserts that when MemoryMax,
// MemorySwapMax, and PidsLimit are zero/empty (not set), no --memory,
// --memory-swap, or --pids-limit flags appear in buildRunArgs. This preserves
// existing behaviour for callers not using the nix module.
func TestBuildRunArgs_ResourceCapsAbsentWhenUnset(t *testing.T) {
	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		// MemoryMax, MemorySwapMax: empty string (zero value)
		// PidsLimit: 0 (zero value)
	})
	args := m.buildRunArgs()

	for _, arg := range args {
		if arg == "--memory" || strings.HasPrefix(arg, "--memory=") {
			t.Errorf("unexpected --memory flag when MemoryMax is empty: %q", arg)
		}
		if arg == "--memory-swap" || strings.HasPrefix(arg, "--memory-swap=") {
			t.Errorf("unexpected --memory-swap flag when MemorySwapMax is empty: %q", arg)
		}
		if arg == "--pids-limit" || strings.HasPrefix(arg, "--pids-limit=") {
			t.Errorf("unexpected --pids-limit flag when PidsLimit is zero: %q", arg)
		}
	}
}

// TestBuildRunArgs_ResourceCapMemoryOnlySet asserts that when only MemoryMax
// is set, only --memory is emitted; --memory-swap and --pids-limit are absent.
func TestBuildRunArgs_ResourceCapMemoryOnlySet(t *testing.T) {
	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
		MemoryMax:     "4g",
		// MemorySwapMax and PidsLimit intentionally left at zero values.
	})
	args := m.buildRunArgs()

	foundMemory := false
	for _, arg := range args {
		if arg == "--memory=4g" {
			foundMemory = true
		}
		if arg == "--memory-swap" || strings.HasPrefix(arg, "--memory-swap=") {
			t.Errorf("unexpected --memory-swap when MemorySwapMax is empty: %q", arg)
		}
		if arg == "--pids-limit" || strings.HasPrefix(arg, "--pids-limit=") {
			t.Errorf("unexpected --pids-limit when PidsLimit is zero: %q", arg)
		}
	}
	if !foundMemory {
		t.Errorf("--memory=4g not found in args: %v", args)
	}
}

// ── OpencodeConfigFilePath / WriteOpencodeConfig tests ───────────────────────

func TestOpencodeConfigFilePath_Deterministic(t *testing.T) {
	// The path derivation from session name must be stable across calls.
	const sessionName = "nixos-config@feat"
	a := OpencodeConfigFilePath(sessionName)
	b := OpencodeConfigFilePath(sessionName)
	if a != b {
		t.Errorf("OpencodeConfigFilePath is not deterministic: %q != %q", a, b)
	}
}

func TestOpencodeConfigFilePath_Format(t *testing.T) {
	// The path must equal filepath.Join(os.TempDir(), "prism-opencode-config-"+sessionName).
	const sessionName = "my-repo@branch"
	got := OpencodeConfigFilePath(sessionName)
	want := filepath.Join(os.TempDir(), "prism-opencode-config-"+sessionName)
	if got != want {
		t.Errorf("OpencodeConfigFilePath(%q) = %q, want %q", sessionName, got, want)
	}
}

func TestWriteOpencodeConfig_WritesContent(t *testing.T) {
	// Override TMPDIR so the written file lands in the test's temp dir.
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	const sessionName = "test-session@write"
	const content = `{"model":"claude-sonnet-4-6"}`

	if err := WriteOpencodeConfig(sessionName, content); err != nil {
		t.Fatalf("WriteOpencodeConfig returned error: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(OpencodeConfigFilePath(sessionName)) })

	data, err := os.ReadFile(OpencodeConfigFilePath(sessionName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != content {
		t.Errorf("file content = %q, want %q", string(data), content)
	}
}

func TestWriteOpencodeConfig_Mode644(t *testing.T) {
	// The written file must have mode 0o644.
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	const sessionName = "test-session@mode"
	if err := WriteOpencodeConfig(sessionName, "{}"); err != nil {
		t.Fatalf("WriteOpencodeConfig returned error: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(OpencodeConfigFilePath(sessionName)) })

	info, err := os.Stat(OpencodeConfigFilePath(sessionName))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("file mode = %04o, want 0644", got)
	}
}

func TestOpencodeConfigFilePath_MatchesManagerMethod(t *testing.T) {
	// The exported function must return the same path as the manager method.
	m := New(Config{
		SessionName:   "nixos-config@feature",
		AllocatedPort: 14001,
	})
	exported := OpencodeConfigFilePath(m.name)
	unexported := m.opencodeConfigFilePath()
	if exported != unexported {
		t.Errorf("OpencodeConfigFilePath(%q) = %q, manager.opencodeConfigFilePath() = %q — must be identical",
			m.name, exported, unexported)
	}
}
