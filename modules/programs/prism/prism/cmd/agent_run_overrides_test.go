package cmd

// agent_run_overrides_test.go — regression tests for the CLI override path.
//
// Covers the CLI override path that threads `prism spawn --model` /
// `--variant` through `prism agent-run` and into `populatePIConfig`, where
// the override wins over the active profile slot's Model/Thinking before
// PIInvocation builds the final pi argv.
//
// The end-to-end picture:
//
//   1. cmd/spawn.go reads --model / --variant flags, puts them on
//      session.SpawnOpts.{Model,Variant}.
//   2. internal/session threads them into the AgentPaneCmd built for bwrap/
//      sandbox-exec, producing a tmux pane command like
//      `prism agent-run --session <X> --model <M> --variant <V>`.
//   3. cmd/agent_run.go parses --model / --variant, stashes them on the
//      per-session cache, then `populatePIConfig` reads the cache and
//      replaces slot.Model / slot.Thinking with the override values when
//      non-empty.
//   4. internal/container/pi_invocation.go reads PIModel / PIThinking from
//      the container.Config and emits --model / --thinking on pi's argv.
//
// The unit tests below pin steps 3 and 4 explicitly so that a regression
// at either layer is caught without depending on the spawn driver.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
)

// writeProfiles writes a profiles.json under $XDG_CONFIG_HOME/prism/ with a
// single profile named "test" defining a worker slot. The caller supplies the
// slot values so multiple scenarios can share the helper.
func writeProfiles(t *testing.T, configHome, profileName, model, thinking string) {
	t.Helper()
	prismDir := filepath.Join(configHome, "prism")
	if err := os.MkdirAll(prismDir, 0o700); err != nil {
		t.Fatalf("mkdir prism config dir: %v", err)
	}
	pf := config.ProfilesFile{
		Default: profileName,
		Profiles: map[string]config.ProfileEntry{
			profileName: {
				"worker": config.RoleSlot{
					Provider: "anthropic",
					Model:    model,
					Thinking: thinking,
				},
			},
		},
	}
	b, err := json.Marshal(&pf)
	if err != nil {
		t.Fatalf("marshal profiles: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prismDir, "profiles.json"), b, 0o600); err != nil {
		t.Fatalf("write profiles.json: %v", err)
	}
}

// writeFakePiBinary writes a non-executable-fast shim named `pi` into binDir
// and returns the path. populatePIConfig calls `exec.LookPath("pi")` so the
// shim just needs to be discoverable on PATH and have the executable bit set
// — its contents are never run by the code under test.
func writeFakePiBinary(t *testing.T, binDir string) string {
	t.Helper()
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin dir: %v", err)
	}
	piPath := filepath.Join(binDir, "pi")
	if err := os.WriteFile(piPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}
	return piPath
}

// isolateForPopulatePIConfig sets up an isolated HOME, XDG_CONFIG_HOME,
// XDG_STATE_HOME, and PATH so that populatePIConfig's calls to
// LoadProfiles, ResolveActiveProfile, EnsurePIAgentConfigDir, and
// exec.LookPath all resolve against the tmp directory rather than touching
// the developer's real environment. Returns the configHome and the fake pi
// binary path so test cases can assert against them if needed.
func isolateForPopulatePIConfig(t *testing.T) (configHome, piBinary string) {
	t.Helper()
	tmp := t.TempDir()
	configHome = filepath.Join(tmp, "config")
	stateHome := filepath.Join(tmp, "state")
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatalf("mkdir config home: %v", err)
	}
	if err := os.MkdirAll(stateHome, 0o700); err != nil {
		t.Fatalf("mkdir state home: %v", err)
	}
	piBinary = writeFakePiBinary(t, binDir)

	// HOME governs both EnsurePIAgentConfigDir (creates $HOME/.pi/agent)
	// and the fallback for XDG_STATE_HOME / XDG_CONFIG_HOME.
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", stateHome)
	// Restrict PATH to only the bin dir so exec.LookPath("pi") resolves to
	// our shim deterministically and cannot accidentally pick up a real pi
	// from /etc/profiles/per-user/<u>/bin or /run/current-system/sw/bin.
	t.Setenv("PATH", binDir)
	return configHome, piBinary
}

// TestPopulatePIConfig_SlotOnly is the no-regression baseline: with no CLI
// overrides set, ctrCfg.PIModel and ctrCfg.PIThinking must come from the
// active profile slot.
func TestPopulatePIConfig_SlotOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForPopulatePIConfig(t)
	writeProfiles(t, configHome, "test", "anthropic/claude-opus-4-7", "medium")

	ctrCfg := container.Config{}
	cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext"}
	err := populatePIConfig(&ctrCfg, "prism-test@session", "worker", cfg, piOverrides{})
	if err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}

	if ctrCfg.PIModel != "anthropic/claude-opus-4-7" {
		t.Errorf("PIModel = %q, want slot value %q", ctrCfg.PIModel, "anthropic/claude-opus-4-7")
	}
	if ctrCfg.PIThinking != "medium" {
		t.Errorf("PIThinking = %q, want slot value %q", ctrCfg.PIThinking, "medium")
	}
	if ctrCfg.PIProvider != "anthropic" {
		t.Errorf("PIProvider = %q, want slot value %q", ctrCfg.PIProvider, "anthropic")
	}
}

