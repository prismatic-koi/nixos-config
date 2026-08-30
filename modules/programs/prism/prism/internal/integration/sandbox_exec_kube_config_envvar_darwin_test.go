//go:build darwin

package integration_test

// sandbox_exec_kube_config_envvar_darwin_test.go — integration coverage for
// the env-var delivery of the kube config.
//
// The staging-HOME .kube/config symlink is gone. kubectl resolves the config
// via KUBECONFIG pointing at the host XDG path
// (~/.config/kube/agents-config) and writes its discovery/http cache to
// <sessionDir>/kube-cache via KUBECACHEDIR (kubectl defaults to
// $HOME/.kube/cache, which exists on the host and EPERMs under
// deny-default). No literal SBPL grant exists on the XDG symlink path (it is
// inert — SBPL evaluates resolved targets): the in-sandbox read of the
// resolved sops target rides the broad (subpath "/private/var/folders")
// allow narrowed by the secrets.d allowlist, whose kube agents-config
// exception is derived from the same stable XDG source path.
//
// This file tests:
//
//  1. Positive: a real config-resolving kubectl invocation —
//     `kubectl config view` with KUBECONFIG at the host XDG path — exits 0
//     inside sandbox-exec AND its output carries the current-context name
//     parsed from the host config. kubectl prints an EMPTY config (exit 0)
//     when the env var is ignored or the file is absent, and fails non-zero
//     when the file is unreadable, so the combined exit-0 + named-context
//     assertion proves a real config read in-sandbox. (The config's
//     exec-plugin auth — aws eks get-token — is NOT exercised: config view
//     performs no auth, and a live EKS probe would be non-hermetic. The
//     exec-plugin env propagation (AWS_CONFIG_FILE et al., Step 3a pair) is
//     covered at unit level by env_test.go and the dispatcher tests.)
//
//  2. Negative: stripping the kube agents-config require-not exception from
//     the secrets.d deny makes the same invocation fail — proving the
//     allowlist exception is the load-bearing grant for the env-var route
//     (sandbox-exec testing convention).
//
//  4. Cache: a discovery round-trip against a test-local fake API server
//     (`kubectl api-resources` with KUBECACHEDIR=<sessionDir>/kube-cache)
//     lands cache files under <sessionDir>/kube-cache and writes NOTHING
//     under the host ~/.kube/cache; the paired negative strips the
//     (subpath <sessionDir>) grant and asserts no cache files land.
//
// The kube-specific sops-rotation coverage lives in
// sandbox_exec_sops_rotation_darwin_test.go (the fake-secrets-tree entries).
// Capability-probe gating applies via requireSandboxExec. Shared helpers
// live in sandbox_exec_helpers_darwin_test.go. The allowlist parse helpers
// live in sandbox_exec_secrets_deny_darwin_test.go and
// sandbox_exec_aws_config_envvar_darwin_test.go.

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// requireNixKubectl resolves the kubectl binary via PATH → symlink chain and
// returns the PATH-resolved path. Skips the test when kubectl is not found
// or does not resolve into /nix/store (an Apple-signed or homebrew binary
// SIGABRTs or skews the signal under the deny-default profile — same
// rationale as requireNixBash).
func requireNixKubectl(t *testing.T) string {
	t.Helper()

	kubectlPath, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skipf("kubectl not found in PATH: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(kubectlPath)
	if err != nil {
		t.Skipf("EvalSymlinks(%q): %v", kubectlPath, err)
	}
	if !strings.HasPrefix(resolved, "/nix/store/") {
		t.Skipf("kubectl resolves to %q which is not a /nix/store/ path — cannot use as test binary under the deny-default sandbox", resolved)
	}
	return kubectlPath
}

// kubeCurrentContextRe matches the current-context line of a kubeconfig
// (and of `kubectl config view` output). The value may be YAML-quoted.
var kubeCurrentContextRe = regexp.MustCompile(`(?m)^current-context:\s*(\S+)`)

// kubeHostConfigForTest locates the host XDG kube config
// (~/.config/kube/agents-config), requires it to be sops-backed (resolving
// into a secrets.d/<N>/ path — the mechanism under test), and parses the
// current-context name from it. Skips when any precondition is missing.
//
// Returns (configPath, currentContext). configPath is the stable XDG
// symlink path (what KUBECONFIG carries in production).
func kubeHostConfigForTest(t *testing.T) (configPath, currentContext string) {
	t.Helper()

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	configPath = filepath.Join(home, ".config", "kube", "agents-config")

	resolved, err := filepath.EvalSymlinks(configPath)
	if err != nil {
		t.Skipf("host kube config %s absent or unresolvable: %v", configPath, err)
	}
	if !strings.Contains(resolved, "/secrets.d/") {
		t.Skipf("host kube config resolves to %q which is not sops-backed — the #2211 allowlist mechanism under test does not apply", resolved)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Skipf("cannot read host kube config from the test process: %v", err)
	}
	match := kubeCurrentContextRe.FindSubmatch(content)
	if match == nil {
		t.Skipf("host kube config has no current-context line — cannot distinguish a real config read from kubectl's empty-config default output")
	}
	currentContext = strings.Trim(string(match[1]), `"'`)
	if currentContext == "" {
		t.Skipf("host kube config current-context is empty — cannot distinguish a real config read from kubectl's empty-config default output")
	}
	return configPath, currentContext
}

// kubectlConfigViewCmd builds the in-sandbox `kubectl config view`
// invocation with the production env-var shape: HOME at the real host home,
// KUBECONFIG at the host XDG path, KUBECACHEDIR at the
// session work dir, and the Step 3a AWS pair at the host XDG paths
// (production parity — config view does not invoke the exec plugin, so the
// pair is inert here).
func kubectlConfigViewCmd(t *testing.T, profilePath, sessionDir, kubectlBin, nixBash, configPath string) *exec.Cmd {
	t.Helper()
	home, _ := os.UserHomeDir()
	script := shQuote(kubectlBin) + " config view"
	return exec.Command(sandboxExecPath, "-f", profilePath,
		"/usr/bin/env",
		"HOME="+realUserHome(t),
		"KUBECONFIG="+configPath,
		"KUBECACHEDIR="+container.SessionWorkDirKubeCacheDirPath(sessionDir),
		"AWS_CONFIG_FILE="+filepath.Join(home, ".config", "aws", "readonly-config"),
		"AWS_SHARED_CREDENTIALS_FILE="+filepath.Join(home, ".config", "aws", "credentials"),
		nixBash, "-c", script)
}

// TestSandboxExecKubeConfig_EnvVarResolution is the positive integration
// test for the env-var delivery route. It runs
// a real config-resolving kubectl invocation inside sandbox-exec under the
// production profile, with KUBECONFIG pointing at the host XDG path, and
// asserts the current-context from the host config appears in the output
// (exit 0 alone is not enough — kubectl prints an empty config with exit 0
// when the file is simply absent).
func TestSandboxExecKubeConfig_EnvVarResolution(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	kubectlBin := requireNixKubectl(t)

	configPath, currentContext := kubeHostConfigForTest(t)

	// BareRoot variant: traversing the XDG symlink under the real HOME needs
	// the ancestor block's file-read-metadata allow (same as the
	// stable-chain tests and the aws env-var tests).
	m := newProfileManagerWithBareRoot(t)

	prepared, _ := preparePositiveProfile(t, m)

	// The profile must carry the kube agents-config allowlist exception —
	// the grant the env-var route rides on.
	resolvedName := secretsDNameForTest(t, configPath)
	found := false
	for _, name := range parseSecretsDAllowlist(prepared.content) {
		if name == resolvedName {
			found = true
		}
	}
	if !found {
		t.Fatalf("profile allowlist does not carry the kube config exception %q — collectSecretsDAllowlistNames regressed (issue #2235).\nProfile:\n%s",
			resolvedName, prepared.content)
	}

	sessionDir, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	cmd := kubectlConfigViewCmd(t, testProfilePath, sessionDir, kubectlBin, nixBash, configPath)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("kubectl config view failed in-sandbox under the production profile.\n"+
			"kubectl could not resolve the config via KUBECONFIG=%s (issue #2235 AC).\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			configPath, runErr, string(out), testProfilePath)
	}
	if !strings.Contains(string(out), currentContext) {
		t.Fatalf("kubectl config view exited 0 but the output does not contain the host config's current-context %q — "+
			"the config was not actually read (kubectl prints an empty config when KUBECONFIG is absent/ignored).\nOutput: %s",
			currentContext, string(out))
	}
	t.Logf("ka pai — kubectl resolved current-context %q from %s in-sandbox via KUBECONFIG", currentContext, configPath)
}

// TestSandboxExecKubeConfig_EnvVarResolutionDeniedWithoutAllowlistException
// is the paired negative test (sandbox-exec testing convention). It strips
// the kube agents-config require-not exception from the secrets.d deny and
// asserts the same kubectl invocation fails — proving
// the positive is not green by accident: the allowlist exception is the
// load-bearing grant for the env-var config read.
func TestSandboxExecKubeConfig_EnvVarResolutionDeniedWithoutAllowlistException(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	kubectlBin := requireNixKubectl(t)

	configPath, _ := kubeHostConfigForTest(t)

	m := newProfileManagerWithBareRoot(t)

	resolvedName := secretsDNameForTest(t, configPath)

	// Remove the require-not exception line for the kube config name,
	// mirroring the generator's emission format exactly (generateProfile +
	// regexQuotePath).
	exceptionLine := `    (require-not (regex #"/secrets\.d/[0-9]+/` + regexQuoteForTest(resolvedName) + `$"))` + "\n"
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p, exceptionLine, "")
	})

	sessionDir, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}

	cmd := kubectlConfigViewCmd(t, mutatedPath, sessionDir, kubectlBin, nixBash, configPath)
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("kubectl config view succeeded WITHOUT the %q allowlist exception.\n"+
			"The exception is not the load-bearing grant — investigate.\n"+
			"Output: %s\nMutated profile: %s", resolvedName, string(out), mutatedPath)
	} else {
		t.Logf("ka pai — kube config read correctly denied without the allowlist exception (exit: %v)", runErr)
	}
}

