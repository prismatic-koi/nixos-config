package cmd

// Tests for issue #2633's second gap: `prism spawn --pr <number>` targets a
// pre-existing PR exactly like `prism pr <number>` does, so it must receive
// the same read-only guidance. The actual injection happens once, host-side,
// in runSpawn (see the withPRReadOnlyGuidance call site in spawn.go) — this
// is the single point both `prism pr` (via the host-API /spawn handler
// shelling out to `prism spawn --pr`) and a direct `prism spawn --pr` funnel
// through. These tests cover the PR carve-out of the empty-prompt guard at
// both the direct (host) and proxy (sandboxed) layers, mirroring the
// existing keybind carve-out tests in spawn_empty_prompt_test.go.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestRunSpawn_PRFlag_EmptyPromptAccepted verifies that runSpawn, with --pr
// set and no --prompt, does NOT return emptyPromptError — the host-side
// withPRReadOnlyGuidance injection makes the prompt non-empty before the
// guard runs. As with the keybind carve-out tests, the test environment has
// no real git repo, so runSpawn fails downstream (e.g. resolveBareRoot); the
// assertion is negative: the error must not be the empty-prompt rejection.
func TestRunSpawn_PRFlag_EmptyPromptAccepted(t *testing.T) {
	cmd := buildSpawnCmdForEmptyPromptTest(t)
	_ = cmd.Flags().Set("pr", "386")
	// No --prompt / --prompt-file set — promptText resolves to "".

	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_KEYBIND_SPAWN", "")
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")
	withNoopTmux(t)

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("runSpawn returned nil — expected a downstream failure (no git repo) past the empty-prompt guard")
	}
	if strings.Contains(err.Error(), "a prompt is required") ||
		strings.Contains(err.Error(), "empty string — supply a non-empty prompt") ||
		strings.Contains(err.Error(), "empty stdin — supply a non-empty prompt") ||
		strings.Contains(err.Error(), "file is empty — supply a non-empty prompt") {
		t.Errorf("runSpawn returned empty-prompt error despite --pr being set: %v", err)
	}
}

// TestRunSpawn_NoPR_EmptyPromptStillRejected is the negative control: without
// --pr set, the empty-prompt guard must still fire as before.
func TestRunSpawn_NoPR_EmptyPromptStillRejected(t *testing.T) {
	cmd := buildSpawnCmdForEmptyPromptTest(t)
	// No --pr, no --prompt.

	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_KEYBIND_SPAWN", "")
	t.Setenv("PRISM_BARE_ROOT", "")
	withNoopTmux(t)

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("runSpawn with empty prompt, no --pr, no keybind: expected non-nil error, got nil")
	}
	if !strings.Contains(err.Error(), "a prompt is required") {
		t.Errorf("error %q does not contain 'a prompt is required'", err.Error())
	}
}

// TestProxySpawn_PRFlag_EmptyPromptForwarded verifies that proxySpawn, with
// --pr set and no --prompt, does not reject locally (layers 1+2) but instead
// forwards the request to the host API — where the layer-3 carve-out in
// host_api.go (req.PR != "") and the host-side runSpawn injection take over.
// This is the "sandboxed caller" half of the AC: a container-routed
// `prism spawn --pr <number>` with no caller prompt must reach the host, not
// be rejected client-side the way a plain empty-prompt spawn would be.
func TestProxySpawn_PRFlag_EmptyPromptForwarded(t *testing.T) {
	type spawnReq struct {
		PR          string `json:"pr"`
		Prompt      string `json:"prompt"`
		FromKeybind bool   `json:"from_keybind"`
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
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@feat-386"}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())
	t.Setenv("PRISM_KEYBIND_SPAWN", "")

	cmd := buildSpawnCmdForEmptyPromptTest(t)
	_ = cmd.Flags().Set("pr", "386")
	// No --prompt set.

	if err := proxySpawn(srv.apiURL(), cmd); err != nil {
		if strings.Contains(err.Error(), "a prompt is required") {
			t.Fatalf("proxySpawn returned empty-prompt error despite --pr being set: %v", err)
		}
		t.Fatalf("proxySpawn: %v", err)
	}

	select {
	case req := <-reqCh:
		if req.PR != "386" {
			t.Errorf("pr = %q, want %q", req.PR, "386")
		}
		if req.Prompt != "" {
			t.Errorf("prompt = %q, want empty — the client forwards the caller's prompt as-is; guidance is injected host-side", req.Prompt)
		}
		if req.FromKeybind {
			t.Errorf("from_keybind = true, want false — the PR carve-out is a distinct discriminator from the keybind one")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for /spawn request — proxySpawn likely errored before the round-trip")
	}
}
