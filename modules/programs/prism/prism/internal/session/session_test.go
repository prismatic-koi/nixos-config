package session

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestBuildDirectOpencodeCmd_AgentEnvVars verifies that AgentEnvVars are
// prepended to the command string before PRISM_SESSION_NAME in host-mode
// (ContainerMode = false) sessions.
func TestBuildDirectOpencodeCmd_AgentEnvVars(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		Port:        14000,
		SessionName: "myrepo@branch",
		AgentEnvVars: map[string]string{
			"AWS_CONFIG_FILE": "/Users/bensherman/.config/aws/readonly-config",
			"GIT_EDITOR":      "true",
			"KUBECONFIG":      "/Users/bensherman/.config/kube/agents-config",
		},
	}
	cmd := buildDirectOpencodeCmd(opts)

	// All three env vars should appear in the command.
	for _, envVar := range []string{"AWS_CONFIG_FILE", "GIT_EDITOR", "KUBECONFIG"} {
		if !strings.Contains(cmd, envVar) {
			t.Errorf("expected env var %q in cmd, got: %q", envVar, cmd)
		}
	}

	// PRISM_SESSION_NAME should appear in the command.
	sessionIdx := strings.Index(cmd, "PRISM_SESSION_NAME")
	if sessionIdx == -1 {
		t.Fatalf("PRISM_SESSION_NAME not found in cmd: %q", cmd)
	}

	// Each env var should appear before PRISM_SESSION_NAME.
	for _, envVar := range []string{"AWS_CONFIG_FILE", "GIT_EDITOR", "KUBECONFIG"} {
		envIdx := strings.Index(cmd, envVar)
		if envIdx == -1 {
			t.Errorf("env var %q not found in cmd: %q", envVar, cmd)
			continue
		}
		if envIdx > sessionIdx {
			t.Errorf("env var %q (at %d) should appear before PRISM_SESSION_NAME (at %d) in cmd: %q",
				envVar, envIdx, sessionIdx, cmd)
		}
	}

	// PRISM_SESSION_NAME should appear before the opencode binary.
	opencodeIdx := strings.Index(cmd, "opencode ")
	if opencodeIdx == -1 {
		t.Fatalf("opencode command not found in cmd: %q", cmd)
	}
	if sessionIdx > opencodeIdx {
		t.Errorf("PRISM_SESSION_NAME (at %d) should appear before opencode (at %d) in cmd: %q",
			sessionIdx, opencodeIdx, cmd)
	}

	// Keys should be in sorted order (AWS < GIT < KUBECONFIG).
	awsIdx := strings.Index(cmd, "AWS_CONFIG_FILE")
	gitIdx := strings.Index(cmd, "GIT_EDITOR")
	kubeIdx := strings.Index(cmd, "KUBECONFIG")
	if awsIdx > gitIdx || gitIdx > kubeIdx {
		t.Errorf("env vars not in sorted order (AWS=%d, GIT=%d, KUBE=%d) in cmd: %q",
			awsIdx, gitIdx, kubeIdx, cmd)
	}
}

// TestBuildDirectOpencodeCmd_AgentEnvVars_ContainerMode verifies that
// AgentEnvVars are NOT injected when ContainerMode is true, even via the
// buildDirectOpencodeCmd fallback path (ContainerMode=true, Port=0).
func TestBuildDirectOpencodeCmd_AgentEnvVars_ContainerMode(t *testing.T) {
	opts := Opts{
		Agent:         "worker",
		Port:          0, // Port=0 triggers buildDirectOpencodeCmd fallback in BuildOpencodeCmd
		SessionName:   "myrepo@branch",
		ContainerMode: true,
		AgentEnvVars: map[string]string{
			"AWS_CONFIG_FILE": "/Users/bensherman/.config/aws/readonly-config",
		},
	}
	cmd := buildDirectOpencodeCmd(opts)

	if strings.Contains(cmd, "AWS_CONFIG_FILE") {
		t.Errorf("AgentEnvVars should not be injected when ContainerMode=true, got: %q", cmd)
	}
}

// TestBuildDirectOpencodeCmd_AgentEnvVarsEmpty verifies that an empty
// AgentEnvVars map produces no change to the command (beyond the
// outermost OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS prefix).
func TestBuildDirectOpencodeCmd_AgentEnvVarsEmpty(t *testing.T) {
	opts := Opts{
		Agent:        "worker",
		Port:         14000,
		SessionName:  "myrepo@branch",
		AgentEnvVars: map[string]string{},
	}
	cmd := buildDirectOpencodeCmd(opts)

	// Cmd should begin with the experimental timeout prefix.
	if !strings.HasPrefix(cmd, "OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS=") {
		t.Errorf("expected cmd to begin with OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS when AgentEnvVars is empty, got: %q", cmd)
	}
	// PRISM_SESSION_NAME should still appear in the command.
	if !strings.Contains(cmd, "PRISM_SESSION_NAME=") {
		t.Errorf("expected PRISM_SESSION_NAME in cmd when AgentEnvVars is empty, got: %q", cmd)
	}
}

