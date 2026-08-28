package session

// host_overrides_test.go — issue #2086 regression tests for the host-mode
// direct pi launch command.
//
// In host mode, the tmux pane runs pi directly (not via `prism agent-run`),
// so the CLI overrides --model and --variant must be appended to pi's argv
// here in buildDirectAgentCmd. The container modes (bwrap / sandbox-exec)
// route through AgentPaneCmd; see
// internal/container/agent_pane_overrides_test.go for those.
//
// Mapping reminder: pi's CLI flag for the variant axis is `--thinking`
// (not `--variant`). The user-facing prism flag is `--variant` to match
// the on-disk profiles.json key name; internally it lands on Opts.Variant
// and buildDirectAgentCmd emits it as `--thinking <Y>` so pi parses it.
// See pi --help: "--thinking <level>  Set thinking level: off, minimal,
// low, medium, high, xhigh".

import (
	"strings"
	"testing"
)

// TestBuildDirectAgentCmd_HostModeModelOverride asserts that Opts.Model
// produces `--model <X>` on the direct pi launch command.
func TestBuildDirectAgentCmd_HostModeModelOverride(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		SessionName: "myrepo@branch",
		HarnessName: "pi",
		Model:       "anthropic/claude-opus-4-8",
	}
	cmd := buildDirectAgentCmd(opts)
	if !strings.Contains(cmd, "--model 'anthropic/claude-opus-4-8'") {
		t.Errorf("expected --model in direct cmd; got %q", cmd)
	}
}

// TestBuildDirectAgentCmd_HostModeVariantOverride asserts that Opts.Variant
// produces `--thinking <Y>` (pi's flag name) on the direct pi launch command.
func TestBuildDirectAgentCmd_HostModeVariantOverride(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		SessionName: "myrepo@branch",
		HarnessName: "pi",
		Variant:     "high",
	}
	cmd := buildDirectAgentCmd(opts)
	if !strings.Contains(cmd, "--thinking 'high'") {
		t.Errorf("expected --thinking 'high' in direct cmd; got %q", cmd)
	}
}

// TestBuildDirectAgentCmd_HostModeBothOverrides asserts the combined case:
// when both Model and Variant are set, both flag pairs appear, and the
// pre-#2086 invariants (pi appears, --agent appears) are preserved.
func TestBuildDirectAgentCmd_HostModeBothOverrides(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		SessionName: "myrepo@branch",
		HarnessName: "pi",
		Model:       "anthropic/claude-opus-4-8",
		Variant:     "high",
	}
	cmd := buildDirectAgentCmd(opts)

	for _, want := range []string{
		"pi ",                                 // binary
		"--agent worker",                      // role flag still emitted
		"--model 'anthropic/claude-opus-4-8'", // override
		"--thinking 'high'",                   // override
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("expected %q in direct cmd; got %q", want, cmd)
		}
	}
}

// TestBuildDirectAgentCmd_HostModeNoOverrides_NoFlags is the no-regression
// guard: when Model and Variant are empty, neither --model nor --thinking
// appears on the host-mode direct command (matching the pre-#2086 shape,
// where pi's own defaults take over). Host mode consults no profile slot at
// all — config.SlotForRole has a single caller, populatePIConfig in
// cmd/agent_run.go, which runs only on the bwrap / sandbox-exec path.
func TestBuildDirectAgentCmd_HostModeNoOverrides_NoFlags(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		SessionName: "myrepo@branch",
		HarnessName: "pi",
	}
	cmd := buildDirectAgentCmd(opts)
	if strings.Contains(cmd, "--model") {
		t.Errorf("--model must not appear when Opts.Model is empty; got %q", cmd)
	}
	if strings.Contains(cmd, "--thinking") {
		t.Errorf("--thinking must not appear when Opts.Variant is empty; got %q", cmd)
	}
}

