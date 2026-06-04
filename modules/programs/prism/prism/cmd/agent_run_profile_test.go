package cmd

// agent_run_profile_test.go — issue #2092 regression tests.
//
// Covers the DB-backed profile-resolution path that closes the
// `prism spawn --profile <X>` / `--abtest <A> <B>` silent-drop. The
// canonical post-#2090 source of "what profile did the user spawn this
// session with" is `spawn_inputs.profile_name`. populatePIConfig now reads
// that column and passes it as the highest-precedence input to
// ResolveActiveProfile, beating the state-file / nix-default fallback.
//
// The tests pin three behaviours:
//
//  1. Positive — a non-empty spawn_inputs.profile_name wins over the
//     active profile, so `prism spawn --profile <X>` results in the agent
//     running on profile <X>'s slot (AC #1).
//  2. Negative — when the spawn_inputs row is missing or
//     profile_name is NULL, populatePIConfig falls through to the
//     active-profile resolution unchanged (AC #4 / no-regression on
//     legacy rows and restart semantics).
//  3. --abtest — two sessions sharing an abtest_pair_id but recording
//     distinct profile_name values each resolve to their own slot,
//     proving the per-leg profile passthrough required by AC #2.
//
// Test-suite isolation contract (AGENTS.md, issue #1608):
//   - sidecartest.NewIsolated redirects $XDG_STATE_HOME to a t.TempDir()
//     and sets PRISM_TEST_MODE_RESTRICT_HOSTAPI so no host bus / DB / tmux
//     state is touched.
//   - The cmd-package DB is redirected via SetTestDBPath to the
//     sidecartest-owned DB so openDB() inside spawnTimeProfileForSession
//     resolves to the same database that the test seeds.
//   - Session names use the "prism-test@" prefix.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// profileSpec describes a single profile + worker slot to write into
// profiles.json. The tests below build small profile maps and pass them to
// writeMultiProfiles so each scenario can assert which profile's slot won.
type profileSpec struct {
	Model    string
	Thinking string
}

// writeMultiProfiles writes a profiles.json under $XDG_CONFIG_HOME/prism/
// with the supplied profiles, naming `defaultName` as the nix-default. Every
// profile defines a worker slot with provider="anthropic" and the supplied
// model / thinking.
func writeMultiProfiles(t *testing.T, configHome, defaultName string, profiles map[string]profileSpec) {
	t.Helper()
	prismDir := filepath.Join(configHome, "prism")
	if err := os.MkdirAll(prismDir, 0o700); err != nil {
		t.Fatalf("mkdir prism config dir: %v", err)
	}
	entries := make(map[string]config.ProfileEntry, len(profiles))
	for name, spec := range profiles {
		entries[name] = config.ProfileEntry{
			"worker": config.RoleSlot{
				Provider: "anthropic",
				Model:    spec.Model,
				Thinking: spec.Thinking,
			},
		}
	}
	pf := config.ProfilesFile{
		Default:  defaultName,
		Profiles: entries,
	}
	b, err := json.Marshal(&pf)
	if err != nil {
		t.Fatalf("marshal profiles: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prismDir, "profiles.json"), b, 0o600); err != nil {
		t.Fatalf("write profiles.json: %v", err)
	}
}

// isolateForProfileLookup combines the existing isolateForPopulatePIConfig
// host-isolation (HOME / XDG_CONFIG_HOME / PATH + fake pi binary) with a
// sidecartest.NewIsolated DB so that:
//
//   - populatePIConfig's call to config.LoadProfiles reads from the
//     test's profiles.json (under the configHome we set).
//   - populatePIConfig's call to EnsurePIAgentConfigDir creates the
//     ~/.pi/agent directory inside the test tempdir.
//   - populatePIConfig's call to exec.LookPath("pi") resolves to a fake
//     shim under PATH=tempdir/bin.
//   - spawnTimeProfileForSession's call to openDB resolves to the
//     sidecartest-owned DB (via SetTestDBPath) — this is where the tests
//     seed sessions + spawn_inputs rows below.
//
// The returned *db.DB is the seeded DB; the returned configHome is the
// XDG_CONFIG_HOME tempdir so tests can write profiles.json into it.
func isolateForProfileLookup(t *testing.T) (configHome string, testDB *db.DB) {
	t.Helper()
	// sidecartest.NewIsolated sets XDG_STATE_HOME to a tempdir and opens an
	// isolated DB under a separate private tempdir. We then point
	// cmd.openDB at the sidecartest DB via SetTestDBPath so that the
	// helper resolves to the same database the test seeds.
	bus := sidecartest.NewIsolated(t, "")
	SetTestDBPath(bus.DB.Path())
	t.Cleanup(func() { SetTestDBPath("") })

	// Host isolation: HOME / XDG_CONFIG_HOME / PATH / fake pi binary.
	// We do NOT clobber XDG_STATE_HOME — sidecartest already set it to its
	// own tempdir, which is also where ActiveProfile() reads the
	// state-file from. Letting both honour the same XDG_STATE_HOME keeps
	// the active-profile state file inside the test sandbox.
	tmp := t.TempDir()
	configHome = filepath.Join(tmp, "config")
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatalf("mkdir config home: %v", err)
	}
	_ = writeFakePiBinary(t, binDir)

	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	// Restrict PATH so exec.LookPath("pi") is deterministic.
	t.Setenv("PATH", binDir)

	return configHome, bus.DB
}

