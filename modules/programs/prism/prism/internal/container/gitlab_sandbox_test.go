package container

// gitlab_sandbox_test.go — unit coverage for the gitlab.com sandbox reach:
//
//   - the generated ssh config carries a `Host gitlab.com` stanza in BOTH
//     isolation modes, pointing at the SAME mounted access key as github.com;
//   - the github.com stanza is unchanged;
//   - glab's config dir is redirected to per-session, sandbox-writable state
//     in both modes;
//   - the sandbox-exec secrets.d allowlist admits `gitlab_token` when — and
//     ONLY when — a GitLab token path is configured, and never admits any
//     other secret name.
//
// The Darwin host-run coverage (real ssh to gitlab.com, real glab) cannot run
// in a nix build sandbox; see the PR for the host commands.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// sshStanzas parses a generated ssh config into host → directives, where each
// directive keeps its "Keyword Value" form with surrounding whitespace
// trimmed. Anything before the first Host line is an error the caller sees as
// a missing host.
func sshStanzas(t *testing.T, content string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	host := ""
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "Host "); ok {
			host = strings.TrimSpace(rest)
			if _, dup := out[host]; dup {
				t.Errorf("ssh config declares Host %q twice; content:\n%s", host, content)
			}
			out[host] = nil
			continue
		}
		if host == "" {
			t.Errorf("ssh config has directive %q before any Host line; content:\n%s", line, content)
			continue
		}
		out[host] = append(out[host], line)
	}
	return out
}