// TestBuildDirectAgentCmd_NonPiHarness_NoOverrideFlags scopes the override
// emission to the pi harness. Other harnesses do not have --model /
// --thinking flags in the same shape, so we must not append them blindly.
func TestBuildDirectAgentCmd_NonPiHarness_NoOverrideFlags(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		SessionName: "myrepo@branch",
		HarnessName: "opencode", // not pi
		Model:       "anthropic/claude-opus-4-8",
		Variant:     "high",
	}
	cmd := buildDirectAgentCmd(opts)
	if strings.Contains(cmd, "--model") {
		t.Errorf("--model must not appear for non-pi harness; got %q", cmd)
	}
	if strings.Contains(cmd, "--thinking") {
		t.Errorf("--thinking must not appear for non-pi harness; got %q", cmd)
	}
}

// ── issue #2852: the host-mode --provider clause ────────────────────────────
//
// Unlike the variant axis, the provider axis needs no flag-name translation:
// prism's --provider and pi's --provider are the same flag.

// TestBuildDirectAgentCmd_HostModeProviderOverride asserts that Opts.Provider
// produces `--provider <P>` on the direct pi launch command.
func TestBuildDirectAgentCmd_HostModeProviderOverride(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		SessionName: "myrepo@branch",
		HarnessName: "pi",
		Provider:    "openrouter",
	}
	cmd := buildDirectAgentCmd(opts)
	if !strings.Contains(cmd, "--provider 'openrouter'") {
		t.Errorf("expected --provider 'openrouter' in direct cmd; got %q", cmd)
	}
}

// TestBuildDirectAgentCmd_HostModeNoProvider_NoFlag is the #2852
// no-regression guard: an empty Provider must never render a --provider
// argument with an empty value.
func TestBuildDirectAgentCmd_HostModeNoProvider_NoFlag(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		SessionName: "myrepo@branch",
		HarnessName: "pi",
		Model:       "anthropic/claude-opus-4-8",
	}
	cmd := buildDirectAgentCmd(opts)
	if strings.Contains(cmd, "--provider") {
		t.Errorf("--provider must not appear when Opts.Provider is empty; got %q", cmd)
	}
}

// TestBuildDirectAgentCmd_NonPiHarness_NoProviderFlag is the #2852 edge-case
// AC for the host-mode emit site: a non-pi harness never receives --provider.
func TestBuildDirectAgentCmd_NonPiHarness_NoProviderFlag(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		SessionName: "myrepo@branch",
		HarnessName: "opencode", // not pi
		Provider:    "openrouter",
	}
	cmd := buildDirectAgentCmd(opts)
	if strings.Contains(cmd, "--provider") {
		t.Errorf("--provider must not appear for non-pi harness; got %q", cmd)
	}
}

// TestBuildDirectAgentCmd_ProviderBeforePrompt verifies --provider lands
// before any positional prompt, for the same reason --model does: pi would
// otherwise parse the flag as message bytes.
func TestBuildDirectAgentCmd_ProviderBeforePrompt(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		SessionName: "myrepo@branch",
		HarnessName: "pi",
		Provider:    "openrouter",
		Prompt:      "do the thing",
	}
	cmd := buildDirectAgentCmd(opts)
	providerIdx := strings.Index(cmd, "--provider")
	promptIdx := strings.Index(cmd, "--prompt")
	if providerIdx == -1 || promptIdx == -1 {
		t.Fatalf("expected --provider and --prompt both present; got %q", cmd)
	}
	if providerIdx > promptIdx {
		t.Errorf("--provider (at %d) must appear before --prompt (at %d) in %q", providerIdx, promptIdx, cmd)
	}
}

