package profile

// inherit_test.go — issue #2097 unit tests for the child-spawn
// profile-inheritance helper.
//
// InheritFromParent composes SpawnTimeForSession with
// config.ResolveActiveProfile to give callers a single entry point that
// honours the same precedence chain as the worker-layer #2092 fix:
//
//   1. Parent's spawn_inputs.profile_name (highest).
//   2. Runtime state file ($XDG_STATE_HOME/prism/active-profile).
//   3. pf.Default — the nix-configured default (lowest).
//
// The tests below pin each rung of that ladder against an isolated DB +
// state-file tempdir owned by sidecartest, plus a small per-test
// ProfilesFile fixture.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// makeProfiles is a tiny fixture builder: it returns a ProfilesFile with
// `defaultName` as the nix-default and the supplied profile names each
// declaring a single "worker" slot. The test only cares about the name
// resolution here, so the slot bodies are intentionally minimal.
func makeProfiles(defaultName string, names ...string) *config.ProfilesFile {
	entries := make(map[string]config.ProfileEntry, len(names))
	for _, n := range names {
		entries[n] = config.ProfileEntry{
			"worker": config.RoleSlot{Provider: "anthropic", Model: n + "/model", Thinking: "medium"},
		}
	}
	return &config.ProfilesFile{
		Default:  defaultName,
		Profiles: entries,
	}
}

// writeStateFile drops the runtime active-profile state file into the
// sidecartest-owned $XDG_STATE_HOME so ResolveActiveProfile's middle
// rung (state-file) has something to read. sidecartest sets
// XDG_STATE_HOME on t.Setenv so this resolves to the test sandbox.
func writeStateFile(t *testing.T, profileName string) {
	t.Helper()
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		t.Fatalf("writeStateFile: XDG_STATE_HOME unset \u2014 sidecartest.NewIsolated must run first")
	}
	dir := filepath.Join(stateHome, "prism")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "active-profile"), []byte(profileName+"\n"), 0o644); err != nil {
		t.Fatalf("write active-profile state file: %v", err)
	}
}

// TestInheritFromParent_ParentSpawnProfileWinsOverState pins the AC #1
// positive: when the parent has spawn_inputs.profile_name=X and the
// state-file points to a different profile, the parent's X wins. This
// is the inverse of the pre-#2097 silent-drop: every review / investigate
// would have returned the state-file value instead.
func TestInheritFromParent_ParentSpawnProfileWinsOverState(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const parent = "prism-test@worker-parent-wins"
	const parentProfile = "parent-X"
	seedSessionWithSpawnInputs(t, bus.DB, parent, parentProfile)

	// Set the state file to a competing profile \u2014 it must lose.
	writeStateFile(t, "state-file-Y")
	pf := makeProfiles("nix-default", parentProfile, "state-file-Y")

	got, err := InheritFromParent(bus.DB, parent, pf)
	if err != nil {
		t.Fatalf("InheritFromParent: %v", err)
	}
	if got != parentProfile {
		t.Errorf("InheritFromParent = %q, want %q (parent spawn_inputs.profile_name must beat state file)",
			got, parentProfile)
	}
}

// TestInheritFromParent_ParentSpawnProfileWinsOverNixDefault pins the
// same precedence one rung down: parent's profile_name beats the
// nix-default fallback (state file absent). This is the common case
// for users without a `prism profile set` state file.
func TestInheritFromParent_ParentSpawnProfileWinsOverNixDefault(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const parent = "prism-test@worker-parent-vs-nix-default"
	const parentProfile = "parent-modern"
	seedSessionWithSpawnInputs(t, bus.DB, parent, parentProfile)

	// No state file \u2014 the only fallback is pf.Default.
	pf := makeProfiles("nix-default", parentProfile, "nix-default")

	got, err := InheritFromParent(bus.DB, parent, pf)
	if err != nil {
		t.Fatalf("InheritFromParent: %v", err)
	}
	if got != parentProfile {
		t.Errorf("InheritFromParent = %q, want %q", got, parentProfile)
	}
}

// TestInheritFromParent_LegacyParentFallsThroughToStateFile is the
// AC #8 negative: a parent with no spawn_inputs row (pre-#2090
// session) falls through to the state-file value when one is set.
// This preserves restart semantics for legacy sessions.
func TestInheritFromParent_LegacyParentFallsThroughToStateFile(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const parent = "prism-test@worker-legacy"
	// Parent has no sessions row \u2014 SpawnTimeForSession returns "".
	writeStateFile(t, "state-file-default")
	pf := makeProfiles("nix-default", "state-file-default", "nix-default")

	got, err := InheritFromParent(bus.DB, parent, pf)
	if err != nil {
		t.Fatalf("InheritFromParent: %v", err)
	}
	if got != "state-file-default" {
		t.Errorf("InheritFromParent = %q, want \"state-file-default\" (legacy parent must fall through to state file)", got)
	}
}

