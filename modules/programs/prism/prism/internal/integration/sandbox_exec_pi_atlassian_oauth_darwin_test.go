//go:build darwin

package integration_test

// sandbox_exec_pi_atlassian_oauth_darwin_test.go — integration tests for the
// PI Atlassian MCP OAuth token persistence via sandbox-exec symlink (issue #1597).
//
// Background:
//
//	The Atlassian MCP extension stores OAuth tokens at
//	homedir()/.pi/agent/atlassian-mcp-oauth.json. Inside a Darwin
//	sandbox-exec pi session, $HOME is overridden to a per-session staging
//	HOME, so writes to that path land in the staging dir and are destroyed
//	when the session ends. The fix in PrepareSandboxExecHome (for Harness ==
//	"pi" on Darwin):
//
//	  1. Creates <stagingHome>/.pi/agent/ as a real directory.
//	  2. Touches ~/.pi/agent/atlassian-mcp-oauth.json on the host with mode
//	     0600 if absent.
//	  3. Symlinks <stagingHome>/.pi/agent/atlassian-mcp-oauth.json → the
//	     real host path.
//
//	The SBPL profile already emits (allow file-read* file-write* ...
//	(subpath ~/.pi/agent)) for pi sessions, so writes inside the sandbox go
//	through the symlink to the real host path.
//
// These tests verify:
//
//  1. Positive: a bash command running inside sandbox-exec (with the
//     production SBPL profile for Harness == "pi") writes JSON bytes to
//     $HOME/.pi/agent/atlassian-mcp-oauth.json; those exact bytes are
//     readable from the real host ~/.pi/agent/atlassian-mcp-oauth.json
//     after sandbox-exec exits. This confirms the symlink write-through
//     works end-to-end.
//
//  2. Negative: when the symlink at <stagingHome>/.pi/agent/atlassian-mcp-oauth.json
//     is removed (breaking the write-through), the same write either fails
//     or does NOT appear at the real host path, proving the positive test
//     is not a no-op. (Per docs/sandbox-exec-testing.md, issue #1192.)

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// newPIOAuthPersistenceManager creates a Manager configured for a pi-harness
// session with a BareRoot (required for the pi agent dir subpath allow in the
// SBPL profile).
func newPIOAuthPersistenceManager(t *testing.T) *container.Manager {
	t.Helper()
	instanceID := "integ-sbx-pi-atlassian-oauth-" + strings.ReplaceAll(t.Name(), "/", "-")
	cfg := container.Config{
		SessionName: "integ-sandbox-exec-pi-atlassian-oauth-test",
		InstanceID:  instanceID,
		Worktree:    t.TempDir(),
		Harness:     "pi",
		BareRoot:    t.TempDir(), // needed for the BareRoot-ancestor allow block
	}
	return container.New(cfg)
}

// TestSandboxExecPIAtlassianOAuth_WriteThrough is the positive integration
// test. It verifies that:
//
//  1. PrepareSandboxExecHome for Harness=="pi" creates a symlink at
//     <stagingHome>/.pi/agent/atlassian-mcp-oauth.json pointing at the real
//     host ~/.pi/agent/atlassian-mcp-oauth.json.
//  2. A bash command inside sandbox-exec (using the production SBPL profile)
//     can write JSON bytes to $HOME/.pi/agent/atlassian-mcp-oauth.json.
//  3. Those exact bytes are readable from the real host path after sandbox-exec
//     exits, confirming the symlink write-through is functional end-to-end.
func TestSandboxExecPIAtlassianOAuth_WriteThrough(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}

	// Ensure ~/.pi/agent/ exists on the host so PrepareSandboxExecHome can
	// create the host token file.
	piAgentDir := filepath.Join(home, ".pi", "agent")
	if mkErr := os.MkdirAll(piAgentDir, 0o700); mkErr != nil {
		t.Fatalf("MkdirAll ~/.pi/agent: %v", mkErr)
	}

	// Stash any existing atlassian-mcp-oauth.json so we can restore it and
	// so we start with a clean state for the write-through assertion.
	hostTokenPath := filepath.Join(piAgentDir, "atlassian-mcp-oauth.json")
	stashPath := hostTokenPath + ".integ-pi-atlassian-oauth-stash"
	stashed := false
	if _, statErr := os.Lstat(hostTokenPath); statErr == nil {
		if renErr := os.Rename(hostTokenPath, stashPath); renErr == nil {
			stashed = true
			t.Cleanup(func() {
				_ = os.Remove(hostTokenPath)
				_ = os.Rename(stashPath, hostTokenPath)
			})
		}
	}
	if !stashed {
		t.Cleanup(func() { _ = os.Remove(hostTokenPath) })
	}

	m := newPIOAuthPersistenceManager(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// Verify the symlink exists in the staging HOME before running sandbox-exec.
	stagingTokenPath := filepath.Join(stagingHome, ".pi", "agent", "atlassian-mcp-oauth.json")
	linkTarget, readlinkErr := os.Readlink(stagingTokenPath)
	if readlinkErr != nil {
		t.Fatalf("staging token symlink not created by PrepareSandboxExecHome: %v", readlinkErr)
	}
	if linkTarget != hostTokenPath {
		t.Fatalf("staging token symlink target = %q, want %q", linkTarget, hostTokenPath)
	}

	prepared, _ := preparePositiveProfile(t, m)

	// Confirm the profile contains the (subpath ~/.pi/agent) rule.
	piAgentSubpath := "(subpath " + sbplQuoteForTest(piAgentDir) + ")"
	if !strings.Contains(prepared.content, piAgentSubpath) {
		t.Fatalf("generated profile does not contain %q\nProfile:\n%s", piAgentSubpath, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Use a unique sentinel value for this test run so we can assert the
	// specific bytes were written (not a leftover from a previous run).
	const sentinel = `{"access_token":"integ-pi-atlassian-oauth-positive-sentinel"}`

	// Inside sandbox-exec with HOME=<stagingHome>, write the sentinel JSON to
	// $HOME/.pi/agent/atlassian-mcp-oauth.json. The symlink routes the write
	// through to the real host path.
	script := "printf '%s' " + shQuote(sentinel) + " > " +
		shQuote(filepath.Join(stagingHome, ".pi", "agent", "atlassian-mcp-oauth.json"))
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c", script)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("write to $HOME/.pi/agent/atlassian-mcp-oauth.json inside sandbox failed.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s\nStagingTokenPath: %s",
			runErr, string(out), testProfilePath, stagingTokenPath)
	}

	// Assert the sentinel bytes are readable from the real host path.
	got, readErr := os.ReadFile(hostTokenPath)
	if readErr != nil {
		t.Fatalf("cannot read host token file %s after sandbox write: %v", hostTokenPath, readErr)
	}
	if string(got) != sentinel {
		t.Errorf("host token content = %q, want %q\n"+
			"(write inside sandbox-exec did not reach the host path via symlink)",
			string(got), sentinel)
	}
}

