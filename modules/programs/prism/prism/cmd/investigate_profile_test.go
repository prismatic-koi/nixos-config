package cmd

// investigate_profile_test.go — issue #2097 regression tests for the
// investigate-spawn profile-inheritance path.
//
// `prism investigate` is the second of the two child-spawn front doors
// fixed in #2097 (the first being `prism review`). Before #2097 the
// spawned investigate session resolved its profile via
// ResolveActiveProfile(pf, "") and silently picked up the host default,
// regardless of the invoker's `--profile` choice.
//
// buildInvestigateSpawnOpts is the testable seam extracted from
// spawnInvestigateSession in #2097 — it returns the SpawnOpts that
// would be handed to session.SpawnSession, without actually spawning
// (no tmux, no sidecar, no port allocation). Asserting on the
// SpawnOpts.ProfileName field there is equivalent to asserting on the
// spawn_inputs.profile_name row that SpawnSession would write
// (session.SpawnInputsFromOpts maps the field directly — covered by
// cmd/spawn_inputs_writer_test.go).
//
// The tests below pin:
//
//   - AC #6 positive — modern invoker → child SpawnOpts.ProfileName
//     equals the invoker's spawn_inputs.profile_name.
//   - AC #7 abtest — two invokers with distinct profile_name values,
//     mirroring an `--abtest` pair, each produce a child whose
//     ProfileName matches the leg's profile (no cross-bleed).
//   - AC #8 negative — invoker with no spawn_inputs row falls through
//     to state-file > nix-default. No regression.
//
// Test-suite isolation contract (AGENTS.md, issue #1608):
//   - sidecartest.NewIsolated redirects $XDG_STATE_HOME to a t.TempDir()
//     and sets PRISM_TEST_MODE_RESTRICT_HOSTAPI so no host bus / DB /
//     tmux state is touched.
//   - SetTestDBPath is used to point cmd.openDB at the sidecartest-owned
//     DB (the investigate code path calls openDB internally; the seam
//     accepts an external DB but the test verifies the full flow).
//   - Session names use the "prism-test@" prefix.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// seedInvokerSession creates the rows buildInvestigateSpawnOpts needs:
// a sessions row (for spawn_inputs FK), a spawn_inputs row with the
// supplied profile_name (or NULL if empty), and an agent_status row
// (CurrentStatus dependency in the builder).
//
// abtestPairID is optional — pass "" to leave NULL.
func seedInvokerSession(t *testing.T, d *db.DB, sessionName, profileName, abtestPairID string) {
	t.Helper()
	iid := uuid.New().String()
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: sessionName,
		Repo:        "prism-test",
		Worktree:    "/tmp/" + sessionName,
		Harness:     "pi",
	}); err != nil {
		t.Fatalf("InsertSession %q: %v", sessionName, err)
	}
	si := db.SpawnInputs{InstanceID: iid}
	if profileName != "" {
		p := profileName
		si.ProfileName = &p
	}
	if abtestPairID != "" {
		pair := abtestPairID
		si.AbtestPairID = &pair
	}
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs %q: %v", sessionName, err)
	}
	// agent_status row — buildInvestigateSpawnOpts reads CurrentStatus for
	// the invoker's repo / worktree / isolation_mode. The state literal here
	// matches the kept-active states used elsewhere in the cmd-side tests.
	if err := d.UpsertStatusSeedRootAgentName(
		sessionName,
		"prism-test",
		"/tmp/"+sessionName,
		"idle",
		nil, nil,
		"worker",
		"pi",
		"bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName %q: %v", sessionName, err)
	}
}

