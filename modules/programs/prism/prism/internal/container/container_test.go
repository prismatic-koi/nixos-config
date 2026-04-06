package container

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	// AC-4: opencode config mounted read-only into /root/.config/opencode.
	m := New(Config{
		SessionName:   "my-repo@feat",
		AllocatedPort: 14000,
	})
	args := m.buildRunArgs()

	found := false
	for i, arg := range args {
		if arg == "--volume" && i+1 < len(args) {
			v := args[i+1]
			if strings.Contains(v, ":/root/.config/opencode:") {
				found = true
				if !strings.Contains(v, ":ro") {
					t.Errorf("opencode config mount %q should be :ro (AC-4)", v)
				}
				// Must NOT have :Z (config is read-only, no relabelling needed).
				if strings.HasSuffix(v, ":Z") || strings.Contains(v, ":ro,Z") {
					t.Errorf("opencode config mount %q should not have :Z (AC-4)", v)
				}
				break
			}
		}
	}
	if !found {
		t.Errorf("opencode config volume mount not found in args: %v", args)
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

func TestBuildRunArgs_NetworkSlirp4netns(t *testing.T) {
	// AC-12: explicit network mode for isolation.
	m := New(Config{SessionName: "repo@br", AllocatedPort: 14000})
	args := m.buildRunArgs()
	found := false
	for i, arg := range args {
		if arg == "--network" && i+1 < len(args) && args[i+1] == "slirp4netns" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("--network slirp4netns not found in args: %v", args)
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
			if strings.Contains(v, "/prism-git") {
				t.Errorf("unexpected git mount when BareRoot is empty: %q", v)
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

func TestCredentialEnvVars_GitDirInjectedWhenBareRootSet(t *testing.T) {
	m := New(Config{
		SessionName:    "repo@feat",
		AllocatedPort:  14000,
		BareRoot:       "/home/user/code/my-repo",
		WorktreeGitDir: "/home/user/code/my-repo/.bare/worktrees/feat",
	})
	vars := m.credentialEnvVars()
	foundGitDir := false
	for _, kv := range vars {
		if kv == "GIT_DIR=/prism-git/worktrees/feat" {
			foundGitDir = true
		}
		if strings.HasPrefix(kv, "GIT_COMMON_DIR=") {
			t.Errorf("GIT_COMMON_DIR should not be injected; got %q", kv)
		}
	}
	if !foundGitDir {
		t.Errorf("GIT_DIR=/prism-git/worktrees/feat not injected; vars=%v", vars)
	}
}

func TestCredentialEnvVars_NoGitDirWhenBareRootEmpty(t *testing.T) {
	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
	})
	vars := m.credentialEnvVars()
	for _, kv := range vars {
		if strings.HasPrefix(kv, "GIT_DIR=") {
			t.Errorf("GIT_DIR should not be injected when BareRoot is empty; got %q", kv)
		}
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
		got := isNoSuchContainer(tc.output)
		if got != tc.want {
			t.Errorf("isNoSuchContainer(%q) = %v, want %v", tc.output, got, tc.want)
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
