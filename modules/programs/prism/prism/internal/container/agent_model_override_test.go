package container

// agent_model_override_test.go — issue #2863 regression tests for
// Config.AgentModel, the per-role model override carried by
// `prism spawn --model-override <role>=<model>`.
//
// Two layers live in this package:
//
//   - AgentPaneCmd (bwrap and sandbox-exec) renders
//     `prism agent-run --agent-model <X>` so the entry survives the hop into
//     the pane process.
//   - PIInvocation ranks Config.AgentModel above Config.PIModel when it emits
//     pi's single --model argument. That ranking is what makes the top rung of
//     `prism agent-context`'s precedence["model"] chain true.

import (
	"strings"
	"testing"
)

const (
	// roleModel is the per-role --model-override value; slotModel is the
	// competing profile-slot / --model value it must beat.
	roleModel = "google/gemini-2.5-pro"
	slotModel = "anthropic/claude-opus-4-7"
)

// TestPIInvocation_AgentModelWinsOverPIModel is the core guard: with both
// fields set, pi's argv carries the per-role model exactly once and the
// lower rung does not appear at all.
func TestPIInvocation_AgentModelWinsOverPIModel(t *testing.T) {
	cfg := Config{
		PIBinaryPath:          "/nix/store/abc-pi/bin/pi",
		PIProvider:            "anthropic",
		PIModel:               slotModel,
		AgentModel:            roleModel,
		PIThinking:            "medium",
		PIExtensionSandboxDir: "/etc/prism/pi-extensions",
	}
	args := PIInvocation(cfg)

	if !hasPair(args, "--model", roleModel) {
		t.Errorf("expected --model %s (per-role override); got %v", roleModel, redactedArgs(args))
	}
	if hasPair(args, "--model", slotModel) {
		t.Errorf("lower rung %s leaked onto the argv; got %v", slotModel, redactedArgs(args))
	}
	// pi takes the last --model on its argv, so emitting both would make the
	// winner depend on order rather than on the documented precedence.
	if n := countFlag(args, "--model"); n != 1 {
		t.Errorf("expected exactly one --model argument, got %d; %v", n, redactedArgs(args))
	}
	// The other axes must be untouched by a model-axis override.
	if !hasPair(args, "--thinking", "medium") {
		t.Errorf("expected --thinking medium to survive; got %v", redactedArgs(args))
	}
	if !hasPair(args, "--provider", "anthropic") {
		t.Errorf("expected --provider anthropic to survive; got %v", redactedArgs(args))
	}
}

// TestPIInvocation_AgentModelAloneReachesArgv covers the shape produced when
// only --model-override is supplied: PIModel still holds the profile slot
// value, and the override replaces it.
func TestPIInvocation_AgentModelAloneReachesArgv(t *testing.T) {
	cfg := Config{
		PIBinaryPath:          "/nix/store/abc-pi/bin/pi",
		PIModel:               slotModel,
		AgentModel:            roleModel,
		PIExtensionSandboxDir: "/etc/prism/pi-extensions",
	}
	if args := PIInvocation(cfg); !hasPair(args, "--model", roleModel) {
		t.Errorf("expected --model %s; got %v", roleModel, redactedArgs(args))
	}
}

// TestPIInvocation_EmptyAgentModelFallsThroughToPIModel is the
// no-regression AC: with no per-role entry, PIModel alone decides the model,
// and no empty-string --model argument is emitted.
func TestPIInvocation_EmptyAgentModelFallsThroughToPIModel(t *testing.T) {
	t.Run("PIModel set", func(t *testing.T) {
		cfg := Config{
			PIBinaryPath:          "/nix/store/abc-pi/bin/pi",
			PIModel:               slotModel,
			PIExtensionSandboxDir: "/etc/prism/pi-extensions",
		}
		args := PIInvocation(cfg)
		if !hasPair(args, "--model", slotModel) {
			t.Errorf("expected the slot model on the argv; got %v", redactedArgs(args))
		}
		if n := countFlag(args, "--model"); n != 1 {
			t.Errorf("expected exactly one --model argument, got %d; %v", n, redactedArgs(args))
		}
	})

	t.Run("neither set", func(t *testing.T) {
		cfg := Config{
			PIBinaryPath:          "/nix/store/abc-pi/bin/pi",
			PIExtensionSandboxDir: "/etc/prism/pi-extensions",
		}
		args := PIInvocation(cfg)
		if countFlag(args, "--model") != 0 {
			t.Errorf("expected no --model argument when both fields are empty; got %v", redactedArgs(args))
		}
	})
}

// TestAgentPaneCmd_AgentModelFlagAppended pins the pane-command rendering for
// both pane-owned modes. The flag name here must match the flag
// `prism agent-run` registers — see TestAgentRunCmd_FlagsRegistered in the
// cmd package, which pins the other end.
func TestAgentPaneCmd_AgentModelFlagAppended(t *testing.T) {
	isolators := map[string]Isolator{
		"bwrap":        &bwrapIsolator{},
		"sandbox-exec": &sandboxExecIsolator{},
	}
	for mode, iso := range isolators {
		t.Run(mode, func(t *testing.T) {
			withFakePrismBinary(t, "/nix/store/abcd-prism/bin/prism")
			got, err := iso.AgentPaneCmd(AgentPaneOpts{
				SessionName: "prism-test@" + mode,
				Model:       slotModel,
				AgentModel:  roleModel,
			})
			if err != nil {
				t.Fatalf("%s AgentPaneCmd: unexpected error: %v", mode, err)
			}
			if !strings.Contains(got, "--agent-model '"+roleModel+"'") {
				t.Errorf("expected --agent-model %q on the pane command; got %q", roleModel, got)
			}
			// Both rungs travel: agent-run needs them apart so PIInvocation
			// can rank them.
			if !strings.Contains(got, "--model '"+slotModel+"'") {
				t.Errorf("expected --model %q to still be forwarded; got %q", slotModel, got)
			}
		})
	}
}

// TestAgentPaneCmd_EmptyAgentModelOmitsFlag is the no-regression guard for
// the pane command: with no per-role entry the --agent-model flag is absent
// entirely, rather than rendered with an empty string.
func TestAgentPaneCmd_EmptyAgentModelOmitsFlag(t *testing.T) {
	isolators := map[string]Isolator{
		"bwrap":        &bwrapIsolator{},
		"sandbox-exec": &sandboxExecIsolator{},
	}
	for mode, iso := range isolators {
		t.Run(mode, func(t *testing.T) {
			withFakePrismBinary(t, "/nix/store/abcd-prism/bin/prism")
			got, err := iso.AgentPaneCmd(AgentPaneOpts{
				SessionName: "prism-test@" + mode,
				Model:       slotModel,
			})
			if err != nil {
				t.Fatalf("%s AgentPaneCmd: unexpected error: %v", mode, err)
			}
			if strings.Contains(got, "--agent-model") {
				t.Errorf("expected no --agent-model flag; got %q", got)
			}
		})
	}
}

// countFlag returns how many times name appears as a standalone argument in
// args. Used to assert that the model axis contributes exactly one --model
// argument, whichever rung wins.
func countFlag(args []string, name string) int {
	n := 0
	for _, a := range args {
		if a == name {
			n++
		}
	}
	return n
}