// fakeKubeAPIServer starts a minimal unauthenticated fake Kubernetes API
// server on the host loopback (the test process is unsandboxed; the
// sandboxed kubectl connects out — (allow network*) covers it). It serves
// just enough legacy discovery for `kubectl api-resources` to complete a
// round-trip and write its discovery cache. Returns the server URL.
func fakeKubeAPIServer(t *testing.T) *url.URL {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"kind":"APIVersions","versions":["v1"],"serverAddressByClientCIDRs":[{"clientCIDR":"0.0.0.0/0","serverAddress":"`+r.Host+`"}]}`)
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"kind":"APIResourceList","groupVersion":"v1","resources":[{"name":"pods","singularName":"","namespaced":true,"kind":"Pod","verbs":["get","list"]}]}`)
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse fake API server URL %q: %v", srv.URL, err)
	}
	return u
}

// writeFakeKubeconfig writes a minimal kubeconfig pointing at the fake API
// server into dir and returns its path. The file lives under a t.TempDir()
// (i.e. /var/folders on Darwin), so the broad system-paths allow covers the
// in-sandbox read regardless of the mutation under test — the kubeconfig
// read is deliberately NOT the grant being exercised by the cache tests.
func writeFakeKubeconfig(t *testing.T, dir string, server *url.URL) string {
	t.Helper()
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: ` + server.String() + `
  name: prism-2235-fake
contexts:
- context:
    cluster: prism-2235-fake
    user: prism-2235-anon
  name: prism-2235-fake
current-context: prism-2235-fake
users:
- name: prism-2235-anon
  user: {}
`
	path := filepath.Join(dir, "prism-2235-kubeconfig")
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatalf("write fake kubeconfig: %v", err)
	}
	return path
}

// kubectlAPIResourcesCmd builds the in-sandbox `kubectl api-resources`
// invocation against the fake API server with the production KUBECACHEDIR
// shape.
func kubectlAPIResourcesCmd(t *testing.T, profilePath, sessionDir, kubectlBin, nixBash, kubeconfigPath string) *exec.Cmd {
	t.Helper()
	script := shQuote(kubectlBin) + " api-resources --request-timeout=10s"
	return exec.Command(sandboxExecPath, "-f", profilePath,
		"/usr/bin/env",
		"HOME="+realUserHome(t),
		"KUBECONFIG="+kubeconfigPath,
		"KUBECACHEDIR="+container.SessionWorkDirKubeCacheDirPath(sessionDir),
		nixBash, "-c", script)
}

// countRegularFilesUnder returns the number of regular files under root
// (0 when root does not exist).
func countRegularFilesUnder(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // unreadable subtree — count what we can
		}
		if d.Type().IsRegular() {
			count++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Logf("walk %s: %v", root, err)
	}
	return count
}

// TestSandboxExecKubeConfig_CacheWritesLandInSessionWorkDir is the cache
// test: a kubectl discovery round-trip (against a test-local
// fake API server) writes its cache under <sessionDir>/kube-cache — the
// KUBECACHEDIR redirect riding the existing (subpath <sessionDir>) RW grant
// — and writes NOTHING under the host ~/.kube/cache.
func TestSandboxExecKubeConfig_CacheWritesLandInSessionWorkDir(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	kubectlBin := requireNixKubectl(t)

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}

	server := fakeKubeAPIServer(t)
	kubeconfigPath := writeFakeKubeconfig(t, t.TempDir(), server)

	m := newProfileManagerWithBareRoot(t)

	prepared, _ := preparePositiveProfile(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	sessionDir, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}
	kubeCacheDir := container.SessionWorkDirKubeCacheDirPath(sessionDir)

	// The discovery cache is keyed by host_port (e.g. 127.0.0.1_55555) —
	// attributable to this test's fake server, so the host-side negative
	// assertion below cannot be confused by unrelated host cache content.
	hostPortKey := strings.ReplaceAll(server.Host, ":", "_")

	cmd := kubectlAPIResourcesCmd(t, testProfilePath, sessionDir, kubectlBin, nixBash, kubeconfigPath)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("kubectl api-resources against the fake API server failed in-sandbox.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s", runErr, string(out), testProfilePath)
	}
	if !strings.Contains(string(out), "pods") {
		t.Fatalf("kubectl api-resources exited 0 but did not list the fake server's pods resource — no real discovery round-trip happened.\nOutput: %s", string(out))
	}

	// Cache files must have landed under <sessionDir>/kube-cache.
	if got := countRegularFilesUnder(t, kubeCacheDir); got == 0 {
		t.Errorf("no cache files under %s after a successful discovery round-trip — KUBECACHEDIR redirect not effective (issue #2235 AC)", kubeCacheDir)
	} else {
		t.Logf("ka pai — %d cache file(s) under %s", got, kubeCacheDir)
	}

	// And NOT under the host ~/.kube/cache: no entry attributable to the
	// fake server's host_port may appear anywhere under it.
	hostKubeCache := filepath.Join(home, ".kube", "cache")
	if _, statErr := os.Stat(hostKubeCache); statErr == nil {
		walkErr := filepath.WalkDir(hostKubeCache, func(path string, _ fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if strings.Contains(filepath.Base(path), hostPortKey) {
				t.Errorf("host kube cache entry %s attributable to the fake server (%s) — cache writes leaked to the host (issue #2235 AC)", path, hostPortKey)
			}
			return nil
		})
		if walkErr != nil {
			t.Logf("walk %s: %v", hostKubeCache, walkErr)
		}
	}
}

// TestSandboxExecKubeConfig_CacheWriteDeniedWithoutSessionWorkDirGrant is
// the paired negative for the cache test (sandbox-exec testing convention):
// with the (subpath <sessionDir>) line stripped from the profile,
// the same discovery round-trip lands no cache files under
// <sessionDir>/kube-cache — proving the session-work-dir grant is what
// permits the cache writes in the positive test. kubectl treats cache-write
// failures as non-fatal, so the assertion is on the filesystem, not on the
// exit code.
func TestSandboxExecKubeConfig_CacheWriteDeniedWithoutSessionWorkDirGrant(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	kubectlBin := requireNixKubectl(t)

	server := fakeKubeAPIServer(t)
	kubeconfigPath := writeFakeKubeconfig(t, t.TempDir(), server)

	m := newProfileManagerWithBareRoot(t)

	sessionDir, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}
	kubeCacheDir := container.SessionWorkDirKubeCacheDirPath(sessionDir)

	// Strip the session-work-dir subpath line (the grant under test). The
	// kubeconfig itself lives under /var/folders (broad allow), so its read
	// is unaffected — the mutation isolates the cache-write grant.
	sessionDirLine := "  (subpath " + sbplQuoteForTest(sessionDir) + ")\n"
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p, sessionDirLine, "")
	})

	cmd := kubectlAPIResourcesCmd(t, mutatedPath, sessionDir, kubectlBin, nixBash, kubeconfigPath)
	out, runErr := cmd.CombinedOutput()
	t.Logf("kubectl api-resources without the session-work-dir grant: exit=%v", runErr)
	_ = out

	if got := countRegularFilesUnder(t, kubeCacheDir); got != 0 {
		t.Errorf("%d cache file(s) landed under %s WITHOUT the (subpath <sessionDir>) grant — "+
			"the positive cache test is not exercising that grant.\nMutated profile: %s",
			got, kubeCacheDir, mutatedPath)
	} else {
		t.Logf("ka pai — no cache files land without the session-work-dir grant (grant is load-bearing)")
	}
}
