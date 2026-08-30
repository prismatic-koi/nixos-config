package cmd

// hostapi_containers_test.go — proxy-path tests for the --containers flag
// forwarding contract. Mirrors the existing --isolation proxy tests so the
// cross-spawn-boundary behaviour is locked in at this layer too: a
// containerised session running `prism spawn --containers ...` for a child
// must reach the host sidecar with body["containers"]=true.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestProxySpawn_ContainersForwardedWhenSet verifies that --containers on
// the proxy path (PRISM_HOST_API set) is forwarded as the "containers" JSON
// field with value true. This is the inverse of the cross-spawn
// forwarding behaviour at the proxy boundary: a coordinator inside a sandbox who
// passes `prism spawn --containers` must have that flag reach the host
// sidecar, otherwise the child session's spawn_inputs.containers_flag and
// agent_status.containers_enabled stay at 0 and the child's proxy is never
// started.
func TestProxySpawn_ContainersForwardedWhenSet(t *testing.T) {
	type spawnReq struct {
		Branch     string `json:"branch"`
		Containers bool   `json:"containers"`
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
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@containers-branch"}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())
	t.Setenv("PRISM_BARE_ROOT", "/prism-git")

	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().String("isolation", "", "")
	cmd.Flags().Bool("containers", false, "")
	cmd.Flags().Bool("ignore-concurrency-cap", false, "")
	cmd.Flags().String("harness", "pi", "")
	addPromptFlags(cmd)
	_ = cmd.Flags().Set("branch", "containers-branch")
	_ = cmd.Flags().Set("prompt", "hi")
	_ = cmd.Flags().Set("containers", "true")

	if err := proxySpawn(srv.apiURL(), cmd); err != nil {
		t.Fatalf("proxySpawn: %v", err)
	}

	select {
	case req := <-reqCh:
		if !req.Containers {
			t.Errorf("containers = %v, want true (issue #2323: --containers must cross the proxy boundary)", req.Containers)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}

// TestProxySpawn_ContainersOmittedWhenUnset verifies that when --containers
// is not passed on the command line, the JSON body does NOT include a
// "containers" key. This is the symmetric guarantee for cross-spawn
// forwarding: an unset child does NOT accidentally inherit a parent's
// enabled state via a default-zero-value bleed. Mirrors
// TestProxySpawn_IsolationOmittedWhenUnset for --isolation.
func TestProxySpawn_ContainersOmittedWhenUnset(t *testing.T) {
	rawCh := make(chan map[string]any, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]any
		_ = json.NewDecoder(r.Body).Decode(&raw)
		rawCh <- raw
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@no-containers-branch"}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())
	t.Setenv("PRISM_BARE_ROOT", "/prism-git")

	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().String("isolation", "", "")
	cmd.Flags().Bool("containers", false, "")
	cmd.Flags().Bool("ignore-concurrency-cap", false, "")
	cmd.Flags().String("harness", "pi", "")
	addPromptFlags(cmd)
	_ = cmd.Flags().Set("branch", "no-containers-branch")
	_ = cmd.Flags().Set("prompt", "hi")
	// --containers deliberately NOT set.

	if err := proxySpawn(srv.apiURL(), cmd); err != nil {
		t.Fatalf("proxySpawn: %v", err)
	}

	select {
	case raw := <-rawCh:
		if _, present := raw["containers"]; present {
			t.Errorf("containers key present in body when --containers not passed; body = %v (issue #2323: unset child must not inherit parent state)", raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}

// TestProxySpawn_ContainersFalseExplicitlyForwarded verifies that an
// explicit `--containers=false` is forwarded (so the child sees
// containers: false in the request body), distinguishing it from the
// "flag not passed" case above. This matches the --isolation
// "absent vs empty" semantic.
//
// In practice this is rare on the CLI (passing --containers=false has no
// observable effect because the default is also false), but the wire
// contract must respect the user's explicit choice — a future flag flip
// in config.json could change the default, and an explicit false would
// then be meaningful.
func TestProxySpawn_ContainersFalseExplicitlyForwarded(t *testing.T) {
	type spawnReq struct {
		Branch     string `json:"branch"`
		Containers bool   `json:"containers"`
	}

	reqCh := make(chan spawnReq, 1)
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req spawnReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqCh <- req
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@explicit-false-branch"}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())
	t.Setenv("PRISM_BARE_ROOT", "/prism-git")

	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().String("isolation", "", "")
	cmd.Flags().Bool("containers", false, "")
	cmd.Flags().Bool("ignore-concurrency-cap", false, "")
	cmd.Flags().String("harness", "pi", "")
	addPromptFlags(cmd)
	_ = cmd.Flags().Set("branch", "explicit-false-branch")
	_ = cmd.Flags().Set("prompt", "hi")
	_ = cmd.Flags().Set("containers", "false")

	if err := proxySpawn(srv.apiURL(), cmd); err != nil {
		t.Fatalf("proxySpawn: %v", err)
	}

	select {
	case req := <-reqCh:
		if req.Containers {
			t.Errorf("containers = %v, want false (explicit --containers=false must round-trip as false)", req.Containers)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}
