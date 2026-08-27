package cmd

// model_override_live_test.go — issue #2863.
//
// Pins the agent-run half of the per-role model override for the two
// pane-owned isolation modes. bwrap and sandbox-exec share this step: both
// dispatch through `prism agent-run`, which stashes its flags on
// piOverrides and calls populatePIConfig, and both then launch pi via
// container.PIInvocation.
//
// The full chain for those modes is:
//
//  1. cmd/spawn.go parses --model-override role=model into
//     session.SpawnOpts.ModelsByRole.
//  2. internal/session resolves the entry for this session's own role and
//     renders `prism agent-run --agent-model <X>` into the tmux pane command
//     (internal/session/model_override_test.go).
//  3. cmd/agent_run.go parses --agent-model and populatePIConfig copies it to
//     container.Config.AgentModel — pinned below.
//  4. container.PIInvocation ranks AgentModel above PIModel and emits pi's
//     --model (internal/container/agent_model_override_test.go).
//
// Host mode skips steps 2-4 and emits pi's --model directly; see
// internal/session/model_override_test.go.

import (
	"runtime"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
)

// TestPopulatePIConfig_AgentModelReachesPIArgv is the core bwrap /
// sandbox-exec guard: the per-role override outranks both `--model` and the
// profile slot on the argv that agent-run hands to pi.
func TestPopulatePIConfig_AgentModelReachesPIArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForPopulatePIConfig(t)
	// Slot says opus-4-7. `prism spawn --model` says opus-4-8. The per-role
	// `--model-override worker=…` says gemini-2.5-pro and must win.
	writeProfiles(t, configHome, "test", "anthropic/claude-opus-4-7", "medium")

	ctrCfg := container.Config{}
	cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext"}
	overrides := piOverrides{
		Model:      "anthropic/claude-opus-4-8",
		AgentModel: "google/gemini-2.5-pro",
	}
	if err := populatePIConfig(&ctrCfg, "prism-test@session", "worker", cfg, overrides); err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}

	// populatePIConfig is the writer of Config.AgentModel. It keeps the two
	// rungs apart rather than collapsing them, so PIInvocation can rank them.
	if ctrCfg.AgentModel != "google/gemini-2.5-pro" {
		t.Errorf("AgentModel = %q, want the per-role override %q", ctrCfg.AgentModel, "google/gemini-2.5-pro")
	}
	if ctrCfg.PIModel != "anthropic/claude-opus-4-8" {
		t.Errorf("PIModel = %q, want the session-wide --model %q", ctrCfg.PIModel, "anthropic/claude-opus-4-8")
	}

	args := container.PIInvocation(ctrCfg)
	if !argvHasPair(args, "--model", "google/gemini-2.5-pro") {
		t.Errorf("pi argv missing the per-role model: %v", args)
	}
	if argvHasPair(args, "--model", "anthropic/claude-opus-4-8") {
		t.Errorf("the session-wide --model leaked onto pi's argv: %v", args)
	}
	if argvHasPair(args, "--model", "anthropic/claude-opus-4-7") {
		t.Errorf("the profile slot model leaked onto pi's argv: %v", args)
	}
}

// TestPopulatePIConfig_AgentModelBeatsSlotWithoutModelFlag covers the common
// shape: --model-override supplied on its own, with no --model.
func TestPopulatePIConfig_AgentModelBeatsSlotWithoutModelFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForPopulatePIConfig(t)
	writeProfiles(t, configHome, "test", "anthropic/claude-opus-4-7", "medium")

	ctrCfg := container.Config{}
	cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext"}
	overrides := piOverrides{AgentModel: "google/gemini-2.5-pro"}
	if err := populatePIConfig(&ctrCfg, "prism-test@session", "worker", cfg, overrides); err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}

	// The slot value still lands on PIModel — the override does not erase the
	// lower rung, it outranks it at the argv-rendering point.
	if ctrCfg.PIModel != "anthropic/claude-opus-4-7" {
		t.Errorf("PIModel = %q, want the slot value %q", ctrCfg.PIModel, "anthropic/claude-opus-4-7")
	}
	args := container.PIInvocation(ctrCfg)
	if !argvHasPair(args, "--model", "google/gemini-2.5-pro") {
		t.Errorf("pi argv missing the per-role model: %v", args)
	}
	if argvHasPair(args, "--model", "anthropic/claude-opus-4-7") {
		t.Errorf("the profile slot model leaked onto pi's argv: %v", args)
	}
	// The other axes must be untouched by a model-axis override.
	if !argvHasPair(args, "--thinking", "medium") {
		t.Errorf("pi argv lost the slot thinking value: %v", args)
	}
	if !argvHasPair(args, "--provider", "anthropic") {
		t.Errorf("pi argv lost the slot provider value: %v", args)
	}
}

// TestPopulatePIConfig_NoAgentModelIsUnchanged is the no-regression AC for
// the sandboxed modes: with no per-role entry, AgentModel stays empty and the
// argv is exactly the pre-#2863 `--model` / slot result.
func TestPopulatePIConfig_NoAgentModelIsUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForPopulatePIConfig(t)
	writeProfiles(t, configHome, "test", "anthropic/claude-opus-4-7", "medium")

	cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext"}

	t.Run("slot only", func(t *testing.T) {
		ctrCfg := container.Config{}
		if err := populatePIConfig(&ctrCfg, "prism-test@session", "worker", cfg, piOverrides{}); err != nil {
			t.Fatalf("populatePIConfig: %v", err)
		}
		if ctrCfg.AgentModel != "" {
			t.Errorf("AgentModel = %q, want empty when no --model-override applies", ctrCfg.AgentModel)
		}
		if args := container.PIInvocation(ctrCfg); !argvHasPair(args, "--model", "anthropic/claude-opus-4-7") {
			t.Errorf("pi argv lost the slot model: %v", args)
		}
	})

	t.Run("--model only", func(t *testing.T) {
		ctrCfg := container.Config{}
		overrides := piOverrides{Model: "anthropic/claude-opus-4-8"}
		if err := populatePIConfig(&ctrCfg, "prism-test@session", "worker", cfg, overrides); err != nil {
			t.Fatalf("populatePIConfig: %v", err)
		}
		if ctrCfg.AgentModel != "" {
			t.Errorf("AgentModel = %q, want empty when no --model-override applies", ctrCfg.AgentModel)
		}
		args := container.PIInvocation(ctrCfg)
		if !argvHasPair(args, "--model", "anthropic/claude-opus-4-8") {
			t.Errorf("pi argv lost the --model override: %v", args)
		}
		if argvHasPair(args, "--model", "anthropic/claude-opus-4-7") {
			t.Errorf("the slot model leaked onto pi's argv: %v", args)
		}
	})
}

// TestStoreLoadAgentRunOverrides_AgentModelRoundtrip pins the per-session
// cache that ferries the parsed flag from runAgentRun (which owns argv) to
// the registered per-mode handler (which receives only AgentRunOpts). A drop
// here would silently disable the override on sandbox-exec.
func TestStoreLoadAgentRunOverrides_AgentModelRoundtrip(t *testing.T) {
	const session = "prism-test@agent-model-roundtrip"
	storeAgentRunOverrides(session, piOverrides{AgentModel: "google/gemini-2.5-pro"})
	t.Cleanup(func() { clearAgentRunStatus(session) })

	if got := loadAgentRunOverrides(session).AgentModel; got != "google/gemini-2.5-pro" {
		t.Errorf("loadAgentRunOverrides().AgentModel = %q, want %q", got, "google/gemini-2.5-pro")
	}
}
