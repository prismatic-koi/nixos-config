//go:build darwin

package integration_test

// sandbox_exec_aws_sso_darwin_test.go — integration coverage for the
// ~/.aws/sso and ~/.aws/cli carve-outs in the SBPL profile (issue #1380).
//
// The SBPL profile contains a broad (deny file-read* file-write* (subpath
// ~/.aws)) to prevent host credential leakage. However, the staging HOME
// creates symlinks for ~/.aws/sso and ~/.aws/cli pointing at the host paths
// so that AWS SSO auth and kubectl (which reads SSO tokens) work inside the
// sandbox.
//
// generateProfile emits explicit (allow file-read* (subpath ~/.aws/sso)) and
// (allow file-read* (subpath ~/.aws/cli)) rules after the broad deny. In SBPL,
// more-specific rules override broader ones, so these carve-outs let the
// sandbox traverse into those two subdirs while the rest of ~/.aws remains
// denied.
//
// This file tests:
//
//  1. Positive case: when ~/.aws/sso exists on the host, a sentinel file
//     inside it is readable from inside the sandbox via the staging HOME
//     symlink chain.
//
//  2. Negative case: removing the (subpath ~/.aws/sso) carve-out from the
//     profile causes the same read to fail — proving the positive is not
//     green by accident and that the carve-out is load-bearing.
//
//  3. A symmetric positive/negative pair for ~/.aws/cli.
//
// Shared helpers (requireSandboxExec, requireNixBash, newProfileManager,
// newProfileManagerWithBareRoot, augmentProfileForTest, withMutatedProfile,
// sbplQuoteForTest, shQuote) live in sandbox_exec_helpers_darwin_test.go.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// awsSSOSentinelContent is the content written to the sentinel file inside
// ~/.aws/sso during the positive tests. Using a unique marker ensures the
// output check is not fooled by empty output or by another file's content.
const awsSSOSentinelContent = "prism-1380-aws-sso-sentinel"

// awsCLISentinelContent is the analogous sentinel for the ~/.aws/cli tests.
const awsCLISentinelContent = "prism-1380-aws-cli-sentinel"

// realUserHome returns the user's real (non-staging) home directory. It uses
// the REAL_HOME env var when set (the prism harness exports this so agents
// running inside a sandbox-exec session can find the host HOME), falling
// back to os.UserHomeDir(). Inside a prism sandbox-exec session, UserHomeDir()
// returns the staging HOME, which makes writes to ~/.aws/sso fail (the
// carve-out grants only read access); REAL_HOME bypasses that.
func realUserHome(t *testing.T) string {
	t.Helper()
	if rh := os.Getenv("REAL_HOME"); rh != "" {
		return rh
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	return home
}

// prepareSSOSentinel creates ~/.aws/sso under the user's real HOME (NOT under
// t.TempDir, which resolves to /private/var/folders and is broadly allowed)
// and plants a sentinel file inside it.
//
// The helper uses the real user HOME (see realUserHome) so that the sentinel
// is inside the host ~/.aws/sso — the directory that the carve-out rules
// govern — rather than the staging HOME's symlinked view.
//
// If the directory cannot be created or written to (e.g. the test is running
// inside a sandbox that denies write access to ~/.aws/sso), the test is
// skipped rather than failed.
//
// Returns (ssoDir, sentinelPath). The caller owns cleanup via t.Cleanup.
func prepareSSOSentinel(t *testing.T) (ssoDir, sentinelPath string) {
	t.Helper()
	home := realUserHome(t)
	awsDir := filepath.Join(home, ".aws")
	if mkErr := os.MkdirAll(awsDir, 0o700); mkErr != nil {
		t.Skipf("cannot create ~/.aws for test: %v", mkErr)
	}
	ssoDir = filepath.Join(awsDir, "sso")
	if mkErr := os.MkdirAll(ssoDir, 0o700); mkErr != nil {
		t.Skipf("cannot create ~/.aws/sso for test: %v", mkErr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ssoDir) })

	sentinelPath = filepath.Join(ssoDir, ".prism-1380-test-sentinel")
	if wErr := os.WriteFile(sentinelPath, []byte(awsSSOSentinelContent), 0o600); wErr != nil {
		t.Skipf("cannot plant ~/.aws/sso sentinel (may be running inside a restricted sandbox): %v", wErr)
	}
	return ssoDir, sentinelPath
}