// TestSandboxExecPIAtlassianOAuth_WriteThroughBroken is the paired negative
// test. It removes the symlink at <stagingHome>/.pi/agent/atlassian-mcp-oauth.json
// (breaking the write-through) and asserts that the same write either fails or
// does NOT appear at the real host path. This proves the positive test is not
// green by accident — the symlink is the specific mechanism that routes writes
// from the staging HOME to the host.
func TestSandboxExecPIAtlassianOAuth_WriteThroughBroken(t *testing.T) {
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

	// Stash any existing token file so we start clean.
	hostTokenPath := filepath.Join(piAgentDir, "atlassian-mcp-oauth.json")
	stashPath := hostTokenPath + ".integ-pi-atlassian-oauth-neg-stash"
	stashed := false
	if _, statErr := os.Lstat(hostTokenPath); statErr == nil {
		if renErr := os.Rename(hostTokenPath, stashPath); renErr == nil {
			stashed = true
			t.Cleanup(func() {
				_ = os.Remove(hostTokenPath)
				_ = os.Rename(stashPath, hostTokenPath)
			})
		}
	}
	if !stashed {
		t.Cleanup(func() { _ = os.Remove(hostTokenPath) })
	}

	m := newPIOAuthPersistenceManager(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// Break the write-through: remove the symlink from the staging HOME.
	// The directory entry <stagingHome>/.pi/agent/atlassian-mcp-oauth.json
	// becomes absent; the sandbox write will either fail or create a file
	// inside the ephemeral staging dir rather than at the host path.
	stagingTokenPath := filepath.Join(stagingHome, ".pi", "agent", "atlassian-mcp-oauth.json")
	if err := os.Remove(stagingTokenPath); err != nil {
		t.Fatalf("remove staging token symlink: %v", err)
	}

	prepared, _ := preparePositiveProfile(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	const sentinel = `{"access_token":"integ-pi-atlassian-oauth-negative-sentinel"}`

	// Allow the sandbox write to fail OR succeed into the staging dir — either
	// outcome is acceptable for the negative test; what matters is that the
	// host path does NOT contain the sentinel.
	script := "printf '%s' " + shQuote(sentinel) + " > " +
		shQuote(filepath.Join(stagingHome, ".pi", "agent", "atlassian-mcp-oauth.json"))
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c", script)
	_, _ = cmd.CombinedOutput() // ignore exit code — the sandbox may deny or permit the write

	// The host token file must NOT contain the negative sentinel: without the
	// symlink, the sandbox write cannot reach the host path.
	got, readErr := os.ReadFile(hostTokenPath)
	if readErr == nil && string(got) == sentinel {
		t.Errorf("host token contains negative sentinel %q even though the staging symlink was removed.\n"+
			"The positive test is not catching the write-through — investigate.\n"+
			"Host token path: %s", sentinel, hostTokenPath)
	} else {
		t.Logf("ka pai — write did NOT reach host path without staging symlink (host content: %q)", string(got))
	}
}
