//go:build linux

package iris

// credential_broker_mount_linux_test.go — Linux-only mount-layout audit for
// the D-7 bash sandbox. Asserts that bashToolMountsLinux NEVER includes any
// pi-process credential path or LLM API key env var, regardless of which
// optional credentials are present on the host.

import (
	"strings"
	"testing"
)

// TestBashToolMountsLinux_ForbiddenPathsExhaustive walks every forbidden
// pi-process credential path and confirms it is absent from the bwrap
// argument list. This is the exhaustive version of the existing
// TestBashToolMountsLinux_PiCredentialsAbsent test, made explicit per #1638.
func TestBashToolMountsLinux_ForbiddenPathsExhaustive(t *testing.T) {
	home := t.TempDir()
	worktree := t.TempDir()
	tmpDir := t.TempDir()

	mounts := bashToolMountsLinux(home, worktree, tmpDir, "", "", "", false)
	mountStr := strings.Join(mounts, " ")

	for _, path := range forbiddenPiPaths() {
		if strings.Contains(mountStr, path) {
			t.Errorf("bashToolMountsLinux contains forbidden pi credential path %q\n  full args: %v",
				path, mounts)
		}
	}
}

// TestBashToolMountsLinux_NoLLMKeyMounts asserts no forbidden LLM API key
// name appears as a --setenv arg in the bwrap invocation. This is a
// belt-and-braces complement to TestBashEnv_NoForbiddenKeys.
//
// Note: bashToolMountsLinux does not emit --setenv pairs (those are appended
// in runBashInSandbox after the mount args), so this test currently only
// checks that no LLM key appears anywhere in the mount layout (e.g. via a
// stray bind-mount of a credentials file).
func TestBashToolMountsLinux_NoLLMKeyMounts(t *testing.T) {
	home := t.TempDir()
	worktree := t.TempDir()
	tmpDir := t.TempDir()

	mounts := bashToolMountsLinux(home, worktree, tmpDir, "", "", "", false)
	mountStr := strings.Join(mounts, " ")

	for _, k := range forbiddenEnvKeys() {
		if strings.Contains(mountStr, k) {
			t.Errorf("bashToolMountsLinux args contain forbidden LLM key name %q\n  full args: %v",
				k, mounts)
		}
	}
}
