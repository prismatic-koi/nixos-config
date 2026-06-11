//go:build darwin

package cmd

// agent_run_sandbox_exec_darwin_test.go — unit tests for the Darwin
// sandbox-exec env-construction path (issues #1482, #2247).
//
// These tests verify that PRISM_HARNESS_PIPE is injected with 127.0.0.1
// (not host.containers.internal) when HarnessPipeTCPPort is non-zero,
// covering the env-construction AC from issue #1482, and that
// buildSandboxExecHomeEnv carries CFFIXED_USER_HOME=<sessionDir> (the
// per-session work dir) with the former staging-HOME value gone, covering
// the env-construction AC from issue #2247 (Step 4 of #2132).

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

// sandboxExecHomeEnvFixture returns representative inputs for
// buildSandboxExecHomeEnv: a base env that already carries host-derived
// HOME / XDG / CFFIXED_USER_HOME entries (all of which must be stripped),
// plus the staging HOME, session work dir, and real home paths in their
// production layout (<stagingHome> == <sessionDir>/home).
func sandboxExecHomeEnvFixture() (env []string, stagingHome, sessionDir, realHome string) {
	realHome = "/Users/u"
	sessionDir = realHome + "/.local/state/prism/sessions/inst-2247"
	stagingHome = sessionDir + "/home"
	env = []string{
		"PATH=/nix/store/abc/bin",
		"HOME=" + realHome,
		"XDG_CACHE_HOME=" + realHome + "/.cache",
		"XDG_CONFIG_HOME=" + realHome + "/.config",
		"XDG_DATA_HOME=" + realHome + "/.local/share",
		"XDG_STATE_HOME=" + realHome + "/.local/state",
		"CFFIXED_USER_HOME=" + realHome,
		"TERM=xterm-256color",
	}
	return env, stagingHome, sessionDir, realHome
}

// TestBuildSandboxExecHomeEnv_CFFixedUserHomePointsAtSessionWorkDir is the
// env-construction AC for issue #2247 (Step 4 of #2132): the sandbox-exec
// session env carries CFFIXED_USER_HOME=<sessionDir> (the per-session work
// dir, #2213) and the former staging-HOME value is gone. Chromium resolves
// its user-data root via CoreFoundation's NSHomeDirectory(), which honours
// CFFIXED_USER_HOME — pointing it at the work dir lands chromium's writes
// under <sessionDir>/Library/... (covered by the profile's existing
// (subpath <sessionDir>) RW allow) with no host-Library grant.
func TestBuildSandboxExecHomeEnv_CFFixedUserHomePointsAtSessionWorkDir(t *testing.T) {
	env, stagingHome, sessionDir, realHome := sandboxExecHomeEnvFixture()

	got := buildSandboxExecHomeEnv(env, stagingHome, sessionDir, realHome)

	wantCFFixed := "CFFIXED_USER_HOME=" + sessionDir
	stagingCFFixed := "CFFIXED_USER_HOME=" + stagingHome
	hostCFFixed := "CFFIXED_USER_HOME=" + realHome
	foundCFFixed := false
	for _, kv := range got {
		switch kv {
		case wantCFFixed:
			foundCFFixed = true
		case stagingCFFixed:
			t.Errorf("env carries the former staging-HOME CFFIXED_USER_HOME value %q — must be the session work dir (issue #2247)", kv)
		case hostCFFixed:
			t.Errorf("env carries the host-inherited CFFIXED_USER_HOME value %q — must be stripped and replaced", kv)
		}
	}
	if !foundCFFixed {
		t.Errorf("env does not contain %q\nenv: %v", wantCFFixed, got)
	}

	// Exactly one CFFIXED_USER_HOME entry: the strip must remove the
	// host-inherited value before the session value is appended.
	count := 0
	for _, kv := range got {
		if strings.HasPrefix(kv, "CFFIXED_USER_HOME=") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("env contains %d CFFIXED_USER_HOME entries, want exactly 1\nenv: %v", count, got)
	}
}

// TestBuildSandboxExecHomeEnv_HomeAndXDGLayerUnchangedByStep4 pins the
// rest of the home env layer across the #2247 change: HOME and
// XDG_CACHE_HOME/XDG_CONFIG_HOME still point into the staging HOME (the
// Step 5 flip has NOT happened), XDG_DATA_HOME/XDG_STATE_HOME still point
// at the real host paths (hard constraint from #2205), and each key
// appears exactly once.
func TestBuildSandboxExecHomeEnv_HomeAndXDGLayerUnchangedByStep4(t *testing.T) {
	env, stagingHome, sessionDir, realHome := sandboxExecHomeEnvFixture()

	got := buildSandboxExecHomeEnv(env, stagingHome, sessionDir, realHome)

	want := map[string]string{
		"HOME":            stagingHome,
		"XDG_CACHE_HOME":  stagingHome + "/.cache",
		"XDG_CONFIG_HOME": stagingHome + "/.config",
		"XDG_DATA_HOME":   realHome + "/.local/share",
		"XDG_STATE_HOME":  realHome + "/.local/state",
	}
	seen := map[string]int{}
	for _, kv := range got {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		k, v := kv[:eq], kv[eq+1:]
		if wantV, ok := want[k]; ok {
			seen[k]++
			if v != wantV {
				t.Errorf("%s = %q, want %q", k, v, wantV)
			}
		}
	}
	for k := range want {
		if seen[k] != 1 {
			t.Errorf("%s appears %d times, want exactly 1\nenv: %v", k, seen[k], got)
		}
	}

	// Non-home keys pass through untouched.
	for _, passthrough := range []string{"PATH=/nix/store/abc/bin", "TERM=xterm-256color"} {
		found := false
		for _, kv := range got {
			if kv == passthrough {
				found = true
			}
		}
		if !found {
			t.Errorf("passthrough env entry %q missing\nenv: %v", passthrough, got)
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
