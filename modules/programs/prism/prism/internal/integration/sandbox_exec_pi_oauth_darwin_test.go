//go:build darwin

package integration_test

// sandbox_exec_pi_oauth_darwin_test.go — integration tests for the PI OAuth
// token-refresh lockfile allow in the SBPL profile.
//
// Background:
//
//	OAuth token refresh inside pi-coding-agent uses
//	proper-lockfile.lock(authPath, {realpath: true}). With realpath:true,
//	proper-lockfile resolves any symlink in authPath and then calls
//	mkdir(<resolved>.lock) to acquire the lock. That mkdir requires write
//	permission on the parent directory ~/.pi/agent/, not just on auth.json.
//
//	A (literal ~/.pi/agent/auth.json) rule is insufficient: the sandbox
//	denies the mkdir (EPERM) and the refresh silently fails after ~30 s of
//	retries. The rule is therefore (subpath ~/.pi/agent) for pi sessions.
//
// These tests verify:
//
//  1. Positive: in a pi-harness manager config, creating a directory named
//     auth.json.lock inside ~/.pi/agent/ succeeds under the production SBPL
//     profile.
//  2. Negative: mutating the profile back to a (literal auth.json) rule
//     causes the same mkdir to fail, proving the positive test is not a
//     no-op.
//
// Each positive test is paired with a negative test per the convention in
// docs/sandbox-exec-testing.md.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// newPIOAuthProfileManager creates a Manager configured for a pi-harness
// session. Harness is set to "pi" so generateProfile emits the
// (subpath ~/.pi/agent) allow rule required for proper-lockfile.
func newPIOAuthProfileManager(t *testing.T) *container.Manager {
	t.Helper()
	instanceID := "integ-sbx-pi-oauth-" + strings.ReplaceAll(t.Name(), "/", "-")
	cfg := container.Config{
		SessionName: "integ-sandbox-exec-pi-oauth-test",
		InstanceID:  instanceID,
		Worktree:    t.TempDir(),
		Harness:     "pi",
		// writeGitconfig requires [user] identity — see newProfileManager
		// (sandbox_exec_helpers_darwin_test.go).
		GitUserName:  "test-user",
		GitUserEmail: "test@example.com",
	}
	return container.New(cfg)
}

// TestSandboxExecPIOAuth_LockDirCreatable is the positive integration test.
// It verifies that mkdir auth.json.lock inside ~/.pi/agent/ succeeds under
// the production SBPL profile for a pi-harness session. This is the exact
// operation that proper-lockfile performs when acquiring the lock for the
// OAuth token refresh.
func TestSandboxExecPIOAuth_LockDirCreatable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}

	// Ensure ~/.pi/agent exists so the subpath rule has something to cover.
	piAgentDir := filepath.Join(home, ".pi", "agent")
	if mkErr := os.MkdirAll(piAgentDir, 0o700); mkErr != nil {
		t.Fatalf("MkdirAll ~/.pi/agent: %v", mkErr)
	}

	m := newPIOAuthProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)

	// Confirm the profile contains the (subpath ~/.pi/agent) rule.
	piAgentSubpath := "(subpath " + sbplQuoteForTest(piAgentDir) + ")"
	if !strings.Contains(prepared.content, piAgentSubpath) {
		t.Fatalf("generated profile does not contain %q — the pi agent dir subpath rule was not emitted.\nProfile:\n%s",
			piAgentSubpath, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// The lock dir that proper-lockfile creates: <authPath>.lock adjacent to
	// auth.json inside ~/.pi/agent/. We use a unique name so parallel test
	// runs do not collide.
	lockDir := filepath.Join(piAgentDir, "auth.json.integ-pi-oauth-positive.lock")
	t.Cleanup(func() { _ = os.Remove(lockDir) })

	// Run: sandbox-exec mkdir auth.json.lock — this is the operation that
	// proper-lockfile performs when acquiring the OAuth refresh lock.
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		nixBash, "-c", "mkdir "+shQuote(lockDir))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("mkdir auth.json.lock inside ~/.pi/agent/ failed under production profile.\n"+
			"This is the proper-lockfile operation that OAuth token refresh requires.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			runErr, string(out), testProfilePath)
	}
}

// TestSandboxExecPIOAuth_LockDirDeniedWithLiteralOnlyRule is the paired
// negative test. It mutates the profile to replace the (subpath ~/.pi/agent)
// rule with a (literal auth.json) rule — the narrower rule that causes the
// OAuth refresh to fail. The same mkdir that the positive test asserts
// succeeds must fail with the narrower profile.
//
// This proves that the positive test is not green by accident: the subpath
// rule is the specific mechanism that allows the mkdir.
func TestSandboxExecPIOAuth_LockDirDeniedWithLiteralOnlyRule(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}

	piAgentDir := filepath.Join(home, ".pi", "agent")
	if mkErr := os.MkdirAll(piAgentDir, 0o700); mkErr != nil {
		t.Fatalf("MkdirAll ~/.pi/agent: %v", mkErr)
	}

	m := newPIOAuthProfileManager(t)

	// Mutate: replace the (subpath ~/.pi/agent) rule with a (literal
	// auth.json) rule — the narrower rule that causes the OAuth refresh to
	// fail.
	piAgentDir2 := piAgentDir // capture for closure
	authJSONLiteral := filepath.Join(piAgentDir2, "auth.json")
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		// The production profile emits:
		//   (allow file-read* file-write* file-test-existence file-read-metadata
		//     (subpath "<piAgentDir>"))
		//
		// Note the double closing paren on the subpath line: the first closes
		// (subpath ...) and the second closes the surrounding (allow ...) block.
		// Replace the indented (subpath ...)) line with a (literal auth.json))
		// line. The surrounding (allow ...) line is left intact so the profile
		// remains syntactically valid SBPL.
		subpathLine := "  (subpath " + sbplQuoteForTest(piAgentDir2) + "))\n"
		literalLine := "  (literal " + sbplQuoteForTest(authJSONLiteral) + "))\n"
		return strings.ReplaceAll(p, subpathLine, literalLine)
	})

	lockDir := filepath.Join(piAgentDir, "auth.json.integ-pi-oauth-negative.lock")
	t.Cleanup(func() { _ = os.Remove(lockDir) })

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		nixBash, "-c", "mkdir "+shQuote(lockDir))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("mkdir auth.json.lock succeeded with literal-only auth.json rule.\n"+
			"The negative test is not catching the regression — investigate.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — mkdir auth.json.lock correctly denied with literal-only rule (exit: %v)", runErr)
	}
}
