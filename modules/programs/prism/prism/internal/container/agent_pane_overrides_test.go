package container

// agent_pane_overrides_test.go — issue #2086 regression test for the tmux
// pane command that bwrap and sandbox-exec emit when `prism spawn --model`
// and/or `--variant` are supplied.
//
// The shape under test is the output of `iso.AgentPaneCmd(...)` for the
// pane-owned modes. The pre-#2086 shape was:
//
//	prism agent-run --session '<X>'
//
// The post-#2086 shape, when AgentPaneOpts.Model and/or .Variant are set,
// appends the override flags:
//
//	prism agent-run --session '<X>' --model '<M>' --variant '<V>'
//
// `prism agent-run` then parses --model / --variant and threads the values
// into populatePIConfig, where they override the active profile slot's
// Model / Thinking on the final pi argv (see TestPIInvocation_CLIOverride*
// and TestPopulatePIConfig_CLIOverrideWins).

import (
	"strings"
	"testing"
)

// TestBwrapAgentPaneCmd_OverrideFlagsAppended asserts that --model and
// --variant land on the tmux pane command when AgentPaneOpts carries them.
func TestBwrapAgentPaneCmd_OverrideFlagsAppended(t *testing.T) {
	iso := &bwrapIsolator{}
	got := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
		Model:       "anthropic/claude-opus-4-8",
		Variant:     "high",
	})

	// Order is load-bearing for readability of `ps` output but not for
	// cobra's flag parser. Use substring checks so a reordering inside
	// appendAgentRunOverrides does not break the test (cobra still parses
	// the flags correctly regardless of position).
	for _, want := range []string{
		"prism agent-run --session 'prism-test@bwrap'",
		"--model 'anthropic/claude-opus-4-8'",
		"--variant 'high'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bwrap AgentPaneCmd missing %q; got %q", want, got)
		}
	}
}

// TestBwrapAgentPaneCmd_NoOverrideFlagsByDefault is the no-regression case:
// when AgentPaneOpts carries no override fields the pane command is exactly
// the pre-#2086 shape so existing tests, restore semantics, and operator
// expectations are preserved.
func TestBwrapAgentPaneCmd_NoOverrideFlagsByDefault(t *testing.T) {
	iso := &bwrapIsolator{}
	got := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
	})
	want := "prism agent-run --session 'prism-test@bwrap'"
	if got != want {
		t.Errorf("bwrap AgentPaneCmd = %q, want %q", got, want)
	}
	if strings.Contains(got, "--model") || strings.Contains(got, "--variant") {
		t.Errorf("expected no --model/--variant when overrides empty; got %q", got)
	}
}

// TestSandboxExecAgentPaneCmd_OverrideFlagsAppended mirrors the bwrap test
// for sandbox-exec — both isolators dispatch through the same
// appendAgentRunOverrides helper so a divergence would be a real bug.
func TestSandboxExecAgentPaneCmd_OverrideFlagsAppended(t *testing.T) {
	iso := &sandboxExecIsolator{}
	got := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@sbx",
		Model:       "anthropic/claude-opus-4-8",
		Variant:     "high",
	})
	for _, want := range []string{
		"prism agent-run --session 'prism-test@sbx'",
		"--model 'anthropic/claude-opus-4-8'",
		"--variant 'high'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sandbox-exec AgentPaneCmd missing %q; got %q", want, got)
		}
	}
}

// TestSandboxExecAgentPaneCmd_NoOverrideFlagsByDefault — no-regression case.
func TestSandboxExecAgentPaneCmd_NoOverrideFlagsByDefault(t *testing.T) {
	iso := &sandboxExecIsolator{}
	got := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@sbx",
	})
	want := "prism agent-run --session 'prism-test@sbx'"
	if got != want {
		t.Errorf("sandbox-exec AgentPaneCmd = %q, want %q", got, want)
	}
}

// TestBwrapAgentPaneCmd_PartialOverride_ModelOnly verifies that supplying
// only Model (no Variant) emits --model but not --variant. Mirrors the
// operator workflow where one axis is varied for an A/B test.
func TestBwrapAgentPaneCmd_PartialOverride_ModelOnly(t *testing.T) {
	iso := &bwrapIsolator{}
	got := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
		Model:       "anthropic/claude-opus-4-8",
	})
	if !strings.Contains(got, "--model 'anthropic/claude-opus-4-8'") {
		t.Errorf("expected --model in pane cmd; got %q", got)
	}
	if strings.Contains(got, "--variant") {
		t.Errorf("--variant must not appear when Variant is empty; got %q", got)
	}
}

// TestBwrapAgentPaneCmd_ShellQuoting_SingleQuotedValues ensures values pass
// through shellQuoteContainer so weird characters in a model name (or in the
// session name) cannot break out of the shell context. Cobra parses argv
// regardless, but the tmux pane wraps the command in `sh -c "<cmd>"` so
// unquoted values are a real injection risk.
func TestBwrapAgentPaneCmd_ShellQuoting_SingleQuotedValues(t *testing.T) {
	iso := &bwrapIsolator{}
	got := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
		// A value containing a single quote exercises the escape path in
		// shellQuoteContainer.
		Model: "danger'name",
	})
	want := `--model 'danger'\''name'`
	if !strings.Contains(got, want) {
		t.Errorf("expected shell-escaped Model %q in cmd; got %q", want, got)
	}
}

// TestBwrapAgentPaneCmd_EmptySessionName_FallsBackToDirect verifies the
// defensive fallback: when SessionName is empty the pane command is the
// host-mode DirectCmd unchanged. Pre-#2086 behaviour; the override fields
// must not be appended onto the DirectCmd fallback (those flags are owned
// by `prism agent-run`, not by the direct pi launch).
func TestBwrapAgentPaneCmd_EmptySessionName_FallsBackToDirect(t *testing.T) {
	iso := &bwrapIsolator{}
	got := iso.AgentPaneCmd(AgentPaneOpts{
		DirectCmd: "pi --agent worker",
		Model:     "anthropic/claude-opus-4-8",
		Variant:   "high",
	})
	if got != "pi --agent worker" {
		t.Errorf("expected fallback to DirectCmd; got %q", got)
	}
}