// TestInheritFromParent_LegacyParentFallsThroughToNixDefault completes
// the AC #8 chain: with no spawn_inputs row AND no state file, the
// nix-default wins. This is the "no per-session profile, no per-user
// override" path and is what every pre-#2097 review / investigate ran
// on.
func TestInheritFromParent_LegacyParentFallsThroughToNixDefault(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const parent = "prism-test@worker-legacy-no-state"
	// No sessions row, no state file.
	pf := makeProfiles("nix-default", "nix-default")

	got, err := InheritFromParent(bus.DB, parent, pf)
	if err != nil {
		t.Fatalf("InheritFromParent: %v", err)
	}
	if got != "nix-default" {
		t.Errorf("InheritFromParent = %q, want \"nix-default\"", got)
	}
}

// TestInheritFromParent_NullProfileNameFallsThrough pins the spawn_inputs
// row with NULL profile_name shape \u2014 the row exists but the column is
// empty. This is a path the host-mode spawn front door currently writes,
// so we must not regress its resolution.
func TestInheritFromParent_NullProfileNameFallsThrough(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const parent = "prism-test@worker-null-column"
	seedSessionWithSpawnInputs(t, bus.DB, parent, "" /* NULL profile_name */)
	writeStateFile(t, "state-file-fallback")
	pf := makeProfiles("nix-default", "state-file-fallback", "nix-default")

	got, err := InheritFromParent(bus.DB, parent, pf)
	if err != nil {
		t.Fatalf("InheritFromParent: %v", err)
	}
	if got != "state-file-fallback" {
		t.Errorf("InheritFromParent = %q, want \"state-file-fallback\" (NULL profile_name must fall through)", got)
	}
}

// TestInheritFromParent_AbtestPairResolvesPerLeg is the AC #7 abtest
// shape: two parents with the same abtest_pair_id but distinct
// profile_name values each resolve to their own profile. This is the
// end-to-end fix for the live abtest reproducer in the issue body
// (`zero-output-classify-v2-anthropic-opus-max` vs the `4-8` leg) \u2014
// the per-leg variance must survive the child-spawn step.
func TestInheritFromParent_AbtestPairResolvesPerLeg(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	// Set a state-file value too \u2014 if the parent's profile_name is ever
	// dropped, the state file would be the silent next rung. Asserting
	// the leg-specific profile beats the state-file value too is a
	// stronger guard than just beating the nix-default.
	writeStateFile(t, "state-file-X")
	pf := makeProfiles("nix-default",
		"abtest-leg-a", "abtest-leg-b", "state-file-X", "nix-default")

	legs := []struct {
		sessionName, profile string
	}{
		{"prism-test@worker-abtest-leg-a", "abtest-leg-a"},
		{"prism-test@worker-abtest-leg-b", "abtest-leg-b"},
	}
	for _, leg := range legs {
		seedSessionWithSpawnInputs(t, bus.DB, leg.sessionName, leg.profile)
	}

	for _, leg := range legs {
		t.Run(leg.profile, func(t *testing.T) {
			got, err := InheritFromParent(bus.DB, leg.sessionName, pf)
			if err != nil {
				t.Fatalf("InheritFromParent: %v", err)
			}
			if got != leg.profile {
				t.Errorf("InheritFromParent(%q) = %q, want %q (per-leg profile must survive child-spawn)",
					leg.sessionName, got, leg.profile)
			}
			if got == "state-file-X" || got == "nix-default" {
				t.Errorf("InheritFromParent(%q) leaked state/nix-default \u2014 issue #2097 regression",
					leg.sessionName)
			}
		})
	}
}

// TestInheritFromParent_NilProfilesFile pins the nil-pf tolerance:
// when no profiles.json is loaded, the helper still works \u2014 it
// returns the parent's spawn-time profile verbatim. This is the
// host-mode investigate path that may run without profiles.json
// present on disk; ResolveActiveProfile's nil-tolerant behaviour
// must propagate through unchanged.
func TestInheritFromParent_NilProfilesFile(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const parent = "prism-test@worker-nil-pf"
	const parentProfile = "raw-spawn-time"
	seedSessionWithSpawnInputs(t, bus.DB, parent, parentProfile)

	got, err := InheritFromParent(bus.DB, parent, nil)
	if err != nil {
		t.Fatalf("InheritFromParent: %v", err)
	}
	if got != parentProfile {
		t.Errorf("InheritFromParent(nil pf) = %q, want %q (nil pf must propagate the parent's profile)",
			got, parentProfile)
	}
}
