package container

// agent_credentials_test.go — the agent credential delivery at the isolator
// boundary (internal/agentcred):
//
//   - bwrap binds each credential file read-only, at the path that its file
//     variable holds, and sets the variables of each credential and the
//     registry variable of the redactor;
//   - no argv element and no variable holds the content of a file;
//   - a session with no delivery gets no bind and no credential variable;
//   - a credential with a file that does not resolve is dropped, and the
//     other credentials stay;
//   - sandbox-exec gets the same variables, and its profile admits a read of
//     exactly one more secrets.d name for each delivered file.
//
// The live bwrap coverage is in agent_credentials_bwrap_test.go. The Darwin
// coverage of the profile is in
// internal/integration/sandbox_exec_credential_file_darwin_test.go.
//
// SECURITY: every name and value is synthetic.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/agentcred"
	"github.com/prismatic-koi/prism/internal/payload"
)

const (
	syntheticCredentialPassword = "synthetic-credential-password-container-0123456789"
	syntheticCredentialToken    = "synthetic-credential-token-container-9876543210"
	syntheticCredentialUser     = "synthetic-agent-user"
	syntheticCredentialHost     = "host.example.invalid"
	passwordSecretName          = "example_password"
	tokenSecretName             = "second_token"
)

// credentialFileChain writes content to a concrete file in a fake sops tree
// and returns the path of a symlink to it, as sops-nix makes
// /run/secrets/<name>. Each file of one test is in the same tree.
func credentialFileChain(t *testing.T, root, name, content string) (link, concrete string) {
	t.Helper()
	concrete = filepath.Join(root, "secrets.d", "7", name)
	if err := os.MkdirAll(filepath.Dir(concrete), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(concrete, []byte(content+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(root, "secrets", name)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(concrete, link); err != nil {
		t.Fatal(err)
	}
	return link, concrete
}

func credentialRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// twoCredentials returns a delivery of two credentials: a password login
// (one file, one value, one literal) and a token (one file).
func twoCredentials(passwordPath, tokenPath string) agentcred.Delivery {
	return agentcred.Delivery{Credentials: []agentcred.Credential{
		{
			Name:  "example",
			Files: []agentcred.File{{Env: "EXAMPLE_PASSWORD_FILE", Path: passwordPath, Marker: "EXAMPLE_PASSWORD"}},
			Vars:  map[string]string{"EXAMPLE_HOST": syntheticCredentialHost, "EXAMPLE_USERNAME": syntheticCredentialUser},
		},
		{
			Name:  "second",
			Files: []agentcred.File{{Env: "SECOND_TOKEN_FILE", Path: tokenPath, Marker: "SECOND_TOKEN"}},
		},
	}}
}

func credentialBwrapArgs(t *testing.T, d agentcred.Delivery) []string {
	t.Helper()
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:      "home-ops@main",
		Worktree:         t.TempDir(),
		AllocatedPort:    14010,
		AgentEnvVars:     map[string]string{"GIT_EDITOR": "true"},
		AgentCredentials: d,
	})
	t.Cleanup(cleanup)
	return (&bwrapIsolator{name: m.name}).BuildArgs(m)
}

func assertNoCredentialValueInArgs(t *testing.T, args []string) {
	t.Helper()
	for i, a := range args {
		if strings.Contains(a, syntheticCredentialPassword) || strings.Contains(a, syntheticCredentialToken) {
			// Print the index only: the element holds the value.
			t.Errorf("argv element %d holds the value of a credential file", i)
		}
	}
}

// credentialArgNames are the names that a session with no delivery must not
// have in its argv.
var credentialArgNames = []string{"EXAMPLE_PASSWORD_FILE", "EXAMPLE_HOST", "EXAMPLE_USERNAME", "SECOND_TOKEN_FILE", payload.CredentialFilesEnvName}

func TestBwrapBuildArgs_AgentCredentialDelivery(t *testing.T) {
	root := credentialRoot(t)
	passwordLink, passwordConcrete := credentialFileChain(t, root, passwordSecretName, syntheticCredentialPassword)
	tokenLink, tokenConcrete := credentialFileChain(t, root, tokenSecretName, syntheticCredentialToken)
	args := credentialBwrapArgs(t, twoCredentials(passwordLink, tokenLink))

	for _, b := range [][2]string{{passwordConcrete, passwordLink}, {tokenConcrete, tokenLink}} {
		if !hasROBindSrcDst(args, b[0], b[1]) {
			t.Errorf("want --ro-bind %q %q (Src=resolved, Dst=the file variable) in args: %v", b[0], b[1], redactedArgs(args))
		}
		if hasROBindSrcDst(args, b[0], b[0]) {
			t.Errorf("unexpected --ro-bind %q %q: the session reads the path that the file variable holds", b[0], b[0])
		}
	}
	for k, v := range map[string]string{
		"EXAMPLE_PASSWORD_FILE":        passwordLink,
		"EXAMPLE_USERNAME":             syntheticCredentialUser,
		"EXAMPLE_HOST":                 syntheticCredentialHost,
		"SECOND_TOKEN_FILE":            tokenLink,
		payload.CredentialFilesEnvName: "EXAMPLE_PASSWORD_FILE=EXAMPLE_PASSWORD,SECOND_TOKEN_FILE=SECOND_TOKEN",
	} {
		if !hasSetenv(args, k, v) {
			t.Errorf("want --setenv %s %s in args: %v", k, v, redactedArgs(args))
		}
	}
	assertNoCredentialValueInArgs(t, args)
}

func TestBwrapBuildArgs_NoAgentCredentialDeliveryGivesNothing(t *testing.T) {
	root := credentialRoot(t)
	link, concrete := credentialFileChain(t, root, passwordSecretName, syntheticCredentialPassword)
	args := credentialBwrapArgs(t, agentcred.Delivery{})
	for i, a := range args {
		if slices.Contains(credentialArgNames, a) || a == link || a == concrete {
			t.Errorf("argv element %d = %q, want no credential variable and no bind", i, a)
		}
	}
	assertNoCredentialValueInArgs(t, args)
}

// A credential whose file does not resolve gives no bind and no variable,
// so no variable names a missing file. The other credential stays, and the
// registry lists its file only.
func TestBwrapBuildArgs_UnresolvableCredentialFileDropsThatCredential(t *testing.T) {
	root := credentialRoot(t)
	missing := filepath.Join(root, "secrets", passwordSecretName)
	tokenLink, tokenConcrete := credentialFileChain(t, root, tokenSecretName, syntheticCredentialToken)
	args := credentialBwrapArgs(t, twoCredentials(missing, tokenLink))
	for i, a := range args {
		if strings.HasPrefix(a, "EXAMPLE_") || a == missing {
			t.Errorf("argv element %d = %q, want no variable and no bind of the unresolvable credential", i, a)
		}
	}
	if !hasROBindSrcDst(args, tokenConcrete, tokenLink) || !hasSetenv(args, "SECOND_TOKEN_FILE", tokenLink) {
		t.Errorf("the other credential is missing from args: %v", redactedArgs(args))
	}
	if !hasSetenv(args, payload.CredentialFilesEnvName, "SECOND_TOKEN_FILE=SECOND_TOKEN") {
		t.Errorf("want the registry of the other credential only: %v", redactedArgs(args))
	}
}

func TestBwrapCredentialFileBind(t *testing.T) {
	link, concrete := credentialFileChain(t, credentialRoot(t), passwordSecretName, syntheticCredentialPassword)
	for _, tc := range []struct {
		name, path string
		want       []string
	}{
		{"empty", "", nil},
		{"missing", filepath.Join(filepath.Dir(link), "missing"), nil},
		{"symlink", link, []string{"--ro-bind", concrete, link}},
		{"plain file", concrete, []string{"--ro-bind", concrete, concrete}},
	} {
		got := bwrapCredentialFileBind(tc.path)
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("%s: bwrapCredentialFileBind(%q) = %q, want %q", tc.name, tc.path, got, tc.want)
		}
	}
}

