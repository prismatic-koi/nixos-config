//go:build darwin

package integration_test

// sandbox_exec_credential_file_darwin_test.go — integration coverage for the
// agent credential carve-out in the secrets.d allowlist.
//
// A session of a repo in the repo list of an agent credential gets each file
// variable of the credential: the variable holds the stable sops path, and
// the agent reads the file in-sandbox. The carve-out adds one name for each
// delivered file, derived from Config.AgentCredentials. internal/agentcred
// leaves that field empty for a session of any other repo and for a review
// role.
//
// This file proves, with a real /usr/bin/sandbox-exec run, that:
//
//  1. TestSandboxExecCredentialFile_ReadableWhenDelivered — each delivered
//     file is readable in-sandbox through its stable symlink, while
//     github_token and an undelivered credential file in the same generation
//     dir stay denied.
//  2. TestSandboxExecCredentialFile_ExceptionIsLoadBearing — the paired
//     negative: strip the exception and the same read fails.
//  3. TestSandboxExecCredentialFile_DeniedWithNoDelivery — a Manager with no
//     delivery (another repo, or a review role) cannot read the same files.
//  4. TestSandboxExecCredentialFile_WriteDeleteReplaceDenied — the session
//     cannot write, delete, or replace a delivered file or its stable
//     symlink.
//  5. TestSandboxExecCredentialFile_WriteWorksWithWriteGrant — the paired
//     negative of (4): with a write grant appended, the same write works.
//
// The tests run against a FAKE secrets.d tree under the per-user TMPDIR. The
// production deny regexes are anchored at /var/folders, where the per-user
// TMPDIR is, so the real rules cover the fake tree with no profile mutation.
//
// Secret hygiene: the fake secrets are sentinels. No test in this file reads
// a real credential. A read of a denied file discards its content in-sandbox.
//
// See docs/sandbox-exec-testing.md for the convention.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/agentcred"
	"github.com/prismatic-koi/prism/internal/container"
)

const (
	// fakeCredentialSentinel is the content of each fake credential file,
	// with the secret name after it. It is not a credential and matches no
	// credential shape.
	fakeCredentialSentinel = "prism-agent-credential-sentinel-"

	credentialPasswordSecretName = "example_password"
	credentialTokenSecretName    = "second_token"
	// credentialUndeliveredName is a credential file of another credential,
	// which the session does not get.
	credentialUndeliveredName = "other_password"
)

// fakeCredentialTree is a fake sops layout under the per-user TMPDIR:
//
//	<base>/secrets.d/<counter>/<name>   (sentinel content) for each name
//	<base>/secrets.d/<counter>/github_token
//	<base>/stable/<name>                → the counter path, for each name
type fakeCredentialTree struct {
	base       string
	concrete   map[string]string
	stable     map[string]string
	deniedPath string
}

// setupFakeCredentialFiles builds the tree for the two delivered files and
// the undelivered one. Skips when the TMPDIR is not under /var/folders.
func setupFakeCredentialFiles(t *testing.T) fakeCredentialTree {
	t.Helper()
	tmp, err := os.MkdirTemp("", "prism-agent-credential-")
	if err != nil {
		t.Fatalf("MkdirTemp for fake credential files: %v", err)
	}
	t.Cleanup(func() {
		_ = filepath.Walk(tmp, func(p string, _ os.FileInfo, _ error) error {
			_ = os.Chmod(p, 0o700)
			return nil
		})
		_ = os.RemoveAll(tmp)
	})
	base, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", tmp, err)
	}
	if !strings.HasPrefix(base, "/private/var/folders/") && !strings.HasPrefix(base, "/var/folders/") {
		t.Skipf("TMPDIR-derived base %s is not under /var/folders — the production secrets.d deny regexes would not apply", base)
	}
	tree := fakeCredentialTree{base: base, concrete: map[string]string{}, stable: map[string]string{}}
	counterDir := filepath.Join(base, "secrets.d", "800")
	stableDir := filepath.Join(base, "stable")
	for _, d := range []string{counterDir, stableDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("create %s: %v", d, err)
		}
	}
	for _, name := range []string{credentialPasswordSecretName, credentialTokenSecretName, credentialUndeliveredName} {
		concrete := filepath.Join(counterDir, name)
		if err := os.WriteFile(concrete, []byte(fakeCredentialSentinel+name+"\n"), 0o400); err != nil {
			t.Fatalf("write fake credential file %s: %v", name, err)
		}
		stable := filepath.Join(stableDir, name)
		if err := os.Symlink(concrete, stable); err != nil {
			t.Fatalf("symlink stable %s: %v", name, err)
		}
		tree.concrete[name] = concrete
		tree.stable[name] = stable
	}
	tree.deniedPath = filepath.Join(counterDir, "github_token")
	if err := os.WriteFile(tree.deniedPath, []byte("ghp_fake-agent-credential-denied"), 0o600); err != nil {
		t.Fatalf("write fake github_token control: %v", err)
	}
	return tree
}

