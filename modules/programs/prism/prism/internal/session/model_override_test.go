package session

// model_override_test.go — issue #2863 regression tests for the per-role
// model override on the session launch path.
//
// `prism spawn --model-override <role>=<model>` lands on
// Opts.ModelsByRole. Before #2863 that map reached only the sidecar and the
// agent_status.agent_model reporting column, so the flag selected no model on
// any isolation mode. It now decides the model pi runs on for the session's
// own role, above `--model` and above the profile slot.
//
// Two emit sites carry it, one per launch shape:
//
//   - host: buildDirectAgentCmd emits pi's `--model` directly, so it applies
//     the precedence itself and emits ONE flag.
//   - bwrap / sandbox-exec: BuildAgentCmd puts the entry on
//     AgentPaneOpts.AgentModel, which renders as
//     `prism agent-run --agent-model <X>`. The precedence is applied later,
//     by container.PIInvocation. See cmd/model_override_live_test.go and
//     internal/container/agent_model_override_test.go for those steps.

import (
	"strings"
	"testing"
)

const (
	// perRoleModel is the value supplied via --model-override for the role
	// under test; sessionWideModel is the competing --model value.
	perRoleModel     = "google/gemini-2.5-pro"
	sessionWideModel = "anthropic/claude-opus-4-8"
)

// TestBuildDirectAgentCmd_PerRoleOverrideWinsOverModel is the host-mode core
// guard: with both flags supplied, pi's argv carries the per-role model and
// not the session-wide one.
func TestBuildDirectAgentCmd_PerRoleOverrideWinsOverModel(t *testing.T) {
	opts := Opts{
		Agent:        "worker",
		SessionName:  "myrepo@branch",
		HarnessName:  "pi",
		Model:        sessionWideModel,
		ModelsByRole: map[string]string{"worker": perRoleModel},
	}
	cmd := buildDirectAgentCmd(opts)

	if !strings.Contains(cmd, "--model '"+perRoleModel+"'") {
		t.Errorf("expected --model %q (per-role override) in direct cmd; got %q", perRoleModel, cmd)
	}
	if strings.Contains(cmd, sessionWideModel) {
		t.Errorf("session-wide --model leaked onto the argv alongside the per-role override; got %q", cmd)
	}
	// Exactly one --model flag: pi takes the last occurrence, so two flags
	// would make the winner depend on emit order rather than on precedence.
	if n := strings.Count(cmd, "--model "); n != 1 {
		t.Errorf("expected exactly one --model flag, got %d; cmd: %q", n, cmd)
	}
}

// TestBuildDirectAgentCmd_PerRoleOverrideWithoutModelFlag covers the common
// shape: --model-override alone, with no --model, must still beat the profile
// slot. Host mode leaves slot resolution to pi itself, so "beats the slot"
// means the flag is present on the argv at all.
func TestBuildDirectAgentCmd_PerRoleOverrideWithoutModelFlag(t *testing.T) {
	opts := Opts{
		Agent:        "review-goal",
		SessionName:  "myrepo@branch~review-1-review-goal",
		HarnessName:  "pi",
		ModelsByRole: map[string]string{"review-goal": perRoleModel},
	}
	cmd := buildDirectAgentCmd(opts)

	if !strings.Contains(cmd, "--model '"+perRoleModel+"'") {
		t.Errorf("expected --model %q in direct cmd; got %q", perRoleModel, cmd)
	}
}

// TestBuildDirectAgentCmd_OverrideForOtherRoleIsNoOp is the edge-case AC: an
// entry naming a role this session does not run must not alter the model.
// Both sub-cases matter — with a --model present the flag must survive
// untouched, and without one no flag may be invented.
func TestBuildDirectAgentCmd_OverrideForOtherRoleIsNoOp(t *testing.T) {
	t.Run("--model survives untouched", func(t *testing.T) {
		opts := Opts{
			Agent:        "worker",
			SessionName:  "myrepo@branch",
			HarnessName:  "pi",
			Model:        sessionWideModel,
			ModelsByRole: map[string]string{"review-goal": perRoleModel},
		}
		cmd := buildDirectAgentCmd(opts)
		if !strings.Contains(cmd, "--model '"+sessionWideModel+"'") {
			t.Errorf("expected the session-wide --model %q to survive; got %q", sessionWideModel, cmd)
		}
		if strings.Contains(cmd, perRoleModel) {
			t.Errorf("another role's override reached this session's argv; got %q", cmd)
		}
	})

	t.Run("no --model is invented", func(t *testing.T) {
		opts := Opts{
			Agent:        "worker",
			SessionName:  "myrepo@branch",
			HarnessName:  "pi",
			ModelsByRole: map[string]string{"review-goal": perRoleModel},
		}
		cmd := buildDirectAgentCmd(opts)
		if strings.Contains(cmd, "--model ") {
			t.Errorf("expected no --model flag when only another role is overridden; got %q", cmd)
		}
	})
}

