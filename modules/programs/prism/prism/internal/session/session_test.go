package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// TestDefaultAgent covers the three cases described in issue #952:
//  1. main worktree (parent has .bare, basename == "main") → "coordinator"
//  2. non-main worktree (parent has .bare, basename ≠ "main") → "worker"
//  3. non-worktree path (parent does NOT have .bare) → ""
//
// It also verifies that an explicit non-empty value always wins regardless of
// directory type.
func TestDefaultAgent(t *testing.T) {
	// Set up a temporary directory structure:
	//   <tmp>/
	//     bare-root/       ← acts as the bare+worktree project root
	//       .bare          ← signals prism bare layout to IsBareRepo
	//       main/          ← worktree at "main"
	//       feature-branch/ ← non-main worktree
	//     regular-repo/    ← plain directory (no .bare in parent)

	tmp := t.TempDir()
	bareRoot := filepath.Join(tmp, "bare-root")
	if err := os.MkdirAll(filepath.Join(bareRoot, "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bareRoot, "feature-branch"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create .bare marker so IsBareRepo(bareRoot) returns true.
	if err := os.WriteFile(filepath.Join(bareRoot, ".bare"), []byte("gitdir"), 0o644); err != nil {
		t.Fatal(err)
	}
	regularRepo := filepath.Join(tmp, "regular-repo")
	if err := os.MkdirAll(regularRepo, 0o755); err != nil {
		t.Fatal(err)
	}

	mainWorktree := filepath.Join(bareRoot, "main")
	featureWorktree := filepath.Join(bareRoot, "feature-branch")

	tests := []struct {
		name      string
		directory string
		explicit  string
		want      string
	}{
		{"main worktree → coordinator", mainWorktree, "", "coordinator"},
		{"feature worktree → worker", featureWorktree, "", "worker"},
		{"regular repo → empty", regularRepo, "", ""},
		{"non-git dir → empty", filepath.Join(tmp, "documents"), "", ""},
		{"explicit overrides main", mainWorktree, "worker", "worker"},
		{"explicit overrides feature", featureWorktree, "coordinator", "coordinator"},
		{"explicit overrides regular", regularRepo, "worker", "worker"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DefaultAgent(tc.directory, tc.explicit)
			if got != tc.want {
				t.Errorf("DefaultAgent(%q, %q) = %q, want %q", tc.directory, tc.explicit, got, tc.want)
			}
		})
	}
}

// prepended to the command string before PRISM_SESSION_NAME in host-mode
// (ContainerMode = false) sessions.
func TestBuildDirectAgentCmd_AgentEnvVars(t *testing.T) {
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
	cmd := buildDirectAgentCmd(opts)

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

	// PRISM_SESSION_NAME should appear before the pi binary.
	piIdx := strings.Index(cmd, "pi ")
	if piIdx == -1 {
		t.Fatalf("pi command not found in cmd: %q", cmd)
	}
	if sessionIdx > piIdx {
		t.Errorf("PRISM_SESSION_NAME (at %d) should appear before pi (at %d) in cmd: %q",
			sessionIdx, piIdx, cmd)
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

// TestBuildDirectAgentCmd_AgentEnvVarsEmpty verifies that an empty
// AgentEnvVars map produces no change to the command (beyond the
// outermost RuntimeEnvVars prefix when provided via the harness).
func TestBuildDirectAgentCmd_AgentEnvVarsEmpty(t *testing.T) {
	opts := Opts{
		Agent:        "worker",
		Port:         14000,
		SessionName:  "myrepo@branch",
		AgentEnvVars: map[string]string{},
		RuntimeEnvVars: map[string]string{
			"PRISM_TEST_HARNESS_ENV_VAR": "900000",
		},
	}
	cmd := buildDirectAgentCmd(opts)

	// Cmd should begin with the runtime env var prefix (from harness).
	if !strings.HasPrefix(cmd, "PRISM_TEST_HARNESS_ENV_VAR=") {
		t.Errorf("expected cmd to begin with PRISM_TEST_HARNESS_ENV_VAR when RuntimeEnvVars is set, got: %q", cmd)
	}
	// PRISM_SESSION_NAME should still appear in the command.
	if !strings.Contains(cmd, "PRISM_SESSION_NAME=") {
		t.Errorf("expected PRISM_SESSION_NAME in cmd when AgentEnvVars is empty, got: %q", cmd)
	}
}

// TestBuildDirectAgentCmd_AgentEnvVarsNil verifies that a nil AgentEnvVars
// map produces no change to the command (beyond the
// outermost RuntimeEnvVars prefix when provided via the harness).
func TestBuildDirectAgentCmd_AgentEnvVarsNil(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		Port:        14000,
		SessionName: "myrepo@branch",
		// AgentEnvVars intentionally nil
		RuntimeEnvVars: map[string]string{
			"PRISM_TEST_HARNESS_ENV_VAR": "900000",
		},
	}
	cmd := buildDirectAgentCmd(opts)

	// Cmd should begin with the runtime env var prefix (from harness).
	if !strings.HasPrefix(cmd, "PRISM_TEST_HARNESS_ENV_VAR=") {
		t.Errorf("expected cmd to begin with PRISM_TEST_HARNESS_ENV_VAR when RuntimeEnvVars is set, got: %q", cmd)
	}
	// PRISM_SESSION_NAME should still appear in the command.
	if !strings.Contains(cmd, "PRISM_SESSION_NAME=") {
		t.Errorf("expected PRISM_SESSION_NAME in cmd when AgentEnvVars is nil, got: %q", cmd)
	}
}

// ── Isolation mode command construction ─────────────────────────────────────

// TestBuildAgentCmd_BwrapMode verifies that IsolationMode="bwrap" produces
// "<abs-path>/prism agent-run --session '<session-name>'". Post-#2260 the
// command begins with a shell-quoted absolute path (os.Executable resolves
// to the running go-test binary under `go test`, hence we only assert the
// shape rather than a specific path).
func TestBuildAgentCmd_BwrapMode(t *testing.T) {
	opts := Opts{
		IsolationMode: "bwrap",
		SessionName:   "nixos-config@feature",
	}
	cmd, err := BuildAgentCmd(opts)
	if err != nil {
		t.Fatalf("BuildAgentCmd: %v", err)
	}
	// The bwrap shape carries an absolute path to the running binary
	// (issue #2260), shell-quoted, followed by ` agent-run --session`.
	if !strings.HasPrefix(cmd, "'/") {
		t.Errorf("bwrap mode: cmd must start with a shell-quoted absolute path; got %q", cmd)
	}
	if !strings.Contains(cmd, " agent-run --session 'nixos-config@feature'") {
		t.Errorf("bwrap mode: cmd missing agent-run subcommand and session; got %q", cmd)
	}
}

// TestBuildAgentCmd_HostMode verifies that IsolationMode="host" produces
// a direct agent command (not a sandbox launcher command).
func TestBuildAgentCmd_HostMode(t *testing.T) {
	opts := Opts{
		IsolationMode: "host",
		Agent:         "worker",
		Port:          14000,
		SessionName:   "nixos-config@feature",
	}
	cmd, err := BuildAgentCmd(opts)
	if err != nil {
		t.Fatalf("BuildAgentCmd: %v", err)
	}
	if strings.Contains(cmd, " agent-run ") {
		t.Errorf("host mode: got prism agent-run command %q, want direct pi invocation", cmd)
	}
	if !strings.Contains(cmd, "pi") {
		t.Errorf("host mode: cmd does not contain 'pi': %q", cmd)
	}
}

// TestBuildAgentCmd_EmptyIsolationMode verifies that an empty IsolationMode
// falls back to the host command (a direct pi invocation).
func TestBuildAgentCmd_EmptyIsolationMode(t *testing.T) {
	opts := Opts{
		SessionName: "nixos-config@feature",
		Agent:       "worker",
		Port:        14000,
	}
	cmd, err := BuildAgentCmd(opts)
	if err != nil {
		t.Fatalf("BuildAgentCmd: %v", err)
	}
	if strings.Contains(cmd, " agent-run ") {
		t.Errorf("empty IsolationMode: got %q, want direct pi invocation", cmd)
	}
	if !strings.Contains(cmd, "pi") {
		t.Errorf("empty IsolationMode: cmd does not contain 'pi': %q", cmd)
	}
}

// TestBuildDirectAgentCmd_PIExtensionFlag verifies that host-mode pi launch
// appends --extension <PIExtensionDir>/prism.ts when the harness is pi (or
// empty, which defaults to pi). This is the #2065 fix bundled into the
// #2064 PR: without it, host-mode sessions launch pi with no --extension,
// and the prism PI extension never loads — silently disabling role-prompt
// injection, the sidecar bridge, and the status bar.
func TestBuildDirectAgentCmd_PIExtensionFlag(t *testing.T) {
	t.Run("pi harness emits --extension when PIExtensionDir is set", func(t *testing.T) {
		opts := Opts{
			HarnessName:    "pi",
			Agent:          "worker",
			PIExtensionDir: "/nix/store/abc-prism-extension",
		}
		cmd := buildDirectAgentCmd(opts)
		if !strings.Contains(cmd, "--extension '/nix/store/abc-prism-extension/prism.ts'") {
			t.Errorf("expected --extension '/nix/store/abc-prism-extension/prism.ts' in cmd; got: %q", cmd)
		}
	})

	t.Run("empty harness defaults to pi and emits --extension", func(t *testing.T) {
		opts := Opts{
			HarnessName:    "",
			Agent:          "worker",
			PIExtensionDir: "/nix/store/abc-prism-extension",
		}
		cmd := buildDirectAgentCmd(opts)
		if !strings.Contains(cmd, "--extension '/nix/store/abc-prism-extension/prism.ts'") {
			t.Errorf("expected --extension on empty-harness cmd; got: %q", cmd)
		}
	})

	t.Run("pi harness with empty PIExtensionDir: low-level emitter omits the flag (production paths are guarded by ValidatePILaunchOpts — see TestValidatePILaunchOpts)", func(t *testing.T) {
		// buildDirectAgentCmd is a pure string emitter; the fail-fast policy
		// for the empty-PIExtensionDir case lives one level up in
		// ValidatePILaunchOpts (called from SpawnSession and Create at
		// LayoutFull). Keeping the emitter pure simplifies test fixtures and
		// matches the host-mode pi-resume helper's shape (also pure).
		opts := Opts{
			HarnessName:    "pi",
			Agent:          "worker",
			PIExtensionDir: "",
		}
		cmd := buildDirectAgentCmd(opts)
		if strings.Contains(cmd, "--extension") {
			t.Errorf("empty PIExtensionDir must not emit a stray --extension at the builder level; got: %q", cmd)
		}
	})

	t.Run("non-pi harness must NOT receive --extension (pi-specific flag)", func(t *testing.T) {
		opts := Opts{
			HarnessName:    "opencode",
			Agent:          "worker",
			PIExtensionDir: "/nix/store/abc-prism-extension",
		}
		cmd := buildDirectAgentCmd(opts)
		if strings.Contains(cmd, "--extension") {
			t.Errorf("non-pi harness must not receive --extension; got: %q", cmd)
		}
	})
}

// TestBuildDirectAgentCmd_AgentFlag confirms that --agent <role> is always
// emitted for pi (and empty=pi) when Agent is non-empty. This is the host-
// mode complement to TestPIInvocation_AgentFlag in the container package —
// together they guarantee the prism PI extension's pi.getFlag("agent")
// receives a value on every code path.
func TestBuildDirectAgentCmd_AgentFlag(t *testing.T) {
	for _, harnessName := range []string{"pi", ""} {
		t.Run("harness="+harnessName, func(t *testing.T) {
			opts := Opts{
				HarnessName: harnessName,
				Agent:       "coordinator",
			}
			cmd := buildDirectAgentCmd(opts)
			if !strings.Contains(cmd, "--agent coordinator") {
				t.Errorf("expected --agent coordinator in cmd; got: %q", cmd)
			}
		})
	}
}

// TestPIExtensionHostPath verifies the helper that buildDirectAgentCmd uses
// to resolve the on-disk extension file path. Empty dir must return empty
// so the caller falls back to no flag rather than emitting a stray
// `--extension /prism.ts` against the filesystem root.
func TestPIExtensionHostPath(t *testing.T) {
	t.Run("non-empty dir resolves to dir/prism.ts", func(t *testing.T) {
		got := container.PIExtensionHostPath("/nix/store/abc-prism-extension")
		if got != "/nix/store/abc-prism-extension/prism.ts" {
			t.Errorf("expected dir/prism.ts; got %q", got)
		}
	})
	t.Run("empty dir returns empty", func(t *testing.T) {
		if got := container.PIExtensionHostPath(""); got != "" {
			t.Errorf("expected empty for empty dir; got %q", got)
		}
	})
}

// TestValidatePILaunchOpts covers the #2065 fail-fast edge-case AC: host-mode
// pi launches must refuse to spawn when cfg.PIExtensionDir is empty, with
// a clear error message mirroring the container-path guard at
// cmd/agent_run.go:730. The check is the policy chokepoint for the
// extension-must-be-loaded invariant on host mode; ValidatePILaunchOpts is
// called from SpawnSession (all spawn-side entry points) and from Create
// when Layout == LayoutFull (switch / restore entry points).
func TestValidatePILaunchOpts(t *testing.T) {
	t.Run("host mode + pi harness + empty PIExtensionDir: rejects with clear error", func(t *testing.T) {
		err := ValidatePILaunchOpts(Opts{
			IsolationMode:  "host",
			HarnessName:    "pi",
			PIExtensionDir: "",
		})
		if err == nil {
			t.Fatalf("expected error for host-mode pi with empty PIExtensionDir; got nil")
		}
		// Must mention piExtensionDir so an operator can grep for the
		// recommendation. Mirrors cmd/agent_run.go:730.
		if !strings.Contains(err.Error(), "piExtensionDir") {
			t.Errorf("expected error to reference 'piExtensionDir'; got: %v", err)
		}
	})

	t.Run("host mode + empty harness (defaults to pi) + empty PIExtensionDir: also rejects", func(t *testing.T) {
		err := ValidatePILaunchOpts(Opts{
			IsolationMode:  "host",
			HarnessName:    "",
			PIExtensionDir: "",
		})
		if err == nil {
			t.Errorf("expected error for empty harness (defaults to pi); got nil")
		}
	})

	t.Run("host mode + pi harness + non-empty PIExtensionDir: accepts", func(t *testing.T) {
		err := ValidatePILaunchOpts(Opts{
			IsolationMode:  "host",
			HarnessName:    "pi",
			PIExtensionDir: "/nix/store/abc-prism-extension",
		})
		if err != nil {
			t.Errorf("expected nil for properly-configured host-mode pi; got: %v", err)
		}
	})

	t.Run("empty isolation (defaults to host) + pi + empty PIExtensionDir: rejects", func(t *testing.T) {
		// effectiveIsolationMode defaults empty IsolationMode to "host",
		// so the guard MUST fire even when the caller leaves the mode unset.
		err := ValidatePILaunchOpts(Opts{
			IsolationMode:  "",
			HarnessName:    "pi",
			PIExtensionDir: "",
		})
		if err == nil {
			t.Errorf("expected error when IsolationMode is empty (defaults to host); got nil")
		}
	})

	t.Run("container modes pass the check (container paths have their own guard at agent_run.go:730)", func(t *testing.T) {
		for _, mode := range []string{"bwrap", "sandbox-exec"} {
			err := ValidatePILaunchOpts(Opts{
				IsolationMode:  mode,
				HarnessName:    "pi",
				PIExtensionDir: "",
			})
			if err != nil {
				t.Errorf("mode=%q: expected nil (container paths route through PIInvocation which has its own guard); got: %v", mode, err)
			}
		}
	})

	t.Run("non-pi harness in host mode passes the check (extension is pi-specific)", func(t *testing.T) {
		// The prism PI extension is a pi-only artefact; a non-pi harness
		// (e.g. a hypothetical opencode-in-host-mode) must not be blocked
		// for missing PIExtensionDir — it does not load the extension at all.
		err := ValidatePILaunchOpts(Opts{
			IsolationMode:  "host",
			HarnessName:    "opencode",
			PIExtensionDir: "",
		})
		if err != nil {
			t.Errorf("expected nil for non-pi harness; got: %v", err)
		}
	})
}

// TestCreate_LayoutFull_FailsFastOnEmptyPIExtensionDir confirms that the
// SpawnSession-parallel switch/restore path (which goes through Create with
// LayoutFull) also fails fast on the empty-PIExtensionDir / host-mode-pi
// combination, not just SpawnSession. The non-LayoutFull layouts must NOT
// be blocked because they don't launch an agent pane.
func TestCreate_LayoutFull_FailsFastOnEmptyPIExtensionDir(t *testing.T) {
	dir := t.TempDir()

	t.Run("LayoutFull + host + pi + empty PIExtensionDir: rejects", func(t *testing.T) {
		err := Create("unit-test-2065-full", dir, Opts{
			Layout:         LayoutFull,
			IsolationMode:  "host",
			HarnessName:    "pi",
			PIExtensionDir: "",
		})
		if err == nil {
			t.Fatalf("expected error from Create for empty PIExtensionDir on LayoutFull; got nil")
		}
		if !strings.Contains(err.Error(), "piExtensionDir") {
			t.Errorf("expected error to reference 'piExtensionDir'; got: %v", err)
		}
	})

	t.Run("LayoutBare + empty PIExtensionDir: accepted (no agent pane to misconfigure)", func(t *testing.T) {
		// LayoutBare is the dashboard's dead-session recovery path and
		// the scratchpad fallback in restore. Both run with no agent.
		// They legitimately leave PIExtensionDir empty and must NOT be
		// blocked by the guard.
		//
		// The guard under test fires BEFORE any tmux invocation, so its
		// behaviour is observable even when tmux is unusable (tmux is
		// intentionally NOT in nativeCheckInputs in pkgs/prism.nix — see
		// the long comment there — so the nix build sandbox runs this
		// subtest without tmux):
		//
		//   - tmux usable: Create must succeed end-to-end.
		//   - tmux unusable: Create must fail at the tmux launch
		//     ("new-session"), NOT at the PIExtensionDir guard. If the guard
		//     were wrongly applied to LayoutBare it would reject before
		//     reaching tmux, producing a "piExtensionDir" error in both
		//     environments — so either branch catches the regression.
		//
		// "Usable" is decided by a functional probe, not exec.LookPath:
		// since the suite-wide $TMUX_TMPDIR redirect (#2230) this subtest
		// starts a fresh private tmux server instead of reusing the live
		// host server (the old form created its session on the developer's
		// LIVE server — exactly the leak class #2230 eliminates). Inside a
		// prism worker sandbox the tmux binary is on $PATH but a fresh
		// server cannot fork window processes ("fork failed: Operation not
		// permitted"), so LookPath alone would put a sandboxed run in the
		// wrong branch.
		name := "unit-test-2065-bare"
		const probe = "unit-test-2065-probe"
		tmuxAvailable := tmux.NewSessionDetached(probe, "/tmp") == nil
		if tmuxAvailable {
			_ = tmux.KillSession(probe)
			defer tmux.KillSession(name)
		}
		err := Create(name, dir, Opts{
			Layout:         LayoutBare,
			IsolationMode:  "host",
			HarnessName:    "pi",
			PIExtensionDir: "",
		})
		if err != nil && strings.Contains(err.Error(), "piExtensionDir") {
			t.Errorf("LayoutBare must not be blocked by the PIExtensionDir guard; got: %v", err)
		}
		if tmuxAvailable && err != nil {
			t.Errorf("LayoutBare Create failed with tmux available: %v", err)
		}
		if !tmuxAvailable && err == nil {
			t.Errorf("expected Create to fail at the tmux launch when tmux is absent from $PATH; got nil")
		}
	})
}

// TestBuildDirectAgentCmd_AgentEnvVars_ValuesQuoted verifies that env var
// values containing spaces or special characters are properly shell-quoted.
func TestBuildDirectAgentCmd_AgentEnvVars_ValuesQuoted(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		Port:        14000,
		SessionName: "myrepo@branch",
		AgentEnvVars: map[string]string{
			"GIT_EDITOR": "true",
		},
	}
	cmd := buildDirectAgentCmd(opts)

	// Value should be single-quoted.
	if !strings.Contains(cmd, "GIT_EDITOR='true'") {
		t.Errorf("expected GIT_EDITOR='true' in cmd, got: %q", cmd)
	}
}

// ── isolation mode DB persistence (issue #894 fix) ───────────────────────────
//
// These tests verify that the DB writes performed by setupFullLayout BEFORE
// tmux.NewWindow opens window 1 produce the correct agent_status values.
// They exercise the same openDB() + SetIsolationMode path that the
// fix adds to setupFullLayout, ensuring the mode is persisted correctly for all
// three isolation modes ("bwrap", "host", "sandbox-exec").
//
// The tests use SetTestDBPath to redirect the session package's openDB() to an
// isolated temp DB, then seed an agent_status row (as ensureAndSwitch does
// before calling session.Create/setupFullLayout), invoke the same DB writes,
// and assert the expected column values.

// openIsolationTestDB creates a fresh temp DB and registers cleanup.
// It also seeds an agent_status row for sessionName so that SetIsolationMode
// has a row to UPDATE.
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
// has isolation_mode = "bwrap".
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
}

