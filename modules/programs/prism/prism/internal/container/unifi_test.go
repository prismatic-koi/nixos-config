package container

// unifi_test.go — the UniFi credential delivery at the isolator boundary
// (issues #3472 and #3476):
//
//   - bwrap binds the password file read-only, at the path that
//     UNIFI_PASSWORD_FILE holds, and sets UNIFI_USERNAME and UNIFI_HOST;
//   - no argv element and no variable holds the password value;
//   - a session with no delivery gets no bind and no UniFi variable;
//   - sandbox-exec gets the same three variables, and its profile admits a
//     read of exactly one more secrets.d name.
//
// The live bwrap coverage is in unifi_bwrap_test.go. The Darwin coverage of
// the profile is in
// internal/integration/sandbox_exec_unifi_password_darwin_test.go.
//
// SECURITY: the password value is synthetic.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/unifi"
)

const (
	syntheticUniFiPassword = "synthetic-unifi-password-3476-container-0123456789"
	syntheticUniFiUser     = "synthetic-agent-user"
	syntheticUniFiHost     = "10.0.0.1"
	uniFiSecretName        = "unifi_password"
)

// uniFiPasswordChain writes the password to a concrete file and returns the
// path of a symlink to it, as sops-nix makes /run/secrets/unifi_password.
func uniFiPasswordChain(t *testing.T) (link, concrete string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	concrete = filepath.Join(dir, "secrets.d", "7", uniFiSecretName)
	if err := os.MkdirAll(filepath.Dir(concrete), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(concrete, []byte(syntheticUniFiPassword+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(dir, "secrets", uniFiSecretName)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(concrete, link); err != nil {
		t.Fatal(err)
	}
	return link, concrete
}

func uniFiBwrapArgs(t *testing.T, d unifi.Delivery) []string {
	t.Helper()
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "home-ops@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
		AgentEnvVars:  map[string]string{"GIT_EDITOR": "true"},
		UniFi:         d,
	})
	t.Cleanup(cleanup)
	return (&bwrapIsolator{name: m.name}).BuildArgs(m)
}

func assertNoUniFiPasswordInArgs(t *testing.T, args []string) {
	t.Helper()
	for i, a := range args {
		if strings.Contains(a, syntheticUniFiPassword) {
			// Print the index only: the element holds the password.
			t.Errorf("argv element %d holds the UniFi password value", i)
		}
	}
}

func TestBwrapBuildArgs_UniFiDelivery(t *testing.T) {
	link, concrete := uniFiPasswordChain(t)
	args := uniFiBwrapArgs(t, unifi.Delivery{PasswordPath: link, Username: syntheticUniFiUser, Host: syntheticUniFiHost})

	if !hasROBindSrcDst(args, concrete, link) {
		t.Errorf("want --ro-bind %q %q (Src=resolved, Dst=UNIFI_PASSWORD_FILE) in args: %v", concrete, link, redactedArgs(args))
	}
	if hasROBindSrcDst(args, concrete, concrete) {
		t.Errorf("unexpected --ro-bind %q %q: the session reads the path that UNIFI_PASSWORD_FILE holds", concrete, concrete)
	}
	if !hasSetenv(args, "UNIFI_PASSWORD_FILE", link) {
		t.Errorf("want --setenv UNIFI_PASSWORD_FILE %s in args: %v", link, redactedArgs(args))
	}
	if !hasSetenv(args, "UNIFI_USERNAME", syntheticUniFiUser) {
		t.Errorf("want --setenv UNIFI_USERNAME %s in args: %v", syntheticUniFiUser, redactedArgs(args))
	}
	if !hasSetenv(args, "UNIFI_HOST", syntheticUniFiHost) {
		t.Errorf("want --setenv UNIFI_HOST %s in args: %v", syntheticUniFiHost, redactedArgs(args))
	}
	assertNoUniFiPasswordInArgs(t, args)
}

func TestBwrapBuildArgs_NoUniFiDeliveryGivesNothing(t *testing.T) {
	link, concrete := uniFiPasswordChain(t)
	args := uniFiBwrapArgs(t, unifi.Delivery{})
	for i, a := range args {
		if strings.HasPrefix(a, "UNIFI_") || a == link || a == concrete {
			t.Errorf("argv element %d = %q, want no UniFi variable and no bind", i, a)
		}
	}
	assertNoUniFiPasswordInArgs(t, args)
}

// A password path that does not resolve gives no bind, and then no variable
// that names the missing file.
func TestBwrapBuildArgs_UnresolvableUniFiPasswordGivesNothing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), uniFiSecretName)
	args := uniFiBwrapArgs(t, unifi.Delivery{PasswordPath: missing, Username: syntheticUniFiUser, Host: syntheticUniFiHost})
	for i, a := range args {
		if strings.HasPrefix(a, "UNIFI_") || a == missing {
			t.Errorf("argv element %d = %q, want no UniFi variable and no bind", i, a)
		}
	}
}

func TestBwrapUniFiPasswordBind(t *testing.T) {
	link, concrete := uniFiPasswordChain(t)
	for _, tc := range []struct {
		name, path string
		want       []string
	}{
		{"empty", "", nil},
		{"missing", filepath.Join(filepath.Dir(link), "missing"), nil},
		{"symlink", link, []string{"--ro-bind", concrete, link}},
		{"plain file", concrete, []string{"--ro-bind", concrete, concrete}},
	} {
		got := bwrapUniFiPasswordBind(tc.path)
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("%s: bwrapUniFiPasswordBind(%q) = %q, want %q", tc.name, tc.path, got, tc.want)
		}
	}
}