// TestBuildDirectAgentCmd_NoOverrideIsUnchanged is the no-regression AC: with
// an empty ModelsByRole the host command is byte-identical to the pre-#2863
// output for the same opts.
func TestBuildDirectAgentCmd_NoOverrideIsUnchanged(t *testing.T) {
	base := Opts{
		Agent:       "worker",
		SessionName: "myrepo@branch",
		HarnessName: "pi",
		Model:       sessionWideModel,
		Variant:     "high",
	}
	want := buildDirectAgentCmd(base)

	withNilMap := base
	withNilMap.ModelsByRole = nil
	if got := buildDirectAgentCmd(withNilMap); got != want {
		t.Errorf("nil ModelsByRole changed the command:\n got: %q\nwant: %q", got, want)
	}

	withEmptyMap := base
	withEmptyMap.ModelsByRole = map[string]string{}
	if got := buildDirectAgentCmd(withEmptyMap); got != want {
		t.Errorf("empty ModelsByRole changed the command:\n got: %q\nwant: %q", got, want)
	}

	if !strings.Contains(want, "--model '"+sessionWideModel+"'") {
		t.Errorf("baseline lost the session-wide --model; got %q", want)
	}
}

// TestBuildAgentCmd_SandboxedModesCarryAgentModel pins the pane-command half
// of the bwrap and sandbox-exec chain: the per-role entry leaves the session
// package as `prism agent-run --agent-model <X>`, and the session-wide
// `--model` rides alongside it rather than being overwritten here (the two
// are separate rungs; PIInvocation ranks them).
func TestBuildAgentCmd_SandboxedModesCarryAgentModel(t *testing.T) {
	for _, mode := range []string{"bwrap", "sandbox-exec"} {
		t.Run(mode, func(t *testing.T) {
			opts := Opts{
				Agent:         "worker",
				SessionName:   "myrepo@branch",
				HarnessName:   "pi",
				IsolationMode: mode,
				Model:         sessionWideModel,
				ModelsByRole:  map[string]string{"worker": perRoleModel},
			}
			cmd, err := BuildAgentCmd(opts)
			if err != nil {
				t.Fatalf("BuildAgentCmd(%s): %v", mode, err)
			}
			if !strings.Contains(cmd, "--agent-model '"+perRoleModel+"'") {
				t.Errorf("expected --agent-model %q on the %s pane command; got %q", perRoleModel, mode, cmd)
			}
			if !strings.Contains(cmd, "--model '"+sessionWideModel+"'") {
				t.Errorf("expected the session-wide --model to still be forwarded on %s; got %q", mode, cmd)
			}
		})
	}
}

// TestBuildAgentCmd_SandboxedModesOmitAgentModelForOtherRole is the
// sandboxed half of the edge-case AC: an entry for a role this session does
// not run emits no --agent-model flag, so `prism agent-run` sees the
// pre-#2863 argv.
func TestBuildAgentCmd_SandboxedModesOmitAgentModelForOtherRole(t *testing.T) {
	for _, mode := range []string{"bwrap", "sandbox-exec"} {
		t.Run(mode, func(t *testing.T) {
			opts := Opts{
				Agent:         "worker",
				SessionName:   "myrepo@branch",
				HarnessName:   "pi",
				IsolationMode: mode,
				ModelsByRole:  map[string]string{"review-goal": perRoleModel},
			}
			cmd, err := BuildAgentCmd(opts)
			if err != nil {
				t.Fatalf("BuildAgentCmd(%s): %v", mode, err)
			}
			if strings.Contains(cmd, "--agent-model") {
				t.Errorf("another role's override reached the %s pane command; got %q", mode, cmd)
			}
		})
	}
}

// TestRoleModelOverride_Lookup pins the shared resolver both emit sites call,
// including the degenerate inputs that must not panic or match.
func TestRoleModelOverride_Lookup(t *testing.T) {
	cases := []struct {
		name   string
		agent  string
		byRole map[string]string
		want   string
	}{
		{name: "hit", agent: "worker", byRole: map[string]string{"worker": perRoleModel}, want: perRoleModel},
		{name: "miss", agent: "worker", byRole: map[string]string{"review-goal": perRoleModel}, want: ""},
		{name: "nil map", agent: "worker", byRole: nil, want: ""},
		{name: "empty map", agent: "worker", byRole: map[string]string{}, want: ""},
		{name: "empty role", agent: "", byRole: map[string]string{"worker": perRoleModel}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roleModelOverride(Opts{Agent: tc.agent, ModelsByRole: tc.byRole})
			if got != tc.want {
				t.Errorf("roleModelOverride = %q, want %q", got, tc.want)
			}
		})
	}
}