// TestIsolationMode_HostWrittenBeforeWindow verifies that after the DB writes
// performed by setupFullLayout, the agent_status row has isolation_mode = "host".
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
}

// ── agentPaneEnvVars (initial-prompt env var) ─────────────────────────────────

// TestAgentPaneEnvVars_WithPrompt verifies that agentPaneEnvVars returns a map
// containing PRISM_INITIAL_PROMPT when opts.Prompt is non-empty AND the
// isolation mode consumes the env var (bwrap / sandbox-exec). Host mode is
// covered by TestAgentPaneEnvVars_HostMode_Skipped — the env var is omitted
// there because the host launch path reads the prompt from a file directly
// (#1064).
func TestAgentPaneEnvVars_WithPrompt(t *testing.T) {
	opts := Opts{Prompt: "hello", IsolationMode: "bwrap"}
	got := agentPaneEnvVars(opts)
	if got == nil {
		t.Fatal("agentPaneEnvVars(bwrap, Prompt=hello): got nil, want non-nil map")
	}
	if v, ok := got["PRISM_INITIAL_PROMPT"]; !ok || v != "hello" {
		t.Errorf("agentPaneEnvVars(bwrap, Prompt=hello): PRISM_INITIAL_PROMPT = %q, want %q", v, "hello")
	}
}

