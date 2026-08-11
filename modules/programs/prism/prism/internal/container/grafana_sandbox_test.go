package container

// grafana_sandbox_test.go — unit coverage for the pi grafana MCP config
// bundle carve-out in the sandbox-exec secrets.d allowlist (issue #2746):
//
//   - the allowlist admits the bundle's secret name when — and ONLY when — a
//     Grafana config path is configured on the Manager;
//   - it admits exactly ONE name, so no other secret rides along;
//   - a non-sops path produces no exception at all;
//   - the bwrap argv is untouched by the new Config field, which is
//     sandbox-exec-only (bwrap keeps deriving its --ro-bind from
//     AgentEnvVars, issue #2452).
//
// The Darwin host-run coverage (a real /usr/bin/sandbox-exec read of the
// bundle) lives in
// internal/integration/sandbox_exec_grafana_config_darwin_test.go, per the
// sandbox-exec testing convention (docs/sandbox-exec-testing.md, #1192).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// grafanaSecretName is the sops key the pi grafana extension reads on a host
// configured with nx.programs.prism.pi.grafana.config = "home" — the bundle
// navi, tui, and m4mac all share.
const grafanaSecretName = "grafana_config_home"

// grafanaExceptionRule is the require-not exception the profile must carry
// for the grafana config bundle.
const grafanaExceptionRule = `    (require-not (regex #"/secrets\.d/[0-9]+/` + grafanaSecretName + `$"))` + "\n"

// fakeGrafanaSopsPath builds the stable host symlink for the grafana bundle
// inside a newFakeHome tree — the ~/.config/sops-nix/secrets/<name> shape
// home-manager sops-nix produces on Darwin — pointing into a fake secrets.d
// generation dir, and returns that stable path.
func fakeGrafanaSopsPath(t *testing.T, fakeHome, secretsBase, counter string) string {
	t.Helper()
	sopsDir := filepath.Join(fakeHome, ".config", "sops-nix", "secrets")
	if err := os.MkdirAll(sopsDir, 0o700); err != nil {
		t.Fatalf("create fake sops symlink dir: %v", err)
	}
	fakeSopsChain(t, fakeHome, secretsBase, counter, map[string]string{
		".config/sops-nix/secrets/" + grafanaSecretName: grafanaSecretName,
	})
	return filepath.Join(sopsDir, grafanaSecretName)
}

// TestGenerateProfile_SecretsDAllowlistAdmitsGrafanaConfig verifies that a
// configured, sops-backed GrafanaConfigPath produces exactly one extra
// require-not exception — for the grafana bundle and nothing else (#2746).
func TestGenerateProfile_SecretsDAllowlistAdmitsGrafanaConfig(t *testing.T) {
	fakeHome := newFakeHome(t)
	secretsBase := filepath.Join(t.TempDir(), "secrets.d")

	// One unrelated allowlisted source (the ssh access key) so the count
	// assertion below distinguishes "one grafana exception was added" from
	// "the allowlist collapsed to a single entry".
	fakeSopsChain(t, fakeHome, secretsBase, "42", map[string]string{
		".ssh/prismatic-koi-ed25519": "ssh/prismatic-koi-ed25519",
	})
	grafanaPath := fakeGrafanaSopsPath(t, fakeHome, secretsBase, "42")

	m := newSandboxExecManager(Config{
		SessionName:       "repo@main",
		GrafanaConfigPath: grafanaPath,
	})
	profile := generateProfile(m)

	if !strings.Contains(profile, grafanaExceptionRule) {
		t.Errorf("profile missing the grafana allowlist exception:\n%s\nfull profile:\n%s",
			grafanaExceptionRule, profile)
	}
	// The counter must not be baked in — the #1410/#1573 rotation property.
	if strings.Contains(profile, `/secrets\.d/42/`) {
		t.Errorf("profile bakes the concrete secrets.d counter into a regex; full profile:\n%s", profile)
	}
	// Exactly two exceptions: the ssh access key and grafana. The grafana
	// carve-out adds ONE name and no more (issue #2211 inventory rule).
	if got := strings.Count(profile, "(require-not "); got != 2 {
		t.Errorf("expected exactly 2 require-not exceptions (ssh access key + grafana), got %d; full profile:\n%s",
			got, profile)
	}
	// No other secret may ride along — in particular the daily-driver
	// github_token, the age keys, and the admin kube config stay denied.
	for _, denied := range []string{"github_token", "age-keys.txt", "workkube", "gitlab_token"} {
		if strings.Contains(profile, "/"+denied+"$") {
			t.Errorf("profile allowlists %q — the #2211 inventory rule permits only consumed secrets; full profile:\n%s",
				denied, profile)
		}
	}
}