// writeProfilesJSON drops a minimal profiles.json under
// $XDG_CONFIG_HOME/prism/ so config.LoadProfiles (called from
// buildInvestigateSpawnOpts) returns a non-nil ProfilesFile with the
// supplied default + named profiles.
func writeProfilesJSON(t *testing.T, configHome, defaultName string, profileNames ...string) {
	t.Helper()
	dir := filepath.Join(configHome, "prism")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir prism config dir: %v", err)
	}
	entries := make(map[string]config.ProfileEntry, len(profileNames))
	for _, n := range profileNames {
		entries[n] = config.ProfileEntry{
			"investigate": config.RoleSlot{Provider: "anthropic", Model: n + "/model-investigate", Thinking: "medium"},
			"worker":      config.RoleSlot{Provider: "anthropic", Model: n + "/model-worker", Thinking: "medium"},
		}
	}
	pf := config.ProfilesFile{Default: defaultName, Profiles: entries}
	b, err := json.Marshal(&pf)
	if err != nil {
		t.Fatalf("marshal profiles: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profiles.json"), b, 0o600); err != nil {
		t.Fatalf("write profiles.json: %v", err)
	}
}

// isolateForInvestigateBuilder combines sidecartest's $XDG_STATE_HOME
// + isolated DB with a fresh $XDG_CONFIG_HOME so config.LoadProfiles
// reads our profiles.json fixture. The returned configHome is where
// the test should drop its profiles.json. The DB is returned for
// seeding invoker rows.
func isolateForInvestigateBuilder(t *testing.T) (configHome string, testDB *db.DB) {
	t.Helper()
	bus := sidecartest.NewIsolated(t, "")
	SetTestDBPath(bus.DB.Path())
	t.Cleanup(func() { SetTestDBPath("") })

	tmp := t.TempDir()
	configHome = filepath.Join(tmp, "config")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatalf("mkdir config home: %v", err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	// XDG_STATE_HOME is owned by sidecartest \u2014 don't clobber it.
	return configHome, bus.DB
}

// writeStateFileForInvestigate writes the active-profile state file
// into the sidecartest-owned $XDG_STATE_HOME. Used to exercise the
// state-file rung of the precedence chain.
func writeStateFileForInvestigate(t *testing.T, profileName string) {
	t.Helper()
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		t.Fatalf("writeStateFileForInvestigate: XDG_STATE_HOME unset")
	}
	dir := filepath.Join(stateHome, "prism")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "active-profile"), []byte(profileName+"\n"), 0o644); err != nil {
		t.Fatalf("write active-profile state file: %v", err)
	}
}

// TestInvestigateFanout_ProfileInheritedFromModernInvoker is the AC #6
// positive: invoker has spawn_inputs.profile_name=X → the child's
// SpawnOpts.ProfileName = X. SpawnSession (via SpawnInputsFromOpts)
// then writes profile_name=X on the child's audit row.
func TestInvestigateFanout_ProfileInheritedFromModernInvoker(t *testing.T) {
	configHome, d := isolateForInvestigateBuilder(t)
	const invoker = "prism-test@worker-investigate-modern"
	const invokerProfile = "anthropic-opus"
	seedInvokerSession(t, d, invoker, invokerProfile, "")
	// State-file points at a competing profile to prove invoker wins.
	writeStateFileForInvestigate(t, "state-file-competing")
	writeProfilesJSON(t, configHome, "nix-default",
		invokerProfile, "state-file-competing", "nix-default")

	opts, _, _, err := buildInvestigateSpawnOpts(d, invoker, "trace ssh auth", "")
	if err != nil {
		t.Fatalf("buildInvestigateSpawnOpts: %v", err)
	}
	if opts.ProfileName != invokerProfile {
		t.Errorf("SpawnOpts.ProfileName = %q, want %q (modern invoker must inherit its own profile)",
			opts.ProfileName, invokerProfile)
	}
	// Defence-in-depth: the child should also not leak to the competing
	// state-file value or to the nix-default.
	if opts.ProfileName == "state-file-competing" {
		t.Errorf("investigate child leaked to state-file profile \u2014 #2097 regression")
	}
	if opts.ProfileName == "nix-default" {
		t.Errorf("investigate child leaked to nix-default \u2014 #2097 regression")
	}
}

