//go:build darwin

package integration_test

// sandbox_exec_aws_cache_writable_darwin_test.go — integration coverage for
// the write-access allow on the ~/.aws/sso and ~/.aws/cli carve-outs in the
// SBPL profile.
//
// The SBPL profile §5 emits:
//
//	(allow file-read* file-write*
//	  (subpath "~/.aws/sso")
//	  (subpath "~/.aws/cli"))
//
// The aws CLI writes STS token cache entries to ~/.aws/cli/cache/ and
// refreshes SSO tokens in ~/.aws/sso/. Without file-write* the CLI fails
// with EPERM, and kubectl against EKS breaks too (its exec-credential
// plugin shells out to aws and gets EPERM).
//
// The staging HOME does not symlink .aws/sso or .aws/cli. The carve-out
// rules above are the SOLE in-sandbox capability for the two cache dirs,
// exercised at the real host paths.
//
// This file tests:
//
//  1. Positive case (SSO write): with the production profile, writing a
//     file inside the real ~/.aws/sso succeeds.
//
//  2. Negative case (SSO write denied without file-write*): removing the
//     file-write* from the carve-out causes the same write to fail —
//     proving the positive is not green by accident and that file-write*
//     is load-bearing.
//
//  3. Positive case (CLI write): symmetric to SSO, but for ~/.aws/cli.
//
//  4. Negative case (CLI write denied without file-write*): symmetric
//     negative pair for CLI.
//
//  5. Security case: a write attempt to ~/.aws/<other> (a path inside the
//     broad deny but outside the carve-outs) must still fail. This guards
//     against the carve-out being accidentally widened to cover the entire
//     ~/.aws subtree.
//
//  6. Edge-case: when ~/.aws/cli and ~/.aws/sso do NOT exist on the host
//     at session-spawn time, the generated profile still loads under
//     /usr/bin/sandbox-exec and a non-AWS workload exits 0. This validates
//     that the carve-out rules (which reference non-existent paths) do not
//     break sandbox initialisation — sandbox-exec silently ignores
//     (subpath ...) rules for paths that do not exist on the host.
//
// Shared helpers (requireSandboxExec, requireNixBash, newProfileManagerWithBareRoot,
// withMutatedProfile, preparePositiveProfile, writeAugmentedPositiveProfile,
// realUserHome, prepareSSOSentinel, prepareCLISentinel, sbplQuoteForTest,
// shQuote) live in sandbox_exec_helpers_darwin_test.go and
// sandbox_exec_aws_sso_darwin_test.go.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// awsCacheSSOWriteContent is the content written to the test file inside
// ~/.aws/sso during the write-positive tests.
const awsCacheSSOWriteContent = "prism-1558-aws-sso-write-sentinel"

// awsCacheCLIWriteContent is the analogous content for the ~/.aws/cli tests.
const awsCacheCLIWriteContent = "prism-1558-aws-cli-write-sentinel"

// TestSandboxExecProfile_AWSSSOWritable is the positive integration test for
// write access to ~/.aws/sso. It:
//
//  1. Creates ~/.aws/sso under the real user HOME (so the write attempt
//     targets the deny-covered path, not /private/var/folders).
//  2. Generates the production profile (which emits file-read* file-write*
//     for both ~/.aws/sso and ~/.aws/cli).
//  3. Writes a file inside the real ~/.aws/sso from inside the sandbox and
//     asserts exit 0 (no staging symlink involved).
func TestSandboxExecProfile_AWSSSOWritable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	ssoDir, _ := prepareSSOSentinel(t)

	m := newProfileManagerWithBareRoot(t)

	prepared, _ := preparePositiveProfile(t, m)

	// The carve-out must include file-write* in the generated profile.
	realHome := realUserHome(t)
	awsSSOPath := filepath.Join(realHome, ".aws", "sso")
	expectedWriteRule := "(allow file-read* file-write*\n  (subpath " + sbplQuoteForTest(awsSSOPath)
	if !strings.Contains(prepared.content, expectedWriteRule) {
		t.Fatalf("generated profile does not contain a file-write* carve-out for ~/.aws/sso.\n"+
			"Expected to find: %q\nProfile:\n%s", expectedWriteRule, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Write a sentinel file inside the REAL ~/.aws/sso from inside the
	// sandbox — the carve-out grants the real path directly.
	writeTarget := filepath.Join(ssoDir, ".prism-1558-write-test")
	t.Cleanup(func() { _ = os.Remove(writeTarget) })

	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c",
		"echo "+shQuote(awsCacheSSOWriteContent)+" > "+shQuote(writeTarget))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("write to ~/.aws/sso failed under production profile with file-write* carve-out.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s\nTarget: %s",
			runErr, string(out), testProfilePath, writeTarget)
	}

	// Verify the write actually landed on the host.
	got, readErr := os.ReadFile(writeTarget)
	if readErr != nil {
		t.Fatalf("sentinel not found on host after sandbox write: %v", readErr)
	}
	if !strings.Contains(string(got), awsCacheSSOWriteContent) {
		t.Errorf("written file does not contain expected content.\nGot: %s", string(got))
	}
}

