//go:build darwin

package integration_test

// sandbox_exec_rw_real_paths_darwin_test.go — integration coverage for the
// section-5e RW grant block on the real host paths (Step 3e of #2132, issue
// #2245): ~/.cache/nix, ~/.cache/bun, ~/.npm, ~/.mcp-auth.
//
// The RW staging symlinks for these dirs are gone; the explicit (subpath ...)
// RW grants emitted by generateProfile are the sole in-sandbox capability
// (none of the paths is sops-backed, so the #2211 allowlist plays no part).
//
// This file tests:
//
//  1. Positive: under the production profile, a create→read→remove
//     round-trip of a uniquely-named file succeeds inside each real host
//     dir (skipping dirs absent on this host). The staging HOME is asserted
//     to contain no 3e entries.
//
//  2. Negative (whole-block strip, per the #2243 lesson): removing the
//     ENTIRE section-5e allow block makes the same writes fail — proving
//     the block is load-bearing for every path in the set (stripping only a
//     single (subpath ...) line would leave the others granted and, in the
//     single-filter case, risk a filter-less allow-everything clause).
//
// The ~/.aws/{sso,cli} half of Step 3e is covered by the carve-out tests in
// sandbox_exec_aws_sso_darwin_test.go and
// sandbox_exec_aws_cache_writable_darwin_test.go (the §5 carve-outs are the
// capability there, not the 5e block).
//
// Per docs/sandbox-exec-testing.md (issue #1192); #2207 capability-probe
// gating via requireSandboxExec.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// rwRealPaths3e returns the 3e RW dirs that exist on this host, keyed by a
// short label. Dirs absent on the host are skipped (the grant is emitted
// unconditionally, but a round-trip needs a real dir to write into).
func rwRealPaths3e(t *testing.T) map[string]string {
	t.Helper()
	home := realUserHome(t)
	candidates := map[string]string{
		"cache-nix": filepath.Join(home, ".cache", "nix"),
		"cache-bun": filepath.Join(home, ".cache", "bun"),
		"npm":       filepath.Join(home, ".npm"),
		"mcp-auth":  filepath.Join(home, ".mcp-auth"),
	}
	present := map[string]string{}
	for label, dir := range candidates {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			present[label] = dir
		} else {
			t.Logf("3e dir %s absent on this host — skipping its round-trip", dir)
		}
	}
	if len(present) == 0 {
		t.Skip("none of the 3e dirs exist on this host — nothing to round-trip")
	}
	return present
}

// rwGrantBlock5e returns the exact section-5e allow block emitted by
// generateProfile, for the presence assertion in the positive test and the
// whole-block strip in the negative.
func rwGrantBlock5e(t *testing.T) string {
	t.Helper()
	home := realUserHome(t)
	return "(allow file-read* file-write* file-test-existence file-read-metadata\n" +
		"  (subpath " + sbplQuoteForTest(filepath.Join(home, ".cache", "nix")) + ")\n" +
		"  (subpath " + sbplQuoteForTest(filepath.Join(home, ".cache", "bun")) + ")\n" +
		"  (subpath " + sbplQuoteForTest(filepath.Join(home, ".npm")) + ")\n" +
		"  (subpath " + sbplQuoteForTest(filepath.Join(home, ".mcp-auth")) + "))\n"
}

// TestSandboxExecRWRealPaths_RoundTrip is the positive integration test for
// the section-5e RW grants: a create→read→remove round-trip succeeds at each
// real host path under the production profile.
func TestSandboxExecRWRealPaths_RoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	dirs := rwRealPaths3e(t)

	// BareRoot manager: real-path access under HOME requires the
	// BareRoot-ancestor block's metadata allow on (subpath HOME).
	m := newProfileManagerWithBareRoot(t)

	prepared, _ := preparePositiveProfile(t, m)

	block := rwGrantBlock5e(t)
	if !strings.Contains(prepared.content, block) {
		t.Fatalf("generated profile does not contain the section-5e RW block:\n%s\nProfile:\n%s", block, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	for label, dir := range dirs {
		t.Run(label, func(t *testing.T) {
			testFile := filepath.Join(dir, ".prism-2245-3e-rw-"+label)
			t.Cleanup(func() { _ = os.Remove(testFile) }) // host-side safety net
			sentinel := "prism-2245-3e-" + label + "-sentinel"
			script := "printf '%s' " + shQuote(sentinel) + " > " + shQuote(testFile) +
				" && cat " + shQuote(testFile) +
				" && rm " + shQuote(testFile)
			cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
				"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c", script)
			out, runErr := cmd.CombinedOutput()
			if runErr != nil {
				t.Fatalf("in-sandbox RW round-trip in %s failed under the production profile.\n"+
					"Exit: %v\nOutput: %s\nProfile: %s", dir, runErr, string(out), testProfilePath)
			}
			if !strings.Contains(string(out), sentinel) {
				t.Errorf("round-trip read in %s did not return the sentinel.\nGot: %s", dir, string(out))
			}
		})
	}
}

// TestSandboxExecRWRealPaths_DeniedWithoutGrantBlock is the paired negative
// test: with the ENTIRE section-5e block stripped, the same writes fail in
// every present 3e dir — proving the block is the load-bearing capability.
func TestSandboxExecRWRealPaths_DeniedWithoutGrantBlock(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	dirs := rwRealPaths3e(t)

	m := newProfileManagerWithBareRoot(t)

	block := rwGrantBlock5e(t)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.Replace(p, block, "", 1)
	})

	for label, dir := range dirs {
		t.Run(label, func(t *testing.T) {
			testFile := filepath.Join(dir, ".prism-2245-3e-denied-"+label)
			t.Cleanup(func() { _ = os.Remove(testFile) }) // in case the deny fails
			script := "echo prism-2245-denied > " + shQuote(testFile)
			cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
				"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c", script)
			out, runErr := cmd.CombinedOutput()
			if runErr == nil {
				t.Errorf("write into %s succeeded WITHOUT the section-5e block.\n"+
					"The 5e grant is not the load-bearing rule — investigate.\n"+
					"Output: %s\nMutated profile: %s", dir, string(out), mutatedPath)
			} else {
				t.Logf("ka pai — write into %s correctly denied without the 5e block (exit: %v)", dir, runErr)
			}
		})
	}
}