// TestGenerateProfile_SecretsDAllowlistOmitsGrafanaConfigWhenUnconfigured is
// the paired negative: a host with no Grafana config path (the default —
// nx.programs.prism.pi.grafana.enable is false, or the session is a review
// role whose GRAFANA_MCP_CONFIG_PATH is stripped by #2533) emits no grafana
// exception, so the bundle stays denied exactly as it was before #2746.
func TestGenerateProfile_SecretsDAllowlistOmitsGrafanaConfigWhenUnconfigured(t *testing.T) {
	fakeHome := newFakeHome(t)
	secretsBase := filepath.Join(t.TempDir(), "secrets.d")

	// The bundle EXISTS on the host and is sops-backed; only the config key
	// is absent. Existence alone must never admit it.
	fakeGrafanaSopsPath(t, fakeHome, secretsBase, "42")

	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	if strings.Contains(profile, grafanaExceptionRule) {
		t.Errorf("profile allowlists the grafana bundle with no GrafanaConfigPath configured; full profile:\n%s", profile)
	}
	if strings.Contains(profile, grafanaSecretName) {
		t.Errorf("profile mentions %s with no GrafanaConfigPath configured; full profile:\n%s",
			grafanaSecretName, profile)
	}
}

// TestGenerateProfile_SecretsDAllowlistIgnoresNonSopsGrafanaPath verifies
// that a GrafanaConfigPath that does not resolve into a secrets.d tree (a
// plain file, e.g. a Linux /run/secrets path or a test fixture) produces no
// exception: the secrets.d deny never covers it, so no carve-out is needed
// and none is emitted.
func TestGenerateProfile_SecretsDAllowlistIgnoresNonSopsGrafanaPath(t *testing.T) {
	fakeHome := newFakeHome(t)
	plain := filepath.Join(fakeHome, "plain-grafana-config")
	if err := os.WriteFile(plain, []byte("GRAFANA_URL=https://example.invalid\n"), 0o600); err != nil {
		t.Fatalf("write plain grafana config file: %v", err)
	}

	m := newSandboxExecManager(Config{
		SessionName:       "repo@main",
		GrafanaConfigPath: plain,
	})
	profile := generateProfile(m)

	if got := strings.Count(profile, "(require-not "); got != 0 {
		t.Errorf("expected zero require-not exceptions for a non-sops grafana path, got %d; full profile:\n%s", got, profile)
	}
}

// TestBwrapBuildArgs_IgnoresGrafanaConfigPath pins the #2746 scope boundary:
// GrafanaConfigPath is a sandbox-exec-only field. The bwrap isolator keeps
// deriving its grafana --ro-bind from AgentEnvVars (issue #2452), so setting
// the new field with no matching AgentEnvVars entry must leave the bwrap
// argv exactly as it was before this change.
func TestBwrapBuildArgs_IgnoresGrafanaConfigPath(t *testing.T) {
	tmp := t.TempDir()
	concrete := filepath.Join(tmp, "concrete-grafana-config")
	if err := os.WriteFile(concrete, []byte("GRAFANA_URL=https://example.invalid\n"), 0o600); err != nil {
		t.Fatalf("WriteFile concrete: %v", err)
	}
	stable := filepath.Join(tmp, grafanaSecretName)
	if err := os.Symlink(concrete, stable); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		// The sandbox-exec field is set; AgentEnvVars deliberately is not.
		GrafanaConfigPath: stable,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	for _, arg := range redactedArgs(args) {
		if arg == stable || arg == concrete {
			t.Errorf("bwrap argv references the grafana path %q purely because GrafanaConfigPath is set — "+
				"the bwrap path must stay driven by AgentEnvVars (issue #2452); args=%v",
				arg, redactedArgs(args))
		}
	}
}
