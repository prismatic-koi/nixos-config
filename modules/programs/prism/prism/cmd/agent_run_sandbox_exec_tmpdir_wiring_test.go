package cmd

// agent_run_sandbox_exec_tmpdir_wiring_test.go — source-level wiring guard
// for the per-session TMPDIR env (#3150). The dispatcher has a darwin build
// tag and ends in an exec of sandbox-exec, so a source-level assertion is
// the guard that runs on every platform. See
// agent_run_sandbox_exec_gotoolchain_wiring_test.go for the same pattern.

import (
	"os"
	"strings"
	"testing"
)

// TestSandboxExecDispatcher_InjectsSessionTmpDirEnv asserts that the Darwin
// dispatcher puts container.SessionTmpDirEnv into the sandbox env, after the
// profile-level agent env vars. The profile grants no write on /tmp or on
// the user $TMPDIR, so without TMPDIR every tool that writes a temp file
// fails. The append must follow AppendSandboxEnvVarsKV, because the last
// occurrence of a key wins.
func TestSandboxExecDispatcher_InjectsSessionTmpDirEnv(t *testing.T) {
	src, err := os.ReadFile(dispatcherSource)
	if err != nil {
		t.Fatalf("read %s: %v", dispatcherSource, err)
	}
	s := string(src)

	const wantAppend = "env = append(env, container.SessionTmpDirEnv(tmpDir)...)"
	appendAt := strings.Index(s, wantAppend)
	if appendAt < 0 {
		t.Fatalf("%s does not append %q — the sandbox gets no TMPDIR and cannot write a temp file (#3150).", dispatcherSource, wantAppend)
	}
	if !strings.Contains(s, "tmpDir, tmpDirErr := m.SessionTmpDir()") {
		t.Errorf("%s does not take tmpDir from m.SessionTmpDir(), the dir that Prepare created and the profile grants.", dispatcherSource)
	}
	const agentEnv = "env = container.AppendSandboxEnvVarsKV(env, ctrCfg)"
	agentEnvAt := strings.Index(s, agentEnv)
	if agentEnvAt < 0 {
		t.Fatalf("%s no longer calls %q; update this guard.", dispatcherSource, agentEnv)
	}
	if appendAt < agentEnvAt {
		t.Errorf("%s appends the TMPDIR env before %q, so a TMPDIR in the profile-level agent env vars wins.", dispatcherSource, agentEnv)
	}
}
