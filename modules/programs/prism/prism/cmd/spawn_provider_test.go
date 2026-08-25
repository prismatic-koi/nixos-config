package cmd

// spawn_provider_test.go — tests for the --provider override flag
// (issue #2852).
//
// Coverage:
//   - --provider + --abtest mutual exclusion (runSpawn path)
//   - --provider + non-pi --harness loud rejection before session creation
//   - proxySpawn forwards "provider" in the JSON body and enforces the
//     --abtest mutual exclusion client-side

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/harness"
)

// buildProviderCmd returns a *cobra.Command with all flags registered
// (mirrors the real spawnCmd registration) so runSpawn / proxySpawn can be
// called directly in tests. Mirrors buildAbtestCmd plus the provider flag.
func buildProviderCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("pr", "", "")
	cmd.Flags().String("repo", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().StringArray("abtest", nil, "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().String("provider", "", "")
	cmd.Flags().Bool("attach", false, "")
	cmd.Flags().String("harness", "pi", "")
	cmd.Flags().StringArray("model-override", nil, "")
	cmd.Flags().String("isolation", "", "")
	cmd.Flags().Bool("ignore-concurrency-cap", false, "")
	cmd.Flags().String("prompt-source", "", "")
	addPromptFlags(cmd)
	return cmd
}

// TestRunSpawn_ProviderAbtestMutualExclusion verifies that passing both
// --provider and --abtest returns a clear error with no side effects,
// matching the existing --profile behaviour (#2852).
func TestRunSpawn_ProviderAbtestMutualExclusion(t *testing.T) {
	cmd := buildProviderCmd(t)
	_ = cmd.Flags().Set("provider", "openrouter")
	_ = cmd.Flags().Set("abtest", "profileA")
	_ = cmd.Flags().Set("abtest", "profileB")

	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("expected error for --provider + --abtest, got nil")
	}
	if !strings.Contains(err.Error(), "--provider") || !strings.Contains(err.Error(), "--abtest") {
		t.Errorf("expected error naming both flags, got: %v", err)
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' in error, got: %v", err)
	}
}

// TestRunSpawn_ProviderNonPiHarnessRejected verifies that --provider combined
// with a non-pi --harness exits non-zero with an error naming both flags,
// before any session state is created (#2852).
func TestRunSpawn_ProviderNonPiHarnessRejected(t *testing.T) {
	// Register a second (fake) harness so the harness Lookup validation
	// passes and the pi-only provider gate is what fires. Only "pi" is
	// registered in production.
	if err := harness.Register(harness.Registration{
		Name:  "test-other-harness",
		Shape: harness.TransportHTTPPort,
		Factory: func(endpoint string, _ *http.Client, role, model string) harness.Harness {
			return &harness.FakeHarness{}
		},
	}); err != nil {
		t.Fatalf("register fake harness: %v", err)
	}

	cmd := buildProviderCmd(t)
	_ = cmd.Flags().Set("provider", "openrouter")
	_ = cmd.Flags().Set("harness", "test-other-harness")
	_ = cmd.Flags().Set("prompt", "hi")

	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("expected error for --provider + non-pi --harness, got nil")
	}
	if !strings.Contains(err.Error(), "--provider") || !strings.Contains(err.Error(), "--harness") {
		t.Errorf("expected error naming both flags, got: %v", err)
	}
}

// TestProxySpawn_ProviderForwarded verifies that proxySpawn includes the
// provider field in the JSON body sent to the host-API /spawn endpoint.
func TestProxySpawn_ProviderForwarded(t *testing.T) {
	type spawnReq struct {
		Branch   string `json:"branch"`
		Provider string `json:"provider"`
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
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@test-provider"}`))
	})

	cmd := buildProviderCmd(t)
	_ = cmd.Flags().Set("branch", "test-provider")
	_ = cmd.Flags().Set("prompt", "hi")
	_ = cmd.Flags().Set("provider", "openrouter")

	if err := proxySpawn(srv.apiURL(), cmd); err != nil {
		t.Fatalf("proxySpawn: %v", err)
	}

	select {
	case req := <-reqCh:
		if req.Provider != "openrouter" {
			t.Errorf("provider = %q, want %q", req.Provider, "openrouter")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}

// TestProxySpawn_ProviderAbtestMutualExclusion verifies that proxySpawn
// rejects --provider + --abtest client-side for fast feedback before the
// round-trip to the host API (#2852).
func TestProxySpawn_ProviderAbtestMutualExclusion(t *testing.T) {
	cmd := buildProviderCmd(t)
	_ = cmd.Flags().Set("branch", "test-provider-abtest")
	_ = cmd.Flags().Set("prompt", "hi")
	_ = cmd.Flags().Set("provider", "openrouter")
	_ = cmd.Flags().Set("abtest", "profileA")
	_ = cmd.Flags().Set("abtest", "profileB")

	err := proxySpawn("http://localhost:1", cmd)
	if err == nil {
		t.Fatal("expected error for --provider + --abtest on proxy path, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' in error, got: %v", err)
	}
}
