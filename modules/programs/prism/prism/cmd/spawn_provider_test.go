package cmd

// spawn_provider_test.go — tests for the --provider flag on prism spawn
// (issue #2852).
//
// Coverage:
//   - --provider with a non-pi --harness is rejected before any state is
//     created, and the error names BOTH flags
//   - --provider with --abtest is rejected, matching --profile
//   - --provider alone passes validation
//   - proxySpawn forwards the provider field only when a value was given
//   - proxySpawn applies the same two rejections client-side
//
// The runSpawn cases follow the isolation contract established by
// spawn_harness_test.go: PRISM_HOST_API is cleared so the proxy branch is not
// taken, and PRISM_SPAWN_PATH points at a non-git temp dir so resolveBareRoot
// fails with "not inside a git repo" instead of reading the live tmux pane
// path and creating real worktrees. Every case here rejects BEFORE
// resolveBareRoot, so no side effect is possible either way.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// buildProviderCmd returns a *cobra.Command carrying the flag set runSpawn
// reads, including --provider. Mirrors buildAbtestCmd in spawn_abtest_test.go.
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

// isolateSpawnValidation clears the env that would otherwise let runSpawn
// reach the live tmux pane / host API.
func isolateSpawnValidation(t *testing.T) {
	t.Helper()
	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")
}

// ── runSpawn rejections ──────────────────────────────────────────────────────

// TestRunSpawn_ProviderWithNonPiHarness_Rejected is the issue #2852 edge-case
// AC: --provider alongside a non-pi --harness exits non-zero before any
// session is created, with an error naming both flags.
//
// The check deliberately runs before the harness-registry lookup, so the
// error is about the combination rather than about an unregistered harness
// name. Provider decides routing and billing, so the silent-scoping used by
// --model / --variant would leave the operator confidently wrong.
func TestRunSpawn_ProviderWithNonPiHarness_Rejected(t *testing.T) {
	cmd := buildProviderCmd(t)
	_ = cmd.Flags().Set("provider", "openrouter")
	_ = cmd.Flags().Set("harness", "not-pi")
	_ = cmd.Flags().Set("prompt", "do the mahi")
	isolateSpawnValidation(t)

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("expected error for --provider with a non-pi --harness, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"--provider", "--harness", "openrouter", "not-pi"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q", msg, want)
		}
	}
}

// TestRunSpawn_ProviderWithAbtest_Rejected is the issue #2852 edge-case AC
// for the abtest combination: it is refused with a mutual-exclusion error,
// matching the existing --profile behaviour. Each abtest arm draws its
// provider from its own profile slot by design.
func TestRunSpawn_ProviderWithAbtest_Rejected(t *testing.T) {
	cmd := buildProviderCmd(t)
	_ = cmd.Flags().Set("provider", "openrouter")
	_ = cmd.Flags().Set("abtest", "profileA")
	_ = cmd.Flags().Set("abtest", "profileB")
	_ = cmd.Flags().Set("prompt", "do the mahi")
	isolateSpawnValidation(t)

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("expected error for --provider with --abtest, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--provider") {
		t.Errorf("error %q does not name --provider", err.Error())
	}
}

// TestRunSpawn_ProviderWithPiHarness_PassesValidation verifies the happy
// path clears both checks. Like the harness tests, it asserts only that
// runSpawn does not fail on the provider validation — the command still
// fails later on the missing git repo.
func TestRunSpawn_ProviderWithPiHarness_PassesValidation(t *testing.T) {
	cmd := buildProviderCmd(t)
	_ = cmd.Flags().Set("provider", "openrouter")
	_ = cmd.Flags().Set("harness", "pi")
	_ = cmd.Flags().Set("prompt", "do the mahi")
	isolateSpawnValidation(t)
	withNoopTmux(t)

	err := runSpawn(cmd, nil)
	if err != nil && strings.Contains(err.Error(), "--provider") {
		t.Errorf("runSpawn with --provider and --harness pi returned a provider validation error: %v", err)
	}
}

// TestRunSpawn_NoProviderWithNonPiHarness_NoProviderError is the
// no-regression guard: without --provider, a non-pi --harness must fail on
// the harness registry as it always has, not on the new provider check.
func TestRunSpawn_NoProviderWithNonPiHarness_NoProviderError(t *testing.T) {
	cmd := buildProviderCmd(t)
	_ = cmd.Flags().Set("harness", "not-pi")
	_ = cmd.Flags().Set("prompt", "do the mahi")
	isolateSpawnValidation(t)

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("expected the pre-existing unknown-harness error, got nil")
	}
	if strings.Contains(err.Error(), "--provider") {
		t.Errorf("provider validation fired without --provider; got: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown harness") {
		t.Errorf("expected 'unknown harness' error, got: %v", err)
	}
}

// ── validateProviderFlag unit cases ──────────────────────────────────────────

