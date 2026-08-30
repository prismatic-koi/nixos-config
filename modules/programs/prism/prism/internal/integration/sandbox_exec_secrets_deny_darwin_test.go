//go:build darwin

package integration_test

// sandbox_exec_secrets_deny_darwin_test.go — integration coverage for the
// sops secrets.d narrowing:
//
//	(deny file-write* (regex #"^/var/folders/.*/secrets\.d/") …)
//	(deny file-read*  (require-all (require-any (regex …)) (require-not …)…))
//
// The broad (subpath "/private/var/folders") allow exposes the entire
// home-manager sops-nix secrets tree to the sandbox. The narrowing denies the
// secrets.d subtree and carves out require-not exceptions for exactly the
// inventoried agent-needed secret NAMES, counter-independent ([0-9]+) so the
// rotation property is preserved.
//
// Coverage:
//
//  1. TestSandboxExecSecretsDeny_DeniedNamesUnreadable — reading the real
//     non-allowlisted secrets (github_token, the daily-driver RSA key) inside
//     the sandbox fails with EPERM.
//  2. TestSandboxExecSecretsDeny_NegationAllowsReads — the paired negative:
//     a profile mutation disabling the secrets.d deny regexes makes the same
//     reads succeed, proving the deny is the load-bearing rule (not
//     deny-default or POSIX permissions).
//  3. TestSandboxExecSecretsDeny_AllowlistedStableChainsReadable — reads
//     through the stable host symlink chains (~/.ssh/<key>,
//     ~/.config/aws/readonly-config, ~/.config/kube/agents-config) still
//     succeed under the production profile.
//  4. TestSandboxExecSecretsDeny_RotationSimulation — a fake secrets.d tree
//     under the per-user TMPDIR proves allowlisted reads survive a counter
//     rotation (100 → 101) and denied reads stay denied at both counters.
//     Unlike (3), the fake counters have no per-symlink (literal …) allows in
//     the profile, so this test isolates the require-not exceptions as the
//     only mechanism that can permit the read.
//  5. TestSandboxExecSecretsDeny_RotationSimulation_ExceptionsLoadBearing —
//     the paired negative for (4): stripping the require-not exceptions makes
//     the allowlisted-name read fail.
//
// Secret hygiene: tests never print secret file content. In-sandbox reads of
// real secrets redirect stdout to /dev/null and assert on exit status and
// stderr only. Only the fake rotation tree uses readable sentinel content.
//
// See docs/sandbox-exec-testing.md for the convention.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// secretsDeniedCandidates are the secrets.d-relative names that must stay
// denied: the daily-driver GitHub PAT and the daily-driver (non-prism) SSH
// private key. Neither may ever appear in the allowlist.
var secretsDeniedCandidates = []string{
	"github_token",
	"ssh/prismatic-koi-rsa",
}

// realSecretsDRoot returns the host sops secrets.d root derived from the
// per-user TMPDIR (the same base sops-nix uses on Darwin), skipping the test
// when the tree does not exist on this host.
func realSecretsDRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(os.TempDir(), "secrets.d")
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		t.Skipf("no sops secrets.d tree at %s on this host: %v", root, err)
	}
	return root
}

// currentSecretsCounterDir returns the path of the highest numeric
// generation dir under the secrets.d root, skipping when none is found or
// the root is unreadable (for example, the test process itself is already
// inside a sandbox).
func currentSecretsCounterDir(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("cannot enumerate %s from the test process (already sandboxed?): %v", root, err)
	}
	best := -1
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if n, convErr := strconv.Atoi(e.Name()); convErr == nil && n > best {
			best = n
		}
	}
	if best < 0 {
		t.Skipf("no numeric generation dir under %s", root)
	}
	return filepath.Join(root, strconv.Itoa(best))
}

// secretsDExceptionRe matches a require-not exception line emitted by
// generateProfile and captures the (regex-escaped) secret name.
var secretsDExceptionRe = regexp.MustCompile(`\(require-not \(regex #"/secrets\\\.d/\[0-9\]\+/(.+)\$"\)\)`)

// parseSecretsDAllowlist extracts the allowlisted secrets.d-relative names
// from the generated profile content, undoing the regex-metacharacter
// escaping applied by the generator. Parsing the emitted rules (rather than
// re-deriving from the host) means the tests exercise exactly what the
// profile enforces.
func parseSecretsDAllowlist(profile string) []string {
	var names []string
	for _, m := range secretsDExceptionRe.FindAllStringSubmatch(profile, -1) {
		names = append(names, unescapeRegexQuote(m[1]))
	}
	return names
}