// TestSandboxExecProfile_AWSSSOWriteDeniedWithoutFileWrite is the paired
// negative test for AWSSSOWritable. It removes file-write* from the
// ~/.aws/sso carve-out line and asserts the same write fails — proving
// that file-write* is load-bearing and the positive is not green by accident.
func TestSandboxExecProfile_AWSSSOWriteDeniedWithoutFileWrite(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	ssoDir, _ := prepareSSOSentinel(t)

	m := newProfileManagerWithBareRoot(t)

	// Remove file-write* from the carve-out allow block. The production
	// profile emits:
	//   (allow file-read* file-write*
	//     (subpath "<awsSSOPath>")
	//     (subpath "<awsCLIPath>"))
	// We replace it with the read-only form to remove the write capability.
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p,
			"(allow file-read* file-write*\n  (subpath "+sbplQuoteForTest(filepath.Join(realUserHome(t), ".aws", "sso")),
			"(allow file-read*\n  (subpath "+sbplQuoteForTest(filepath.Join(realUserHome(t), ".aws", "sso")),
		)
	})

	writeTarget := filepath.Join(ssoDir, ".prism-1558-write-test-neg")
	t.Cleanup(func() { _ = os.Remove(writeTarget) })

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c",
		"echo "+shQuote(awsCacheSSOWriteContent)+" > "+shQuote(writeTarget))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write to ~/.aws/sso succeeded WITHOUT file-write* in the carve-out.\n"+
			"file-write* is not the load-bearing rule — investigate.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — ~/.aws/sso write correctly denied without file-write* (exit: %v)", runErr)
	}
}

// TestSandboxExecProfile_AWSCLIWritable is the positive integration test for
// write access to ~/.aws/cli, symmetric to AWSSSOWritable.
func TestSandboxExecProfile_AWSCLIWritable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	cliDir, _ := prepareCLISentinel(t)

	m := newProfileManagerWithBareRoot(t)

	prepared, _ := preparePositiveProfile(t, m)

	// The carve-out must include file-write* and the cli path.
	realHome := realUserHome(t)
	awsCLIPath := filepath.Join(realHome, ".aws", "cli")
	if !strings.Contains(prepared.content, "file-write*") {
		t.Fatalf("generated profile does not contain file-write* anywhere — the carve-out fix was not applied.\nProfile:\n%s",
			prepared.content)
	}
	if !strings.Contains(prepared.content, awsCLIPath) {
		t.Fatalf("generated profile does not contain the ~/.aws/cli carve-out path %q.\nProfile:\n%s",
			awsCLIPath, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	writeTarget := filepath.Join(cliDir, ".prism-1558-write-test")
	t.Cleanup(func() { _ = os.Remove(writeTarget) })

	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c",
		"echo "+shQuote(awsCacheCLIWriteContent)+" > "+shQuote(writeTarget))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("write to ~/.aws/cli failed under production profile with file-write* carve-out.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s\nTarget: %s",
			runErr, string(out), testProfilePath, writeTarget)
	}

	got, readErr := os.ReadFile(writeTarget)
	if readErr != nil {
		t.Fatalf("sentinel not found on host after sandbox write: %v", readErr)
	}
	if !strings.Contains(string(got), awsCacheCLIWriteContent) {
		t.Errorf("written file does not contain expected content.\nGot: %s", string(got))
	}
}

// TestSandboxExecProfile_AWSCLIWriteDeniedWithoutFileWrite is the paired
// negative test for AWSCLIWritable. It removes file-write* from the carve-out
// and asserts the same write fails.
func TestSandboxExecProfile_AWSCLIWriteDeniedWithoutFileWrite(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	cliDir, _ := prepareCLISentinel(t)

	m := newProfileManagerWithBareRoot(t)

	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p,
			"(allow file-read* file-write*\n  (subpath "+sbplQuoteForTest(filepath.Join(realUserHome(t), ".aws", "sso")),
			"(allow file-read*\n  (subpath "+sbplQuoteForTest(filepath.Join(realUserHome(t), ".aws", "sso")),
		)
	})

	writeTarget := filepath.Join(cliDir, ".prism-1558-write-test-neg")
	t.Cleanup(func() { _ = os.Remove(writeTarget) })

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c",
		"echo "+shQuote(awsCacheCLIWriteContent)+" > "+shQuote(writeTarget))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write to ~/.aws/cli succeeded WITHOUT file-write* in the carve-out.\n"+
			"file-write* is not the load-bearing rule — investigate.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — ~/.aws/cli write correctly denied without file-write* (exit: %v)", runErr)
	}
}

