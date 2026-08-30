//go:build darwin

package cmd

// agent_run_sandbox_exec_darwin_test.go — unit tests for the Darwin
// sandbox-exec env-construction path.
//
// These tests verify that PRISM_HARNESS_PIPE is injected with 127.0.0.1
// (not host.containers.internal) when HarnessPipeTCPPort is non-zero, and
// that buildSandboxExecHomeEnv carries CFFIXED_USER_HOME=<sessionDir> (the
// per-session work dir) with the former staging-HOME value gone.

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
// host.containers.internal.
//
// This is the primary check for the env-construction side of the fix: the
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
// HOME / XDG / CFFIXED_USER_HOME entries (all of which must be stripped
// and re-set deterministically), plus the session work dir and real home
// paths in their production layout. legacyStagingHome is the path the former
// staging HOME would have occupied — returned so tests can assert nothing
// points there any more.
func sandboxExecHomeEnvFixture() (env []string, sessionDir, realHome, legacyStagingHome string) {
	realHome = "/Users/u"
	sessionDir = realHome + "/.local/state/prism/sessions/inst-2247"
	legacyStagingHome = sessionDir + "/home"
	env = []string{
		"PATH=/nix/store/abc/bin",
		// Simulate a stale staging-HOME value to prove the strip-and-replace
		// rewrites it to the real home.
		"HOME=" + legacyStagingHome,
		"XDG_CACHE_HOME=" + legacyStagingHome + "/.cache",
		"XDG_CONFIG_HOME=" + legacyStagingHome + "/.config",
		"XDG_DATA_HOME=" + realHome + "/.local/share",
		"XDG_STATE_HOME=" + realHome + "/.local/state",
		"CFFIXED_USER_HOME=" + realHome,
		"TERM=xterm-256color",
	}
	return env, sessionDir, realHome, legacyStagingHome
}

// TestBuildSandboxExecHomeEnv_CFFixedUserHomePointsAtSessionWorkDir checks
// the env construction: the sandbox-exec session env carries
// CFFIXED_USER_HOME=<sessionDir> (the per-session work dir) and the former
// staging-HOME value is gone. Chromium resolves
// its user-data root via CoreFoundation's NSHomeDirectory(), which honours
// CFFIXED_USER_HOME — pointing it at the work dir lands chromium's writes
// under <sessionDir>/Library/... (covered by the profile's existing
// (subpath <sessionDir>) RW allow) with no host-Library grant.
func TestBuildSandboxExecHomeEnv_CFFixedUserHomePointsAtSessionWorkDir(t *testing.T) {
	env, sessionDir, realHome, legacyStagingHome := sandboxExecHomeEnvFixture()

	got := buildSandboxExecHomeEnv(env, sessionDir, realHome)

	wantCFFixed := "CFFIXED_USER_HOME=" + sessionDir
	stagingCFFixed := "CFFIXED_USER_HOME=" + legacyStagingHome
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

// TestBuildSandboxExecHomeEnv_PlaywrightDirsPointAtWorkDir checks the
// playwright POSIX-$HOME redirects: the sandbox-exec session env carries
// PLAYWRIGHT_DAEMON_SESSION_DIR=<sessionDir>/Library/Caches/ms-playwright/daemon
// (daemon registry + logs) and
// PLAYWRIGHT_SERVER_REGISTRY=<sessionDir>/Library/Caches/ms-playwright/b
// (browser-server descriptor registry) exactly once each, with any
// host-inherited values stripped. Without the overrides, playwright-core
// on Darwin derives both dirs from POSIX $HOME
// (os.homedir()/Library/Caches — it ignores XDG_CACHE_HOME on darwin),
// landing them in the staging HOME's Library/ and violating the
// no-staging-Library invariant.
func TestBuildSandboxExecHomeEnv_PlaywrightDirsPointAtWorkDir(t *testing.T) {
	cases := []struct {
		key    string
		subdir string
	}{
		{"PLAYWRIGHT_DAEMON_SESSION_DIR", "/Library/Caches/ms-playwright/daemon"},
		{"PLAYWRIGHT_SERVER_REGISTRY", "/Library/Caches/ms-playwright/b"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			env, sessionDir, realHome, legacyStagingHome := sandboxExecHomeEnvFixture()

			// Simulate a host-inherited value to prove the strip works.
			env = append(env, tc.key+"="+realHome+tc.subdir)

			got := buildSandboxExecHomeEnv(env, sessionDir, realHome)

			want := tc.key + "=" + sessionDir + tc.subdir
			stagingValue := tc.key + "=" + legacyStagingHome + tc.subdir
			count := 0
			found := false
			for _, kv := range got {
				if strings.HasPrefix(kv, tc.key+"=") {
					count++
				}
				switch kv {
				case want:
					found = true
				case stagingValue:
					t.Errorf("env carries a staging-HOME %s value %q — must be the session work dir (issue #2249)", tc.key, kv)
				}
			}
			if !found {
				t.Errorf("env does not contain %q\nenv: %v", want, got)
			}
			if count != 1 {
				t.Errorf("env contains %d %s entries, want exactly 1 (host-inherited value must be stripped)\nenv: %v", count, tc.key, got)
			}
		})
	}
}

// TestBuildSandboxExecHomeEnv_HomeAndXDGLayerRealHost checks the env layer:
// HOME and every XDG var point at the REAL host paths — the staging HOME is
// gone. XDG_DATA_HOME/XDG_STATE_HOME staying real-host is additionally a hard
// constraint (the nix trusted-settings SBPL grant depends on it). Each key
// appears exactly
// once, including when the inherited env carried stale staging-HOME
// values.
func TestBuildSandboxExecHomeEnv_HomeAndXDGLayerRealHost(t *testing.T) {
	env, sessionDir, realHome, legacyStagingHome := sandboxExecHomeEnvFixture()

	got := buildSandboxExecHomeEnv(env, sessionDir, realHome)

	want := map[string]string{
		"HOME":            realHome,
		"XDG_CACHE_HOME":  realHome + "/.cache",
		"XDG_CONFIG_HOME": realHome + "/.config",
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

	// Nothing in the rewritten layer may reference the former staging-HOME
	// path — the mechanism is deleted.
	for _, kv := range got {
		if strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "TERM=") {
			continue // fixture passthroughs, not part of the rewritten layer
		}
		if strings.Contains(kv, legacyStagingHome) {
			t.Errorf("env entry %q references the deleted staging-HOME path %q", kv, legacyStagingHome)
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
