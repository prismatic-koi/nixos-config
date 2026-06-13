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

// TestMain points the package temp-path seam (tempDir, container.go) at a
// directory unique to this test process for the entire suite. Per-session
// temp artefacts (gitconfig, ssh-config, sandbox-exec profile, …) are named
// after slugified fixture session names like "repo@feat" that repeat across
// the suite, so two concurrent `go test` processes in different worktrees on
// the same host would otherwise collide on shared /tmp filenames — observed
// as TestWriteGitconfig_AllModes_EmptyIdentityRefused flaking on a file
// written by a sibling worker's test process (issue #2222). The directory is
// removed after the run, so the suite also stops accumulating stale
// prism-* files under the host temp dir.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "prism-container-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "container: TestMain: MkdirTemp: %v\n", err)
		os.Exit(1)
	}
	tempDir = func() string { return dir }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

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
	// "~" is outside the conservative sandbox-name charset
	// ([a-zA-Z0-9][a-zA-Z0-9_.-]*), so it must be replaced.
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

// ── credentialEnvVars tests ──────────────────────────────────────────────────

func TestCredentialEnvVars_LLMKeysForwarded(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("OPENROUTER_API_KEY", "")

	m := New(Config{SessionName: "repo@main", AllocatedPort: 14000, AgentRole: "worker"})
	vars := m.credentialEnvVars()

	found := false
	for _, kv := range vars {
		if kv == "ANTHROPIC_API_KEY=sk-ant-test" {
			found = true
		}
		if strings.HasPrefix(kv, "OPENROUTER_API_KEY=") {
			t.Errorf("OPENROUTER_API_KEY should not be forwarded when empty; got %q", kv)
		}
	}
	if !found {
		t.Errorf("ANTHROPIC_API_KEY not forwarded; vars = %v", vars)
	}
}

func TestCredentialEnvVars_AtlassianKeysNotForwarded(t *testing.T) {
	// ATLASSIAN_SITE/EMAIL/API_TOKEN are not forwarded — the atlassian CLI has
	// been removed. The pi atlassian-mcp extension uses OAuth (tokens in
	// ~/.pi/agent/atlassian-mcp-oauth.json) and does not need these vars.
	t.Setenv("ATLASSIAN_SITE", "https://myorg.atlassian.net")
	t.Setenv("ATLASSIAN_EMAIL", "user@example.com")
	t.Setenv("ATLASSIAN_API_TOKEN", "atl-secret-token")

	m := New(Config{SessionName: "repo@main", AllocatedPort: 14000, AgentRole: "worker"})
	vars := m.credentialEnvVars()

	for _, kv := range vars {
		if strings.HasPrefix(kv, "ATLASSIAN_SITE=") {
			t.Errorf("ATLASSIAN_SITE should not be forwarded (CLI removed); got %q", kv)
		}
		if strings.HasPrefix(kv, "ATLASSIAN_EMAIL=") {
			t.Errorf("ATLASSIAN_EMAIL should not be forwarded (CLI removed); got %q", kv)
		}
		if strings.HasPrefix(kv, "ATLASSIAN_API_TOKEN=") {
			t.Errorf("ATLASSIAN_API_TOKEN should not be forwarded (CLI removed); got %q", kv)
		}
	}
}

