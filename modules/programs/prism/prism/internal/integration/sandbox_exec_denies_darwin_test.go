//go:build darwin

package integration_test

// sandbox_exec_denies_darwin_test.go — integration coverage for the explicit
// deny rules in the SBPL profile (issue #1192 AC #4):
//
//   - (deny file-read* file-write* (subpath "<HOME>/.aws"))
//   - (deny file-read* file-write* (subpath "/etc/wireguard"))
//   - (deny file-read* file-write* (subpath "/etc/wpa_supplicant"))
//   - (deny file-read* file-write* (subpath "/private/etc/wireguard"))
//   - (deny file-read* file-write* (subpath "/private/etc/wpa_supplicant"))
//
// Approach: the wireguard/wpa_supplicant directories typically do not
// exist on developer macOS workstations, so a literal stat on those paths
// is dominated by ENOENT rather than EPERM and does not distinguish "denied
// by sandbox" from "absent on disk". The ~/.aws deny is defensive — it
// guards against future symlink-target allows accidentally exposing the
// host ~/.aws subtree; under the current generator output there is no
// matching file-read* allow that the deny overrides, so removing the deny
// from the production profile does not change observable behaviour for
// arbitrary reads (deny-default already blocks them).
//
// We therefore split the coverage into:
//
//   1. Profile-content assertions
//      (`TestSandboxExecProfile_Denies_Present`) that the five expected
//      deny rules are emitted. These guard against the rules being
//      silently dropped from the generator — the regression mode the
//      backfilling convention exists to catch.
//
//   2. A deny-mechanism integration test pair
//      (`TestSandboxExecProfile_DenyOverridesAllow_BlocksReads` /
//      `TestSandboxExecProfile_DenyOverridesAllow_NegationAllowsReads`)
//      that proves the deny precedence works end-to-end against
//      sandbox-exec. The positive test mutates the generated profile to
//      add an allow + deny pair on a controlled host path and asserts the
//      deny wins. The negative test removes only the deny and asserts the
//      same read now succeeds — proving the deny rule itself is what
//      blocked the read in the positive case.
//
// This is more precise than testing ~/.aws or wireguard directly: it
// isolates the deny mechanism (allow + deny → deny wins) from the
// orthogonal question of whether the production allow set covers a given
// path. The production deny rules guard against future regressions where
// such an allow IS introduced; the mechanism test confirms that, when
// such a regression occurs, the deny rule will catch it.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSandboxExecProfile_Denies_Present asserts that the five expected
// deny rules appear in the generated profile. This is a substring guard
// against silent rule deletion — the integration deny-mechanism test
// below validates that the rules are actually load-bearing.
func TestSandboxExecProfile_Denies_Present(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	// The expected deny rules. Each entry is the verbatim line as emitted
	// by generateProfile (including the leading two-space indent that
	// nests it inside the parent (deny ...) block).
	expected := []string{
		"  (subpath \"/etc/wireguard\")",
		"  (subpath \"/etc/wpa_supplicant\")",
		"  (subpath \"/private/etc/wireguard\")",
		"  (subpath \"/private/etc/wpa_supplicant\"))",
		"  (subpath \"" + home + "/.aws\"))",
	}

	for _, exp := range expected {
		if !strings.Contains(prepared.content, exp) {
			t.Errorf("generated profile is missing the deny rule line %q.\n"+
				"Either the rule was removed from generateProfile or its formatting changed.\n"+
				"If the format changed intentionally, update this test's expected lines.",
				exp)
		}
	}

	// Also assert there is at least one (deny file-read* file-write* ... rule
	// preceding each subpath, so we know the lines are actually inside a
	// deny block (not, say, accidentally emitted under an allow block).
	denyHeader := "(deny file-read* file-write*\n"
	if strings.Count(prepared.content, denyHeader) < 2 {
		t.Errorf("expected at least 2 occurrences of %q (one for /etc subtrees, one for ~/.aws); got %d.\n"+
			"Profile:\n%s",
			denyHeader, strings.Count(prepared.content, denyHeader), prepared.content)
	}
}

// denyMechanismMarker is the marker comment we inject around the
// synthetic allow + deny pair so the negative test can locate and remove
// only the deny half.
const denyMechanismMarker = ";; --- prism-1192 deny-mechanism test rules ---"

// injectDenyMechanismRules appends an `(allow file-read* (subpath dir))`
// followed by `(deny file-read* file-write* (subpath dir))` for the given
// directory. SBPL evaluates rules top-to-bottom with later rules winning
// for overlapping path scope; appending the deny after the allow at the
// end of the profile means the deny is the most-specific applicable rule
// for the dir subtree.
//
// Returns the mutated profile content with the new rules appended after
// a marker comment so injectDenyMechanismRules_RemoveDeny can strip just
// the deny half for the negative test.
func injectDenyMechanismRules(profile, dir string) string {
	suffix := "\n" + denyMechanismMarker + "\n" +
		"(allow file-read* (subpath " + sbplQuoteForTest(dir) + "))\n" +
		"(deny file-read* file-write* (subpath " + sbplQuoteForTest(dir) + "))\n"
	return profile + suffix
}