// newCredentialProfileManager is newGrafanaProfileManager with an agent
// credential delivery in place of the grafana path. With deliver false, the
// Manager gets no delivery.
func newCredentialProfileManager(t *testing.T, tree fakeCredentialTree, deliver bool) *container.Manager {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	wrap, err := os.MkdirTemp(home, ".prism-agent-credential-bareroot-wrap-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home) for BareRoot wrap: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wrap) })
	bareRoot, err := os.MkdirTemp(wrap, "bareroot-*")
	if err != nil {
		t.Fatalf("MkdirTemp(wrap) for BareRoot: %v", err)
	}
	var d agentcred.Delivery
	if deliver {
		d = agentcred.Delivery{Credentials: []agentcred.Credential{
			{
				Name:  "example",
				Files: []agentcred.File{{Env: "EXAMPLE_PASSWORD_FILE", Path: tree.stable[credentialPasswordSecretName], Marker: "EXAMPLE_PASSWORD"}},
				Vars:  map[string]string{"EXAMPLE_HOST": "host.example.invalid"},
			},
			{
				Name:  "second",
				Files: []agentcred.File{{Env: "SECOND_TOKEN_FILE", Path: tree.stable[credentialTokenSecretName], Marker: "SECOND_TOKEN"}},
			},
		}}
	}
	return container.New(container.Config{
		SessionName:      "integ-sandbox-exec-agent-credential-test",
		InstanceID:       "integ-sbx-" + strings.ReplaceAll(t.Name(), "/", "-"),
		Worktree:         t.TempDir(),
		BareRoot:         bareRoot,
		GitUserName:      "test-user",
		GitUserEmail:     "test@example.com",
		AgentCredentials: d,
	})
}

func credentialExceptionRegex(name string) string { return `/secrets\.d/[0-9]+/` + name + `$` }

var deliveredCredentialNames = []string{credentialPasswordSecretName, credentialTokenSecretName}

func TestSandboxExecCredentialFile_ReadableWhenDelivered(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	tree := setupFakeCredentialFiles(t)

	m := newCredentialProfileManager(t, tree, true)
	prepared, _ := preparePositiveProfile(t, m)
	for _, name := range deliveredCredentialNames {
		if !strings.Contains(prepared.content, credentialExceptionRegex(name)) {
			t.Fatalf("profile carries no %s exception — the generator did not derive it from Config.AgentCredentials.\nProfile:\n%s", name, prepared.content)
		}
	}
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	for _, name := range deliveredCredentialNames {
		out, runErr := exec.Command(sandboxExecPath, "-f", profilePath,
			nixBash, "-c", `cat "$0"`, tree.stable[name]).CombinedOutput()
		if runErr != nil {
			t.Errorf("in-sandbox read of %s failed under the production profile.\nExit: %v\nOutput: %s", name, runErr, out)
		} else if strings.TrimSpace(string(out)) != fakeCredentialSentinel+name {
			t.Errorf("read of %s exited 0 but the sentinel is missing.\nOutput: %s", name, out)
		}
	}

	for _, denied := range []string{tree.deniedPath, tree.stable[credentialUndeliveredName], tree.concrete[credentialUndeliveredName]} {
		runErr, errOut := sandboxCatDiscard(profilePath, nixBash, denied)
		if runErr == nil {
			t.Errorf("in-sandbox read of %s SUCCEEDED next to the credential carve-out — the exception widened the subtree", denied)
		} else if !strings.Contains(errOut, "Operation not permitted") {
			t.Errorf("read of %s failed but not with EPERM.\nExit: %v\nStderr: %s", denied, runErr, errOut)
		}
	}
}

func TestSandboxExecCredentialFile_ExceptionIsLoadBearing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	tree := setupFakeCredentialFiles(t)

	m := newCredentialProfileManager(t, tree, true)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		for _, name := range deliveredCredentialNames {
			p = strings.ReplaceAll(p, credentialExceptionRegex(name), `/secrets\.d/[0-9]+/prism-agent-credential-disabled$`)
		}
		return p
	})
	for _, name := range deliveredCredentialNames {
		if runErr, out := sandboxCatDiscard(mutatedPath, nixBash, tree.stable[name]); runErr == nil {
			t.Errorf("in-sandbox read of %s succeeded WITHOUT its exception — the positive test is a no-op.\nOutput: %s", name, out)
		}
	}
}

