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

// withFakePrismBinary stubs prismBinaryPathFn for the duration of t so
// AgentPaneCmd renders a deterministic absolute path instead of os.Executable()'s
// runtime value (which is the go-test binary under `go test`). Restored on
// cleanup. This is the canonical test fixture for any case that needs to
// assert the rendered command contains a specific binary path.
func withFakePrismBinary(t *testing.T, path string) {
	t.Helper()
	orig := prismBinaryPathFn
	prismBinaryPathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { prismBinaryPathFn = orig })
}

// withErrorPrismBinary stubs prismBinaryPathFn to return an error so
// AgentPaneCmd exercises the os.Executable-failed branch. Restored on cleanup.
func withErrorPrismBinary(t *testing.T, err error) {
	t.Helper()
	orig := prismBinaryPathFn
	prismBinaryPathFn = func() (string, error) { return "", err }
	t.Cleanup(func() { prismBinaryPathFn = orig })
}

// TestBwrapAgentPaneCmd_OverrideFlagsAppended asserts that --model and
// --variant land on the tmux pane command when AgentPaneOpts carries them.
func TestBwrapAgentPaneCmd_OverrideFlagsAppended(t *testing.T) {
	withFakePrismBinary(t, "/nix/store/abcd-prism/bin/prism")
	iso := &bwrapIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
		Model:       "anthropic/claude-opus-4-8",
		Variant:     "high",
	})
	if err != nil {
		t.Fatalf("bwrap AgentPaneCmd: unexpected error: %v", err)
	}

	// Order is load-bearing for readability of `ps` output but not for
	// cobra's flag parser. Use substring checks so a reordering inside
	// appendAgentRunOverrides does not break the test (cobra still parses
	// the flags correctly regardless of position).
	for _, want := range []string{
		"'/nix/store/abcd-prism/bin/prism' agent-run --session 'prism-test@bwrap'",
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
	withFakePrismBinary(t, "/nix/store/abcd-prism/bin/prism")
	iso := &bwrapIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
	})
	if err != nil {
		t.Fatalf("bwrap AgentPaneCmd: unexpected error: %v", err)
	}
	want := "'/nix/store/abcd-prism/bin/prism' agent-run --session 'prism-test@bwrap'"
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
	withFakePrismBinary(t, "/nix/store/abcd-prism/bin/prism")
	iso := &sandboxExecIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@sbx",
		Model:       "anthropic/claude-opus-4-8",
		Variant:     "high",
	})
	if err != nil {
		t.Fatalf("sandbox-exec AgentPaneCmd: unexpected error: %v", err)
	}
	for _, want := range []string{
		"'/nix/store/abcd-prism/bin/prism' agent-run --session 'prism-test@sbx'",
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
	withFakePrismBinary(t, "/nix/store/abcd-prism/bin/prism")
	iso := &sandboxExecIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@sbx",
	})
	if err != nil {
		t.Fatalf("sandbox-exec AgentPaneCmd: unexpected error: %v", err)
	}
	want := "'/nix/store/abcd-prism/bin/prism' agent-run --session 'prism-test@sbx'"
	if got != want {
		t.Errorf("sandbox-exec AgentPaneCmd = %q, want %q", got, want)
	}
}

// TestBwrapAgentPaneCmd_PartialOverride_ModelOnly verifies that supplying
// only Model (no Variant) emits --model but not --variant. Mirrors the
// operator workflow where one axis is varied for an A/B test.
func TestBwrapAgentPaneCmd_PartialOverride_ModelOnly(t *testing.T) {
	withFakePrismBinary(t, "/nix/store/abcd-prism/bin/prism")
	iso := &bwrapIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
		Model:       "anthropic/claude-opus-4-8",
	})
	if err != nil {
		t.Fatalf("bwrap AgentPaneCmd: unexpected error: %v", err)
	}
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
	withFakePrismBinary(t, "/nix/store/abcd-prism/bin/prism")
	iso := &bwrapIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
		// A value containing a single quote exercises the escape path in
		// shellQuoteContainer.
		Model: "danger'name",
	})
	if err != nil {
		t.Fatalf("bwrap AgentPaneCmd: unexpected error: %v", err)
	}
	want := `--model 'danger'\''name'`
	if !strings.Contains(got, want) {
		t.Errorf("expected shell-escaped Model %q in cmd; got %q", want, got)
	}
}

// ── issue #2852: the --provider clause ────────────────────────────────────────

// TestBwrapAgentPaneCmd_ProviderFlagAppended asserts that AgentPaneOpts.Provider
// lands on the tmux pane command as `--provider <P>` for a pi harness, so
// `prism agent-run` can override the profile slot's provider on pi's argv.
func TestBwrapAgentPaneCmd_ProviderFlagAppended(t *testing.T) {
	withFakePrismBinary(t, "/nix/store/abcd-prism/bin/prism")
	iso := &bwrapIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
		Provider:    "openrouter",
		HarnessName: "pi",
	})
	if err != nil {
		t.Fatalf("bwrap AgentPaneCmd: unexpected error: %v", err)
	}
	if !strings.Contains(got, "--provider 'openrouter'") {
		t.Errorf("expected --provider in pane cmd; got %q", got)
	}
}

