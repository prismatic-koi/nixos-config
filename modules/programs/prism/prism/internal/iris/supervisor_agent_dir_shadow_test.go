package iris

// supervisor_agent_dir_shadow_test.go — regression tests for issue #1778.
//
// Iris previously set PI_CODING_AGENT_DIR=<run-dir>/<instance>/pi-agent in
// the pi-child environment. That env var relocates pi's entire agent dir
// (auth.json, settings.json, models.json, extensions/, themes/, skills/,
// prompts/, sessions/) — not just session storage. The net effect was that
// iris-spawned pi could not authenticate to anthropic/openai because it
// saw an empty per-session auth.json instead of ~/.pi/agent/auth.json.
//
// The fix:
//   - drop PI_CODING_AGENT_DIR from Supervisor.buildEnv (AC-1)
//   - pass --session-dir <run-dir>/<instance>/pi-sessions to pi (AC-2)
//
// These tests pin both invariants so a future regression is caught at
// build time.
//
// AC-7: these tests only inspect buildEnv / buildPiArgs / ensurePISessionDir
// output — no sidecar is constructed, so no sidecartest.NewIsolated is
// required.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newShadowTestSupervisor wires up a minimal Supervisor for the env/args
// inspection tests. It is intentionally narrow: no pi process is spawned,
// no harness socket is accepted on. The run dir is allocated via
// os.MkdirTemp (not t.TempDir) so the harness socket path stays under the
// 108-byte sun_path limit — same pattern as newTestSupervisor in
// set_pi_session_path_test.go.
func newShadowTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	runDir, err := os.MkdirTemp("", "iris-shadow-")
	if err != nil {
		t.Fatalf("MkdirTemp runDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	cfg := SupervisorConfig{
		SessionName:   "iris-worker@shadow-test",
		Worktree:      tmp,
		Role:          "worker",
		RunDir:        runDir,
		Database:      database,
		ExtensionPath: "/nonexistent/prism.ts",
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(func() { sup.harness.Close() })
	t.Cleanup(sup.closeSessionLogFile)
	return sup
}

// TestSupervisor_BuildEnv_DoesNotSetPICodingAgentDir is the core regression
// guard for issue #1778 / AC-1. iris must never set PI_CODING_AGENT_DIR in
// the pi-child environment, because doing so shadows ~/.pi/agent/ and
// breaks LLM provider authentication.
//
// We explicitly clear PI_CODING_AGENT_DIR from the test process env first so
// that any inherited value (the developer or CI may run iris itself with
// PI_CODING_AGENT_DIR set for unrelated reasons) does not mask iris's own
// injection. After scrubbing, the var must NOT reappear in buildEnv output.
func TestSupervisor_BuildEnv_DoesNotSetPICodingAgentDir(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "")
	if err := os.Unsetenv("PI_CODING_AGENT_DIR"); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}

	sup := newShadowTestSupervisor(t)

	env := sup.buildEnv()

	for _, kv := range env {
		if strings.HasPrefix(kv, "PI_CODING_AGENT_DIR=") {
			t.Fatalf("buildEnv emitted PI_CODING_AGENT_DIR which shadows ~/.pi/agent/ (issue #1778): %q", kv)
		}
	}
}