// Two credentials that use one file get one bind.
func TestBwrapCredentialBinds_OneBindForASharedFile(t *testing.T) {
	link, concrete := credentialFileChain(t, credentialRoot(t), passwordSecretName, syntheticCredentialPassword)
	d := twoCredentials(link, link)
	kept, binds := bwrapCredentialBinds(d)
	if len(kept.Credentials) != 2 {
		t.Errorf("kept %d credentials, want 2", len(kept.Credentials))
	}
	if want := []string{"--ro-bind", concrete, link}; !slices.Equal(binds, want) {
		t.Errorf("binds = %q, want %q", binds, want)
	}
}

func TestAppendSandboxEnvVarsKV_AgentCredentialDelivery(t *testing.T) {
	root := credentialRoot(t)
	passwordLink, _ := credentialFileChain(t, root, passwordSecretName, syntheticCredentialPassword)
	tokenLink, _ := credentialFileChain(t, root, tokenSecretName, syntheticCredentialToken)
	env := AppendSandboxEnvVarsKV(nil, Config{
		AgentEnvVars:     map[string]string{"EXAMPLE_HOST": "profile-value"},
		AgentCredentials: twoCredentials(passwordLink, tokenLink),
	})
	last := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		last[k] = v
		if strings.Contains(v, syntheticCredentialPassword) || strings.Contains(v, syntheticCredentialToken) {
			t.Errorf("variable %s holds the value of a credential file", k)
		}
	}
	for k, want := range map[string]string{
		"EXAMPLE_USERNAME":      syntheticCredentialUser,
		"EXAMPLE_PASSWORD_FILE": passwordLink,
		"SECOND_TOKEN_FILE":     tokenLink,
		// The last occurrence wins, so the delivery wins over a profile var.
		"EXAMPLE_HOST":                 syntheticCredentialHost,
		payload.CredentialFilesEnvName: "EXAMPLE_PASSWORD_FILE=EXAMPLE_PASSWORD,SECOND_TOKEN_FILE=SECOND_TOKEN",
	} {
		if last[k] != want {
			t.Errorf("%s = %q, want %q", k, last[k], want)
		}
	}

	for _, kv := range AppendSandboxEnvVarsKV(nil, Config{AgentEnvVars: map[string]string{"A": "b"}}) {
		if k, _, _ := strings.Cut(kv, "="); slices.Contains(credentialArgNames, k) {
			t.Errorf("a session with no delivery got %q", kv)
		}
	}
}