// unescapeRegexQuote reverses the generator's regexQuotePath escaping:
// every backslash-prefixed character is replaced by the character itself.
func unescapeRegexQuote(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// sandboxCatDiscard runs `cat <path> > /dev/null` inside sandbox-exec under
// the given profile and returns the run error and combined output (stderr
// only — stdout is discarded inside the sandbox so secret content never
// reaches the test log).
func sandboxCatDiscard(profilePath, nixBash, path string) (error, string) {
	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(path)+" > /dev/null")
	out, err := cmd.CombinedOutput()
	return err, string(out)
}

// TestSandboxExecSecretsDeny_DeniedNamesUnreadable asserts that reading the
// real non-allowlisted secrets (github_token and the daily-driver RSA key)
// inside the sandbox fails with EPERM under the production profile.
func TestSandboxExecSecretsDeny_DeniedNamesUnreadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	root := realSecretsDRoot(t)
	counterDir := currentSecretsCounterDir(t, root)

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)

	// Guard: the denied candidates must never be allowlisted.
	allowNames := parseSecretsDAllowlist(prepared.content)
	for _, denied := range secretsDeniedCandidates {
		for _, allowed := range allowNames {
			if denied == allowed {
				t.Fatalf("denied candidate %q appears in the profile allowlist — inventory violation (issue #2211 AC #2)", denied)
			}
		}
	}

	profilePath := writeAugmentedPositiveProfile(t, prepared)

	tested := 0
	for _, rel := range secretsDeniedCandidates {
		target := filepath.Join(counterDir, filepath.FromSlash(rel))
		// Precondition: the file exists and is readable from the
		// (unsandboxed) test process — so an in-sandbox failure can only be
		// sandbox-caused, not ENOENT or POSIX permissions.
		f, openErr := os.Open(target)
		if openErr != nil {
			t.Logf("skipping %s: not present/readable on host: %v", rel, openErr)
			continue
		}
		_ = f.Close()
		tested++

		runErr, out := sandboxCatDiscard(profilePath, nixBash, target)
		if runErr == nil {
			t.Errorf("in-sandbox read of denied secret %q SUCCEEDED under the production profile — the secrets.d deny is not effective (issue #2211)", rel)
			continue
		}
		if !strings.Contains(out, "Operation not permitted") {
			t.Errorf("in-sandbox read of %q failed, but not with EPERM (want \"Operation not permitted\").\nExit: %v\nStderr: %s",
				rel, runErr, out)
		} else {
			t.Logf("ka pai — %s correctly unreadable in-sandbox (exit: %v)", rel, runErr)
		}
	}
	if tested == 0 {
		t.Skipf("none of the denied candidate secrets exist on this host: %v", secretsDeniedCandidates)
	}
}

// disableSecretsDDeny rewrites the secrets.d deny regex token so the deny
// (and its exceptions) no longer match any real path. Used by the paired
// negative test to prove the deny rules are load-bearing.
func disableSecretsDDeny(p string) string {
	return strings.ReplaceAll(p, `secrets\.d/`, `prism-2211-disabled\.d/`)
}

// TestSandboxExecSecretsDeny_NegationAllowsReads is the paired negative
// test: a profile mutation that removes the secrets.d deny must make the
// denied reads succeed — proving
// TestSandboxExecSecretsDeny_DeniedNamesUnreadable is not green by accident
// (the broad /private/var/folders allow otherwise permits the read).
func TestSandboxExecSecretsDeny_NegationAllowsReads(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	root := realSecretsDRoot(t)
	counterDir := currentSecretsCounterDir(t, root)

	m := newProfileManager(t)
	mutatedPath := withMutatedProfile(t, m, disableSecretsDDeny)

	tested := 0
	for _, rel := range secretsDeniedCandidates {
		target := filepath.Join(counterDir, filepath.FromSlash(rel))
		f, openErr := os.Open(target)
		if openErr != nil {
			t.Logf("skipping %s: not present/readable on host: %v", rel, openErr)
			continue
		}
		_ = f.Close()
		tested++

		runErr, out := sandboxCatDiscard(mutatedPath, nixBash, target)
		if runErr != nil {
			t.Errorf("in-sandbox read of %q still fails with the secrets.d deny disabled.\n"+
				"The negative test is not isolating the deny — something else blocks the read.\n"+
				"Exit: %v\nStderr: %s", rel, runErr, out)
		} else {
			t.Logf("ka pai — %s readable once the deny is disabled (deny is load-bearing)", rel)
		}
	}
	if tested == 0 {
		t.Skipf("none of the denied candidate secrets exist on this host: %v", secretsDeniedCandidates)
	}
}