// prepareCLISentinel is the analogous helper for ~/.aws/cli.
func prepareCLISentinel(t *testing.T) (cliDir, sentinelPath string) {
	t.Helper()
	home := realUserHome(t)
	awsDir := filepath.Join(home, ".aws")
	if mkErr := os.MkdirAll(awsDir, 0o700); mkErr != nil {
		t.Skipf("cannot create ~/.aws for test: %v", mkErr)
	}
	cliDir = filepath.Join(awsDir, "cli")
	if mkErr := os.MkdirAll(cliDir, 0o700); mkErr != nil {
		t.Skipf("cannot create ~/.aws/cli for test: %v", mkErr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cliDir) })

	sentinelPath = filepath.Join(cliDir, ".prism-1380-test-sentinel")
	if wErr := os.WriteFile(sentinelPath, []byte(awsCLISentinelContent), 0o600); wErr != nil {
		t.Skipf("cannot plant ~/.aws/cli sentinel (may be running inside a restricted sandbox): %v", wErr)
	}
	return cliDir, sentinelPath
}

// TestSandboxExecProfile_AWSSSOReadable is the positive integration test for
// the ~/.aws/sso carve-out (issue #1380). It:
//
//  1. Creates ~/.aws/sso and plants a sentinel file inside it.
//  2. Calls PrepareSandboxExecHome so the staging HOME contains
//     <stagingHome>/.aws/sso → ~/.aws/sso.
//  3. Generates the production profile (which emits the carve-out rule).
//  4. Reads the sentinel via the staging HOME symlink chain from inside
//     the sandbox and asserts exit 0 and correct content.
func TestSandboxExecProfile_AWSSSOReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	_, sentinelPath := prepareSSOSentinel(t)

	// BareRoot manager: the carve-out path resolution under HOME requires the
	// BareRoot-ancestor block's file-read-metadata allow on (subpath HOME).
	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// Verify the staging HOME has the sso symlink before generating the profile.
	ssoLink := filepath.Join(stagingHome, ".aws", "sso")
	if _, lstatErr := os.Lstat(ssoLink); lstatErr != nil {
		t.Fatalf("staging HOME must have a .aws/sso symlink after PrepareSandboxExecHome: %v", lstatErr)
	}

	prepared, _ := preparePositiveProfile(t, m)

	// The carve-out rule must appear in the generated profile.
	// Use the real user home (not the staging home) because generateProfile
	// derives the carve-out path from os.UserHomeDir() called at profile
	// generation time, which resolves to the real host HOME (not the staging
	// HOME that the sandbox sets as $HOME).
	realHome := realUserHome(t)
	awsSSOPath := filepath.Join(realHome, ".aws", "sso")
	if !strings.Contains(prepared.content, awsSSOPath) {
		t.Fatalf("generated profile does not contain the ~/.aws/sso carve-out path %q.\n"+
			"The carve-out rule was not emitted by generateProfile.\nProfile:\n%s",
			awsSSOPath, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Read the sentinel via <stagingHome>/.aws/sso/<sentinel> inside the sandbox.
	sentinelInSandbox := filepath.Join(ssoLink, filepath.Base(sentinelPath))
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(sentinelInSandbox))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("read ~/.aws/sso sentinel failed under production profile with carve-out.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			runErr, string(out), testProfilePath)
	}
	if !strings.Contains(string(out), awsSSOSentinelContent) {
		t.Errorf("read exited 0 but output does not contain sentinel marker.\nOutput: %s", string(out))
	}
}

