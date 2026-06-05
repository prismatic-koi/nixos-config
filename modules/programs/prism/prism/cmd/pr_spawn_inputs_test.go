package cmd

// Tests for buildPRSpawnInputs — the `prism pr` flag-to-column mapping for
// the spawn_inputs audit row. Issue #2105.
//
// `prism pr` does not flow through SpawnSession (it uses ensureAndSwitch →
// session.Create), so it has its own writer that mirrors
// internal/session/spawn.go::spawnInputsFromOpts. These tests pin the
// audit-vs-effective split for isolation_mode / isolation_flag on the PR
// path so the two writers stay in sync.
//
// Test-suite isolation: buildPRSpawnInputs is a pure builder — no DB / no
// tmux / no env reads — so it is safe to call without sidecartest.

import (
	"testing"
)

// TestBuildPRSpawnInputs_IsolationModeDefaultsWhenFlagOmitted mirrors
// TestSpawnInputsFromOpts_IsolationModeDefaultsWhenFlagOmitted for the
// `prism pr` writer. When --isolation is omitted (isolationFlag == ""),
// the writer must still populate isolation_mode with the resolved mode.
func TestBuildPRSpawnInputs_IsolationModeDefaultsWhenFlagOmitted(t *testing.T) {
	const resolved = "sandbox-exec"
	args := prSpawnInputsArgs{
		isolationFlag: "", // user did not pass --isolation
		isolationMode: resolved,
		promptText:    "do the mahi",
		promptSource:  "cli-positional",
	}
	si := buildPRSpawnInputs(args, "iid-123", "", "", 12345)

	// isolation_mode must be the resolved effective mode — the bug-fix
	// pivot. Without this, the renderer shows "—" for nearly every
	// session because users rarely pass --isolation explicitly.
	if si.IsolationMode == nil || *si.IsolationMode != resolved {
		t.Errorf("IsolationMode: got %v, want %q", si.IsolationMode, resolved)
	}
	// isolation_flag must stay NULL because the raw flag was not passed —
	// that is the audit trail.
	if si.IsolationFlag != nil {
		t.Errorf("IsolationFlag: got %q, want nil (raw flag was not passed)", *si.IsolationFlag)
	}
}

// TestBuildPRSpawnInputs_IsolationModeMatchesFlagWhenPassed verifies
// that when --isolation is explicitly passed on `prism pr`, BOTH
// columns are populated with that value.
func TestBuildPRSpawnInputs_IsolationModeMatchesFlagWhenPassed(t *testing.T) {
	const mode = "bwrap"
	args := prSpawnInputsArgs{
		isolationFlag: mode, // user passed --isolation bwrap
		isolationMode: mode, // resolver agrees
		promptText:    "do the mahi",
		promptSource:  "cli-positional",
	}
	si := buildPRSpawnInputs(args, "iid-456", "", "", 12345)

	if si.IsolationMode == nil || *si.IsolationMode != mode {
		t.Errorf("IsolationMode: got %v, want %q", si.IsolationMode, mode)
	}
	if si.IsolationFlag == nil || *si.IsolationFlag != mode {
		t.Errorf("IsolationFlag: got %v, want %q", si.IsolationFlag, mode)
	}
}