// credentialExceptionRule is the require-not exception that the profile must
// carry for one credential file.
func credentialExceptionRule(name string) string {
	return `    (require-not (regex #"/secrets\.d/[0-9]+/` + name + `$"))` + "\n"
}

// fakeSopsNixDir makes ~/.config/sops-nix/secrets in a newFakeHome tree, the
// dir of the home-manager sops-nix symlinks on Darwin.
func fakeSopsNixDir(t *testing.T, fakeHome string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(fakeHome, ".config", "sops-nix", "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateProfile_SecretsDAllowlistAdmitsEachCredentialFile(t *testing.T) {
	fakeHome := newFakeHome(t)
	fakeSopsNixDir(t, fakeHome)
	secretsBase := filepath.Join(t.TempDir(), "secrets.d")
	fakeSopsChain(t, fakeHome, secretsBase, "42", map[string]string{
		".ssh/prismatic-koi-ed25519":                     "ssh/prismatic-koi-ed25519",
		".config/sops-nix/secrets/" + passwordSecretName: passwordSecretName,
		".config/sops-nix/secrets/" + tokenSecretName:    tokenSecretName,
		".config/sops-nix/secrets/github_token":          "github_token",
		".config/sops-nix/secrets/hass_api_key":          "hass_api_key",
		".config/sops-nix/secrets/example_host":          "example_host",
	})
	secret := func(name string) string { return filepath.Join(fakeHome, ".config", "sops-nix", "secrets", name) }

	profile := generateProfile(newSandboxExecManager(Config{
		SessionName:      "home-ops@main",
		AgentCredentials: twoCredentials(secret(passwordSecretName), secret(tokenSecretName)),
	}))
	for _, name := range []string{passwordSecretName, tokenSecretName} {
		if !strings.Contains(profile, credentialExceptionRule(name)) {
			t.Errorf("profile missing the exception of %s:\n%s\nfull profile:\n%s", name, credentialExceptionRule(name), profile)
		}
	}
	if strings.Contains(profile, `/secrets\.d/42/`) {
		t.Errorf("profile bakes the secrets.d counter into a regex; full profile:\n%s", profile)
	}
	if got := secretsDExceptionCount(t, profile); got != 3 {
		t.Errorf("want 3 require-not exceptions (ssh access key + two credential files), got %d; full profile:\n%s", got, profile)
	}
	// example_host is the file of a value variable: prism reads it on the
	// host, so the sandbox gets no read of it.
	for _, denied := range []string{"github_token", "hass_api_key", "example_host"} {
		if strings.Contains(profile, "/"+denied+"$") {
			t.Errorf("profile allowlists %q; full profile:\n%s", denied, profile)
		}
	}
	// The write deny on secrets.d has no exception, so the session cannot
	// change, remove, or replace a credential file.
	if !strings.Contains(profile, secretsDDenyWriteRule) {
		t.Errorf("profile missing the secrets.d write deny; full profile:\n%s", profile)
	}
	for _, v := range []string{syntheticCredentialPassword, syntheticCredentialToken, syntheticCredentialHost} {
		if strings.Contains(profile, v) {
			t.Error("the profile holds a credential value or the host value")
		}
	}
}

// A session with no delivery (another repo, or a review role) gets no
// exception, also when the credential files exist on the host.
func TestGenerateProfile_SecretsDAllowlistOmitsCredentialFilesWithNoDelivery(t *testing.T) {
	fakeHome := newFakeHome(t)
	fakeSopsNixDir(t, fakeHome)
	secretsBase := filepath.Join(t.TempDir(), "secrets.d")
	fakeSopsChain(t, fakeHome, secretsBase, "42", map[string]string{
		".config/sops-nix/secrets/" + passwordSecretName: passwordSecretName,
		".config/sops-nix/secrets/" + tokenSecretName:    tokenSecretName,
	})
	profile := generateProfile(newSandboxExecManager(Config{SessionName: "other@main"}))
	for _, name := range []string{passwordSecretName, tokenSecretName} {
		if strings.Contains(profile, name) {
			t.Errorf("profile mentions %s with no delivery; full profile:\n%s", name, profile)
		}
	}
}