func TestAppendSandboxEnvVarsKV_UniFiDelivery(t *testing.T) {
	link, _ := uniFiPasswordChain(t)
	env := AppendSandboxEnvVarsKV(nil, Config{
		AgentEnvVars: map[string]string{"UNIFI_HOST": "profile-value"},
		UniFi:        unifi.Delivery{PasswordPath: link, Username: syntheticUniFiUser, Host: syntheticUniFiHost},
	})
	last := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		last[k] = v
		if strings.Contains(v, syntheticUniFiPassword) {
			t.Errorf("variable %s holds the UniFi password value", k)
		}
	}
	if last["UNIFI_USERNAME"] != syntheticUniFiUser {
		t.Errorf("UNIFI_USERNAME = %q, want %q", last["UNIFI_USERNAME"], syntheticUniFiUser)
	}
	if last["UNIFI_PASSWORD_FILE"] != link {
		t.Errorf("UNIFI_PASSWORD_FILE = %q, want %q", last["UNIFI_PASSWORD_FILE"], link)
	}
	// The last occurrence wins, so the delivery wins over a profile var.
	if last["UNIFI_HOST"] != syntheticUniFiHost {
		t.Errorf("UNIFI_HOST = %q, want %q", last["UNIFI_HOST"], syntheticUniFiHost)
	}

	for _, kv := range AppendSandboxEnvVarsKV(nil, Config{AgentEnvVars: map[string]string{"A": "b"}}) {
		if strings.HasPrefix(kv, "UNIFI_") {
			t.Errorf("a session with no delivery got %q", kv)
		}
	}
}

// uniFiExceptionRule is the require-not exception that the profile must carry
// for the password file.
const uniFiExceptionRule = `    (require-not (regex #"/secrets\.d/[0-9]+/` + uniFiSecretName + `$"))` + "\n"

// fakeSopsNixDir makes ~/.config/sops-nix/secrets in a newFakeHome tree, the
// dir of the home-manager sops-nix symlinks on Darwin.
func fakeSopsNixDir(t *testing.T, fakeHome string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(fakeHome, ".config", "sops-nix", "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateProfile_SecretsDAllowlistAdmitsUniFiPassword(t *testing.T) {
	fakeHome := newFakeHome(t)
	fakeSopsNixDir(t, fakeHome)
	secretsBase := filepath.Join(t.TempDir(), "secrets.d")
	fakeSopsChain(t, fakeHome, secretsBase, "42", map[string]string{
		".ssh/prismatic-koi-ed25519":              "ssh/prismatic-koi-ed25519",
		".config/sops-nix/secrets/unifi_password": uniFiSecretName,
		".config/sops-nix/secrets/github_token":   "github_token",
		".config/sops-nix/secrets/hass_api_key":   "hass_api_key",
		".config/sops-nix/secrets/unifi_host":     "unifi_host",
	})
	passwordPath := filepath.Join(fakeHome, ".config", "sops-nix", "secrets", uniFiSecretName)

	profile := generateProfile(newSandboxExecManager(Config{
		SessionName: "home-ops@main",
		UniFi:       unifi.Delivery{PasswordPath: passwordPath, Username: syntheticUniFiUser, Host: syntheticUniFiHost},
	}))
	if !strings.Contains(profile, uniFiExceptionRule) {
		t.Errorf("profile missing the UniFi password exception:\n%s\nfull profile:\n%s", uniFiExceptionRule, profile)
	}
	if strings.Contains(profile, `/secrets\.d/42/`) {
		t.Errorf("profile bakes the secrets.d counter into a regex; full profile:\n%s", profile)
	}
	if got := secretsDExceptionCount(t, profile); got != 2 {
		t.Errorf("want 2 require-not exceptions (ssh access key + unifi_password), got %d; full profile:\n%s", got, profile)
	}
	for _, denied := range []string{"github_token", "hass_api_key", "unifi_host"} {
		if strings.Contains(profile, "/"+denied+"$") {
			t.Errorf("profile allowlists %q; full profile:\n%s", denied, profile)
		}
	}
	// The write deny on secrets.d has no exception, so the session cannot
	// change, remove, or replace the password file.
	if !strings.Contains(profile, secretsDDenyWriteRule) {
		t.Errorf("profile missing the secrets.d write deny; full profile:\n%s", profile)
	}
	if strings.Contains(profile, syntheticUniFiPassword) || strings.Contains(profile, syntheticUniFiHost) {
		t.Error("the profile holds the password or the host value")
	}
}

// A session with no delivery (another repo, or a review role) gets no
// exception, also when the password file exists on the host.
func TestGenerateProfile_SecretsDAllowlistOmitsUniFiPasswordWithNoDelivery(t *testing.T) {
	fakeHome := newFakeHome(t)
	fakeSopsNixDir(t, fakeHome)
	secretsBase := filepath.Join(t.TempDir(), "secrets.d")
	fakeSopsChain(t, fakeHome, secretsBase, "42", map[string]string{
		".config/sops-nix/secrets/unifi_password": uniFiSecretName,
	})
	profile := generateProfile(newSandboxExecManager(Config{SessionName: "other@main"}))
	if strings.Contains(profile, uniFiSecretName) {
		t.Errorf("profile mentions %s with no delivery; full profile:\n%s", uniFiSecretName, profile)
	}
}