// injectDenyMechanismRules_RemoveDeny is the negative-test variant. It
// appends only the allow (no deny), so reads of files under dir succeed.
func injectDenyMechanismRules_RemoveDeny(profile, dir string) string {
	suffix := "\n" + denyMechanismMarker + "\n" +
		"(allow file-read* (subpath " + sbplQuoteForTest(dir) + "))\n"
	return profile + suffix
}

// TestSandboxExecProfile_DenyOverridesAllow_BlocksReads proves the deny
// mechanism end-to-end: with both (allow file-read*) AND (deny file-read*)
// for the same subtree, reads inside the subtree are blocked.
//
// We use a controlled host directory under HOME (not a path the production
// profile already covers) so the only rules affecting it are the synthetic
// allow + deny pair we inject. The ancestor block from BareRoot grants
// traversal access to HOME so path resolution succeeds; the synthetic
// allow grants data reads on the dir subtree; the synthetic deny then
// overrides and blocks the read. Asserts cat exits non-zero.
//
// This is the positive half of the deny-mechanism convention check. The
// production deny rules (~/.aws, wireguard, wpa_supplicant) rely on this
// same precedence to be load-bearing if a future profile change introduces
// a matching allow.
func TestSandboxExecProfile_DenyOverridesAllow_BlocksReads(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	// Plant a sentinel file under HOME (not under /private/var/folders,
	// which is broadly allowed) so the only rules affecting it are the
	// ones we inject below.
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	subjectDir, err := os.MkdirTemp(home, ".prism-1192-deny-subject-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home) for deny subject: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(subjectDir) })

	sentinel := filepath.Join(subjectDir, "sentinel")
	const sentinelData = "prism-1192-deny-mechanism-sentinel"
	if err := os.WriteFile(sentinel, []byte(sentinelData), 0o600); err != nil {
		t.Fatalf("plant sentinel: %v", err)
	}

	// Use BareRoot so traversal of HOME succeeds.
	m := newProfileManagerWithBareRoot(t)
	prepared, _ := preparePositiveProfile(t, m)

	// Inject allow + deny on the subjectDir into the production profile.
	mutated := injectDenyMechanismRules(prepared.content, subjectDir)
	augmented := augmentProfileForTest(mutated)
	testProfilePath := prepared.path + ".integ-deny-pos"
	if err := os.WriteFile(testProfilePath, []byte(augmented), 0o600); err != nil {
		t.Fatalf("write test profile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(testProfilePath) })

	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		nixBash, "-c", "cat "+shQuote(sentinel))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("read of %s succeeded (exit 0) even with a (deny file-read*) rule overriding the (allow file-read*).\n"+
			"The deny precedence is broken — production deny rules cannot be load-bearing.\n"+
			"Output: %s\nProfile: %s", sentinel, string(out), testProfilePath)
	} else {
		t.Logf("ka pai — deny correctly overrode allow (exit: %v)", runErr)
	}
}

// TestSandboxExecProfile_DenyOverridesAllow_NegationAllowsReads is the
// paired negative test. It injects only the allow (no deny) on the same
// subject dir and asserts the read succeeds. Proves the positive test is
// not green by accident — removing only the deny lets the same read
// through, which is the load-bearing precedence check.
func TestSandboxExecProfile_DenyOverridesAllow_NegationAllowsReads(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	subjectDir, err := os.MkdirTemp(home, ".prism-1192-deny-subject-neg-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home) for deny subject: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(subjectDir) })

	sentinel := filepath.Join(subjectDir, "sentinel")
	const sentinelData = "prism-1192-deny-mechanism-sentinel-neg"
	if err := os.WriteFile(sentinel, []byte(sentinelData), 0o600); err != nil {
		t.Fatalf("plant sentinel: %v", err)
	}

	m := newProfileManagerWithBareRoot(t)
	prepared, _ := preparePositiveProfile(t, m)

	mutated := injectDenyMechanismRules_RemoveDeny(prepared.content, subjectDir)
	augmented := augmentProfileForTest(mutated)
	testProfilePath := prepared.path + ".integ-deny-neg"
	if err := os.WriteFile(testProfilePath, []byte(augmented), 0o600); err != nil {
		t.Fatalf("write test profile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(testProfilePath) })

	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		nixBash, "-c", "cat "+shQuote(sentinel))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("read of %s failed even with only the (allow file-read*) rule (no deny).\n"+
			"This means the allow itself is not load-bearing — the negative test is not\n"+
			"isolating the deny precedence. Investigate the test setup.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			sentinel, runErr, string(out), testProfilePath)
	}
	if !strings.Contains(string(out), sentinelData) {
		t.Errorf("read of %s exited 0 but output does not contain the sentinel marker.\nOutput: %s",
			sentinel, string(out))
	}
}
