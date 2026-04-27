package cmd

// Tests for the --harness flag on prism spawn.
//
// Coverage:
//   - runSpawn rejects unknown harness values before any state is created
//   - runSpawn accepts "opencode" (explicitly or as default)
//   - proxySpawn forwards the harness field to the host-API /spawn endpoint
//   - Container-mode (PRISM_HOST_API set): harness forwarded correctly

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// ── runSpawn harness validation ───────────────────────────────────────────────

// TestRunSpawn_UnknownHarness_ReturnsErrorBeforeStateCreated verifies that
// an unknown --harness value causes runSpawn to return a non-nil error
// immediately, before any worktree, tmux session, or DB row is created.
//
// Because runSpawn validates harness BEFORE git.CreateWorktree(), the error
// fires even when no git repo is available — the test does not need a real repo.
func TestRunSpawn_UnknownHarness_ReturnsErrorBeforeStateCreated(t *testing.T) {
	// Build a minimal cobra command wired with the same flags as spawnCmd.
	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("pr", "", "")
	cmd.Flags().String("repo", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().Bool("host-mode", false, "")
	cmd.Flags().Bool("attach", false, "")
	cmd.Flags().String("harness", "opencode", "")
	addPromptFlags(cmd)

	// Set an unknown harness.
	_ = cmd.Flags().Set("harness", "pi")

	// runSpawn must return an error. We must ensure PRISM_HOST_API is unset
	// so the proxy guard is not triggered (which would try to contact a server).
	t.Setenv("PRISM_HOST_API", "")

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("runSpawn with --harness pi: expected non-nil error, got nil")
	}
	if !strings.Contains(err.Error(), `unknown harness "pi"`) {
		t.Errorf("error %q does not contain expected text 'unknown harness \"pi\"'", err.Error())
	}
	if !strings.Contains(err.Error(), "valid harnesses:") {
		t.Errorf("error %q does not mention 'valid harnesses:'", err.Error())
	}
}