// TestPopulatePIConfig_CLIOverrideWins is the core CLI-override regression guard:
// when a non-empty Model / Variant override is supplied alongside a profile
// that defines a different slot value, the override wins on ctrCfg.PIModel
// and ctrCfg.PIThinking — i.e. on the final pi argv that PIInvocation
// produces from the container.Config.
func TestPopulatePIConfig_CLIOverrideWins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForPopulatePIConfig(t)
	// Profile slot says opus-4-7 / medium. Override says opus-4-8 / high.
	// Post-fix the override must reach ctrCfg.
	writeProfiles(t, configHome, "test", "anthropic/claude-opus-4-7", "medium")

	ctrCfg := container.Config{}
	cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext"}
	overrides := piOverrides{
		Model:   "anthropic/claude-opus-4-8",
		Variant: "high",
	}
	err := populatePIConfig(&ctrCfg, "prism-test@session", "worker", cfg, overrides)
	if err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}

	if ctrCfg.PIModel != "anthropic/claude-opus-4-8" {
		t.Errorf("PIModel = %q, want override value %q", ctrCfg.PIModel, "anthropic/claude-opus-4-8")
	}
	if ctrCfg.PIThinking != "high" {
		t.Errorf("PIThinking = %q, want override value %q", ctrCfg.PIThinking, "high")
	}
	// No provider override was supplied here, so PIProvider must still come
	// from the slot (the fall-through case).
	if ctrCfg.PIProvider != "anthropic" {
		t.Errorf("PIProvider = %q, want slot value %q", ctrCfg.PIProvider, "anthropic")
	}

	// End-to-end: feed the populated ctrCfg into PIInvocation and assert the
	// override pair lands on the final pi argv. This is the same shape the
	// bwrap and sandbox-exec entry points produce just before exec.
	args := container.PIInvocation(ctrCfg)
	if !argvHasPair(args, "--model", "anthropic/claude-opus-4-8") {
		t.Errorf("pi argv missing --model override: %v", args)
	}
	if !argvHasPair(args, "--thinking", "high") {
		t.Errorf("pi argv missing --thinking override: %v", args)
	}
	// Negative: the pre-override slot values must not leak onto the argv.
	if argvHasPair(args, "--model", "anthropic/claude-opus-4-7") {
		t.Errorf("slot model leaked into argv after override: %v", args)
	}
	if argvHasPair(args, "--thinking", "medium") {
		t.Errorf("slot thinking leaked into argv after override: %v", args)
	}
}

// TestPopulatePIConfig_ProviderOverrideWins is the provider-override core guard:
// a non-empty Provider override replaces the profile slot's provider on
// ctrCfg.PIProvider, and therefore on the final pi argv that PIInvocation
// builds from the container.Config.
func TestPopulatePIConfig_ProviderOverrideWins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForPopulatePIConfig(t)
	// writeProfiles pins the slot provider to "anthropic".
	writeProfiles(t, configHome, "test", "anthropic/claude-opus-4-7", "medium")

	ctrCfg := container.Config{}
	cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext"}
	overrides := piOverrides{Provider: "openrouter"}
	if err := populatePIConfig(&ctrCfg, "prism-test@session", "worker", cfg, overrides); err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}

	if ctrCfg.PIProvider != "openrouter" {
		t.Errorf("PIProvider = %q, want override value %q", ctrCfg.PIProvider, "openrouter")
	}
	// The other axes must be untouched by a provider-only override.
	if ctrCfg.PIModel != "anthropic/claude-opus-4-7" {
		t.Errorf("PIModel = %q, want slot value %q", ctrCfg.PIModel, "anthropic/claude-opus-4-7")
	}
	if ctrCfg.PIThinking != "medium" {
		t.Errorf("PIThinking = %q, want slot value %q", ctrCfg.PIThinking, "medium")
	}

	// End-to-end: the override must land on the final pi argv, and the slot
	// provider must not leak alongside it.
	args := container.PIInvocation(ctrCfg)
	if !argvHasPair(args, "--provider", "openrouter") {
		t.Errorf("pi argv missing --provider override: %v", args)
	}
	if argvHasPair(args, "--provider", "anthropic") {
		t.Errorf("slot provider leaked into argv after override: %v", args)
	}
}