// seedSessionWithSpawnInputs inserts the minimal pair of rows
// (sessions, spawn_inputs) needed for spawnTimeProfileForSession to
// return the supplied profile name for the given session.
//
// When profileName is "", the spawn_inputs row is still written but with
// NULL profile_name — exercising the negative path where the row exists
// but the column is empty.
func seedSessionWithSpawnInputs(t *testing.T, d *db.DB, sessionName, profileName string) (instanceID string) {
	t.Helper()
	instanceID = uuid.New().String()
	if err := d.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Repo:        "prism-test",
		Worktree:    "/tmp/" + sessionName,
		Harness:     "pi",
	}); err != nil {
		t.Fatalf("seedSessionWithSpawnInputs InsertSession %q: %v", sessionName, err)
	}
	si := db.SpawnInputs{InstanceID: instanceID}
	if profileName != "" {
		p := profileName
		si.ProfileName = &p
	}
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("seedSessionWithSpawnInputs InsertSpawnInputs %q: %v", sessionName, err)
	}
	return instanceID
}

// TestPopulatePIConfig_SpawnProfileWinsOverActive is the core issue #2092
// positive: when spawn_inputs.profile_name = "spawn-profile" and the
// nix-default (acting as the active-profile fallback) is "active-profile",
// populatePIConfig must use spawn-profile's slot. Before #2092 the active
// profile silently won and the spawn-time choice was discarded.
func TestPopulatePIConfig_SpawnProfileWinsOverActive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, d := isolateForProfileLookup(t)
	writeMultiProfiles(t, configHome, "active-profile", map[string]profileSpec{
		"active-profile": {Model: "anthropic/claude-opus-4-7", Thinking: "medium"},
		"spawn-profile":  {Model: "anthropic/claude-opus-4-8", Thinking: "high"},
	})

	const sessionName = "prism-test@worker-spawn-profile-wins"
	seedSessionWithSpawnInputs(t, d, sessionName, "spawn-profile")

	ctrCfg := container.Config{}
	cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext"}
	if err := populatePIConfig(&ctrCfg, sessionName, "worker", cfg, piOverrides{}); err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}

	if ctrCfg.PIModel != "anthropic/claude-opus-4-8" {
		t.Errorf("PIModel = %q, want spawn-profile slot %q (spawn_inputs.profile_name must beat the active profile)",
			ctrCfg.PIModel, "anthropic/claude-opus-4-8")
	}
	if ctrCfg.PIThinking != "high" {
		t.Errorf("PIThinking = %q, want spawn-profile slot %q",
			ctrCfg.PIThinking, "high")
	}
	// Negative: the active profile's values must not leak through.
	if ctrCfg.PIModel == "anthropic/claude-opus-4-7" {
		t.Errorf("active profile model leaked into PIModel — spawn-time profile was silently dropped")
	}
}

// TestPopulatePIConfig_EmptySpawnProfileFallsThrough is the AC #4 / #6
// negative: when spawn_inputs exists but profile_name is NULL,
// populatePIConfig must fall through to the active-profile resolution
// unchanged. This preserves restart / restore semantics for legacy
// sessions that pre-date #2090 and any spawn path that legitimately does
// not record a profile.
func TestPopulatePIConfig_EmptySpawnProfileFallsThrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, d := isolateForProfileLookup(t)
	writeMultiProfiles(t, configHome, "active-profile", map[string]profileSpec{
		"active-profile": {Model: "anthropic/claude-opus-4-7", Thinking: "medium"},
		"spawn-profile":  {Model: "anthropic/claude-opus-4-8", Thinking: "high"},
	})

	const sessionName = "prism-test@worker-empty-spawn-profile"
	// Seed with empty profile_name → spawn_inputs row exists, column is NULL.
	seedSessionWithSpawnInputs(t, d, sessionName, "")

	ctrCfg := container.Config{}
	cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext"}
	if err := populatePIConfig(&ctrCfg, sessionName, "worker", cfg, piOverrides{}); err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}

	if ctrCfg.PIModel != "anthropic/claude-opus-4-7" {
		t.Errorf("PIModel = %q, want active-profile slot %q (NULL spawn_inputs.profile_name must fall through to the active profile)",
			ctrCfg.PIModel, "anthropic/claude-opus-4-7")
	}
	if ctrCfg.PIThinking != "medium" {
		t.Errorf("PIThinking = %q, want active-profile slot %q",
			ctrCfg.PIThinking, "medium")
	}
}