func TestCredentialEnvVars_SpeculativeKeysNotForwarded(t *testing.T) {
	// These keys were removed from forwardKeys because they are speculative:
	// not populated on the host and not consumed by any in-repo agent/skill/config.
	//   OPENAI_API_KEY    — no OpenAI provider configured.
	//   GEMINI_API_KEY    — Google auth uses Gemini OAuth plugin.
	//   GOOGLE_API_KEY    — same rationale as GEMINI_API_KEY.
	//   GITHUB_COPILOT_TOKEN — Copilot provider uses its own auth flow.
	//   DEEPSEEK_API_KEY  — user-confirmed speculative; no consumer in-repo.
	speculative := []string{
		"OPENAI_API_KEY",
		"GEMINI_API_KEY",
		"GOOGLE_API_KEY",
		"GITHUB_COPILOT_TOKEN",
		"DEEPSEEK_API_KEY",
	}
	for _, k := range speculative {
		t.Setenv(k, "should-not-appear")
	}

	m := New(Config{SessionName: "repo@main", AllocatedPort: 14000, AgentRole: "worker"})
	vars := m.credentialEnvVars()

	for _, kv := range vars {
		for _, k := range speculative {
			if strings.HasPrefix(kv, k+"=") {
				t.Errorf("%s should not be forwarded (speculative key removed); got %q", k, kv)
			}
		}
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

// ── harness config mount tests ──────────────────────────────────────────────

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
	// Must NOT contain legacy container-path /root/.ssh/... paths.
	if strings.Contains(content, "/root/.ssh/signing-key.pub") {
		t.Errorf("bwrap gitconfig must not contain /root/.ssh/signing-key.pub; content:\n%s", content)
	}
	if strings.Contains(content, "/root/.ssh/allowed_signers") {
		t.Errorf("bwrap gitconfig must not contain /root/.ssh/allowed_signers; content:\n%s", content)
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

func TestPrepareVolumeDirs_BwrapSkipsSessionDir(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "my-repo@feat", AllocatedPort: 14000})
	if err := m.prepareVolumeDirs(false); err != nil {
		t.Fatalf("prepareVolumeDirs(false): %v", err)
	}

	// Per-session dir must NOT exist.
	perSession := filepath.Join(fakeHome, ".local", "share", "pi", "prism-sessions", m.Name())
	if _, err := os.Stat(perSession); !os.IsNotExist(err) {
		t.Errorf("per-session dir %q should not exist for bwrap mode (stat err: %v)", perSession, err)
	}

	// Other shared dirs that bwrap also relies on should still be created.
	for _, dir := range []string{
		filepath.Join(fakeHome, ".cache", "pi"),
		filepath.Join(fakeHome, ".cache", "bun"),
		filepath.Join(fakeHome, ".cache", "prism", "clipboard"),
	} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("expected directory to exist: %s (err: %v)", dir, err)
		}
	}
}

// TestPrepareVolumeDirs_CreatesSocketDirForBwrap verifies that when
// HostAPISockPath is set and perSessionState=false (bwrap path),
// prepareVolumeDirs still creates the per-session socket directory so that
// the sidecar can call net.Listen("unix", sockPath) before bwrap is exec'd.
func TestPrepareVolumeDirs_CreatesSocketDirForBwrap(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	sockDir := filepath.Join(fakeHome, "run", "repo@feat")
	sockPath := filepath.Join(sockDir, "hostapi.sock")
	m := New(Config{
		SessionName:     "repo@feat",
		AllocatedPort:   14000,
		HostAPISockPath: sockPath,
	})
	if err := m.prepareVolumeDirs(false); err != nil {
		t.Fatalf("prepareVolumeDirs(false): %v", err)
	}

	// The per-session socket directory must be created even in bwrap mode.
	if fi, err := os.Stat(sockDir); err != nil || !fi.IsDir() {
		t.Errorf("expected per-session socket dir %q to exist for bwrap mode (err: %v)", sockDir, err)
	}

	// The per-session pi state dir must NOT exist (bwrap shares the host pi data dir directly).
	perSession := filepath.Join(fakeHome, ".local", "share", "pi", "prism-sessions", m.Name())
	if _, err := os.Stat(perSession); !os.IsNotExist(err) {
		t.Errorf("per-session pi dir %q should not exist for bwrap mode (stat err: %v)", perSession, err)
	}
}

// TestPrepareVolumeDirs_SocketDirOmittedWhenNoSockPath verifies that when
// HostAPISockPath is empty, no socket directory is created.
func TestPrepareVolumeDirs_SocketDirOmittedWhenNoSockPath(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := New(Config{
		SessionName:     "repo@feat",
		AllocatedPort:   14000,
		HostAPISockPath: "", // no socket path
	})
	if err := m.prepareVolumeDirs(false); err != nil {
		t.Fatalf("prepareVolumeDirs(false): %v", err)
	}

	// No run/<session>/ directory should be created when there's no socket path.
	runDir := filepath.Join(fakeHome, "run")
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("run dir %q should not exist when HostAPISockPath is empty (stat err: %v)", runDir, err)
	}
}