// TestSandboxExecProfile_AWSSSODeniedWithoutCarveout is the paired negative
// test. It removes the (subpath ~/.aws/sso) carve-out line from the profile
// and asserts the same read fails — proving the carve-out is load-bearing.
func TestSandboxExecProfile_AWSSSODeniedWithoutCarveout(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	_, sentinelPath := prepareSSOSentinel(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	ssoLink := filepath.Join(stagingHome, ".aws", "sso")

	home, _ := os.UserHomeDir()
	awsSSOPath := filepath.Join(home, ".aws", "sso")

	// Remove the (subpath ~/.aws/sso) carve-out line from the profile.
	// The line is emitted as `  (subpath "<awsSSOPath>")\n` inside the
	// (allow file-read* ...) block that immediately follows the broad deny.
	carveoutLine := "  (subpath " + sbplQuoteForTest(awsSSOPath) + ")\n"
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p, carveoutLine, "")
	})

	sentinelInSandbox := filepath.Join(ssoLink, filepath.Base(sentinelPath))
	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(sentinelInSandbox))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("read ~/.aws/sso sentinel succeeded WITHOUT the (subpath %q) carve-out rule.\n"+
			"The carve-out is not the load-bearing rule — investigate.\n"+
			"Output: %s\nMutated profile: %s", awsSSOPath, string(out), mutatedPath)
	} else {
		t.Logf("ka pai — ~/.aws/sso read correctly denied without carve-out (exit: %v)", runErr)
	}
}

// TestSandboxExecProfile_AWSCLIReadable is the positive integration test for
// the ~/.aws/cli carve-out (issue #1380), symmetric to AWSSSOReadable.
func TestSandboxExecProfile_AWSCLIReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	_, sentinelPath := prepareCLISentinel(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	cliLink := filepath.Join(stagingHome, ".aws", "cli")
	if _, lstatErr := os.Lstat(cliLink); lstatErr != nil {
		t.Fatalf("staging HOME must have a .aws/cli symlink after PrepareSandboxExecHome: %v", lstatErr)
	}

	prepared, _ := preparePositiveProfile(t, m)

	home, _ := os.UserHomeDir()
	awsCLIPath := filepath.Join(home, ".aws", "cli")
	if !strings.Contains(prepared.content, awsCLIPath) {
		t.Fatalf("generated profile does not contain the ~/.aws/cli carve-out path %q.\n"+
			"The carve-out rule was not emitted by generateProfile.\nProfile:\n%s",
			awsCLIPath, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	sentinelInSandbox := filepath.Join(cliLink, filepath.Base(sentinelPath))
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(sentinelInSandbox))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("read ~/.aws/cli sentinel failed under production profile with carve-out.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			runErr, string(out), testProfilePath)
	}
	if !strings.Contains(string(out), awsCLISentinelContent) {
		t.Errorf("read exited 0 but output does not contain sentinel marker.\nOutput: %s", string(out))
	}
}

// TestSandboxExecProfile_AWSCLIDeniedWithoutCarveout is the paired negative
// test for AWSCLIReadable. It removes the (subpath ~/.aws/cli) carve-out and
// asserts the read fails.
func TestSandboxExecProfile_AWSCLIDeniedWithoutCarveout(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	_, sentinelPath := prepareCLISentinel(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	cliLink := filepath.Join(stagingHome, ".aws", "cli")

	home, _ := os.UserHomeDir()
	awsCLIPath := filepath.Join(home, ".aws", "cli")

	carveoutLine := "  (subpath " + sbplQuoteForTest(awsCLIPath) + ")\n"
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p, carveoutLine, "")
	})

	sentinelInSandbox := filepath.Join(cliLink, filepath.Base(sentinelPath))
	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(sentinelInSandbox))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("read ~/.aws/cli sentinel succeeded WITHOUT the (subpath %q) carve-out rule.\n"+
			"The carve-out is not the load-bearing rule — investigate.\n"+
			"Output: %s\nMutated profile: %s", awsCLIPath, string(out), mutatedPath)
	} else {
		t.Logf("ka pai — ~/.aws/cli read correctly denied without carve-out (exit: %v)", runErr)
	}
}