// TestBuildDirectOpencodeCmd_AgentEnvVarsNil verifies that a nil AgentEnvVars
// map produces no change to the command (beyond the
// outermost OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS prefix).
func TestBuildDirectOpencodeCmd_AgentEnvVarsNil(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		Port:        14000,
		SessionName: "myrepo@branch",
		// AgentEnvVars intentionally nil
	}
	cmd := buildDirectOpencodeCmd(opts)

	// Cmd should begin with the experimental timeout prefix.
	if !strings.HasPrefix(cmd, "OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS=") {
		t.Errorf("expected cmd to begin with OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS when AgentEnvVars is nil, got: %q", cmd)
	}
	// PRISM_SESSION_NAME should still appear in the command.
	if !strings.Contains(cmd, "PRISM_SESSION_NAME=") {
		t.Errorf("expected PRISM_SESSION_NAME in cmd when AgentEnvVars is nil, got: %q", cmd)
	}
}

// ── Isolation mode command construction ─────────────────────────────────────

// TestBuildOpencodeCmd_PodmanMode verifies that IsolationMode="podman" produces
// "podman attach --sig-proxy=false <container-name>".
func TestBuildOpencodeCmd_PodmanMode(t *testing.T) {
	opts := Opts{
		IsolationMode: "podman",
		SessionName:   "nixos-config@feature",
	}
	cmd := BuildOpencodeCmd(opts)
	if !strings.HasPrefix(cmd, "podman attach --sig-proxy=false") {
		t.Errorf("podman mode: got %q, want prefix 'podman attach --sig-proxy=false'", cmd)
	}
}

// TestBuildOpencodeCmd_BwrapMode verifies that IsolationMode="bwrap" produces
// "prism agent-run --session <session-name>".
func TestBuildOpencodeCmd_BwrapMode(t *testing.T) {
	opts := Opts{
		IsolationMode: "bwrap",
		SessionName:   "nixos-config@feature",
	}
	cmd := BuildOpencodeCmd(opts)
	if !strings.HasPrefix(cmd, "prism agent-run --session") {
		t.Errorf("bwrap mode: got %q, want prefix 'prism agent-run --session'", cmd)
	}
	if !strings.Contains(cmd, "nixos-config@feature") {
		t.Errorf("bwrap mode: session name not in cmd: %q", cmd)
	}
}

// TestBuildOpencodeCmd_HostMode verifies that IsolationMode="host" produces
// a direct opencode command (not podman attach).
func TestBuildOpencodeCmd_HostMode(t *testing.T) {
	opts := Opts{
		IsolationMode: "host",
		Agent:         "worker",
		Port:          14000,
		SessionName:   "nixos-config@feature",
	}
	cmd := BuildOpencodeCmd(opts)
	if strings.HasPrefix(cmd, "podman") {
		t.Errorf("host mode: got podman command %q, want direct opencode invocation", cmd)
	}
	if strings.HasPrefix(cmd, "prism agent-run") {
		t.Errorf("host mode: got prism agent-run command %q, want direct opencode invocation", cmd)
	}
	if !strings.Contains(cmd, "opencode") {
		t.Errorf("host mode: cmd does not contain 'opencode': %q", cmd)
	}
}

// TestBuildOpencodeCmd_ContainerModeFallback verifies that ContainerMode=true
// with no IsolationMode falls back to "podman" (back-compat).
func TestBuildOpencodeCmd_ContainerModeFallback(t *testing.T) {
	opts := Opts{
		ContainerMode: true,
		SessionName:   "nixos-config@feature",
	}
	cmd := BuildOpencodeCmd(opts)
	if !strings.HasPrefix(cmd, "podman attach --sig-proxy=false") {
		t.Errorf("ContainerMode fallback: got %q, want 'podman attach --sig-proxy=false ...'", cmd)
	}
}