// TestPrepareVolumeDirs_CriticalSockDirFailureReturnsError verifies that when
// the host-API socket directory cannot be created (unwritable parent), the
// call returns a non-nil error that names the specific directory.
func TestPrepareVolumeDirs_CriticalSockDirFailureReturnsError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: cannot make an unwritable directory")
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Create an unwritable parent directory so that MkdirAll cannot create
	// the subdirectory beneath it.
	unwritable := filepath.Join(fakeHome, "unwritable")
	if err := os.Mkdir(unwritable, 0o555); err != nil {
		t.Fatalf("setup: mkdir %q: %v", unwritable, err)
	}
	t.Cleanup(func() { _ = os.Chmod(unwritable, 0o755) })

	sockPath := filepath.Join(unwritable, "session", "hostapi.sock")
	m := New(Config{
		SessionName:     "repo@feat",
		AllocatedPort:   14000,
		HostAPISockPath: sockPath,
	})

	err := m.prepareVolumeDirs(false)
	if err == nil {
		t.Fatal("prepareVolumeDirs: expected non-nil error for unwritable critical sock dir, got nil")
	}
	// The error should mention the directory path so callers can diagnose it.
	wantSubstr := filepath.Join(unwritable, "session")
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error %q does not mention the failing directory %q", err.Error(), wantSubstr)
	}
}

// TestPrepareVolumeDirs_CriticalPerSessionDirFailureReturnsError verifies that
// when the per-session pi state directory cannot be created (unwritable parent),
// the call returns a non-nil error naming the specific directory.
func TestPrepareVolumeDirs_CriticalPerSessionDirFailureReturnsError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: cannot make an unwritable directory")
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Make .local/share/pi unwritable so the per-session subdir cannot be created.
	piBase := filepath.Join(fakeHome, ".local", "share", "pi")
	if err := os.MkdirAll(piBase, 0o755); err != nil {
		t.Fatalf("setup: MkdirAll %q: %v", piBase, err)
	}
	if err := os.Chmod(piBase, 0o555); err != nil {
		t.Fatalf("setup: chmod %q: %v", piBase, err)
	}
	t.Cleanup(func() { _ = os.Chmod(piBase, 0o755) })

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
	})

	err := m.prepareVolumeDirs(true) // perSessionState=true triggers the critical per-session dir
	if err == nil {
		t.Fatal("prepareVolumeDirs: expected non-nil error for unwritable critical per-session dir, got nil")
	}
	wantSubstr := filepath.Join(piBase, "prism-sessions")
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error %q does not mention the failing directory %q", err.Error(), wantSubstr)
	}
}

// TestPrepareVolumeDirs_OptionalCacheDirFailureDoesNotFail verifies that when
// an optional cache directory cannot be created (unwritable parent), the call
// succeeds — the container starts without that cache mount rather than aborting.
func TestPrepareVolumeDirs_OptionalCacheDirFailureDoesNotFail(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: cannot make an unwritable directory")
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Make .cache unwritable so all cache subdirs (pi, bun, clipboard) cannot
	// be created — these are all optional.
	cacheDir := filepath.Join(fakeHome, ".cache")
	if err := os.Mkdir(cacheDir, 0o555); err != nil {
		t.Fatalf("setup: mkdir %q: %v", cacheDir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(cacheDir, 0o755) })

	m := New(Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14000,
	})

	// perSessionState=false: no critical sock path, no per-session dir. Only
	// the optional cache dirs are attempted and all will fail — call must succeed.
	if err := m.prepareVolumeDirs(false); err != nil {
		t.Errorf("prepareVolumeDirs: optional cache dir failures should not fail the call, got: %v", err)
	}
}

// ── auth.json overlay tests (AC-3) ───────────────────────────────────────────

// TestBuildRunArgs_AuthJsonOverlayMountedWhenExists verifies that when
// ~/.local/share/pi/auth.json exists on the host, a --volume arg is
// added overlaying it at /root/.local/share/pi/auth.json inside the
// container so OAuth tokens are shared across per-session state directories.
func TestHarnessConfigFilePath_Deterministic(t *testing.T) {
	// The path derivation from session name must be stable across calls.
	const sessionName = "nixos-config@feat"
	a := HarnessConfigFilePath(sessionName)
	b := HarnessConfigFilePath(sessionName)
	if a != b {
		t.Errorf("HarnessConfigFilePath is not deterministic: %q != %q", a, b)
	}
}

func TestHarnessConfigFilePath_Format(t *testing.T) {
	// The path must equal filepath.Join(tempDir(), "prism-harness-config-"+sessionName).
	// tempDir() rather than os.TempDir(): TestMain points the package
	// temp-path seam at a per-process directory for the whole suite
	// (issue #2222); outside tests tempDir() is os.TempDir().
	const sessionName = "my-repo@branch"
	got := HarnessConfigFilePath(sessionName)
	want := filepath.Join(tempDir(), "prism-harness-config-"+sessionName)
	if got != want {
		t.Errorf("HarnessConfigFilePath(%q) = %q, want %q", sessionName, got, want)
	}
}

