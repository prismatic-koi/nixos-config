package cmd

// isolation_test.go — unit tests for the --isolation flag, isolation mode
// resolution, and sidecar command construction per mode.

import (
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
)

// newSpawnCmdForTest returns a fresh cobra.Command with the same flags as
// spawnCmd but isolated from the global state, preventing test pollution.
func newSpawnCmdForTest() *cobra.Command {
	cmd := &cobra.Command{Use: "spawn-test"}
	cmd.Flags().String("isolation", "", "Isolation mode: podman, bwrap, or host")
	cmd.Flags().Bool("host-mode", false, "Deprecated alias for --isolation host")
	return cmd
}

// TestResolveIsolationMode_ValidValues verifies that each valid isolation mode
// string is accepted and returned correctly by resolveIsolationMode.
func TestResolveIsolationMode_ValidValues(t *testing.T) {
	cases := []struct {
		flag string
		want config.IsolationMode
	}{
		{"podman", config.IsolationPodman},
		{"bwrap", config.IsolationBwrap},
		{"sandbox-exec", config.IsolationSandboxExec},
		{"host", config.IsolationHost},
	}

	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			if tc.flag == "bwrap" && runtime.GOOS != "linux" {
				t.Skip("bwrap only available on Linux")
			}
			if tc.flag == "sandbox-exec" && runtime.GOOS != "darwin" {
				t.Skip("sandbox-exec only available on macOS")
			}
			cmd := newSpawnCmdForTest()
			if err := cmd.Flags().Set("isolation", tc.flag); err != nil {
				t.Fatalf("set --isolation: %v", err)
			}
			cfg := config.Config{}
			got, err := resolveIsolationMode(cmd, cfg)
			if err != nil {
				t.Fatalf("resolveIsolationMode: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveIsolationMode_UnknownValue verifies that an unknown isolation mode
// returns an error with a message listing the valid values.
func TestResolveIsolationMode_UnknownValue(t *testing.T) {
	cmd := newSpawnCmdForTest()
	if err := cmd.Flags().Set("isolation", "docker"); err != nil {
		t.Fatalf("set --isolation: %v", err)
	}
	cfg := config.Config{}
	_, err := resolveIsolationMode(cmd, cfg)
	if err == nil {
		t.Fatal("expected error for unknown isolation mode, got nil")
	}
	if !strings.Contains(err.Error(), "unknown isolation mode") {
		t.Errorf("error %q does not contain 'unknown isolation mode'", err.Error())
	}
	// Error message must list all valid values, including sandbox-exec.
	for _, m := range []string{"podman", "bwrap", "sandbox-exec", "host"} {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("error %q does not mention valid mode %q", err.Error(), m)
		}
	}
}

// TestResolveIsolationMode_HostModeAlias verifies that --host-mode maps to "host".
func TestResolveIsolationMode_HostModeAlias(t *testing.T) {
	cmd := newSpawnCmdForTest()
	if err := cmd.Flags().Set("host-mode", "true"); err != nil {
		t.Fatalf("set --host-mode: %v", err)
	}
	cfg := config.Config{}
	got, err := resolveIsolationMode(cmd, cfg)
	if err != nil {
		t.Fatalf("resolveIsolationMode: %v", err)
	}
	if got != config.IsolationHost {
		t.Errorf("got %q, want %q", got, config.IsolationHost)
	}
}

// TestResolveIsolationMode_BothIsolationAndHostMode verifies that using both
// --isolation and --host-mode together returns an error.
func TestResolveIsolationMode_BothIsolationAndHostMode(t *testing.T) {
	cmd := newSpawnCmdForTest()
	if err := cmd.Flags().Set("isolation", "host"); err != nil {
		t.Fatalf("set --isolation: %v", err)
	}
	if err := cmd.Flags().Set("host-mode", "true"); err != nil {
		t.Fatalf("set --host-mode: %v", err)
	}
	cfg := config.Config{}
	_, err := resolveIsolationMode(cmd, cfg)
	if err == nil {
		t.Fatal("expected error when both --isolation and --host-mode are set")
	}
	if !strings.Contains(err.Error(), "--isolation and --host-mode") {
		t.Errorf("error %q does not mention both flags", err.Error())
	}
}

// TestResolveIsolationMode_FallbackFromConfig verifies that when no flags are
// set the effective isolation mode from config.Config is used.
func TestResolveIsolationMode_FallbackFromConfig(t *testing.T) {
	cases := []struct {
		cfgMode config.IsolationMode
		want    config.IsolationMode
	}{
		{config.IsolationPodman, config.IsolationPodman},
		{config.IsolationHost, config.IsolationHost},
	}

	for _, tc := range cases {
		t.Run(string(tc.cfgMode), func(t *testing.T) {
			cmd := newSpawnCmdForTest() // fresh command, no flags set
			cfg := config.Config{DefaultIsolationMode: tc.cfgMode}
			got, err := resolveIsolationMode(cmd, cfg)
			if err != nil {
				t.Fatalf("resolveIsolationMode: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveIsolationMode_BwrapOnDarwin verifies that --isolation bwrap on
// non-Linux returns a clear error.
func TestResolveIsolationMode_BwrapOnDarwin(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("this test validates non-Linux platform rejection; skipping on Linux")
	}
	cmd := newSpawnCmdForTest()
	if err := cmd.Flags().Set("isolation", "bwrap"); err != nil {
		t.Fatalf("set --isolation: %v", err)
	}
	cfg := config.Config{}
	_, err := resolveIsolationMode(cmd, cfg)
	if err == nil {
		t.Fatal("expected error for bwrap on non-Linux, got nil")
	}
	if !strings.Contains(err.Error(), "requires Linux") {
		t.Errorf("error %q does not mention 'requires Linux'", err.Error())
	}
}

// TestResolveIsolationMode_SandboxExecOnLinux verifies that --isolation
// sandbox-exec on non-Darwin (e.g. Linux) returns a clear error naming the
// platform requirement.
func TestResolveIsolationMode_SandboxExecOnLinux(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this test validates non-Darwin platform rejection; skipping on macOS")
	}
	cmd := newSpawnCmdForTest()
	if err := cmd.Flags().Set("isolation", "sandbox-exec"); err != nil {
		t.Fatalf("set --isolation: %v", err)
	}
	cfg := config.Config{}
	_, err := resolveIsolationMode(cmd, cfg)
	if err == nil {
		t.Fatal("expected error for sandbox-exec on non-Darwin, got nil")
	}
	if !strings.Contains(err.Error(), "requires macOS") {
		t.Errorf("error %q does not mention 'requires macOS'", err.Error())
	}
}

// TestResolveIsolationMode_SandboxExecHostModeAlias verifies that
// --isolation sandbox-exec combined with --host-mode returns the existing
// mutual-exclusion error (same behaviour as any other isolation mode + host-mode).
func TestResolveIsolationMode_SandboxExecHostModeAlias(t *testing.T) {
	cmd := newSpawnCmdForTest()
	if err := cmd.Flags().Set("isolation", "sandbox-exec"); err != nil {
		t.Fatalf("set --isolation: %v", err)
	}
	if err := cmd.Flags().Set("host-mode", "true"); err != nil {
		t.Fatalf("set --host-mode: %v", err)
	}
	cfg := config.Config{}
	_, err := resolveIsolationMode(cmd, cfg)
	if err == nil {
		t.Fatal("expected mutual-exclusion error when both --isolation sandbox-exec and --host-mode are set")
	}
	if !strings.Contains(err.Error(), "--isolation and --host-mode") {
		t.Errorf("error %q does not mention both flags", err.Error())
	}
}

// TestResolveIsolationMode_FallbackFromConfig_SandboxExec verifies that when
// no flags are set and config specifies sandbox-exec, the mode is used — but
// only on Darwin (the platform guard rejects it elsewhere).
func TestResolveIsolationMode_FallbackFromConfig_SandboxExec(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec from config is only valid on macOS; platform guard rejects it on other platforms")
	}
	cmd := newSpawnCmdForTest() // fresh command, no flags set
	cfg := config.Config{DefaultIsolationMode: config.IsolationSandboxExec}
	got, err := resolveIsolationMode(cmd, cfg)
	if err != nil {
		t.Fatalf("resolveIsolationMode: %v", err)
	}
	if got != config.IsolationSandboxExec {
		t.Errorf("got %q, want %q", got, config.IsolationSandboxExec)
	}
}

// TestIsolationFlagExists verifies that the --isolation flag is registered
// on spawnCmd with the correct default value.
func TestIsolationFlagExists(t *testing.T) {
	flag := spawnCmd.Flags().Lookup("isolation")
	if flag == nil {
		t.Fatal("--isolation flag not found on spawnCmd")
	}
	// Default should be empty (falls back to config.json at runtime).
	if flag.DefValue != "" {
		t.Errorf("--isolation default = %q, want %q (empty, falls back to config)", flag.DefValue, "")
	}
}

// TestHostModeFlagDeprecationExists verifies that --host-mode still exists
// as a deprecated alias.
func TestHostModeFlagDeprecationExists(t *testing.T) {
	flag := spawnCmd.Flags().Lookup("host-mode")
	if flag == nil {
		t.Fatal("--host-mode flag not found on spawnCmd (should still exist as deprecated alias)")
	}
}

// TestSidecarCmd_IsolationModeFlag verifies that the --isolation-mode flag
// is registered on sidecarCmd.
func TestSidecarCmd_IsolationModeFlag(t *testing.T) {
	flag := sidecarCmd.Flags().Lookup("isolation-mode")
	if flag == nil {
		t.Fatal("--isolation-mode flag not found on sidecarCmd")
	}
	// Default should be empty (sidecar falls back to --container for back-compat).
	if flag.DefValue != "" {
		t.Errorf("--isolation-mode default = %q, want %q (empty, falls back to --container flag)", flag.DefValue, "")
	}
}

// TestAgentRunCmd_SessionFlagExists verifies that the prism agent-run
// subcommand exists and has the required --session flag.
func TestAgentRunCmd_SessionFlagExists(t *testing.T) {
	flag := agentRunCmd.Flags().Lookup("session")
	if flag == nil {
		t.Fatal("--session flag not found on agentRunCmd")
	}
}