// TestAgentPaneEnvVars_NoPrompt verifies that agentPaneEnvVars returns nil
// when opts.Prompt is empty, ensuring no -e flag is emitted.
func TestAgentPaneEnvVars_NoPrompt(t *testing.T) {
	opts := Opts{Prompt: ""}
	got := agentPaneEnvVars(opts)
	if got != nil {
		t.Errorf("agentPaneEnvVars(Prompt=''): got %v, want nil", got)
	}
}

// TestAgentPaneEnvVars_SpecialChars verifies that a prompt containing newlines,
// quotes, backticks, and equals signs is stored verbatim. Asserted on bwrap
// mode (where PRISM_INITIAL_PROMPT is consumed by `prism agent-run`).
func TestAgentPaneEnvVars_SpecialChars(t *testing.T) {
	prompt := "line1\nline2 'single' \"double\" `backtick` KEY=value"
	opts := Opts{Prompt: prompt, IsolationMode: "bwrap"}
	got := agentPaneEnvVars(opts)
	if got == nil {
		t.Fatal("agentPaneEnvVars: got nil, want non-nil map")
	}
	if v := got["PRISM_INITIAL_PROMPT"]; v != prompt {
		t.Errorf("PRISM_INITIAL_PROMPT = %q, want %q", v, prompt)
	}
}