// TestSandboxExecProfile_AWSOutsideCarveoutDenied is the security test: a
// write attempt to ~/.aws/<other> (a path inside the broad deny but outside
// the ~/.aws/sso and ~/.aws/cli carve-outs) must still fail. This guards against the carve-out being accidentally widened
// to cover the entire ~/.aws subtree.
//
// The test creates a temporary directory directly under ~/.aws (NOT under
// sso/ or cli/) and attempts to write a file into it from inside the
// sandbox. The (deny file-read* file-write* (subpath ~/.aws)) rule must
// block the write even though the (allow file-read* file-write* (subpath
// ~/.aws/sso)) and (allow file-read* file-write* (subpath ~/.aws/cli))
// carve-outs exist.
func TestSandboxExecProfile_AWSOutsideCarveoutDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	home := realUserHome(t)
	awsDir := filepath.Join(home, ".aws")
	if mkErr := os.MkdirAll(awsDir, 0o700); mkErr != nil {
		t.Skipf("cannot create ~/.aws for test: %v", mkErr)
	}

	// Create a dir directly under ~/.aws that is NOT sso/ or cli/.
	otherDir, err := os.MkdirTemp(awsDir, ".prism-1558-other-*")
	if err != nil {
		t.Skipf("cannot create temp dir under ~/.aws (may be running inside a restricted sandbox): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(otherDir) })

	m := newProfileManagerWithBareRoot(t)

	prepared, _ := preparePositiveProfile(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Attempt to write directly into otherDir (which is under ~/.aws but
	// outside the sso/ and cli/ carve-outs). The broad deny must block this.
	writeTarget := filepath.Join(otherDir, ".prism-1558-security-test")
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c",
		"echo prism-1558-security > "+shQuote(writeTarget))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write to ~/.aws/<other> succeeded — the deny is not tight enough.\n"+
			"A path outside the sso/ and cli/ carve-outs was writable from inside the sandbox.\n"+
			"Output: %s\nProfile: %s\nTarget: %s", string(out), testProfilePath, writeTarget)
	} else {
		t.Logf("ka pai — write to ~/.aws/<other> correctly denied (exit: %v)", runErr)
	}
}

// TestSandboxExecProfile_AWSCarveoutAbsent_ProfileLoads is the edge-case
// integration test: when ~/.aws/cli and ~/.aws/sso do NOT exist on the host
// at session-spawn time, the generated profile still loads under
// /usr/bin/sandbox-exec and a non-AWS workload exits 0.
//
// generateProfile unconditionally emits the carve-out rules with the host
// paths (filepath.Join(home, ".aws", "sso") and ".aws", "cli").
// sandbox-exec silently ignores (subpath ...) rules for paths that do not
// exist, so the profile must still load and allow a simple non-AWS
// workload (echo) to succeed.
//
// This guards against a regression where the carve-out rules cause sandbox
// initialisation to fail when the host has no ~/.aws/sso or ~/.aws/cli.
func TestSandboxExecProfile_AWSCarveoutAbsent_ProfileLoads(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	home := realUserHome(t)
	awsSSOPath := filepath.Join(home, ".aws", "sso")
	awsCLIPath := filepath.Join(home, ".aws", "cli")

	// If either directory exists, temporarily rename it so this test can
	// simulate a host without ~/.aws/sso and ~/.aws/cli. This is a best-effort
	// skip rather than a skip-if-absent because the test is specifically about
	// profile loading when the dirs are absent. We do not want to silently
	// skip on developer machines where they always exist.
	for _, p := range []string{awsSSOPath, awsCLIPath} {
		if _, statErr := os.Stat(p); statErr == nil {
			// Temporarily rename out of the way.
			tmp := p + ".prism-1558-absent-test-rename"
			if renErr := os.Rename(p, tmp); renErr != nil {
				t.Skipf("cannot temporarily rename %s to simulate absence: %v", p, renErr)
			}
			restorePath := p // capture for closure
			tmpPath := tmp   // capture for closure
			t.Cleanup(func() { _ = os.Rename(tmpPath, restorePath) })
		}
	}

	m := newProfileManagerWithBareRoot(t)

	prepared, _ := preparePositiveProfile(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Run a simple non-AWS workload (echo) to verify the profile loads and
	// the sandbox initialises without error even when the carve-out paths
	// do not exist on the host.
	const echoSentinel = "prism-1558-absent-carveout-ok"
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c",
		"echo "+shQuote(echoSentinel))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("non-AWS workload failed even though ~/.aws/sso and ~/.aws/cli are absent.\n"+
			"The carve-out rules for non-existent paths must not break profile loading.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			runErr, string(out), testProfilePath)
	}
	if !strings.Contains(string(out), echoSentinel) {
		t.Errorf("workload exited 0 but output does not contain expected sentinel.\nOutput: %s", string(out))
	}
	t.Logf("ka pai — profile loads correctly when ~/.aws/sso and ~/.aws/cli are absent (exit: %v)", runErr)
}
