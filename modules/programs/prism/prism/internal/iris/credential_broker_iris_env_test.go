package iris

// credential_broker_iris_env_test.go — immunity test for IRIS_* env vars
// (issues #1693 / #1704). The iris supervisor injects IRIS_DAEMON_SOCK and
// IRIS_SESSION_NAME into the pi child environment so worker-side CLIs (like
// `iris escalate`) can identify their calling session without an extra RPC.
//
// The bash sandbox must NEVER see those vars: the hostapi-immunity invariant
// from #1681 requires the bash subprocess environment to be built from a
// fixed allowlist, not a "host env minus block-list" filter. Adding a new
// IRIS_* var to the supervisor's pi-child env must not silently widen the
// bash-sandbox env surface.
//
// This test pins that invariant for IRIS_SESSION_NAME, IRIS_DAEMON_SOCK, and
// a synthetic IRIS_FUTURE_VAR so future iris env additions inherit the same
// guarantee.

import (
	"strings"
	"testing"
)

// TestCredentialBroker_IRISEnvNeverLeaks asserts that no IRIS_* env var
// present on the host process is forwarded into the bash subprocess
// environment returned by ResolveBash. The check is structural: ResolveBash
// builds env from an explicit allowlist, so a leak would imply the
// allowlist was widened — exactly the regression #1681's invariant guards
// against.
func TestCredentialBroker_IRISEnvNeverLeaks(t *testing.T) {
	clearAllGitHubTokens(t)

	// Set every IRIS_* var we care about, plus a synthetic forward-looking
	// one so adding a new IRIS_FOO setting in the supervisor cannot leak
	// without this test failing.
	irisVars := map[string]string{
		"IRIS_SESSION_NAME": "iris-worker@feat",
		"IRIS_DAEMON_SOCK":  "/run/iris/iris-test.sock",
		"IRIS_FUTURE_VAR":   "should-not-be-forwarded",
	}
	for k, v := range irisVars {
		t.Setenv(k, v)
	}

	b := NewCredentialBroker()
	res := b.ResolveBash("worker", "")

	for k := range irisVars {
		if hasEnvKey(res.Env, k) {
			t.Errorf("bash sandbox env leaked %s: %v", k, redactEnv(res.Env, k))
		}
	}

	// Defence in depth: assert that no env entry whatsoever starts with
	// "IRIS_". Future iris-injected vars are caught by this loop even
	// without an explicit name in irisVars above.
	for _, kv := range res.Env {
		if strings.HasPrefix(kv, "IRIS_") {
			t.Errorf("bash sandbox env contains unexpected IRIS_-prefixed entry: %q", kv)
		}
	}
}

// redactEnv returns the values associated with key in env (for diagnostic
// output on failure). The bash-env contract is "no leak", so on failure the
// raw value is what the developer needs to see.
func redactEnv(env []string, key string) []string {
	prefix := key + "="
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
}