// TestAgentPaneEnvVars_HostMode_Skipped verifies that agentPaneEnvVars returns
// nil for host mode regardless of prompt content (#1064). The host launch
// path uses $(cat <prompt-file>) for delivery, so emitting a large
// PRISM_INITIAL_PROMPT here would re-introduce the same tmux arg-size limit
// the file-based plumbing was added to avoid.
func TestAgentPaneEnvVars_HostMode_Skipped(t *testing.T) {
	opts := Opts{Prompt: "hello", IsolationMode: "host"}
	got := agentPaneEnvVars(opts)
	if got != nil {
		t.Errorf("agentPaneEnvVars(host, Prompt=hello): got %v, want nil — host mode reads prompt from file, not env var", got)
	}
}

// TestAgentPaneEnvVars_PromptFile_PreferredOverInline verifies the post-#1092
// behaviour: when both opts.Prompt and opts.PromptFilePath are set in bwrap
// or sandbox-exec mode, agentPaneEnvVars emits PRISM_INITIAL_PROMPT_FILE
// (carrying the path) and NOT the inline PRISM_INITIAL_PROMPT (which would
// re-introduce the launch-cmd size failure).
func TestAgentPaneEnvVars_PromptFile_PreferredOverInline(t *testing.T) {
	opts := Opts{
		Prompt:         "hello",
		PromptFilePath: "/var/state/prism/run/abc/initial-prompt.txt",
		IsolationMode:  "bwrap",
	}
	got := agentPaneEnvVars(opts)
	if got == nil {
		t.Fatal("agentPaneEnvVars: got nil, want non-nil map")
	}
	if v, ok := got["PRISM_INITIAL_PROMPT_FILE"]; !ok || v != "/var/state/prism/run/abc/initial-prompt.txt" {
		t.Errorf("PRISM_INITIAL_PROMPT_FILE = %q, want %q", v, "/var/state/prism/run/abc/initial-prompt.txt")
	}
	if _, present := got["PRISM_INITIAL_PROMPT"]; present {
		t.Errorf("PRISM_INITIAL_PROMPT must NOT be set when PRISM_INITIAL_PROMPT_FILE is — would inline prompt body and re-introduce #1092: got %v", got)
	}
}