// TestPopulatePIConfig_EmptyProviderFallsThroughToSlot is the empty-provider
// edge case: an empty-string provider override leaves the slot value
// unchanged, so no blank `--provider ""` argument can ever be emitted.
func TestPopulatePIConfig_EmptyProviderFallsThroughToSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForPopulatePIConfig(t)
	writeProfiles(t, configHome, "test", "anthropic/claude-opus-4-7", "medium")

	ctrCfg := container.Config{}
	cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext"}
	// Explicitly empty — the shape produced when --provider is not passed.
	overrides := piOverrides{Provider: ""}
	if err := populatePIConfig(&ctrCfg, "prism-test@session", "worker", cfg, overrides); err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}

	if ctrCfg.PIProvider != "anthropic" {
		t.Errorf("PIProvider = %q, want slot value %q", ctrCfg.PIProvider, "anthropic")
	}
	args := container.PIInvocation(ctrCfg)
	if argvHasPair(args, "--provider", "") {
		t.Errorf("blank --provider argument emitted: %v", args)
	}
	if !argvHasPair(args, "--provider", "anthropic") {
		t.Errorf("expected the slot provider on the argv: %v", args)
	}
}

// TestPopulatePIConfig_PartialOverride_ModelOnly verifies that supplying
// only --model (no --variant) overrides the model but leaves Thinking on
// the slot value. Mirrors the operator workflow where one axis is varied
// for an A/B test while the rest of the slot is preserved.
func TestPopulatePIConfig_PartialOverride_ModelOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForPopulatePIConfig(t)
	writeProfiles(t, configHome, "test", "anthropic/claude-opus-4-7", "medium")

	ctrCfg := container.Config{}
	cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext"}
	overrides := piOverrides{Model: "anthropic/claude-opus-4-8"}
	if err := populatePIConfig(&ctrCfg, "prism-test@session", "worker", cfg, overrides); err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}

	if ctrCfg.PIModel != "anthropic/claude-opus-4-8" {
		t.Errorf("PIModel = %q, want override %q", ctrCfg.PIModel, "anthropic/claude-opus-4-8")
	}
	if ctrCfg.PIThinking != "medium" {
		t.Errorf("PIThinking = %q, want slot %q (no variant override)", ctrCfg.PIThinking, "medium")
	}
}

// TestAgentRunCmd_FlagsRegistered asserts the help surface:
// `prism agent-run --help` lists --model, --variant, and --provider.
// Cobra's flag lookup is the closest verifiable analogue to "appears in
// --help" without re-exec'ing the binary. The registration is load-bearing
// beyond help text: the bwrap / sandbox-exec pane command passes these flags
// on the argv, so an unregistered flag makes `prism agent-run` fail to parse
// its own launch command.
//
// --agent-model is part of the set. The name is asserted at both
// ends of the hop: here for the parser, and in
// internal/container/agent_model_override_test.go for the emitter.
func TestAgentRunCmd_FlagsRegistered(t *testing.T) {
	for _, name := range []string{"session", "model", "agent-model", "variant", "provider"} {
		if f := agentRunCmd.Flags().Lookup(name); f == nil {
			t.Errorf("expected agentRunCmd to define flag --%s", name)
		}
	}
}

// TestStoreLoadAgentRunOverrides_Roundtrip asserts the in-process cache
// shape used to ferry CLI overrides from runAgentRun (which parses argv)
// into the registered per-mode handlers (which receive only
// AgentRunOpts). A regression here would silently drop the overrides for
// sandbox-exec.
func TestStoreLoadAgentRunOverrides_Roundtrip(t *testing.T) {
	const session = "prism-test@cache-roundtrip"
	storeAgentRunOverrides(session, piOverrides{Model: "M", Variant: "V", Provider: "P"})
	t.Cleanup(func() { clearAgentRunStatus(session) })

	got := loadAgentRunOverrides(session)
	if got.Model != "M" || got.Variant != "V" || got.Provider != "P" {
		t.Errorf("loadAgentRunOverrides = %+v, want {Model:M Variant:V Provider:P}", got)
	}

	// Empty zero value when nothing stored.
	if zero := loadAgentRunOverrides("prism-test@no-such-entry"); zero.Model != "" || zero.Variant != "" || zero.Provider != "" {
		t.Errorf("expected zero piOverrides for unknown key; got %+v", zero)
	}

	// clearAgentRunStatus must also purge the overrides entry (single-
	// session-per-process contract).
	clearAgentRunStatus(session)
	if got := loadAgentRunOverrides(session); got.Model != "" || got.Variant != "" {
		t.Errorf("expected overrides to be cleared; got %+v", got)
	}
}

// argvHasPair returns true when flag and val appear consecutively in args.
// Local helper to avoid coupling to the internal/container test helper of
// the same name.
func argvHasPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}