// TestSandboxExecAgentPaneCmd_ProviderFlagAppended mirrors the bwrap case —
// both isolators share appendAgentRunOverrides, so a divergence is a real bug.
func TestSandboxExecAgentPaneCmd_ProviderFlagAppended(t *testing.T) {
	withFakePrismBinary(t, "/nix/store/abcd-prism/bin/prism")
	iso := &sandboxExecIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@sbx",
		Provider:    "openrouter",
		HarnessName: "pi",
	})
	if err != nil {
		t.Fatalf("sandbox-exec AgentPaneCmd: unexpected error: %v", err)
	}
	if !strings.Contains(got, "--provider 'openrouter'") {
		t.Errorf("expected --provider in pane cmd; got %q", got)
	}
}

// TestBwrapAgentPaneCmd_ProviderDefaultsToPiHarness verifies that an empty
// HarnessName counts as pi, matching every other pi-scoped clause in the
// launch path (harnessBinary, the --extension gate, the --exclude-tools gate).
func TestBwrapAgentPaneCmd_ProviderDefaultsToPiHarness(t *testing.T) {
	withFakePrismBinary(t, "/nix/store/abcd-prism/bin/prism")
	iso := &bwrapIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
		Provider:    "openrouter",
	})
	if err != nil {
		t.Fatalf("bwrap AgentPaneCmd: unexpected error: %v", err)
	}
	if !strings.Contains(got, "--provider 'openrouter'") {
		t.Errorf("empty HarnessName must be treated as pi; got %q", got)
	}
}

// TestBwrapAgentPaneCmd_ProviderSuppressedForNonPiHarness is the #2852
// edge-case AC: no --provider emit site fires for a non-pi harness. `prism
// spawn` already rejects that combination up front, so reaching this branch
// means an internal caller built a mismatched AgentPaneOpts — the flag must
// still be withheld rather than handed to a harness that would read it with
// an unrelated meaning.
func TestBwrapAgentPaneCmd_ProviderSuppressedForNonPiHarness(t *testing.T) {
	withFakePrismBinary(t, "/nix/store/abcd-prism/bin/prism")
	iso := &bwrapIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
		Provider:    "openrouter",
		HarnessName: "not-pi",
	})
	if err != nil {
		t.Fatalf("bwrap AgentPaneCmd: unexpected error: %v", err)
	}
	if strings.Contains(got, "--provider") {
		t.Errorf("--provider must not be emitted for a non-pi harness; got %q", got)
	}
}

// TestBwrapAgentPaneCmd_EmptyProviderEmitsNoFlag is the #2852 no-regression
// case: an empty override must never render a blank --provider argument with
// an empty value, which pi would read as an explicit (and invalid) provider
// name.
func TestBwrapAgentPaneCmd_EmptyProviderEmitsNoFlag(t *testing.T) {
	withFakePrismBinary(t, "/nix/store/abcd-prism/bin/prism")
	iso := &bwrapIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
		HarnessName: "pi",
		Model:       "anthropic/claude-opus-4-8",
	})
	if err != nil {
		t.Fatalf("bwrap AgentPaneCmd: unexpected error: %v", err)
	}
	if strings.Contains(got, "--provider") {
		t.Errorf("--provider must be absent when Provider is empty; got %q", got)
	}
}

// TestAppendAgentRunOverrides_ProviderShellQuoted ensures the provider value
// goes through shellQuoteContainer like the other overrides — the pane command
// is wrapped in `sh -c` by tmux, so an unquoted value is an injection risk.
func TestAppendAgentRunOverrides_ProviderShellQuoted(t *testing.T) {
	got := appendAgentRunOverrides("prism agent-run", AgentPaneOpts{
		Provider:    "danger'name",
		HarnessName: "pi",
	})
	want := `--provider 'danger'\''name'`
	if !strings.Contains(got, want) {
		t.Errorf("expected shell-escaped Provider %q in cmd; got %q", want, got)
	}
}

// TestIsPIHarness pins the single predicate every pi-only emit site consults.
func TestIsPIHarness(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"pi", true},
		{"", true},
		{"not-pi", false},
		{"PI", false},
	} {
		if got := IsPIHarness(tc.name); got != tc.want {
			t.Errorf("IsPIHarness(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestBwrapAgentPaneCmd_EmptySessionName_FallsBackToDirect verifies the
// defensive fallback: when SessionName is empty the pane command is the
// host-mode DirectCmd unchanged. Pre-#2086 behaviour; the override fields
// must not be appended onto the DirectCmd fallback (those flags are owned
// by `prism agent-run`, not by the direct pi launch).
func TestBwrapAgentPaneCmd_EmptySessionName_FallsBackToDirect(t *testing.T) {
	iso := &bwrapIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		DirectCmd: "pi --agent worker",
		Model:     "anthropic/claude-opus-4-8",
		Variant:   "high",
	})
	if err != nil {
		t.Fatalf("bwrap AgentPaneCmd: unexpected error: %v", err)
	}
	if got != "pi --agent worker" {
		t.Errorf("expected fallback to DirectCmd; got %q", got)
	}
}
