package session

// model_override_test.go — issue #2863 regression tests for the per-role
// model override on the session launch path.
//
// `prism spawn --model-override <role>=<model>` lands on Opts.ModelsByRole.
// The entry keyed by the session's own role decides the model pi runs on,
// above `--model` and above the profile slot.
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

// TestBuildDirectAgentCmd_NoOverrideIsUnchanged is the no-regression AC: an
// empty or nil ModelsByRole must leave the host command byte-identical to the
// same opts with the field absent.
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
// not run emits no --agent-model flag at all.
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

// TestBuildOptsForAgentOnlyLayout_ForwardsFields pins the SpawnOpts → Opts
// mapping for the agent-only layout, which carries every review agent — the
// primary user of --model-override.
//
// Its sibling buildOptsForLayout carries per-field forwarding guards for the
// same reason; this is the other half of that convention. A field dropped
// from either mapping is silent — the session still launches, and only the
// dropped capability goes missing.
func TestBuildOptsForAgentOnlyLayout_ForwardsFields(t *testing.T) {
	spawnOpts := SpawnOpts{
		SessionName:  "myrepo@branch~review-1-review-goal",
		Worktree:     "/tmp/wt",
		AgentRole:    "review-goal",
		Prompt:       "review the diff",
		HarnessName:  "pi",
		Model:        sessionWideModel,
		Variant:      "high",
		Provider:     "openrouter",
		ModelsByRole: map[string]string{"review-goal": perRoleModel},
		// Non-nil AgentEnvVars takes the filter branch of
		// agentOnlyAgentEnvVars, so the assertion does not depend on the
		// host's profiles.json.
		AgentEnvVars: map[string]string{"PRISM_TEST_ENV": "1"},
	}
	got := buildOptsForAgentOnlyLayout(spawnOpts, 14000, "bwrap")

	if got.ModelsByRole["review-goal"] != perRoleModel {
		t.Errorf("Opts.ModelsByRole[review-goal] = %q, want forwarded value %q",
			got.ModelsByRole["review-goal"], perRoleModel)
	}
	if got.Model != sessionWideModel {
		t.Errorf("Opts.Model = %q, want forwarded value %q", got.Model, sessionWideModel)
	}
	if got.Variant != "high" {
		t.Errorf("Opts.Variant = %q, want forwarded value", got.Variant)
	}
	if got.Provider != "openrouter" {
		t.Errorf("Opts.Provider = %q, want forwarded value", got.Provider)
	}
	if got.AgentEnvVars == nil {
		t.Error("Opts.AgentEnvVars = nil, want the role-filtered map (#2533)")
	}
	// Agent and IsolationMode decide which entry roleModelOverride matches and
	// which emitter BuildAgentCmd dispatches to, so a drop on either would
	// disable the override without touching ModelsByRole itself.
	if got.Agent != "review-goal" {
		t.Errorf("Opts.Agent = %q, want the spawn AgentRole", got.Agent)
	}
	if got.IsolationMode != "bwrap" {
		t.Errorf("Opts.IsolationMode = %q, want the caller's resolved mode", got.IsolationMode)
	}
}

// TestBuildOptsForAgentOnlyLayout_OverrideReachesArgv is the end-to-end half:
// a reviewer's own --model-override entry must survive the SpawnOpts → Opts
// mapping and land on the launch command. Deleting the ModelsByRole line in
// buildOptsForAgentOnlyLayout fails this test.
func TestBuildOptsForAgentOnlyLayout_OverrideReachesArgv(t *testing.T) {
	spawnOpts := SpawnOpts{
		SessionName:  "myrepo@branch~review-1-review-goal",
		Worktree:     "/tmp/wt",
		AgentRole:    "review-goal",
		HarnessName:  "pi",
		ModelsByRole: map[string]string{"review-goal": perRoleModel},
		AgentEnvVars: map[string]string{},
	}

	for _, mode := range []string{"host", "bwrap", "sandbox-exec"} {
		t.Run(mode, func(t *testing.T) {
			cmd, err := BuildAgentCmd(buildOptsForAgentOnlyLayout(spawnOpts, 14000, mode))
			if err != nil {
				t.Fatalf("BuildAgentCmd(%s): %v", mode, err)
			}
			// Host emits pi's --model directly; the sandboxed modes hand the
			// entry to `prism agent-run` as --agent-model.
			want := "--agent-model '" + perRoleModel + "'"
			if mode == "host" {
				want = "--model '" + perRoleModel + "'"
			}
			if !strings.Contains(cmd, want) {
				t.Errorf("expected %q on the %s launch command; got %q", want, mode, cmd)
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
