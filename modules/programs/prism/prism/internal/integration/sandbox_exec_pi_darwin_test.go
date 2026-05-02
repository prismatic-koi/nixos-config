//go:build darwin

package integration_test

// sandbox_exec_pi_darwin_test.go — integration tests for PI harness paths in
// the SBPL profile (issue #1213, P2.DARWIN).
//
// These tests verify that:
//  1. The per-session run directory (which hosts both hostapi.sock and the PI
//     system-prompt temp file) is accessible inside the sandbox — the existing
//     host-API socket dir allow rule covers both (no new SBPL rule needed).
//  2. The PI extension directory (a Nix store path) is readable inside the
//     sandbox — the existing /nix subpath allow covers it (no new rule needed).
//
// The tests confirm that these paths do NOT need new SBPL rules by verifying
// the existing rules already grant access (issue #1213: "no new SBPL rules
// expected"). Each positive test is paired with a negative test that removes
// the covering rule and asserts failure — proving the positive test is not a
// no-op.
//
// See docs/sandbox-exec-testing.md for the testing convention these tests
// support (issue #1192).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// newPIProfileManager creates a Manager configured as it would be for a PI
// session on Darwin: the HostAPISockPath is set so the per-session run dir
// (which also contains the PI system-prompt file) is included in the SBPL
// profile's allow rules.
func newPIProfileManager(t *testing.T, sockPath string) *container.Manager {
	t.Helper()
	instanceID := "integ-sbx-pi-" + strings.ReplaceAll(t.Name(), "/", "-")
	cfg := container.Config{
		SessionName:     "integ-sandbox-exec-pi-test",
		InstanceID:      instanceID,
		Worktree:        t.TempDir(),
		HostAPISockPath: sockPath,
	}
	return container.New(cfg)
}