// TestRunSpawn_HarnessOpencode_Explicit_PassesValidation verifies that when
// --harness opencode is set explicitly, the validation passes (no harness error).
//
// We intentionally exercise the validation only — not the full spawn flow —
// by ensuring PRISM_HOST_API is unset and the command does not have a valid
// repo context, so runSpawn will fail later with a different error (e.g.
// "not inside a git repo"). The test just asserts the error is NOT a harness
// validation error, proving that validation passed.
//
// IMPORTANT ISOLATION: PRISM_SPAWN_PATH must be set to a non-git directory so
// that resolveBareRoot() uses that path directly and returns "not inside a git
// repo" without calling tmux.CurrentPanePath(). If PRISM_SPAWN_PATH is empty,
// resolveBareRoot falls through to CurrentPanePath which reads the live tmux
// pane path — potentially a nixos-config worktree — causing runSpawn to create
// real worktrees, DB rows, and tmux sessions in the live environment. See #1180.
func TestRunSpawn_HarnessOpencode_Explicit_PassesValidation(t *testing.T) {
	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("pr", "", "")
	cmd.Flags().String("repo", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().Bool("host-mode", false, "")
	cmd.Flags().Bool("attach", false, "")
	cmd.Flags().String("harness", "opencode", "")
	addPromptFlags(cmd)

	_ = cmd.Flags().Set("harness", "opencode")

	t.Setenv("PRISM_HOST_API", "")
	// Set PRISM_SPAWN_PATH to a non-git temp dir so resolveBareRoot uses it
	// directly and returns "not inside a git repo" without calling
	// tmux.CurrentPanePath(). An empty value would fall through to
	// CurrentPanePath which reads the live pane path and could trigger real
	// session creation if the live pane is inside a prism bare repo (#1180).
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	// PRISM_BARE_ROOT points to nowhere — runSpawn will fail, but not on harness.
	t.Setenv("PRISM_BARE_ROOT", "")
	// Redirect TmuxBin to a no-op stub so no live tmux server is contacted
	// even if an unexpected code path reaches tmux commands.
	withNoopTmux(t)

	err := runSpawn(cmd, nil)
	// We expect a non-nil error (no git repo), but it must not be a harness
	// validation error.
	if err != nil && strings.Contains(err.Error(), "unknown harness") {
		t.Errorf("runSpawn with --harness opencode returned harness validation error: %v", err)
	}
}

// TestRunSpawn_HarnessDefault_PassesValidation verifies that when --harness is
// omitted (defaults to "opencode"), the validation passes.
//
// Same approach as TestRunSpawn_HarnessOpencode_Explicit_PassesValidation: the
// command will fail after the harness check, but not on the harness itself.
// See that test's IMPORTANT ISOLATION comment for the PRISM_SPAWN_PATH rationale.
func TestRunSpawn_HarnessDefault_PassesValidation(t *testing.T) {
	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("pr", "", "")
	cmd.Flags().String("repo", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().Bool("host-mode", false, "")
	cmd.Flags().Bool("attach", false, "")
	cmd.Flags().String("harness", "opencode", "")
	addPromptFlags(cmd)
	// No --harness flag set — stays at default "opencode".

	t.Setenv("PRISM_HOST_API", "")
	// See TestRunSpawn_HarnessOpencode_Explicit_PassesValidation for why
	// PRISM_SPAWN_PATH must be a non-git temp dir rather than empty string.
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")
	withNoopTmux(t)

	err := runSpawn(cmd, nil)
	if err != nil && strings.Contains(err.Error(), "unknown harness") {
		t.Errorf("runSpawn with default harness returned harness validation error: %v", err)
	}
}

// ── proxySpawn harness forwarding (container mode) ───────────────────────────

// TestProxySpawn_HarnessForwarded verifies that proxySpawn includes the harness
// field in the JSON body sent to the host-API /spawn endpoint.
func TestProxySpawn_HarnessForwarded(t *testing.T) {
	type spawnReq struct {
		Branch  string `json:"branch"`
		Harness string `json:"harness"`
	}

	reqCh := make(chan spawnReq, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/spawn" {
			http.Error(w, `{"error":"wrong path"}`, http.StatusBadRequest)
			return
		}
		var req spawnReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqCh <- req
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@test-harness"}`))
	})

	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().Bool("host-mode", false, "")
	cmd.Flags().String("harness", "opencode", "")
	addPromptFlags(cmd)

	_ = cmd.Flags().Set("branch", "test-harness")
	_ = cmd.Flags().Set("harness", "opencode")

	if err := proxySpawn(srv.apiURL(), cmd); err != nil {
		t.Fatalf("proxySpawn: %v", err)
	}

	select {
	case req := <-reqCh:
		if req.Branch != "test-harness" {
			t.Errorf("branch = %q, want %q", req.Branch, "test-harness")
		}
		if req.Harness != "opencode" {
			t.Errorf("harness = %q, want %q", req.Harness, "opencode")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}

// TestProxySpawn_HarnessDefaultForwarded verifies that when --harness is not
// explicitly set (defaults to "opencode"), the default value is still forwarded
// to the host-API in the JSON body.
func TestProxySpawn_HarnessDefaultForwarded(t *testing.T) {
	type spawnReq struct {
		Branch  string `json:"branch"`
		Harness string `json:"harness"`
	}

	reqCh := make(chan spawnReq, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req spawnReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqCh <- req
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@default-harness"}`))
	})

	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().Bool("host-mode", false, "")
	cmd.Flags().String("harness", "opencode", "")
	addPromptFlags(cmd)

	_ = cmd.Flags().Set("branch", "default-harness")
	// harness stays at default ("opencode")

	if err := proxySpawn(srv.apiURL(), cmd); err != nil {
		t.Fatalf("proxySpawn: %v", err)
	}

	select {
	case req := <-reqCh:
		if req.Harness != "opencode" {
			t.Errorf("harness = %q, want %q (default)", req.Harness, "opencode")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}