// TestPopulatePIConfig_NoSpawnInputsRowFallsThrough exercises the legacy /
// host-mode path where there is no spawn_inputs row at all (e.g. a
// session that pre-dates #2090). populatePIConfig must still resolve to
// the active-profile slot — wedging the launcher because the audit row
// is missing would break restart of pre-#2090 sessions.
func TestPopulatePIConfig_NoSpawnInputsRowFallsThrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForProfileLookup(t)
	writeMultiProfiles(t, configHome, "active-profile", map[string]profileSpec{
		"active-profile": {Model: "anthropic/claude-opus-4-7", Thinking: "medium"},
	})

	const sessionName = "prism-test@worker-no-spawn-inputs"
	// No sessions row, no spawn_inputs row — the lookup must return ""
	// without erroring.

	ctrCfg := container.Config{}
	cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext"}
	if err := populatePIConfig(&ctrCfg, sessionName, "worker", cfg, piOverrides{}); err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}

	if ctrCfg.PIModel != "anthropic/claude-opus-4-7" {
		t.Errorf("PIModel = %q, want active-profile slot %q (missing sessions row must fall through to the active profile)",
			ctrCfg.PIModel, "anthropic/claude-opus-4-7")
	}
}

// TestPopulatePIConfig_AbtestPairResolvesPerLeg is the integration shape
// promised by AC #7: two sessions sharing an abtest_pair_id but each
// carrying its own spawn_inputs.profile_name resolve, on populatePIConfig,
// to their own profile's slot — not the active profile's slot. This is
// the end-to-end fix for the reproducer in the issue body
// (`prism spawn --abtest A B` running both legs on the active profile).
func TestPopulatePIConfig_AbtestPairResolvesPerLeg(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, d := isolateForProfileLookup(t)
	writeMultiProfiles(t, configHome, "active-profile", map[string]profileSpec{
		"active-profile": {Model: "anthropic/claude-opus-4-active", Thinking: "medium"},
		"abtest-leg-a":   {Model: "anthropic/claude-opus-4-leg-a", Thinking: "low"},
		"abtest-leg-b":   {Model: "anthropic/claude-opus-4-leg-b", Thinking: "high"},
	})

	// Seed two sessions, one per leg, with distinct profile_name values.
	// They also share an abtest_pair_id — not strictly required for
	// populatePIConfig (which keys off instance_id, not pair_id) but
	// included to match the on-disk shape `prism spawn --abtest` produces
	// and to make the test failure mode informative if the lookup ever
	// accidentally aggregates by pair_id.
	const pairID = "test-abtest-pair-2092"
	legs := []struct {
		sessionName, profile, wantModel, wantThinking string
	}{
		{
			sessionName:  "prism-test@worker-abtest-leg-a",
			profile:      "abtest-leg-a",
			wantModel:    "anthropic/claude-opus-4-leg-a",
			wantThinking: "low",
		},
		{
			sessionName:  "prism-test@worker-abtest-leg-b",
			profile:      "abtest-leg-b",
			wantModel:    "anthropic/claude-opus-4-leg-b",
			wantThinking: "high",
		},
	}

	for _, leg := range legs {
		iid := uuid.New().String()
		if err := d.InsertSession(db.Session{
			InstanceID:  iid,
			SessionName: leg.sessionName,
			Repo:        "prism-test",
			Worktree:    "/tmp/" + leg.sessionName,
			Harness:     "pi",
		}); err != nil {
			t.Fatalf("InsertSession %q: %v", leg.sessionName, err)
		}
		p := leg.profile
		pair := pairID
		if err := d.InsertSpawnInputs(db.SpawnInputs{
			InstanceID:   iid,
			ProfileName:  &p,
			AbtestPairID: &pair,
		}); err != nil {
			t.Fatalf("InsertSpawnInputs %q: %v", leg.sessionName, err)
		}
	}

	// Now resolve each leg and assert the slot we got matches the leg's
	// profile, not the active profile.
	for _, leg := range legs {
		t.Run(leg.profile, func(t *testing.T) {
			ctrCfg := container.Config{}
			cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext"}
			if err := populatePIConfig(&ctrCfg, leg.sessionName, "worker", cfg, piOverrides{}); err != nil {
				t.Fatalf("populatePIConfig: %v", err)
			}
			if ctrCfg.PIModel != leg.wantModel {
				t.Errorf("PIModel = %q, want leg slot %q (active profile must NOT win for --abtest legs)",
					ctrCfg.PIModel, leg.wantModel)
			}
			if ctrCfg.PIThinking != leg.wantThinking {
				t.Errorf("PIThinking = %q, want leg slot %q",
					ctrCfg.PIThinking, leg.wantThinking)
			}
			// Negative: the active profile must not silently leak through.
			if ctrCfg.PIModel == "anthropic/claude-opus-4-active" {
				t.Errorf("active-profile model leaked into leg %q — spawn-time profile was silently dropped",
					leg.profile)
			}
		})
	}
}

// TestSpawnTimeProfileForSession_EmptySession is a tiny coverage guard:
// the helper must short-circuit on an empty session name without
// touching the DB. This guards against accidental crashes when an
// upstream caller forgets to thread the session name through.
func TestSpawnTimeProfileForSession_EmptySession(t *testing.T) {
	// No isolation needed — the function returns "" before any I/O.
	if got := spawnTimeProfileForSession(""); got != "" {
		t.Errorf("spawnTimeProfileForSession(\"\") = %q, want \"\"", got)
	}
}