// spyTmuxBin creates a fake tmux binary that records its arguments (one per line)
// to argsFile, redirects tmux.TmuxBin for the duration of the test, and returns
// the path to argsFile. Only call this from non-parallel tests.
func spyTmuxBin(t *testing.T) string {
	t.Helper()
	argsFile := t.TempDir() + "/tmux-args"
	wrapperPath := t.TempDir() + "/tmux"
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> " + argsFile + "; done\n"
	if err := os.WriteFile(wrapperPath, []byte(script), 0755); err != nil {
		t.Fatalf("write spy tmux: %v", err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = wrapperPath
	t.Cleanup(func() { tmux.TmuxBin = orig })
	return argsFile
}

// readSpyArgs reads the arguments recorded by the spy tmux binary.
func readSpyArgs(argsFile string) []string {
	data, err := os.ReadFile(argsFile)
	if err != nil {
		return nil
	}
	var args []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			args = append(args, line)
		}
	}
	return args
}

// containsSeq returns true when needle appears as a contiguous sub-slice of haystack.
func containsSeq(haystack, needle []string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j, n := range needle {
			if haystack[i+j] != n {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestSpawnSession_PromptEnvVar_WithPrompt verifies that when opts.Prompt is
// set in a sandboxed isolation mode (bwrap), the tmux new-window call for
// the agent pane contains -e PRISM_INITIAL_PROMPT=<prompt> — the env var is
// consumed by `prism agent-run` to populate the bwrap container's
// initial-prompt path.
// It calls tmux.NewWindow directly (the same call path used by setupFullLayout)
// via the spy so no real tmux session is required.
func TestSpawnSession_PromptEnvVar_WithPrompt(t *testing.T) {
	argsFile := spyTmuxBin(t)

	// Call tmux.NewWindow with the env var map that agentPaneEnvVars would
	// return for a non-empty prompt. This mirrors setupFullLayout's call site.
	opts := Opts{Prompt: "hello", IsolationMode: "bwrap"}
	_ = tmux.NewWindow("test-session", 1, "agent", "/tmp", "echo hi", agentPaneEnvVars(opts))

	args := readSpyArgs(argsFile)
	if !containsSeq(args, []string{"-e", "PRISM_INITIAL_PROMPT=hello"}) {
		t.Errorf("tmux new-window args %v do not contain [-e PRISM_INITIAL_PROMPT=hello]", args)
	}
}

// TestSpawnSession_PromptEnvVar_NoPrompt verifies that when opts.Prompt is
// empty, the tmux new-window call does NOT include any -e flag.
func TestSpawnSession_PromptEnvVar_NoPrompt(t *testing.T) {
	argsFile := spyTmuxBin(t)

	opts := Opts{Prompt: ""}
	_ = tmux.NewWindow("test-session", 1, "agent", "/tmp", "echo hi", agentPaneEnvVars(opts))

	args := readSpyArgs(argsFile)
	for _, a := range args {
		if a == "-e" {
			t.Errorf("tmux new-window args %v contain -e flag, expected none when no prompt", args)
			break
		}
	}
}

// TestDefaultAgentForSession_DBHappyPath verifies that when root_agent_name is
// set in the DB, DefaultAgentForSession returns it regardless of directory.
func TestDefaultAgentForSession_DBHappyPath(t *testing.T) {
	const sessionName = "testrepo@main"
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if err := d.UpsertStatusSeedRootAgentName(sessionName, "testrepo", "/worktrees/main", "idle", nil, nil, "coordinator", "", ""); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}

	got := DefaultAgentForSession(sessionName, "/worktrees/main", "", d)
	if got != "coordinator" {
		t.Errorf("DefaultAgentForSession = %q, want %q", got, "coordinator")
	}
}

// TestDefaultAgentForSession_ExplicitOverridesDB verifies that an explicit
// agent value is returned as-is, bypassing the DB.
func TestDefaultAgentForSession_ExplicitOverridesDB(t *testing.T) {
	const sessionName = "testrepo@main"
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if err := d.UpsertStatusSeedRootAgentName(sessionName, "testrepo", "/worktrees/main", "idle", nil, nil, "coordinator", "", ""); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}

	got := DefaultAgentForSession(sessionName, "/worktrees/main", "worker", d)
	if got != "worker" {
		t.Errorf("DefaultAgentForSession = %q, want %q (explicit should win)", got, "worker")
	}
}

// TestDefaultAgentForSession_PreMigrationNULL verifies that when the DB row
// exists but root_agent_name is NULL (pre-migration), DefaultAgentForSession
// falls back to the directory heuristic.
func TestDefaultAgentForSession_PreMigrationNULL(t *testing.T) {
	const sessionName = "testrepo@main"
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	// Seed a row WITHOUT root_agent_name (NULL).
	if err := d.UpsertStatus(sessionName, "testrepo", "/worktrees/nixos-config", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Directory heuristic for /worktrees/nixos-config returns "coordinator".
	got := DefaultAgentForSession(sessionName, "/worktrees/nixos-config", "", d)
	want := DefaultAgent("/worktrees/nixos-config", "")
	if got != want {
		t.Errorf("DefaultAgentForSession = %q, want directory heuristic %q", got, want)
	}
}

// TestDefaultAgentForSession_NilDB verifies that when d is nil,
// DefaultAgentForSession falls back to the directory heuristic unconditionally.
func TestDefaultAgentForSession_NilDB(t *testing.T) {
	got := DefaultAgentForSession("testrepo@main", "/worktrees/nixos-config", "", nil)
	want := DefaultAgent("/worktrees/nixos-config", "")
	if got != want {
		t.Errorf("DefaultAgentForSession (nil DB) = %q, want %q", got, want)
	}
}

// TestDefaultAgentForSession_NoRow verifies that when no DB row exists for the
// session (new session, not yet seeded), DefaultAgentForSession silently falls
// back to the directory heuristic (no deprecation warning emitted).
func TestDefaultAgentForSession_NoRow(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	// No row seeded — rowExists=false path.
	got := DefaultAgentForSession("testrepo@main", "/worktrees/nixos-config", "", d)
	want := DefaultAgent("/worktrees/nixos-config", "")
	if got != want {
		t.Errorf("DefaultAgentForSession (no row) = %q, want %q", got, want)
	}
}

// TestHarnessBinary asserts that harnessBinary returns the correct binary name
// for each harness value (issue #1290).
func TestHarnessBinary(t *testing.T) {
	tests := []struct {
		harness string
		want    string
	}{
		{"pi", "pi"},
		{"", "pi"},
		{"other", "other"},
	}
	for _, tc := range tests {
		got := harnessBinary(tc.harness)
		if got != tc.want {
			t.Errorf("harnessBinary(%q) = %q, want %q", tc.harness, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests for sanitiseBranchComponent / worktreeBranchComponent (#1479)
// ---------------------------------------------------------------------------

// TestSanitiseBranchComponent covers the sanitisation rules: "." → "_",
// "/" → "--", ":" → "_", and whitespace → "_".
func TestSanitiseBranchComponent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		// want must not contain ".", "/", ":", " ", or "\t"
		wantSubstrings []string // substrings that must appear
		wantAbsent     []string // chars/strings that must NOT appear
	}{
		{
			name:           "dot in branch",
			input:          "prismatic-bot-traefik-40.x",
			wantSubstrings: []string{"prismatic-bot-traefik-40_x"},
			wantAbsent:     []string{"."},
		},
		{
			name:           "slash in branch (regression)",
			input:          "dependabot/foo/bar",
			wantSubstrings: []string{"dependabot--foo--bar"},
			wantAbsent:     []string{"/"},
		},
		{
			name:           "combined slash and dot",
			input:          "dependabot/foo/v2.3.1",
			wantSubstrings: []string{"dependabot--foo--v2_3_1"},
			wantAbsent:     []string{".", "/"},
		},
		{
			name:       "all dots",
			input:      "...",
			wantAbsent: []string{"."},
		},
		{
			name:       "colon in branch",
			input:      "feat:my-feature",
			wantAbsent: []string{":"},
		},
		{
			name:       "whitespace in branch",
			input:      "feature branch",
			wantAbsent: []string{" "},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := sanitiseBranchComponent(tc.input)
			if got == "" {
				t.Errorf("sanitiseBranchComponent(%q) returned empty string", tc.input)
			}
			for _, want := range tc.wantSubstrings {
				if !strings.Contains(got, want) {
					t.Errorf("sanitiseBranchComponent(%q) = %q, want substring %q", tc.input, got, want)
				}
			}
			for _, bad := range tc.wantAbsent {
				if strings.Contains(got, bad) {
					t.Errorf("sanitiseBranchComponent(%q) = %q, still contains %q", tc.input, got, bad)
				}
			}
		})
	}
}

// TestSanitiseBranchComponent_AllUnsafeNoEmpty asserts that a component made
// entirely of unsafe characters (e.g. "...") produces a non-empty result
// without panicking.
func TestSanitiseBranchComponent_AllUnsafeNoEmpty(t *testing.T) {
	inputs := []string{"...", ". . .", ":/.", "\t\t"}
	for _, in := range inputs {
		got := sanitiseBranchComponent(in)
		if got == "" {
			// All dots become underscores, so the result should be non-empty.
			// (The only way to get "" is if the input is ""; that's not the
			// case here because the substitutions preserve length.)
			t.Errorf("sanitiseBranchComponent(%q) returned unexpected empty string", in)
		}
		for _, bad := range []string{".", "/", ":"} {
			if strings.Contains(got, bad) {
				t.Errorf("sanitiseBranchComponent(%q) = %q still contains %q", in, got, bad)
			}
		}
	}
}

// TestWorktreeBranchComponent_FilepathFallback_SanitisesDot verifies that the
// filepath.Base fallback path (used when git is not available) also sanitises
// dots. We pass a directory whose base name contains a dot and is not a git
// repo.
func TestWorktreeBranchComponent_FilepathFallback_SanitisesDot(t *testing.T) {
	// Create a non-git directory whose base name contains a dot.
	dir := filepath.Join(t.TempDir(), "branch-40.x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	got := worktreeBranchComponent(dir)
	if strings.Contains(got, ".") {
		t.Errorf("worktreeBranchComponent(%q) = %q, still contains '.'", dir, got)
	}
	if got == "" {
		t.Errorf("worktreeBranchComponent(%q) returned empty string", dir)
	}
}

// ---------------------------------------------------------------------------
// Test: tmux new-session failure propagation (#1479 AC #5, AC #6)
// ---------------------------------------------------------------------------

// failTmuxBin installs a fake tmux binary that always exits non-zero, returning
// a fixed stderr message. It redirects tmux.TmuxBin for the duration of the
// test. Only call this from non-parallel tests.
func failTmuxBin(t *testing.T, stderrMsg string) {
	t.Helper()
	wrapperPath := t.TempDir() + "/tmux"
	script := fmt.Sprintf("#!/bin/sh\necho '%s' >&2\nexit 1\n", stderrMsg)
	if err := os.WriteFile(wrapperPath, []byte(script), 0755); err != nil {
		t.Fatalf("failTmuxBin: write wrapper: %v", err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = wrapperPath
	t.Cleanup(func() { tmux.TmuxBin = orig })
}

// TestCreate_TmuxNewSessionFailure verifies that when tmux new-session fails,
// session.Create returns a non-nil error.
func TestCreate_TmuxNewSessionFailure(t *testing.T) {
	failTmuxBin(t, "duplicate session: test-session")
	dir := t.TempDir()
	err := Create("test-session", dir, Opts{ForceFresh: true})
	if err == nil {
		t.Fatal("expected non-nil error when tmux new-session fails, got nil")
	}
	if !strings.Contains(err.Error(), "test-session") {
		t.Errorf("error %q does not mention the session name", err.Error())
	}
}
