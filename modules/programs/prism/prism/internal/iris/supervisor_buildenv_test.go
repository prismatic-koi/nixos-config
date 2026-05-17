package iris

// supervisor_buildenv_test.go — pins the pi-child environment contract for
// the iris supervisor (issues #1693 / #1704).
//
// The supervisor injects:
//
//   - IRIS_DAEMON_SOCK   — the per-session harness socket path. Used by the
//                          prism extension to register tool overrides.
//   - IRIS_SESSION_NAME  — the logical session name. Used by worker-side
//                          CLIs (`iris escalate`, future `iris prompt` from
//                          within a session) to identify the calling session
//                          without an extra RPC round-trip.
//
// These two vars are the iris-equivalent of PRISM_SESSION_NAME in prism's
// worker environment. The bash-sandbox immunity test in
// credential_broker_iris_env_test.go locks the symmetric guarantee that
// these vars are NEVER forwarded to the bash sandbox.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSupervisor_BuildEnv_InjectsIRISSessionName asserts that buildEnv emits
// IRIS_SESSION_NAME=<session-name> in the pi-child env.
func TestSupervisor_BuildEnv_InjectsIRISSessionName(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	runDir, err := os.MkdirTemp("", "iris-bldenv-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	cfg := SupervisorConfig{
		SessionName: "iris-worker@example-branch",
		Worktree:    tmp,
		Role:        "worker",
		RunDir:      runDir,
		Database:    database,
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(sup.closeSessionLogFile)

	env := sup.buildEnv()

	wantSession := "IRIS_SESSION_NAME=iris-worker@example-branch"
	wantSock := "IRIS_DAEMON_SOCK="
	var sawSession, sawSock bool
	for _, kv := range env {
		if kv == wantSession {
			sawSession = true
		}
		if strings.HasPrefix(kv, wantSock) {
			sawSock = true
		}
	}
	if !sawSession {
		t.Errorf("buildEnv missing %q; got env=%v", wantSession, env)
	}
	if !sawSock {
		t.Errorf("buildEnv missing IRIS_DAEMON_SOCK; got env=%v", env)
	}
}

// TestSupervisor_BuildEnv_EmptySessionNameOmitsVar asserts that an empty
// SessionName does NOT inject IRIS_SESSION_NAME= (an empty-value var would
// be worse than missing — the worker CLI's emptiness check would see the
// var as "set but empty" and the error message would be wrong).
func TestSupervisor_BuildEnv_EmptySessionNameOmitsVar(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	runDir, err := os.MkdirTemp("", "iris-bldenv-empty-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	cfg := SupervisorConfig{
		SessionName: "", // intentionally empty
		Worktree:    tmp,
		Role:        "worker",
		RunDir:      runDir,
		Database:    database,
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(sup.closeSessionLogFile)

	env := sup.buildEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "IRIS_SESSION_NAME=") {
			t.Errorf("buildEnv emitted IRIS_SESSION_NAME with empty SessionName: %q", kv)
		}
	}
}