func TestSandboxExecCredentialFile_DeniedWithNoDelivery(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	tree := setupFakeCredentialFiles(t)

	m := newCredentialProfileManager(t, tree, false)
	prepared, _ := preparePositiveProfile(t, m)
	for _, name := range deliveredCredentialNames {
		if strings.Contains(prepared.content, name) {
			t.Fatalf("profile mentions %s with no delivery.\nProfile:\n%s", name, prepared.content)
		}
	}
	profilePath := writeAugmentedPositiveProfile(t, prepared)
	for _, name := range deliveredCredentialNames {
		for _, p := range []string{tree.stable[name], tree.concrete[name]} {
			runErr, out := sandboxCatDiscard(profilePath, nixBash, p)
			if runErr == nil {
				t.Errorf("in-sandbox read of %s succeeded with no delivery — the secrets.d deny regressed", p)
			} else if !strings.Contains(out, "Operation not permitted") {
				t.Errorf("read of %s failed but not with EPERM.\nExit: %v\nStderr: %s", p, runErr, out)
			}
		}
	}
}

// credentialWriteScripts are the writes that a session must not do to a
// delivered file. Each script fails as a whole when one step fails.
func credentialWriteScripts(base, concrete, stablePath string) map[string]string {
	forged := filepath.Join(base, "forged")
	return map[string]string{
		"write the file":             "echo forged > " + shQuote(concrete),
		"delete the file":            "rm -f " + shQuote(concrete),
		"rename the file away":       "mv " + shQuote(concrete) + " " + shQuote(forged),
		"replace the stable symlink": "ln -sfn " + shQuote(forged) + " " + shQuote(stablePath),
		"delete the stable symlink":  "rm -f " + shQuote(stablePath),
	}
}

func TestSandboxExecCredentialFile_WriteDeleteReplaceDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	tree := setupFakeCredentialFiles(t)
	for _, name := range deliveredCredentialNames {
		if err := os.Chmod(tree.concrete[name], 0o600); err != nil {
			t.Fatal(err)
		}
	}

	m := newCredentialProfileManager(t, tree, true)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	for _, name := range deliveredCredentialNames {
		for what, script := range credentialWriteScripts(tree.base, tree.concrete[name], tree.stable[name]) {
			out, runErr := exec.Command(sandboxExecPath, "-f", profilePath, nixBash, "-c", script).CombinedOutput()
			if runErr == nil {
				t.Errorf("%s of %s: succeeded in-sandbox under the production profile.\nOutput: %s", what, name, out)
			}
		}
		if data, err := os.ReadFile(tree.concrete[name]); err != nil || string(data) != fakeCredentialSentinel+name+"\n" {
			t.Errorf("the file %s on the host changed (err=%v)", name, err)
		}
		if target, err := os.Readlink(tree.stable[name]); err != nil || target != tree.concrete[name] {
			t.Errorf("the stable symlink of %s on the host changed: %q %v", name, target, err)
		}
	}
}

// The paired negative of the write test. An appended write grant on the
// fake tree wins over every earlier rule, so the same writes work. Thus a
// denied write above is the effect of the production profile, and the
// harness can see a write that succeeds.
func TestSandboxExecCredentialFile_WriteWorksWithWriteGrant(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	tree := setupFakeCredentialFiles(t)
	name := credentialPasswordSecretName
	if err := os.Chmod(tree.concrete[name], 0o600); err != nil {
		t.Fatal(err)
	}

	m := newCredentialProfileManager(t, tree, true)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return p + "\n(allow file-read* file-write* (subpath " + sbplQuoteForTest(tree.base) + "))\n"
	})
	script := credentialWriteScripts(tree.base, tree.concrete[name], tree.stable[name])["write the file"]
	if out, runErr := exec.Command(sandboxExecPath, "-f", mutatedPath, nixBash, "-c", script).CombinedOutput(); runErr != nil {
		t.Errorf("the write failed with a write grant, so the write test proves nothing.\nExit: %v\nOutput: %s", runErr, out)
	}
}