// TestSandboxExecPI_SystemPromptFileReadable verifies the positive path for
// PI's system-prompt file: the per-session run directory is accessible inside
// the sandbox via the existing host-API socket dir rule. This covers issue
// #1213 AC: "No new SBPL rules are needed (confirm and document)".
//
// The test creates a temp file that stands in for the system-prompt file
// (co-located with the socket dir), then runs sandbox-exec to read it with
// the Nix-built bash. Asserts exit 0.
func TestSandboxExecPI_SystemPromptFileReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	// Create a temp directory to act as the per-session run dir. The PI
	// system-prompt file and hostapi.sock are co-located here.
	runDir := t.TempDir()
	sockPath := filepath.Join(runDir, "hostapi.sock")
	systemPromptPath := filepath.Join(runDir, "system-prompt.md")

	// Write a stand-in system-prompt file.
	if err := os.WriteFile(systemPromptPath, []byte("# test system prompt"), 0o600); err != nil {
		t.Fatalf("write system-prompt stand-in: %v", err)
	}

	m := newPIProfileManager(t, sockPath)
	prepared, _ := preparePositiveProfile(t, m)

	// Verify the socket dir rule is present in the generated profile.
	sockDirRule := fmt.Sprintf("(subpath %q)", runDir)
	if !strings.Contains(prepared.content, sockDirRule) {
		// The profile uses SBPL double-quoted strings (quoteSBPL). Reconstruct
		// the expected rule using the actual SBPL quoting.
		quotedDir := `"` + strings.ReplaceAll(strings.ReplaceAll(runDir, `\`, `\\`), `"`, `\"`) + `"`
		sockDirRuleSBPL := "(subpath " + quotedDir + ")"
		if !strings.Contains(prepared.content, sockDirRuleSBPL) {
			t.Fatalf("generated profile does not contain a (subpath ...) rule for the run dir %q.\n"+
				"The host-API socket dir rule should cover the PI system-prompt path.\nProfile:\n%s",
				runDir, prepared.content)
		}
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Run: sandbox-exec -f <profile> bash -c 'cat <system-prompt>'
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		nixBash, "-c", "cat "+shQuote(systemPromptPath))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("sandbox-exec reading PI system-prompt file failed.\n"+
			"Expected the per-session run dir allow to cover %q.\n"+
			"Output: %s\nError: %v",
			systemPromptPath, out, runErr)
	}
}

// TestSandboxExecPI_SystemPromptFileDeniedWithoutRunDirRule verifies the
// negative path: when the per-session run dir rule is removed from the SBPL
// profile, reading the system-prompt file fails. This proves that the
// positive test is not a no-op — the run dir rule really is the covering rule.
func TestSandboxExecPI_SystemPromptFileDeniedWithoutRunDirRule(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	runDir := t.TempDir()
	sockPath := filepath.Join(runDir, "hostapi.sock")
	systemPromptPath := filepath.Join(runDir, "system-prompt.md")

	if err := os.WriteFile(systemPromptPath, []byte("# test system prompt"), 0o600); err != nil {
		t.Fatalf("write system-prompt stand-in: %v", err)
	}

	m := newPIProfileManager(t, sockPath)

	// Build SBPL quoting for runDir. sandbox_exec.go uses quoteSBPL which
	// double-quotes with backslash escaping. Replicate that here.
	quotedRunDir := `"` + strings.ReplaceAll(strings.ReplaceAll(runDir, `\`, `\\`), `"`, `\"`) + `"`

	mutatedProfilePath := withMutatedProfile(t, m, func(content string) string {
		// Remove the (subpath "<runDir>") rule that covers the run dir.
		return strings.Replace(content, "  (subpath "+quotedRunDir+")\n", "", 1)
	})

	cmd := exec.Command(sandboxExecPath, "-f", mutatedProfilePath,
		nixBash, "-c", "cat "+shQuote(systemPromptPath))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("sandbox-exec reading PI system-prompt unexpectedly succeeded with run dir rule removed.\n"+
			"Output: %s", out)
	}
}

// TestSandboxExecPI_NixExtensionDirReadable verifies that a Nix store path
// (standing in for the PI extension dir) is readable inside the sandbox via
// the existing (subpath "/nix") rule. This covers issue #1213 AC: "No new
// SBPL rules are needed" for the extension directory.
func TestSandboxExecPI_NixExtensionDirReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	// Use the resolved bash path as a stand-in for a file in the Nix store.
	nixBashDir := filepath.Dir(nixBash)

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)

	// Verify the /nix rule is present.
	const nixRule = `(subpath "/nix")`
	if !strings.Contains(prepared.content, nixRule) {
		t.Fatalf("generated profile is missing %q — the Nix store allow was not emitted.\nProfile:\n%s",
			nixRule, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Run: sandbox-exec -f <profile> bash -c 'ls <nixBashDir>'
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		nixBash, "-c", "ls "+shQuote(nixBashDir))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("sandbox-exec listing Nix store dir failed.\n"+
			"Expected (subpath \"/nix\") to cover PI extension dir at %q.\n"+
			"Output: %s\nError: %v",
			nixBashDir, out, runErr)
	}
}

// TestSandboxExecPI_NixExtensionDirDeniedWithoutNixRule verifies the negative
// path: removing the (subpath "/nix") rule from the profile denies reads from
// the Nix store. This proves the positive test is not a no-op.
func TestSandboxExecPI_NixExtensionDirDeniedWithoutNixRule(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	nixBashDir := filepath.Dir(nixBash)

	m := newProfileManager(t)

	mutatedProfilePath := withMutatedProfile(t, m, func(content string) string {
		// Remove the (subpath "/nix") line from the file-read* allow block.
		return strings.Replace(content, "  (subpath \"/nix\")\n", "", 1)
	})

	cmd := exec.Command(sandboxExecPath, "-f", mutatedProfilePath,
		nixBash, "-c", "ls "+shQuote(nixBashDir))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("sandbox-exec listing Nix store dir unexpectedly succeeded with /nix rule removed.\n"+
			"Output: %s", out)
	}
}