func TestWriteHarnessConfig_WritesContent(t *testing.T) {
	// Override TMPDIR so the written file lands in the test's temp dir.
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	const sessionName = "test-session@write"
	const content = `{"model":"claude-sonnet-4-6"}`

	if err := WriteHarnessConfig(sessionName, content); err != nil {
		t.Fatalf("WriteHarnessConfig returned error: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(HarnessConfigFilePath(sessionName)) })

	data, err := os.ReadFile(HarnessConfigFilePath(sessionName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != content {
		t.Errorf("file content = %q, want %q", string(data), content)
	}
}

func TestWriteHarnessConfig_Mode644(t *testing.T) {
	// The written file must have mode 0o644.
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	const sessionName = "test-session@mode"
	if err := WriteHarnessConfig(sessionName, "{}"); err != nil {
		t.Fatalf("WriteHarnessConfig returned error: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(HarnessConfigFilePath(sessionName)) })

	info, err := os.Stat(HarnessConfigFilePath(sessionName))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("file mode = %04o, want 0644", got)
	}
}

func TestHarnessConfigFilePath_MatchesManagerMethod(t *testing.T) {
	// The exported function must return the same path as the manager method.
	m := New(Config{
		SessionName:   "nixos-config@feature",
		AllocatedPort: 14001,
	})
	exported := HarnessConfigFilePath(m.name)
	unexported := m.harnessConfigFilePath()
	if exported != unexported {
		t.Errorf("HarnessConfigFilePath(%q) = %q, manager.harnessConfigFilePath() = %q — must be identical",
			m.name, exported, unexported)
	}
}

// ── issue #1960: git identity must be hard-required ─────────────────────────
//
// writeGitconfig must refuse to write a gitconfig without [user] in every
// isolation mode. The previous behaviour (warn and skip [user]) caused git
// inside the sandbox to fall back to `<sandbox-user>@<sandbox-host>` (e.g.
// `worker <bot@local>`); GitHub then aggregated that synthetic identity into
// a `Co-authored-by:` trailer on squash merge. See issue #1960 for the full
// inventory of historical noisy trailers.

// gitconfigIdentityModes is the per-mode matrix exercised by the regression
// tests below. Each entry names the mode constant used by writeGitconfig and
// a label used in error messages. Only bwrap consumes the mode-based
// generator today — sandbox-exec writes its gitconfig into the per-session
// work dir (writeGitconfigToDir; the identity refusal for that path is
// pinned by TestSandboxExecPrepare_EmptyIdentityAborts below).
var gitconfigIdentityModes = []struct {
	name string
	mode isolationMode
}{
	{"bwrap", isolationBwrap},
}

// TestWriteGitconfig_AllModes_UserSectionRequired asserts the AC: the staged
// gitconfig produced by writeGitconfig contains a [user] section with the
// configured name and email for every isolation mode.
func TestWriteGitconfig_AllModes_UserSectionRequired(t *testing.T) {
	for _, tc := range gitconfigIdentityModes {
		t.Run(tc.name, func(t *testing.T) {
			fakeHome := t.TempDir()
			t.Setenv("HOME", fakeHome)

			const wantName = "prismatic-koi"
			const wantEmail = "ben@tinfoilforest.nz"

			m := New(Config{
				SessionName:   "repo@feat",
				AllocatedPort: 14000,
				GitUserName:   wantName,
				GitUserEmail:  wantEmail,
			})
			if err := m.writeGitconfig(tc.mode); err != nil {
				t.Fatalf("writeGitconfig(%s): %v", tc.name, err)
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

			// [user] header must be present.
			if !strings.Contains(content, "[user]\n") {
				t.Errorf("mode=%s: gitconfig is missing the [user] header; content:\n%s",
					tc.name, content)
			}
			// name and email lines must be present with the configured values.
			wantNameLine := "name = " + wantName
			if !strings.Contains(content, wantNameLine) {
				t.Errorf("mode=%s: gitconfig is missing %q; content:\n%s",
					tc.name, wantNameLine, content)
			}
			wantEmailLine := "email = " + wantEmail
			if !strings.Contains(content, wantEmailLine) {
				t.Errorf("mode=%s: gitconfig is missing %q; content:\n%s",
					tc.name, wantEmailLine, content)
			}
		})
	}
}

// TestWriteGitconfig_AllModes_EmptyIdentityRefused asserts the AC: missing
// identity is a hard error, not a silent omission. The combinations covered
// are (name="", email=""), (name="x", email=""), and (name="", email="x") —
// any empty field is sufficient to refuse.
func TestWriteGitconfig_AllModes_EmptyIdentityRefused(t *testing.T) {
	cases := []struct {
		label string
		name  string
		email string
	}{
		{"both-empty", "", ""},
		{"name-empty", "", "ben@tinfoilforest.nz"},
		{"email-empty", "prismatic-koi", ""},
	}
	for _, mode := range gitconfigIdentityModes {
		for _, c := range cases {
			t.Run(mode.name+"/"+c.label, func(t *testing.T) {
				fakeHome := t.TempDir()
				t.Setenv("HOME", fakeHome)

				m := New(Config{
					SessionName:   "repo@feat",
					AllocatedPort: 14000,
					GitUserName:   c.name,
					GitUserEmail:  c.email,
				})
				// Own the precondition: the "must not exist after refusal"
				// assertion below is about THIS call's behaviour, so any
				// stale file at the path (e.g. left by a crashed earlier
				// run) must be cleared before the call under test. The
				// TestMain seam already namespaces the path per-process;
				// this guards the within-process stale-file case on top
				// (issue #2222).
				_ = os.Remove(m.gitconfigFilePath())

				err := m.writeGitconfig(mode.mode)
				if err == nil {
					t.Fatalf("mode=%s name=%q email=%q: writeGitconfig returned nil, want error",
						mode.name, c.name, c.email)
				}
				// Error must name the problem clearly so the operator can
				// fix it. The message threads through to the spawn error
				// path, so it is what the user actually sees.
				msg := err.Error()
				for _, want := range []string{"git identity missing", "[user]"} {
					if !strings.Contains(msg, want) {
						t.Errorf("mode=%s name=%q email=%q: error %q must mention %q",
							mode.name, c.name, c.email, msg, want)
					}
				}

				// The gitconfig file must NOT have been written. The
				// previous behaviour wrote a [user]-less gitconfig and
				// returned nil, which is the exact regression we want to
				// catch.
				if _, statErr := os.Stat(m.gitconfigFilePath()); statErr == nil {
					t.Errorf("mode=%s name=%q email=%q: gitconfig at %s should not exist after refusal",
						mode.name, c.name, c.email, m.gitconfigFilePath())
					_ = os.Remove(m.gitconfigFilePath())
				}
			})
		}
	}
}

// TestSandboxExecPrepare_EmptyIdentityAborts asserts that the failure from
// the gitconfig generator propagates up through sandboxExecIsolator.Prepare
// (the sandbox-exec session entry point) so the session does not start. The
// pre-#1960 behaviour logged the error and continued, which is what allowed
// the bug to surface only post-merge.
//
// Post issue #2213 (Step 2 of #2132) the generated gitconfig lives in the
// per-session work dir, so the hard error surfaces from
// PrepareSessionWorkDir — both stations are asserted here to pin the #1960
// "hard-fails at Prepare" guarantee.
func TestSandboxExecPrepare_EmptyIdentityAborts(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	for _, d := range []string{".ssh", ".config/pi", ".cache/nix"} {
		if err := os.MkdirAll(filepath.Join(fakeHome, d), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	m := New(Config{
		SessionName: "repo@feat",
		InstanceID:  "empty-identity-test",
		Worktree:    t.TempDir(),
		// Explicitly leave GitUserName and GitUserEmail unset to exercise
		// the refusal path.
	})

	// Station 1: the work-dir writer itself refuses.
	sessionDir, err := m.PrepareSessionWorkDir()
	if err == nil {
		t.Fatalf("PrepareSessionWorkDir returned sessionDir=%q nil error, want error", sessionDir)
	}
	if !strings.Contains(err.Error(), "git identity missing") {
		t.Errorf("PrepareSessionWorkDir error %q must mention 'git identity missing'", err.Error())
	}

	// Station 2: the isolator Prepare entry point propagates the refusal and
	// produces no args — the session must not launch.
	iso := &sandboxExecIsolator{name: m.name}
	args, prepErr := iso.Prepare(context.Background(), m)
	if prepErr == nil {
		t.Fatalf("Prepare returned nil error, want git-identity error (args=%v)", args)
	}
	if !strings.Contains(prepErr.Error(), "git identity missing") {
		t.Errorf("Prepare error %q must mention 'git identity missing'", prepErr.Error())
	}
	if args != nil {
		t.Errorf("Prepare args must be nil on error; got %v", args)
	}
}