// assertForgeStanzas asserts that content declares exactly the github.com and
// gitlab.com stanzas, each carrying the same directives and the given
// IdentityFile.
func assertForgeStanzas(t *testing.T, what, content, wantIdentity string) {
	t.Helper()
	stanzas := sshStanzas(t, content)

	var hosts []string
	for h := range stanzas {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	want := []string{"github.com", "gitlab.com"}
	if strings.Join(hosts, ",") != strings.Join(want, ",") {
		t.Fatalf("%s declares hosts %v, want exactly %v; content:\n%s", what, hosts, want, content)
	}

	for _, host := range want {
		got := stanzas[host]
		wantDirectives := []string{
			"StrictHostKeyChecking accept-new",
			"IdentityFile " + wantIdentity,
			"IdentitiesOnly yes",
		}
		if len(got) != len(wantDirectives) {
			t.Errorf("%s: Host %s has %d directives %v, want %d %v; content:\n%s",
				what, host, len(got), got, len(wantDirectives), wantDirectives, content)
			continue
		}
		for i, wantLine := range wantDirectives {
			if got[i] != wantLine {
				t.Errorf("%s: Host %s directive %d = %q, want %q; content:\n%s",
					what, host, i, got[i], wantLine, content)
			}
		}
	}
}

// TestWriteSshConfig_BwrapMode_GitHubAndGitLabStanzas pins AC #1 for the
// bwrap generator: both forge stanzas are present and both point at the
// generic access-key path the bwrap bind-mount provides.
func TestWriteSshConfig_BwrapMode_GitHubAndGitLabStanzas(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	if err := m.writeSshConfig(isolationBwrap); err != nil {
		t.Fatalf("writeSshConfig(isolationBwrap): %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(m.sshConfigFilePath()) })

	data, err := os.ReadFile(m.sshConfigFilePath())
	if err != nil {
		t.Fatalf("read ssh config: %v", err)
	}
	assertForgeStanzas(t, "bwrap ssh config", string(data),
		filepath.Join(fakeHome, ".ssh", "access-key"))
}

// TestWriteSshConfigToDir_GitHubAndGitLabStanzas pins AC #1 for the
// sandbox-exec generator: both forge stanzas are present and both point at
// the STABLE sops symlink path (never a resolved secrets.d/<N> path, which
// rotates).
func TestWriteSshConfigToDir_GitHubAndGitLabStanzas(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "gitlab-ssh-stanza-test",
	})
	sessionDir, err := m.PrepareSessionWorkDir()
	if err != nil {
		t.Fatalf("PrepareSessionWorkDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sessionDir) })

	data, err := os.ReadFile(SessionWorkDirSshConfigPath(sessionDir))
	if err != nil {
		t.Fatalf("read generated ssh-config: %v", err)
	}
	content := string(data)
	assertForgeStanzas(t, "work-dir ssh-config", content,
		filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519"))

	if strings.Contains(content, "secrets.d") {
		t.Errorf("ssh-config embeds a resolved secrets.d path — breaks on sops rotation; content:\n%s", content)
	}
}

// TestSandboxSshConfig_GeneratorsDoNotDrift is the drift guard: the two
// generators must not duplicate the stanza string, or a host added to one
// would be silently missing from the other. They share sandboxSshConfig,
// and this test fails if that is undone — the
// host set and the directive shape must match for the same identity file.
func TestSandboxSshConfig_GeneratorsDoNotDrift(t *testing.T) {
	const identity = "/home/agent/.ssh/some-key"

	// Every host in the shared list must appear, with the identity file.
	got := sandboxSshConfig(identity)
	stanzas := sshStanzas(t, got)
	if len(stanzas) != len(SandboxSshHosts) {
		t.Fatalf("sandboxSshConfig emitted %d stanzas, want %d (%v); content:\n%s",
			len(stanzas), len(SandboxSshHosts), SandboxSshHosts, got)
	}
	for _, host := range SandboxSshHosts {
		directives, ok := stanzas[host]
		if !ok {
			t.Errorf("sandboxSshConfig missing Host %s; content:\n%s", host, got)
			continue
		}
		if !containsLine(directives, "IdentityFile "+identity) {
			t.Errorf("Host %s does not use the passed IdentityFile %q; directives: %v", host, identity, directives)
		}
	}

	// The bwrap generator writes exactly what the shared generator returns
	// for its own identity path — no extra or missing stanza.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	if err := m.writeSshConfig(isolationBwrap); err != nil {
		t.Fatalf("writeSshConfig(isolationBwrap): %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(m.sshConfigFilePath()) })
	data, err := os.ReadFile(m.sshConfigFilePath())
	if err != nil {
		t.Fatalf("read ssh config: %v", err)
	}
	want := sandboxSshConfig(filepath.Join(fakeHome, ".ssh", "access-key"))
	if string(data) != want {
		t.Errorf("bwrap ssh config drifted from sandboxSshConfig.\ngot:\n%s\nwant:\n%s", data, want)
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// TestSessionWorkDirGlabEnv pins the glab config-dir redirect the
// sandbox-exec dispatcher injects. glab aborts on an
// unreadable config dir, so without this every in-sandbox glab call fails.
func TestSessionWorkDirGlabEnv(t *testing.T) {
	dir := "/home/u/.local/state/prism/sessions/abc123"

	if got, want := SessionWorkDirGlabConfigDirPath(dir), dir+"/glab-cli"; got != want {
		t.Errorf("SessionWorkDirGlabConfigDirPath = %q, want %q", got, want)
	}

	got := SessionWorkDirGlabEnv(dir)
	want := []string{"GLAB_CONFIG_DIR=" + dir + "/glab-cli"}
	if len(got) != len(want) {
		t.Fatalf("SessionWorkDirGlabEnv returned %d vars, want %d: %v", len(got), len(want), got)
	}
	if got[0] != want[0] {
		t.Errorf("SessionWorkDirGlabEnv[0] = %q, want %q", got[0], want[0])
	}

	// The redirect must never point at the real host config dir, which holds
	// the owner's interactive glab login.
	if strings.Contains(got[0], ".config/glab-cli") {
		t.Errorf("GLAB_CONFIG_DIR points at the host glab config dir: %q", got[0])
	}
}

// TestBuildArgs_GlabConfigDirRedirect verifies the bwrap half of the same
// redirect: GLAB_CONFIG_DIR points at the per-session /tmp tmpfs, so glab's
// config never reaches the host.
func TestBuildArgs_GlabConfigDirRedirect(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{SessionName: "repo@feat", AllocatedPort: 14000})
	defer cleanup()

	args := m.isolator.(*bwrapIsolator).BuildArgs(m)

	if !hasSetenv(args, "GLAB_CONFIG_DIR", bwrapGlabConfigDir) {
		t.Errorf("--setenv GLAB_CONFIG_DIR %s not found in args: %v", bwrapGlabConfigDir, redactedArgs(args))
	}
	if !strings.HasPrefix(bwrapGlabConfigDir, "/tmp/") {
		t.Errorf("bwrapGlabConfigDir = %q, want a path on the per-session /tmp tmpfs", bwrapGlabConfigDir)
	}
}

// gitlabTokenExceptionRule is the require-not exception the profile must
// carry for the gitlab_token secret name.
const gitlabTokenExceptionRule = `    (require-not (regex #"/secrets\.d/[0-9]+/gitlab_token$"))` + "\n"

// TestGenerateProfile_SecretsDAllowlistAdmitsGitLabToken verifies that a
// configured, sops-backed GitLabTokenPath produces exactly one extra
// require-not exception — for `gitlab_token` and nothing else.
func TestGenerateProfile_SecretsDAllowlistAdmitsGitLabToken(t *testing.T) {
	fakeHome := newFakeHome(t)
	secretsBase := filepath.Join(t.TempDir(), "secrets.d")

	// The GitLab token arrives as a stable host symlink into secrets.d,
	// exactly like the ssh keys — home-manager sops on Darwin puts it at
	// ~/.config/sops-nix/secrets/gitlab_token.
	sopsDir := filepath.Join(fakeHome, ".config", "sops-nix", "secrets")
	if err := os.MkdirAll(sopsDir, 0o700); err != nil {
		t.Fatalf("create fake sops symlink dir: %v", err)
	}
	fakeSopsChain(t, fakeHome, secretsBase, "42", map[string]string{
		".ssh/prismatic-koi-ed25519":            "ssh/prismatic-koi-ed25519",
		".config/sops-nix/secrets/gitlab_token": "gitlab_token",
	})
	gitlabTokenPath := filepath.Join(sopsDir, "gitlab_token")

	m := newSandboxExecManager(Config{
		SessionName:     "repo@main",
		GitLabTokenPath: gitlabTokenPath,
	})
	profile := generateProfile(m)

	if !strings.Contains(profile, gitlabTokenExceptionRule) {
		t.Errorf("profile missing the gitlab_token allowlist exception:\n%s\nfull profile:\n%s",
			gitlabTokenExceptionRule, profile)
	}
	// The counter must not be baked in — the rotation property.
	if strings.Contains(profile, `/secrets\.d/42/`) {
		t.Errorf("profile bakes the concrete secrets.d counter into a regex; full profile:\n%s", profile)
	}
	// The un-deny is one name. No other secret may ride along — in
	// particular the daily-driver github_token stays denied.
	for _, denied := range []string{"github_token", "age-keys.txt", "workkube"} {
		if strings.Contains(profile, "/"+denied+"$") {
			t.Errorf("profile allowlists %q — the #2211 inventory rule permits only consumed secrets; full profile:\n%s",
				denied, profile)
		}
	}
}

// TestGenerateProfile_SecretsDAllowlistOmitsGitLabTokenWhenUnconfigured is
// the paired negative: a host with no GitLab token configured (the default —
// nx.programs.gitlab-cli.enable is false) emits no gitlab exception, so the
// secret stays denied.
func TestGenerateProfile_SecretsDAllowlistOmitsGitLabTokenWhenUnconfigured(t *testing.T) {
	fakeHome := newFakeHome(t)
	secretsBase := filepath.Join(t.TempDir(), "secrets.d")

	// The secret EXISTS on the host and is sops-backed; only the config key
	// is absent. Existence alone must never admit it.
	sopsDir := filepath.Join(fakeHome, ".config", "sops-nix", "secrets")
	if err := os.MkdirAll(sopsDir, 0o700); err != nil {
		t.Fatalf("create fake sops symlink dir: %v", err)
	}
	fakeSopsChain(t, fakeHome, secretsBase, "42", map[string]string{
		".config/sops-nix/secrets/gitlab_token": "gitlab_token",
	})

	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	if strings.Contains(profile, gitlabTokenExceptionRule) {
		t.Errorf("profile allowlists gitlab_token with no GitLabTokenPath configured; full profile:\n%s", profile)
	}
	if strings.Contains(profile, "gitlab_token") {
		t.Errorf("profile mentions gitlab_token with no GitLabTokenPath configured; full profile:\n%s", profile)
	}
}

// TestGenerateProfile_SecretsDAllowlistIgnoresNonSopsGitLabPath verifies that
// a GitLabTokenPath that does not resolve into a secrets.d tree (a plain
// file, e.g. a Linux /run/secrets path or a test fixture) produces no
// exception: the secrets.d deny never covers it, so no carve-out is needed
// and none is emitted.
func TestGenerateProfile_SecretsDAllowlistIgnoresNonSopsGitLabPath(t *testing.T) {
	fakeHome := newFakeHome(t)
	plain := filepath.Join(fakeHome, "plain-gitlab-token")
	if err := os.WriteFile(plain, []byte("dummy"), 0o600); err != nil {
		t.Fatalf("write plain token file: %v", err)
	}

	m := newSandboxExecManager(Config{
		SessionName:     "repo@main",
		GitLabTokenPath: plain,
	})
	profile := generateProfile(m)

	if got := strings.Count(profile, "(require-not "); got != 0 {
		t.Errorf("expected zero require-not exceptions for a non-sops GitLab path, got %d; full profile:\n%s", got, profile)
	}
}