// TestSupervisor_BuildPiArgs_IncludesSessionDir pins AC-2: pi must receive
// --session-dir <run-dir>/<instance>/pi-sessions so two concurrent iris
// sessions cannot write to the same session JSONL.
func TestSupervisor_BuildPiArgs_IncludesSessionDir(t *testing.T) {
	sup := newShadowTestSupervisor(t)

	sessionStoreDir, err := sup.ensurePISessionDir()
	if err != nil {
		t.Fatalf("ensurePISessionDir: %v", err)
	}

	// The directory must actually live under the per-instance run dir,
	// not at the run-dir root, so two instances are isolated.
	wantPrefix := filepath.Join(sup.cfg.RunDir, sup.sess.InstanceID) + string(os.PathSeparator)
	if !strings.HasPrefix(sessionStoreDir, wantPrefix) {
		t.Fatalf("ensurePISessionDir = %q; want prefix %q (per-instance isolation)", sessionStoreDir, wantPrefix)
	}
	if filepath.Base(sessionStoreDir) != "pi-sessions" {
		t.Fatalf("ensurePISessionDir = %q; want basename %q", sessionStoreDir, "pi-sessions")
	}

	// And the directory must exist on disk so pi can write to it
	// without a chicken-and-egg MkdirAll race on first session_start.
	info, err := os.Stat(sessionStoreDir)
	if err != nil {
		t.Fatalf("stat session dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("session dir is not a directory: %v", info.Mode())
	}

	args := sup.buildPiArgs(sessionStoreDir)

	// Locate --session-dir and assert its value.
	found := false
	for i, a := range args {
		if a == "--session-dir" {
			if i+1 >= len(args) {
				t.Fatalf("--session-dir present without a value: args=%v", args)
			}
			if args[i+1] != sessionStoreDir {
				t.Fatalf("--session-dir value = %q; want %q", args[i+1], sessionStoreDir)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("buildPiArgs did not emit --session-dir; args=%v", args)
	}
}

// TestSupervisor_BuildPiArgs_KeepsExtensionFlag pins AC-4: the prism
// extension is still loaded via --extension <path>. This contract is
// unaffected by the #1778 fix and the regression test catches any
// accidental removal during a future refactor.
func TestSupervisor_BuildPiArgs_KeepsExtensionFlag(t *testing.T) {
	sup := newShadowTestSupervisor(t)

	args := sup.buildPiArgs("/tmp/fake-sessions")

	wantExt := sup.cfg.ExtensionPath
	if wantExt == "" {
		t.Fatalf("test setup: ExtensionPath must be non-empty")
	}
	found := false
	for i, a := range args {
		if a == "--extension" {
			if i+1 >= len(args) || args[i+1] != wantExt {
				t.Fatalf("--extension value mismatch: args=%v want=%q", args, wantExt)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("buildPiArgs did not emit --extension; args=%v", args)
	}
}

// TestSupervisor_BuildPiArgs_ResumeSession asserts that the existing D-9
// restore wiring (--session <jsonl-path>) still composes correctly with
// the new --session-dir flag. Both must appear; they are independent.
func TestSupervisor_BuildPiArgs_ResumeSession(t *testing.T) {
	sup := newShadowTestSupervisor(t)
	sup.cfg.SessionContinuePath = "/tmp/resume.jsonl"

	args := sup.buildPiArgs("/tmp/fake-sessions")

	var sawSession, sawSessionDir bool
	for i, a := range args {
		if a == "--session" {
			sawSession = true
			if i+1 >= len(args) || args[i+1] != "/tmp/resume.jsonl" {
				t.Fatalf("--session value mismatch: args=%v", args)
			}
		}
		if a == "--session-dir" {
			sawSessionDir = true
		}
	}
	if !sawSession {
		t.Fatalf("buildPiArgs dropped --session (D-9 restore): args=%v", args)
	}
	if !sawSessionDir {
		t.Fatalf("buildPiArgs dropped --session-dir (#1778 isolation): args=%v", args)
	}
}

// TestSupervisor_EnsurePISessionDir_DoesNotCreatePiAgentDir pins AC-3:
// the old empty pi-agent/ directory (whose presence with an empty
// settings.json triggered the auth.json shadowing) must no longer be
// created under the run dir. Catching this at the filesystem level —
// not just the env-var level — guards against a partial revert that
// keeps the directory but drops the env var.
func TestSupervisor_EnsurePISessionDir_DoesNotCreatePiAgentDir(t *testing.T) {
	sup := newShadowTestSupervisor(t)

	if _, err := sup.ensurePISessionDir(); err != nil {
		t.Fatalf("ensurePISessionDir: %v", err)
	}

	piAgentDir := filepath.Join(sup.cfg.RunDir, sup.sess.InstanceID, "pi-agent")
	if _, err := os.Stat(piAgentDir); !os.IsNotExist(err) {
		t.Fatalf("pi-agent/ should not exist under the run dir (issue #1778); stat err = %v", err)
	}
}