// TestBuildOptsForLayout_ForwardsProvider pins the SpawnOpts → Opts
// forwarding for the provider axis (issue #2852), so a refactor cannot drop
// the field between cmd/spawn.go and the emitters.
func TestBuildOptsForLayout_ForwardsProvider(t *testing.T) {
	spawnOpts := SpawnOpts{
		SessionName: "myrepo@branch",
		Worktree:    "/tmp/wt",
		AgentRole:   "worker",
		Prompt:      "do the thing",
		HarnessName: "pi",
		Provider:    "openrouter",
	}
	got := buildOptsForLayout(spawnOpts, 14000, "")
	if got.Provider != "openrouter" {
		t.Errorf("Opts.Provider = %q, want forwarded value", got.Provider)
	}
}

// TestBuildOptsForLayout_ForwardsModelAndVariant pins the SpawnOpts → Opts
// forwarding so a future refactor cannot silently drop the override fields
// between cmd/spawn.go (which sets SpawnOpts) and buildDirectAgentCmd /
// AgentPaneCmd (which consume Opts.Model / Opts.Variant).
func TestBuildOptsForLayout_ForwardsModelAndVariant(t *testing.T) {
	spawnOpts := SpawnOpts{
		SessionName: "myrepo@branch",
		Worktree:    "/tmp/wt",
		AgentRole:   "worker",
		Prompt:      "do the thing",
		HarnessName: "pi",
		Model:       "anthropic/claude-opus-4-8",
		Variant:     "high",
	}
	got := buildOptsForLayout(spawnOpts, 14000, "")
	if got.Model != "anthropic/claude-opus-4-8" {
		t.Errorf("Opts.Model = %q, want forwarded value", got.Model)
	}
	if got.Variant != "high" {
		t.Errorf("Opts.Variant = %q, want forwarded value", got.Variant)
	}
}

// TestBuildOptsForLayout_ForwardsModelsByRole pins the per-role override map
// on the full-layout mapping, alongside its Provider and Model/Variant
// siblings above. The full layout has always forwarded the field, so this is
// a guard rather than a fix — but the agent-only layout dropped the same
// field (issue #2863), so both mappings now carry a forwarding test.
func TestBuildOptsForLayout_ForwardsModelsByRole(t *testing.T) {
	spawnOpts := SpawnOpts{
		SessionName:  "myrepo@branch",
		Worktree:     "/tmp/wt",
		AgentRole:    "worker",
		Prompt:       "do the thing",
		HarnessName:  "pi",
		ModelsByRole: map[string]string{"worker": "google/gemini-2.5-pro"},
	}
	got := buildOptsForLayout(spawnOpts, 14000, "")
	if got.ModelsByRole["worker"] != "google/gemini-2.5-pro" {
		t.Errorf("Opts.ModelsByRole[worker] = %q, want forwarded value %q",
			got.ModelsByRole["worker"], "google/gemini-2.5-pro")
	}
}

// TestBuildDirectAgentCmd_OverrideFlagsBeforePrompt verifies the flag pair
// appears before any positional prompt — pi treats a positional argument as
// the user message and would parse `--model X` as message bytes if it came
// after the prompt.
func TestBuildDirectAgentCmd_OverrideFlagsBeforePrompt(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		SessionName: "myrepo@branch",
		HarnessName: "pi",
		Model:       "anthropic/claude-opus-4-8",
		Variant:     "high",
		Prompt:      "do the thing",
	}
	cmd := buildDirectAgentCmd(opts)

	modelIdx := strings.Index(cmd, "--model")
	thinkingIdx := strings.Index(cmd, "--thinking")
	promptIdx := strings.Index(cmd, "--prompt")
	if modelIdx == -1 || thinkingIdx == -1 || promptIdx == -1 {
		t.Fatalf("expected --model, --thinking, and --prompt all present; got %q", cmd)
	}
	if modelIdx > promptIdx {
		t.Errorf("--model (at %d) must appear before --prompt (at %d) in %q", modelIdx, promptIdx, cmd)
	}
	if thinkingIdx > promptIdx {
		t.Errorf("--thinking (at %d) must appear before --prompt (at %d) in %q", thinkingIdx, promptIdx, cmd)
	}
}