// TestInvestigateFanout_AbtestLegsResolveIndependently is the AC #7
// shape adapted for investigate: two invokers sharing an
// abtest_pair_id but each carrying its own profile_name. Each
// invoker's investigate-child must carry its own profile, never the
// sibling's.
func TestInvestigateFanout_AbtestLegsResolveIndependently(t *testing.T) {
	configHome, d := isolateForInvestigateBuilder(t)
	const pairID = "test-abtest-pair-2097-investigate"
	writeStateFileForInvestigate(t, "state-file-must-lose")
	writeProfilesJSON(t, configHome, "nix-default",
		"abtest-leg-a", "abtest-leg-b", "state-file-must-lose", "nix-default")

	legs := []struct {
		sessionName, profile string
	}{
		{"prism-test@worker-investigate-leg-a", "abtest-leg-a"},
		{"prism-test@worker-investigate-leg-b", "abtest-leg-b"},
	}
	for _, leg := range legs {
		seedInvokerSession(t, d, leg.sessionName, leg.profile, pairID)
	}

	for _, leg := range legs {
		t.Run(leg.profile, func(t *testing.T) {
			opts, _, _, err := buildInvestigateSpawnOpts(d, leg.sessionName, "leg query "+leg.profile, "")
			if err != nil {
				t.Fatalf("buildInvestigateSpawnOpts: %v", err)
			}
			if opts.ProfileName != leg.profile {
				t.Errorf("leg %q: SpawnOpts.ProfileName = %q, want %q",
					leg.profile, opts.ProfileName, leg.profile)
			}
			if opts.ProfileName == "state-file-must-lose" || opts.ProfileName == "nix-default" {
				t.Errorf("leg %q leaked to state-file / nix-default \u2014 #2097 regression",
					leg.profile)
			}
		})
	}
}

// TestInvestigateFanout_LegacyInvokerFallsThroughToStateFile is the
// AC #8 negative: an invoker with no spawn_inputs row (pre-#2090) →
// the child falls through to the state-file value. No regression.
//
// Note: the invoker still needs an agent_status row (the
// CurrentStatus check is unrelated to spawn_inputs) so we seed that
// separately without going through seedInvokerSession.
func TestInvestigateFanout_LegacyInvokerFallsThroughToStateFile(t *testing.T) {
	configHome, d := isolateForInvestigateBuilder(t)
	const invoker = "prism-test@worker-investigate-legacy"
	if err := d.UpsertStatusSeedRootAgentName(
		invoker, "prism-test", "/tmp/"+invoker,
		"idle", nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("seed legacy invoker status: %v", err)
	}
	// No InsertSession / InsertSpawnInputs \u2014 legacy invoker.

	writeStateFileForInvestigate(t, "state-file-legacy-default")
	writeProfilesJSON(t, configHome, "nix-default",
		"state-file-legacy-default", "nix-default")

	opts, _, _, err := buildInvestigateSpawnOpts(d, invoker, "legacy invoker query", "")
	if err != nil {
		t.Fatalf("buildInvestigateSpawnOpts: %v", err)
	}
	if opts.ProfileName != "state-file-legacy-default" {
		t.Errorf("SpawnOpts.ProfileName = %q, want \"state-file-legacy-default\" (legacy invoker must fall through to state file)",
			opts.ProfileName)
	}
}

// TestInvestigateFanout_LegacyInvokerFallsThroughToNixDefault completes
// the AC #8 chain: legacy invoker + no state-file → resolution lands
// on pf.Default. This is the path every pre-#2097 investigate ran on
// for users who had not invoked `prism profile set`.
func TestInvestigateFanout_LegacyInvokerFallsThroughToNixDefault(t *testing.T) {
	configHome, d := isolateForInvestigateBuilder(t)
	const invoker = "prism-test@worker-investigate-fully-legacy"
	if err := d.UpsertStatusSeedRootAgentName(
		invoker, "prism-test", "/tmp/"+invoker,
		"idle", nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("seed legacy invoker status: %v", err)
	}
	// No InsertSession / InsertSpawnInputs, no state-file write.
	writeProfilesJSON(t, configHome, "nix-default", "nix-default")

	opts, _, _, err := buildInvestigateSpawnOpts(d, invoker, "fully legacy query", "")
	if err != nil {
		t.Fatalf("buildInvestigateSpawnOpts: %v", err)
	}
	if opts.ProfileName != "nix-default" {
		t.Errorf("SpawnOpts.ProfileName = %q, want \"nix-default\" (fully legacy invoker must fall through to nix-default)",
			opts.ProfileName)
	}
}