// TestSandboxExecSecretsDeny_AllowlistedStableChainsReadable asserts that
// the agent-needed secrets remain readable through their stable host symlink
// chains under the production profile. The
// stable paths are the ones embedded in the generated ssh-config/gitconfig
// and staged into the sandbox HOME.
func TestSandboxExecSecretsDeny_AllowlistedStableChainsReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}

	// The inventoried stable sources (mirrors collectSecretsDAllowlistNames
	// with the default key names).
	stableSources := []string{
		filepath.Join(home, ".ssh", "prismatic-koi-ed25519"),
		filepath.Join(home, ".ssh", "prismatic-koi-ed25519.pub"),
		filepath.Join(home, ".ssh", "prismatic-koi-ed25519-signingkey"),
		filepath.Join(home, ".ssh", "prismatic-koi-ed25519-signingkey.pub"),
		filepath.Join(home, ".config", "aws", "readonly-config"),
		filepath.Join(home, ".config", "aws", "credentials"),
		filepath.Join(home, ".config", "kube", "agents-config"),
	}

	// BareRoot variant: following the stable symlink under the real HOME
	// needs the ancestor block's (subpath home) metadata allow.
	m := newProfileManagerWithBareRoot(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	tested := 0
	for _, src := range stableSources {
		resolved, resolveErr := filepath.EvalSymlinks(src)
		if resolveErr != nil || !strings.Contains(resolved, "/secrets.d/") {
			t.Logf("skipping %s: absent or not sops-backed on this host", src)
			continue
		}
		tested++

		runErr, out := sandboxCatDiscard(profilePath, nixBash, src)
		if runErr != nil {
			t.Errorf("in-sandbox read of allowlisted stable chain %s failed under the production profile.\n"+
				"The secrets.d narrowing broke an agent-needed credential read (issue #2211 functional AC).\n"+
				"Exit: %v\nStderr: %s", src, runErr, out)
		} else {
			t.Logf("ka pai — %s readable in-sandbox", src)
		}
	}
	if tested == 0 {
		t.Skipf("no sops-backed stable sources on this host")
	}
}

// setupFakeSecretsTree creates a fake sops-style secrets.d tree under the
// per-user TMPDIR (so the production deny/exception regexes apply to it
// without any profile mutation):
//
//	<base>/secrets.d/<counter>/<allowedName>   (sentinel content)
//	<base>/secrets.d/<counter>/github_token    (fake denied secret)
//
// Returns the base dir. Skips when TMPDIR is not under /var/folders (the
// deny regexes are anchored there).
func setupFakeSecretsTree(t *testing.T, allowedName string, counters ...string) string {
	t.Helper()
	base, err := os.MkdirTemp("", "prism-2211-rot-")
	if err != nil {
		t.Fatalf("MkdirTemp for fake secrets tree: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	canonical, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", base, err)
	}
	if !strings.HasPrefix(canonical, "/private/var/folders/") && !strings.HasPrefix(canonical, "/var/folders/") {
		t.Skipf("TMPDIR-derived base %s is not under /var/folders — the production secrets.d deny regexes would not apply", canonical)
	}

	for _, counter := range counters {
		writeFakeSecretsCounter(t, base, counter, allowedName)
	}
	return base
}

// writeFakeSecretsCounter writes one fake generation dir (allowed + denied
// names) under <base>/secrets.d/<counter>/.
func writeFakeSecretsCounter(t *testing.T, base, counter, allowedName string) {
	t.Helper()
	counterDir := filepath.Join(base, "secrets.d", counter)
	allowedPath := filepath.Join(counterDir, filepath.FromSlash(allowedName))
	if err := os.MkdirAll(filepath.Dir(allowedPath), 0o700); err != nil {
		t.Fatalf("create fake counter dir for %s: %v", allowedName, err)
	}
	if err := os.WriteFile(allowedPath, []byte(fakeAllowedSentinel), 0o600); err != nil {
		t.Fatalf("write fake allowed secret: %v", err)
	}
	deniedPath := filepath.Join(counterDir, "github_token")
	if err := os.WriteFile(deniedPath, []byte("ghp_fake-2211-denied"), 0o600); err != nil {
		t.Fatalf("write fake denied secret: %v", err)
	}
}

const fakeAllowedSentinel = "prism-2211-rotation-sentinel"