// TestValidateProviderFlag covers the helper both front doors share.
func TestValidateProviderFlag(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		harness  string
		abtest   bool
		wantErr  bool
	}{
		{"no provider, non-pi harness", "", "not-pi", false, false},
		{"no provider, abtest", "", "pi", true, false},
		{"provider with pi", "openrouter", "pi", false, false},
		{"provider with empty harness defaults to pi", "openrouter", "", false, false},
		{"provider with non-pi harness", "openrouter", "not-pi", false, true},
		{"provider with abtest", "openrouter", "pi", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProviderFlag(tc.provider, tc.harness, tc.abtest)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

// ── proxySpawn forwarding ────────────────────────────────────────────────────

// buildProviderProxyCmd mirrors the flag set proxySpawn reads.
func buildProviderProxyCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("pr", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().StringArray("abtest", nil, "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().String("provider", "", "")
	cmd.Flags().String("harness", "pi", "")
	cmd.Flags().Bool("ignore-concurrency-cap", false, "")
	cmd.Flags().String("isolation", "", "")
	cmd.Flags().Bool("containers", false, "")
	cmd.Flags().StringArray("model-override", nil, "")
	cmd.Flags().Bool("reuse", false, "")
	cmd.Flags().Bool("wait", false, "")
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().Duration("wait-timeout", defaultSpawnWaitTimeout, "")
	addPromptFlags(cmd)
	return cmd
}

// TestProxySpawn_ProviderForwarded verifies that a coordinator running inside
// a sandbox does not silently lose --provider across the host-API boundary:
// the value must reach the POST body so the host-side spawn re-emits it.
func TestProxySpawn_ProviderForwarded(t *testing.T) {
	reqCh := make(chan map[string]any, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]any
		_ = json.NewDecoder(r.Body).Decode(&raw)
		reqCh <- raw
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@test-provider"}`))
	})

	cmd := buildProviderProxyCmd(t)
	_ = cmd.Flags().Set("branch", "test-provider")
	_ = cmd.Flags().Set("prompt", "hi")
	_ = cmd.Flags().Set("provider", "openrouter")

	if err := proxySpawn(srv.apiURL(), cmd); err != nil {
		t.Fatalf("proxySpawn: %v", err)
	}

	select {
	case raw := <-reqCh:
		if raw["provider"] != "openrouter" {
			t.Errorf("provider = %v, want openrouter", raw["provider"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}

// TestProxySpawn_ProviderAbsentWhenNotPassed is the no-regression guard: with
// no --provider the field must be absent from the body, so the host-side
// spawn keeps the profile slot's provider.
func TestProxySpawn_ProviderAbsentWhenNotPassed(t *testing.T) {
	reqCh := make(chan map[string]any, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]any
		_ = json.NewDecoder(r.Body).Decode(&raw)
		reqCh <- raw
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@no-provider"}`))
	})

	cmd := buildProviderProxyCmd(t)
	_ = cmd.Flags().Set("branch", "no-provider")
	_ = cmd.Flags().Set("prompt", "hi")

	if err := proxySpawn(srv.apiURL(), cmd); err != nil {
		t.Fatalf("proxySpawn: %v", err)
	}

	select {
	case raw := <-reqCh:
		if _, present := raw["provider"]; present {
			t.Errorf("provider field present when --provider was not passed; got %v", raw["provider"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}

// TestProxySpawn_ProviderWithNonPiHarness_RejectedClientSide verifies the
// proxy path rejects the same combination the direct path does, before the
// round trip — mirroring the --isolation precedent, so the error shape is
// identical on both front doors.
func TestProxySpawn_ProviderWithNonPiHarness_RejectedClientSide(t *testing.T) {
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("proxySpawn must reject before contacting the host API")
		w.WriteHeader(http.StatusInternalServerError)
	})

	cmd := buildProviderProxyCmd(t)
	_ = cmd.Flags().Set("branch", "bad-provider")
	_ = cmd.Flags().Set("prompt", "hi")
	_ = cmd.Flags().Set("provider", "openrouter")
	_ = cmd.Flags().Set("harness", "not-pi")

	err := proxySpawn(srv.apiURL(), cmd)
	if err == nil {
		t.Fatal("expected error for --provider with a non-pi --harness, got nil")
	}
	for _, want := range []string{"--provider", "--harness"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err.Error(), want)
		}
	}
}

// TestProxySpawn_ProviderWithAbtest_RejectedClientSide mirrors the direct
// path's abtest rejection on the proxy front door.
func TestProxySpawn_ProviderWithAbtest_RejectedClientSide(t *testing.T) {
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("proxySpawn must reject before contacting the host API")
		w.WriteHeader(http.StatusInternalServerError)
	})

	cmd := buildProviderProxyCmd(t)
	_ = cmd.Flags().Set("branch", "bad-provider-abtest")
	_ = cmd.Flags().Set("prompt", "hi")
	_ = cmd.Flags().Set("provider", "openrouter")
	_ = cmd.Flags().Set("abtest", "profileA")
	_ = cmd.Flags().Set("abtest", "profileB")

	err := proxySpawn(srv.apiURL(), cmd)
	if err == nil {
		t.Fatal("expected error for --provider with --abtest, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' in error, got: %v", err)
	}
}
