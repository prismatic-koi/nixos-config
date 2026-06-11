package cmd

// Suite-wide sandbox-env isolation for the cmd package (issue #2217).
//
// The prism sidecar injects a set of PRISM_* environment variables into every
// agent session it spawns (see cmd/agent_run_sandbox_exec_darwin.go and
// internal/container/container.go). Those variables are UNSET on CI and on a
// developer host shell, but SET inside prism worker sandboxes — so any test
// that exercises a code path consulting them behaves differently across the
// two environments unless it pins them with t.Setenv. Production code in
// cmd/ reads several of them:
//
//   - PRISM_HOST_API   → the proxy-to-host branch in ~20 commands (checkin,
//     cleanup, close, db, escalate, feedback, investigate, list-sessions,
//     logs, merge, pr, prompt, review, spawn, stats, switch, …). Inherited
//     from a sandbox, it points at the LIVE host API socket — a test that
//     reaches the proxy branch unpinned would talk to the real sidecar.
//   - PRISM_SESSION_NAME → session auto-detection (escalate, feedback,
//     investigate, mute, profile, sessions; review.LookupParentSession).
//   - PRISM_SPAWN_PATH / PRISM_BARE_ROOT → cwd/repo fallbacks (checkin
//     --list, list-sessions, spawn).
//   - PRISM_HARNESS_PIPE → not read by Go code today; cleared so the suite
//     controls the full surface the sidecar injects.
//
// As with the tmux isolation (#2214), per-test t.Setenv is not a structural
// fix: it protects only the tests that remembered it. The suite-wide
// neutralisation below, applied by TestMain before m.Run(), unsets the whole
// injected set so the suite sees the same environment on CI, developer
// hosts, and inside worker sandboxes. Tests that need a specific value
// (e.g. pointing PRISM_HOST_API at a mock unix server) still override it
// per-test with t.Setenv, which takes precedence and is restored
// automatically.
//
// An empirical poison-run before this isolation (all five variables set to
// unreachable/poisoned values) showed no currently-failing test — the
// exposure today is latent, not active. The TestMain pin closes the class
// structurally rather than relying on each future test remembering the
// boilerplate.

import (
	"os"
	"testing"
)

// sandboxInjectedPrismEnvVars is the set of PRISM_* variables the sidecar
// injects into agent sessions. Source of truth: the env assembly in
// cmd/agent_run_sandbox_exec_darwin.go (sandbox-exec path) and
// internal/container/container.go (bwrap path).
var sandboxInjectedPrismEnvVars = []string{
	"PRISM_SESSION_NAME",
	"PRISM_HOST_API",
	"PRISM_SPAWN_PATH",
	"PRISM_BARE_ROOT",
	"PRISM_HARNESS_PIPE",
}

// isolateSuiteFromSandboxEnv unsets the sidecar-injected PRISM_* variables
// (matching CI, where they are unset) and returns a restore function that
// puts the original environment back. TestMain calls restore after m.Run()
// for symmetry with the tmux isolation (#2214).
func isolateSuiteFromSandboxEnv() (restore func()) {
	type saved struct {
		val string
		had bool
	}
	origs := make(map[string]saved, len(sandboxInjectedPrismEnvVars))
	for _, k := range sandboxInjectedPrismEnvVars {
		v, ok := os.LookupEnv(k)
		origs[k] = saved{val: v, had: ok}
		os.Unsetenv(k)
	}
	return func() {
		for k, s := range origs {
			if s.had {
				os.Setenv(k, s.val)
			} else {
				os.Unsetenv(k)
			}
		}
	}
}

// TestSuiteSandboxEnvIsolation_NotInherited is the regression guard for the
// #2217 env-inheritance class. Under the TestMain-level neutralisation, none
// of the sidecar-injected PRISM_* variables may be visible to tests that do
// not set them explicitly.
//
// On CI and developer hosts (variables unset) this passes trivially. Inside
// a prism worker sandbox (variables set by the sidecar) it fails if — and
// only if — the suite isolation is removed, which is precisely the
// regression it exists to catch. Verified non-vacuous by disabling the
// isolateSuiteFromSandboxEnv call in TestMain and observing this test fail
// inside a worker sandbox.
func TestSuiteSandboxEnvIsolation_NotInherited(t *testing.T) {
	for _, k := range sandboxInjectedPrismEnvVars {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s=%q leaked into the cmd test suite — sidecar-injected env must be neutralised suite-wide (#2217); check TestMain's isolateSuiteFromSandboxEnv", k, v)
		}
	}
}
