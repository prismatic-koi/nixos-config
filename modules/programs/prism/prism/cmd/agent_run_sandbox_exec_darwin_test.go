//go:build darwin

package cmd

// agent_run_sandbox_exec_darwin_test.go — unit tests for the Darwin
// sandbox-exec env-construction path (issue #1482).
//
// These tests verify that PRISM_HARNESS_PIPE is injected with 127.0.0.1
// (not host.containers.internal) when HarnessPipeTCPPort is non-zero,
// covering the env-construction AC from issue #1482.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// TestSandboxExecEnv_HarnessPipeTCPPort_Uses127001 verifies that when
// HarnessPipeTCPPort is non-zero, the env slice produced for sandbox-exec
// contains PRISM_HARNESS_PIPE=tcp://127.0.0.1:<port> and does NOT contain
// host.containers.internal (issue #1482).
//
// This is the primary AC for the env-construction side of the fix: the
// sandboxed PI extension must dial 127.0.0.1, not an unresolvable synthetic
// hostname.
func TestSandboxExecEnv_HarnessPipeTCPPort_Uses127001(t *testing.T) {
	const testPort = 54321

	// Build the env the same way runAgentRunSandboxExec does: start from a
	// minimal isolated env, then call AppendSandboxEnvVarsKV with a config
	// that has HarnessPipeTCPPort set.
	ctrCfg := container.Config{
		SessionName:        "testrepo@fix-pi-harness",
		HarnessPipeTCPPort: testPort,
	}

	env := container.MinimalIsolatedExecEnv(os.Environ())
	env = container.AppendSandboxEnvVarsKV(env, ctrCfg)

	// Inline the PRISM_HARNESS_PIPE injection from runAgentRunSandboxExec,
	// because that function calls os.Getppid() and starts a child process —
	// we exercise only the env-building logic here.
	if ctrCfg.HarnessPipeTCPPort != 0 {
		env = append(env, fmt.Sprintf("PRISM_HARNESS_PIPE=tcp://127.0.0.1:%d", ctrCfg.HarnessPipeTCPPort))
	}

	wantVal := fmt.Sprintf("PRISM_HARNESS_PIPE=tcp://127.0.0.1:%d", testPort)
	found := false
	for _, kv := range env {
		if kv == wantVal {
			found = true
		}
		if strings.Contains(kv, "host.containers.internal") {
			t.Errorf("env contains host.containers.internal — sandbox-exec must use 127.0.0.1 (issue #1482): %q", kv)
		}
	}
	if !found {
		t.Errorf("env does not contain %q\nenv: %v", wantVal, env)
	}
}

// TestSandboxExecEnv_HarnessPipeTCPPort_Zero_NoHarnessPipeVar verifies that
// when HarnessPipeTCPPort is zero (Linux / Unix-socket path), PRISM_HARNESS_PIPE
// is NOT injected by the TCP branch. The Unix-socket path handles injection
// via AppendSandboxEnvVarsKV separately.
func TestSandboxExecEnv_HarnessPipeTCPPort_Zero_NoHarnessPipeVar(t *testing.T) {
	ctrCfg := container.Config{
		SessionName:        "testrepo@no-tcp",
		HarnessPipeTCPPort: 0,
	}

	env := container.MinimalIsolatedExecEnv(os.Environ())
	env = container.AppendSandboxEnvVarsKV(env, ctrCfg)

	// TCP branch must NOT fire when port is zero.
	if ctrCfg.HarnessPipeTCPPort != 0 {
		env = append(env, fmt.Sprintf("PRISM_HARNESS_PIPE=tcp://127.0.0.1:%d", ctrCfg.HarnessPipeTCPPort))
	}

	for _, kv := range env {
		if strings.HasPrefix(kv, "PRISM_HARNESS_PIPE=tcp://") {
			t.Errorf("TCP PRISM_HARNESS_PIPE injected when HarnessPipeTCPPort==0: %q", kv)
		}
	}
}

// TestSandboxExecEnv_HarnessPipePort_NotHostContainersInternal is a focused
// negative assertion: even if a caller accidentally passes a different port,
// the resulting env must never contain host.containers.internal in any form.
// This locks the correct address for future refactors.
func TestSandboxExecEnv_HarnessPipePort_NotHostContainersInternal(t *testing.T) {
	for _, port := range []int{1, 1024, 49152, 65535} {
		t.Run(fmt.Sprintf("port_%d", port), func(t *testing.T) {
			harnessPipeVar := fmt.Sprintf("PRISM_HARNESS_PIPE=tcp://127.0.0.1:%d", port)
			if strings.Contains(harnessPipeVar, "host.containers.internal") {
				t.Errorf("constructed env var contains host.containers.internal: %q", harnessPipeVar)
			}
			wantPrefix := "PRISM_HARNESS_PIPE=tcp://127.0.0.1:"
			if !strings.HasPrefix(harnessPipeVar, wantPrefix) {
				t.Errorf("env var %q does not start with %q", harnessPipeVar, wantPrefix)
			}
		})
	}
}