// TestSandboxExecSecretsDeny_RotationSimulation proves the rotation
// property of the narrowing: after a secrets.d counter increment,
// allowlisted reads continue to work and denied
// reads remain denied. The fake counters (100, 101) carry no per-symlink
// (literal …) allows in the profile, so the require-not exceptions are the
// only mechanism that can permit the allowlisted read — this is the
// precision check that the exceptions are counter-independent.
func TestSandboxExecSecretsDeny_RotationSimulation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)

	allowNames := parseSecretsDAllowlist(prepared.content)
	if len(allowNames) == 0 {
		t.Skipf("profile carries no secrets.d allowlist exceptions on this host (no sops-backed sources)")
	}
	allowedName := allowNames[0]
	for _, n := range allowNames {
		if n == "github_token" {
			t.Fatalf("github_token appears in the profile allowlist — inventory violation (issue #2211 AC #2)")
		}
	}

	base := setupFakeSecretsTree(t, allowedName, "100")
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	assertCounter := func(counter string) {
		t.Helper()
		counterDir := filepath.Join(base, "secrets.d", counter)
		allowedPath := filepath.Join(counterDir, filepath.FromSlash(allowedName))
		deniedPath := filepath.Join(counterDir, "github_token")

		cmd := exec.Command(sandboxExecPath, "-f", profilePath,
			nixBash, "-c", "cat "+shQuote(allowedPath))
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			t.Errorf("counter %s: in-sandbox read of allowlisted name %q failed — the exception is not counter-independent (#1410/#1573 regression).\nExit: %v\nOutput: %s",
				counter, allowedName, runErr, out)
		} else if !strings.Contains(string(out), fakeAllowedSentinel) {
			t.Errorf("counter %s: allowlisted read exited 0 but sentinel missing.\nOutput: %s", counter, out)
		}

		runErr, errOut := sandboxCatDiscard(profilePath, nixBash, deniedPath)
		if runErr == nil {
			t.Errorf("counter %s: in-sandbox read of denied name github_token SUCCEEDED — the deny is not counter-independent.", counter)
		} else if !strings.Contains(errOut, "Operation not permitted") {
			t.Errorf("counter %s: denied read failed but not with EPERM.\nExit: %v\nStderr: %s", counter, runErr, errOut)
		}
	}

	// Generation 100, then simulate a sops rotation: write 101, prune 100.
	assertCounter("100")
	writeFakeSecretsCounter(t, base, "101", allowedName)
	if err := os.RemoveAll(filepath.Join(base, "secrets.d", "100")); err != nil {
		t.Fatalf("prune fake counter 100: %v", err)
	}
	assertCounter("101")
}

// TestSandboxExecSecretsDeny_RotationSimulation_ExceptionsLoadBearing is the
// paired negative for the rotation simulation: with the require-not
// exceptions stripped from the profile, the allowlisted-name read at a fake
// counter fails — proving the exceptions (not some broader rule) are what
// permit it in the positive test.
func TestSandboxExecSecretsDeny_RotationSimulation_ExceptionsLoadBearing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)

	allowNames := parseSecretsDAllowlist(prepared.content)
	if len(allowNames) == 0 {
		t.Skipf("profile carries no secrets.d allowlist exceptions on this host (no sops-backed sources)")
	}
	allowedName := allowNames[0]

	base := setupFakeSecretsTree(t, allowedName, "100")
	allowedPath := filepath.Join(base, "secrets.d", "100", filepath.FromSlash(allowedName))

	// Strip every require-not exception line (mirrors withMutatedProfile,
	// operating on the already-prepared profile so the allowlist parse above
	// and the mutation use the same content).
	var kept []string
	for _, line := range strings.Split(prepared.content, "\n") {
		if strings.Contains(line, `(require-not (regex #"/secrets\.d/`) {
			continue
		}
		kept = append(kept, line)
	}
	mutated := strings.Join(kept, "\n")
	if mutated == prepared.content {
		t.Fatalf("stripping require-not exceptions changed nothing — mutation is a no-op.\nProfile:\n%s", prepared.content)
	}
	augmented := augmentProfileForTest(mutated)
	mutatedPath := prepared.path + ".integ-2211-noexc"
	if err := os.WriteFile(mutatedPath, []byte(augmented), 0o600); err != nil {
		t.Fatalf("write mutated profile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(mutatedPath) })

	runErr, out := sandboxCatDiscard(mutatedPath, nixBash, allowedPath)
	if runErr == nil {
		t.Errorf("in-sandbox read of %q succeeded WITHOUT the require-not exceptions — "+
			"the exceptions are not load-bearing and the positive rotation test is a no-op.", allowedName)
	} else {
		t.Logf("ka pai — allowlisted name unreadable once exceptions are stripped (exit: %v, stderr: %s)", runErr, out)
	}
}
