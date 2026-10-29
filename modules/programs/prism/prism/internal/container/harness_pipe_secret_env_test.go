package container

// harness_pipe_secret_env_test.go — the agent of a socket-pipe session gets
// the path of the harness pipe secret file (#3356), in bwrap and in
// sandbox-exec. The path is in the per-session run dir, which the sandbox of
// the session can read.

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestBwrapBuildArgs_PipeSecretFileEnvSet: with a harness pipe socket, the
// sandbox gets PRISM_HARNESS_PIPE_SECRET_FILE, and the per-session run dir
// that holds the file is bound at its host path.
func TestBwrapBuildArgs_PipeSecretFileEnvSet(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "run", "abc123def456")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("MkdirAll runDir: %v", err)
	}
	secretPath := filepath.Join(runDir, "pipe.secret")

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:           "repo@feat",
		Worktree:              t.TempDir(),
		AllocatedPort:         14010,
		HostAPISockPath:       filepath.Join(runDir, "hostapi.sock"),
		HarnessPipeSockPath:   filepath.Join(runDir, "pipe.sock"),
		HarnessPipeSecretPath: secretPath,
	})
	defer cleanup()

	args := (&bwrapIsolator{name: m.name}).BuildArgs(m)

	if !hasSetenv(args, HarnessPipeSecretFileEnvVar, secretPath) {
		t.Errorf("--setenv %s %q not found in args: %v", HarnessPipeSecretFileEnvVar, secretPath, redactedArgs(args))
	}
	if !hasBind(args, runDir) {
		t.Errorf("the run dir %q that holds the pipe secret is not bound: %v", runDir, redactedArgs(args))
	}
}

// TestBwrapBuildArgs_NoPipe_NoPipeSecretFileEnv: with no harness pipe, the
// sandbox gets no pipe secret path.
func TestBwrapBuildArgs_NoPipe_NoPipeSecretFileEnv(t *testing.T) {
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:           "repo@feat",
		Worktree:              t.TempDir(),
		AllocatedPort:         14010,
		HarnessPipeSecretPath: filepath.Join(t.TempDir(), "pipe.secret"),
	})
	defer cleanup()

	args := (&bwrapIsolator{name: m.name}).BuildArgs(m)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--setenv" && args[i+1] == HarnessPipeSecretFileEnvVar {
			t.Errorf("--setenv %s with no harness pipe: %v", HarnessPipeSecretFileEnvVar, redactedArgs(args))
		}
	}
}

// TestSandboxExecHarnessPipeEnv: a sandbox-exec agent gets the loopback pipe
// address and the path of the pipe secret file, and nothing with no port.
func TestSandboxExecHarnessPipeEnv(t *testing.T) {
	const secretPath = "/Users/u/.local/state/prism/run/abc123def456/pipe.secret"

	got := SandboxExecHarnessPipeEnv(Config{HarnessPipeTCPPort: 54321, HarnessPipeSecretPath: secretPath})
	want := []string{
		"PRISM_HARNESS_PIPE=tcp://127.0.0.1:54321",
		HarnessPipeSecretFileEnvVar + "=" + secretPath,
	}
	if !slices.Equal(got, want) {
		t.Errorf("SandboxExecHarnessPipeEnv = %v, want %v", got, want)
	}

	if got := SandboxExecHarnessPipeEnv(Config{HarnessPipeSecretPath: secretPath}); got != nil {
		t.Errorf("SandboxExecHarnessPipeEnv with no TCP port = %v, want nil", got)
	}
}
