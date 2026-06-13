//go:build darwin

package integration_test

// sandbox_exec_pi_atlassian_oauth_darwin_test.go — integration tests for the
// PI Atlassian MCP OAuth token capability at its REAL host path (Step 3d of
// #2132, issue #2245; original staging mechanism: issue #1597).
//
// Background:
//
//	The Atlassian MCP extension stores OAuth tokens at
//	homedir()/.pi/agent/atlassian-mcp-oauth.json. A former staging step
//	touch-and-symlinked the file into the per-session staging HOME; #2245
//	removed it (and Step 5 of #2132 deleted the staging HOME wholesale).
//	The capability collapses into the pi-gated RW (subpath ~/.pi/agent)
//	grant (sandbox_exec.go section 6a): reads and writes of the token file
//	at the REAL host path succeed in-sandbox.
//
// These tests verify:
//
//  1. Positive: a bash command running inside sandbox-exec (with the
//     production SBPL profile for Harness == "pi") writes JSON bytes
//     directly to the real ~/.pi/agent/atlassian-mcp-oauth.json and reads
//     them back; the bytes are visible on the host after sandbox-exec
//     exits.
//
//  2. Negative (whole-block strip, per the #2243 lesson): removing the
//     entire section-6a allow block makes the same write fail and nothing
//     appears at the host path — proving the 6a grant is the load-bearing
//     capability for the token file, not some broader rule.
//
// Per docs/sandbox-exec-testing.md (issue #1192).

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
// session with a BareRoot (required for the BareRoot-ancestor allow block,
// which grants the metadata traversal of $HOME that real-path access needs).
func newPIOAuthPersistenceManager(t *testing.T) *container.Manager {
	t.Helper()
	instanceID := "integ-sbx-pi-atlassian-oauth-" + strings.ReplaceAll(t.Name(), "/", "-")
	cfg := container.Config{
		SessionName: "integ-sandbox-exec-pi-atlassian-oauth-test",
		InstanceID:  instanceID,
		Worktree:    t.TempDir(),
		Harness:     "pi",
		BareRoot:    t.TempDir(), // needed for the BareRoot-ancestor allow block
		// Required since #1960 — see newProfileManager
		// (sandbox_exec_helpers_darwin_test.go).
		GitUserName:  "test-user",
		GitUserEmail: "test@example.com",
	}
	return container.New(cfg)
}

// stashHostAtlassianToken moves any existing real
// ~/.pi/agent/atlassian-mcp-oauth.json out of the way for the duration of
// the test and registers a cleanup that restores it. Returns the host token
// path. The host ~/.pi/agent dir is created when absent (and that creation
// is NOT cleaned up — it matches what EnsurePIAgentConfigDir does at spawn).
func stashHostAtlassianToken(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	piAgentDir := filepath.Join(home, ".pi", "agent")
	if mkErr := os.MkdirAll(piAgentDir, 0o700); mkErr != nil {
		t.Skipf("cannot create ~/.pi/agent for test: %v", mkErr)
	}
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
		} else {
			t.Skipf("cannot stash existing host token file: %v", renErr)
		}
	}
	if !stashed {
		t.Cleanup(func() { _ = os.Remove(hostTokenPath) })
	}
	return hostTokenPath
}

// piAgentSubpathBlock returns the exact section-6a allow block emitted by
// generateProfile for the real ~/.pi/agent dir. Used for the presence
// assertion in the positive test and the whole-block strip in the negative.
func piAgentSubpathBlock(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	piAgentDir := filepath.Join(home, ".pi", "agent")
	return "(allow file-read* file-write* file-test-existence file-read-metadata\n" +
		"  (subpath " + sbplQuoteForTest(piAgentDir) + "))\n"
}

// TestSandboxExecPIAtlassianOAuth_RealPathReadWrite is the positive
// integration test for Step 3d of #2132: the oauth token file is read-write
// in-sandbox at its REAL host path via the section-6a (subpath ~/.pi/agent)
// grant, with no staging symlink involved.
func TestSandboxExecPIAtlassianOAuth_RealPathReadWrite(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	hostTokenPath := stashHostAtlassianToken(t)

	m := newPIOAuthPersistenceManager(t)

	prepared, _ := preparePositiveProfile(t, m)

	// The production profile must carry the section-6a block — the sole
	// capability for the token path.
	block := piAgentSubpathBlock(t)
	if !strings.Contains(prepared.content, block) {
		t.Fatalf("generated profile does not contain the section-6a ~/.pi/agent RW block:\n%s\nProfile:\n%s", block, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Write JSON to the REAL host token path from inside the sandbox, then
	// read it back in the same invocation.
	const tokenJSON = `{"access_token":"prism-2245-3d-real-path-sentinel"}`
	script := "printf '%s' " + shQuote(tokenJSON) + " > " + shQuote(hostTokenPath) +
		" && cat " + shQuote(hostTokenPath)
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c", script)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("in-sandbox RW round-trip of the oauth token at its real path failed.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s\nTarget: %s",
			runErr, string(out), testProfilePath, hostTokenPath)
	}
	if !strings.Contains(string(out), "prism-2245-3d-real-path-sentinel") {
		t.Errorf("in-sandbox read did not return the written token bytes.\nGot: %s", string(out))
	}

	// The bytes must be visible on the host after sandbox-exec exits.
	hostBytes, readErr := os.ReadFile(hostTokenPath)
	if readErr != nil {
		t.Fatalf("host-side read of the token file after sandbox exit: %v", readErr)
	}
	if string(hostBytes) != tokenJSON {
		t.Errorf("host token content = %q, want %q", string(hostBytes), tokenJSON)
	}
}

// TestSandboxExecPIAtlassianOAuth_DeniedWithoutPiAgentGrant is the paired
// negative test. It strips the ENTIRE section-6a allow block (whole-block
// strip per the #2243 lesson — removing only the (subpath ...) line would
// leave a filter-less allow-everything clause) and asserts the same write
// fails and nothing lands at the host path — proving the 6a grant is
// load-bearing for the token file.
func TestSandboxExecPIAtlassianOAuth_DeniedWithoutPiAgentGrant(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	hostTokenPath := stashHostAtlassianToken(t)

	m := newPIOAuthPersistenceManager(t)

	block := piAgentSubpathBlock(t)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.Replace(p, block, "", 1)
	})

	const tokenJSON = `{"access_token":"prism-2245-3d-denied-sentinel"}`
	script := "printf '%s' " + shQuote(tokenJSON) + " > " + shQuote(hostTokenPath)
	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c", script)
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("oauth token write succeeded WITHOUT the section-6a ~/.pi/agent block.\n"+
			"The 6a grant is not the load-bearing rule — investigate.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — oauth token write correctly denied without the 6a block (exit: %v)", runErr)
	}

	// Nothing may have landed at the host path.
	if hostBytes, readErr := os.ReadFile(hostTokenPath); readErr == nil && string(hostBytes) == tokenJSON {
		t.Errorf("token bytes appeared at the host path despite the stripped grant — the deny is not effective")
	}
}
