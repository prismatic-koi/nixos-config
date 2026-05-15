//go:build darwin

package iris

// credential_broker_mount_darwin_test.go — Darwin-only SBPL audit for the
// D-7 bash sandbox.  Asserts that the generated SBPL profile never grants
// access to any pi-process credential path and never references any
// forbidden LLM API key name.

import (
	"strings"
	"testing"
)

// TestBashSBPLProfile_ForbiddenPathsExhaustive walks every forbidden
// pi-process credential path and confirms it is absent from the generated
// SBPL profile. The profile is built fresh per call so this is a pure
// generator check — no sandbox-exec invocation required.
func TestBashSBPLProfile_ForbiddenPathsExhaustive(t *testing.T) {
	home := t.TempDir()
	worktree := t.TempDir()
	tmpDir := t.TempDir()

	profile := GenerateBashSBPLProfile(home, worktree, tmpDir, "", "", "", false)

	for _, path := range forbiddenPiPaths() {
		if strings.Contains(profile, path) {
			t.Errorf("GenerateBashSBPLProfile output references forbidden pi credential path %q\n%s",
				path, profile)
		}
	}
}

// TestBashSBPLProfile_NoLLMKeyNames asserts that no LLM API key name appears
// anywhere in the generated SBPL profile (defence in depth — the profile
// should never mention key names since SBPL only references file paths and
// mach-port names).
func TestBashSBPLProfile_NoLLMKeyNames(t *testing.T) {
	home := t.TempDir()
	worktree := t.TempDir()
	tmpDir := t.TempDir()

	profile := GenerateBashSBPLProfile(home, worktree, tmpDir, "", "", "", false)

	for _, k := range forbiddenEnvKeys() {
		if strings.Contains(profile, k) {
			t.Errorf("GenerateBashSBPLProfile output mentions forbidden LLM key %q\n%s",
				k, profile)
		}
	}
}