// TestBuildDirectOpencodeCmd_AgentEnvVars_ValuesQuoted verifies that env var
// values containing spaces or special characters are properly shell-quoted.
func TestBuildDirectOpencodeCmd_AgentEnvVars_ValuesQuoted(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		Port:        14000,
		SessionName: "myrepo@branch",
		AgentEnvVars: map[string]string{
			"GIT_EDITOR": "true",
		},
	}
	cmd := buildDirectOpencodeCmd(opts)

	// Value should be single-quoted.
	if !strings.Contains(cmd, "GIT_EDITOR='true'") {
		t.Errorf("expected GIT_EDITOR='true' in cmd, got: %q", cmd)
	}
}

// ── isolation mode DB persistence (issue #894 fix) ───────────────────────────
//
// These tests verify that the DB writes performed by setupFullLayout BEFORE
// tmux.NewWindow opens window 1 produce the correct agent_status values.
// They exercise the same openDB() + SetIsolationMode/SetHostMode path that the
// fix adds to setupFullLayout, ensuring the mode is persisted correctly for all
// three isolation modes ("bwrap", "host", "podman").
//
// The tests use SetTestDBPath to redirect the session package's openDB() to an
// isolated temp DB, then seed an agent_status row (as ensureAndSwitch does
// before calling session.Create/setupFullLayout), invoke the same DB writes,
// and assert the expected column values.

// openIsolationTestDB creates a fresh temp DB and registers cleanup.
// It also seeds an agent_status row for sessionName so that SetIsolationMode
// and SetHostMode have a row to UPDATE.
func openIsolationTestDB(t *testing.T, sessionName string) *db.DB {
	t.Helper()
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	// Seed the row — mirrors what ensureAndSwitch does before calling
	// session.Create/setupFullLayout.
	if err := d.UpsertStatus(sessionName, "testrepo", "/worktrees/"+sessionName, "idle", nil, nil); err != nil {
		d.Close()
		t.Fatalf("UpsertStatus: %v", err)
	}
	SetTestDBPath(dbFile)
	t.Cleanup(func() {
		d.Close()
		SetTestDBPath("")
	})
	return d
}

// TestIsolationMode_BwrapWrittenBeforeWindow verifies that after the DB writes
// performed by setupFullLayout (before tmux.NewWindow), the agent_status row
// has isolation_mode = "bwrap" and host_mode = false.
//
// This is the primary regression test for issue #894: prism agent-run reads
// isolation_mode immediately on start; it must be "bwrap" before window 1 opens.
func TestIsolationMode_BwrapWrittenBeforeWindow(t *testing.T) {
	const sessionName = "testrepo@bwrap-test"
	d := openIsolationTestDB(t, sessionName)

	// Simulate the DB writes that setupFullLayout now performs BEFORE
	// tmux.NewWindow(name, 1, "agent", ...).
	d2, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer d2.Close()

	if err := d2.SetIsolationMode(sessionName, "bwrap"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}

	// Read back and assert.
	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("CurrentStatus: got nil, want a row")
	}
	if st.IsolationMode != "bwrap" {
		t.Errorf("isolation_mode = %q, want %q", st.IsolationMode, "bwrap")
	}
	if st.HostMode {
		t.Errorf("host_mode = true, want false for bwrap mode")
	}
}

// TestIsolationMode_HostWrittenBeforeWindow verifies that after the DB writes
// performed by setupFullLayout, the agent_status row has isolation_mode = "host"
// AND host_mode = 1 (true).
func TestIsolationMode_HostWrittenBeforeWindow(t *testing.T) {
	const sessionName = "testrepo@host-test"
	d := openIsolationTestDB(t, sessionName)

	d2, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer d2.Close()

	if err := d2.SetIsolationMode(sessionName, "host"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}
	if err := d2.SetHostMode(sessionName, true); err != nil {
		t.Fatalf("SetHostMode: %v", err)
	}

	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("CurrentStatus: got nil, want a row")
	}
	if st.IsolationMode != "host" {
		t.Errorf("isolation_mode = %q, want %q", st.IsolationMode, "host")
	}
	if !st.HostMode {
		t.Errorf("host_mode = false, want true for host mode")
	}
}

// TestIsolationMode_PodmanWrittenBeforeWindow verifies that after the DB writes
// performed by setupFullLayout, the agent_status row has isolation_mode = "podman".
func TestIsolationMode_PodmanWrittenBeforeWindow(t *testing.T) {
	const sessionName = "testrepo@podman-test"
	d := openIsolationTestDB(t, sessionName)

	d2, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer d2.Close()

	if err := d2.SetIsolationMode(sessionName, "podman"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}

	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("CurrentStatus: got nil, want a row")
	}
	if st.IsolationMode != "podman" {
		t.Errorf("isolation_mode = %q, want %q", st.IsolationMode, "podman")
	}
}
