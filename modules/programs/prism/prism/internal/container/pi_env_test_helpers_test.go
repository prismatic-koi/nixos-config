package container

import "testing"

// clearPICodingAgentDir clears PI_CODING_AGENT_DIR for the duration of the
// test, restoring the prior value on teardown via t.Setenv.
//
// Required for any test that exercises piResumeSessionsRoot's host-fallback
// branch (or piResumeHostSessionsRoot directly): the helper
// honours PI_CODING_AGENT_DIR, and the developer host sets that env var
// system-wide (PI_CODING_AGENT_DIR=/run/prism/pi-agent). Without clearing
// it, tests that set up a temp HOME would silently fall through to the
// host's PI data root and either fail or false-pass for the wrong reason.
func clearPICodingAgentDir(t *testing.T) {
	t.Helper()
	t.Setenv("PI_CODING_AGENT_DIR", "")
}
