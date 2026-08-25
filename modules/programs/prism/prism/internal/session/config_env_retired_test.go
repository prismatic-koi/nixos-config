package session

// config_env_retired_test.go — issue #2854 regression guard.
//
// prism used to prefix a harness config env var onto the agent pane command:
//
//	PI_CONFIG_CONTENT='{"model":…}' pi --agent worker …
//
// pi never read that variable. Issue #2086 found the dead letter and chose to
// retire the transport; #2854 completed the retirement. The single live
// carrier of provider / model / thinking is the argv channel:
// buildDirectAgentCmd on host, and appendAgentRunOverrides →
// populatePIConfig → PIInvocation on bwrap and sandbox-exec.
//
// These tests fail if any code path re-introduces an env-var config prefix
// on the pane command. Each is deliberately shaped as "the prefix is absent
// AND the argv flags are present", so a regression that swaps the argv
// channel back for an env var cannot pass them.
//
// The paired presence assertion is what gives the sandboxed modes real
// signal. BuildAgentCmd routes bwrap and sandbox-exec to AgentPaneCmd, which
// builds `prism agent-run --session <name>` and discards the direct pi
// command entirely — so an absence-only assertion would hold for those modes
// even if buildDirectAgentCmd re-grew the prefix. Note the flag-name change
// across that seam: the pane command carries `--variant`, which
// `prism agent-run` later renders as pi's `--thinking`.

import (
	"strings"
	"testing"
)

// retiredConfigEnvNames are env-var names that must never appear on a
// prism-built agent pane command. PI_CONFIG_CONTENT was the pi-adapter name;
// OPENCODE_CONFIG_CONTENT was the opencode-compat name the interface method
// documented.
var retiredConfigEnvNames = []string{
	"PI_CONFIG_CONTENT",
	"OPENCODE_CONFIG_CONTENT",
	"CONFIG_CONTENT",
}

// assertNoConfigEnvPrefix fails the test when cmd carries any retired
// harness-config env-var assignment.
func assertNoConfigEnvPrefix(t *testing.T, cmd string) {
	t.Helper()
	for _, name := range retiredConfigEnvNames {
		if strings.Contains(cmd, name+"=") {
			t.Errorf("pane command must not carry the retired %s env var (#2854); got %q", name, cmd)
		}
	}
}

// TestBuildDirectAgentCmd_NoConfigEnvVarPrefix asserts that the host-mode
// direct pi launch command carries no harness-config env-var prefix, while
// the argv overrides it replaced are still emitted.
func TestBuildDirectAgentCmd_NoConfigEnvVarPrefix(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		SessionName: "myrepo@branch",
		HarnessName: "pi",
		Model:       "anthropic/claude-opus-4-8",
		Variant:     "high",
		Provider:    "openrouter",
	}
	cmd := buildDirectAgentCmd(opts)

	assertNoConfigEnvPrefix(t, cmd)

	// The argv channel must still carry all three axes.
	for _, want := range []string{
		"--model 'anthropic/claude-opus-4-8'",
		"--thinking 'high'",
		"--provider 'openrouter'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("direct cmd missing %s; got %q", want, cmd)
		}
	}
}

// TestBuildAgentCmd_NoConfigEnvVarPrefix_AllIsolationModes asserts the same
// invariant on the pane command BuildAgentCmd renders for every isolation
// mode, not just host. bwrap and sandbox-exec render a `prism agent-run`
// command; host renders a direct pi invocation.
//
// Each subtest pairs the absence assertion with the mode's own presence
// assertion, so none of the three can pass vacuously.
func TestBuildAgentCmd_NoConfigEnvVarPrefix_AllIsolationModes(t *testing.T) {
	cases := []struct {
		mode string
		// wantFlags are the override flags the mode's pane command must
		// carry. Host renders pi's argv directly and so uses pi's own
		// spelling (--thinking); bwrap and sandbox-exec render the
		// `prism agent-run` command, which spells the same axis --variant.
		wantFlags []string
	}{
		{
			mode:      "host",
			wantFlags: []string{"--model 'anthropic/claude-opus-4-8'", "--thinking 'high'", "--provider 'openrouter'"},
		},
		{
			mode:      "bwrap",
			wantFlags: []string{"--model 'anthropic/claude-opus-4-8'", "--variant 'high'", "--provider 'openrouter'"},
		},
		{
			mode:      "sandbox-exec",
			wantFlags: []string{"--model 'anthropic/claude-opus-4-8'", "--variant 'high'", "--provider 'openrouter'"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			opts := Opts{
				Agent:         "worker",
				SessionName:   "myrepo@branch",
				HarnessName:   "pi",
				IsolationMode: tc.mode,
				Model:         "anthropic/claude-opus-4-8",
				Variant:       "high",
				Provider:      "openrouter",
			}
			cmd, err := BuildAgentCmd(opts)
			if err != nil {
				t.Fatalf("BuildAgentCmd(%s): %v", tc.mode, err)
			}
			assertNoConfigEnvPrefix(t, cmd)
			for _, want := range tc.wantFlags {
				if !strings.Contains(cmd, want) {
					t.Errorf("%s pane command missing %s — the argv channel that replaced the env var must still carry it; got %q",
						tc.mode, want, cmd)
				}
			}
		})
	}
}
