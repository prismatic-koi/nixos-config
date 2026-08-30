package cmd

// Tests for the keybind carve-out of the empty-prompt guard. The tmux
// Prefix+a keybind invokes `prism spawn --attach` with no --prompt — the
// operator types the initial prompt to the live agent after the popup
// attaches. The keybind discriminator is the PRISM_KEYBIND_SPAWN environment
// variable (a dedicated sentinel, not the overloaded PRISM_SPAWN_PATH). When
// that is set and no prompt was supplied, runSpawn must skip the empty-prompt
// rejection so the popup does not flash-close with an unreadable error.
//
// Without PRISM_KEYBIND_SPAWN set, emptyPromptError must still fire —
// protecting the shell-invocation path and proving the relaxation is gated on
// the dedicated sentinel only. Crucially, PRISM_SPAWN_PATH alone (which every
// sandbox injects unconditionally) must NOT trip the carve-out: it cannot
// distinguish a keybind spawn from a container worker spawn.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// buildSpawnCmdForEmptyPromptTest mirrors buildAbtestCmd but keeps the test
// helper local to this file so it does not couple to abtest-only fixtures.
func buildSpawnCmdForEmptyPromptTest(t *testing.T) *cobra.Command {
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
	cmd.Flags().Bool("attach", false, "")
	cmd.Flags().String("harness", "pi", "")
	cmd.Flags().StringArray("model-override", nil, "")
	cmd.Flags().String("isolation", "", "")
	cmd.Flags().Bool("ignore-concurrency-cap", false, "")
	cmd.Flags().String("prompt-source", "", "")
	addPromptFlags(cmd)
	return cmd
}

// TestRunSpawn_KeybindCarveOut_EmptyPromptAccepted verifies that runSpawn
// with PRISM_KEYBIND_SPAWN set and no prompt does NOT return
// emptyPromptError.
//
// Because the test environment has no real git repo and no live tmux
// server is available, runSpawn will still fail downstream of the
// layer-1+2 guard — typically on resolveBareRoot ("not inside a git
// repo") or another pre-flight check. The assertion is therefore
// negative: the error must NOT contain the empty-prompt message. That
// is sufficient evidence that the guard let the call through, which is the
// exact behaviour the keybind carve-out promises.
func TestRunSpawn_KeybindCarveOut_EmptyPromptAccepted(t *testing.T) {
	cmd := buildSpawnCmdForEmptyPromptTest(t)
	// No --prompt / --prompt-file flags set — promptText resolves to "".

	t.Setenv("PRISM_HOST_API", "")
	// PRISM_KEYBIND_SPAWN is the dedicated keybind discriminator.
	t.Setenv("PRISM_KEYBIND_SPAWN", "1")
	// PRISM_SPAWN_PATH is purely a working-directory hint. Set it to a
	// non-git temp dir so resolveBareRoot fails with "not inside a git repo"
	// without falling back to tmux.CurrentPanePath() (which would resolve
	// against the real test cwd — the nixos-config repo — and leak a
	// timestamped worktree directory). The leak guard in killsidecar_test.go
	// will fail loudly if this isolation breaks.
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")
	withNoopTmux(t)

	err := runSpawn(cmd, nil)
	if err == nil {
		// runSpawn must fail later (no git repo), but it must not error
		// on the empty-prompt guard. A nil error here would mean the
		// test environment accidentally bootstrapped a session — fail loudly.
		t.Fatal("runSpawn returned nil — expected a downstream failure (no git repo) past the empty-prompt guard")
	}
	if strings.Contains(err.Error(), "a prompt is required") ||
		strings.Contains(err.Error(), "empty string — supply a non-empty prompt") ||
		strings.Contains(err.Error(), "empty stdin — supply a non-empty prompt") ||
		strings.Contains(err.Error(), "file is empty — supply a non-empty prompt") {
		t.Errorf("runSpawn returned empty-prompt error despite PRISM_KEYBIND_SPAWN being set: %v", err)
	}
}

// TestRunSpawn_NoKeybind_EmptyPromptStillRejected verifies that runSpawn
// without PRISM_KEYBIND_SPAWN set (i.e. invoked from a normal shell) and
// no prompt still returns the existing emptyPromptError shape. This
// guards the existing layer-1+2 behaviour for the non-keybind path.
func TestRunSpawn_NoKeybind_EmptyPromptStillRejected(t *testing.T) {
	cmd := buildSpawnCmdForEmptyPromptTest(t)
	// No --prompt flags set.

	t.Setenv("PRISM_HOST_API", "")
	// PRISM_KEYBIND_SPAWN unset — not a keybind invocation. The sentinel is
	// dedicated, so no sandbox injects it; the PRISM_SPAWN_PATH defensive
	// shim is not needed.
	t.Setenv("PRISM_BARE_ROOT", "")
	withNoopTmux(t)

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("runSpawn with empty prompt and no PRISM_KEYBIND_SPAWN: expected non-nil error, got nil")
	}
	// The default branch of emptyPromptError fires when no prompt flag was
	// set at all: "a prompt is required — supply --prompt <text>, --prompt -
	// (stdin), or --prompt-file <path>".
	if !strings.Contains(err.Error(), "a prompt is required") {
		t.Errorf("error %q does not contain 'a prompt is required'", err.Error())
	}
}

// TestRunSpawn_SandboxEnvAlone_DoesNotTriggerKeybindCarveOut covers the
// edge case: a container-style invocation with PRISM_SPAWN_PATH
// set (every sandbox sets this unconditionally as a cwd hint) and
// PRISM_KEYBIND_SPAWN UNSET must still hit the empty-prompt guard. The
// dedicated sentinel cannot be tripped by the sandbox env-injection
// surface that PRISM_SPAWN_PATH belongs to.
func TestRunSpawn_SandboxEnvAlone_DoesNotTriggerKeybindCarveOut(t *testing.T) {
	cmd := buildSpawnCmdForEmptyPromptTest(t)
	// No --prompt flags set.

	t.Setenv("PRISM_HOST_API", "")
	// Simulate the bwrap/sandbox-exec state: PRISM_SPAWN_PATH set to the
	// container's worktree path, but PRISM_KEYBIND_SPAWN unset.
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_KEYBIND_SPAWN", "")
	t.Setenv("PRISM_BARE_ROOT", "")
	withNoopTmux(t)

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("runSpawn returned nil; expected empty-prompt rejection")
	}
	if !strings.Contains(err.Error(), "a prompt is required") {
		t.Errorf("error %q must be the empty-prompt rejection — PRISM_SPAWN_PATH alone must NOT trigger the keybind carve-out post-#2073",
			err.Error())
	}
}

// TestProxySpawn_KeybindCarveOut_EmptyPromptForwarded verifies the parity
// behaviour: when proxySpawn is invoked
// with PRISM_HOST_API set, PRISM_KEYBIND_SPAWN set (the keybind
// discriminator), and no prompt, the proxy must NOT return
// emptyPromptError before the round-trip. Instead it must POST the
// request to the host API with from_keybind=true so the host-side
// handler honours the carve-out.
func TestProxySpawn_KeybindCarveOut_EmptyPromptForwarded(t *testing.T) {
	type spawnReq struct {
		Branch      string `json:"branch"`
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
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@20260101T1200"}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())
	// PRISM_KEYBIND_SPAWN is the dedicated keybind discriminator.
	t.Setenv("PRISM_KEYBIND_SPAWN", "1")

	cmd := buildSpawnCmdForEmptyPromptTest(t)
	// No --prompt set; no --branch set — mirrors the tmux Prefix+a invocation.

	if err := proxySpawn(srv.apiURL(), cmd); err != nil {
		// The error must NOT be the empty-prompt rejection — anything else
		// (e.g. an HTTP-level oddity) would be a separate failure, but a
		// successful HTTP round-trip is what the carve-out requires.
		if strings.Contains(err.Error(), "a prompt is required") ||
			strings.Contains(err.Error(), "empty string — supply a non-empty prompt") {
			t.Fatalf("proxySpawn returned empty-prompt error despite PRISM_KEYBIND_SPAWN set: %v", err)
		}
		t.Fatalf("proxySpawn: %v", err)
	}

	select {
	case req := <-reqCh:
		if req.Prompt != "" {
			t.Errorf("prompt = %q, want empty (keybind path forwards empty prompt)", req.Prompt)
		}
		if !req.FromKeybind {
			t.Errorf("from_keybind = false, want true (keybind discriminator must be forwarded)")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for /spawn request — proxySpawn likely errored before the round-trip")
	}
}

// TestProxySpawn_NoKeybind_EmptyPromptStillRejected verifies the security
// property: when PRISM_HOST_API is set but PRISM_KEYBIND_SPAWN is NOT set, an
// empty prompt must still be rejected at the proxy boundary
// — the carve-out fires only on the keybind sentinel. The host-API
// request must never be made.
func TestProxySpawn_NoKeybind_EmptyPromptStillRejected(t *testing.T) {
	reqCh := make(chan struct{}, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		reqCh <- struct{}{}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"should-not-happen"}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())
	// PRISM_KEYBIND_SPAWN unset — not a keybind invocation.

	cmd := buildSpawnCmdForEmptyPromptTest(t)
	// No --prompt set.

	err := proxySpawn(srv.apiURL(), cmd)
	if err == nil {
		t.Fatal("proxySpawn returned nil; expected empty-prompt rejection")
	}
	if !strings.Contains(err.Error(), "a prompt is required") {
		t.Errorf("error %q does not contain 'a prompt is required'", err.Error())
	}

	select {
	case <-reqCh:
		t.Fatal("host-API was contacted; layer-1+2 guard must reject before the round-trip")
	case <-time.After(100 * time.Millisecond):
		// expected — no request reached the server
	}
}

// TestProxySpawn_ContainerSandboxEnv_DoesNotSetFromKeybind covers the edge
// case at the proxy layer: a container worker-spawn (where PRISM_SPAWN_PATH
// is set unconditionally by the sandbox but PRISM_KEYBIND_SPAWN is NOT set)
// MUST NOT forward from_keybind=true, even when the spawn happens to carry no
// --prompt (the proxy does not narrow on `promptFlag == ""` — the dedicated
// sentinel makes that narrowing unnecessary).
//
// This case wraps an unusual but legitimate flow: a container caller
// proxy-spawning with an explicit empty prompt should hit the
// empty-prompt rejection at the host, not be misclassified as a keybind
// spawn.
func TestProxySpawn_ContainerSandboxEnv_DoesNotSetFromKeybind(t *testing.T) {
	type spawnReq struct {
		Prompt      string `json:"prompt"`
		FromKeybind bool   `json:"from_keybind"`
	}

	reqCh := make(chan spawnReq, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req spawnReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqCh <- req
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@x"}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())
	// Simulate the bwrap-sandbox state: PRISM_SPAWN_PATH set to the
	// container's worktree path, PRISM_KEYBIND_SPAWN unset.
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_KEYBIND_SPAWN", "")

	cmd := buildSpawnCmdForEmptyPromptTest(t)
	_ = cmd.Flags().Set("prompt", "explicit prompt from container worker")

	if err := proxySpawn(srv.apiURL(), cmd); err != nil {
		t.Fatalf("proxySpawn: %v", err)
	}

	select {
	case req := <-reqCh:
		if req.Prompt != "explicit prompt from container worker" {
			t.Errorf("prompt = %q, want %q (supplied prompt must be forwarded verbatim)",
				req.Prompt, "explicit prompt from container worker")
		}
		if req.FromKeybind {
			t.Errorf("from_keybind = true; want false — a container spawn (PRISM_SPAWN_PATH set, PRISM_KEYBIND_SPAWN unset) must NOT trigger the keybind carve-out post-#2073")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for /spawn request")
	}
}

// TestRunSpawn_KeybindCarveOut_DoesNotSwallowSuppliedPrompt covers the edge
// case: when PRISM_KEYBIND_SPAWN is set AND a non-empty --prompt is also
// supplied, the carve-out does not swallow the prompt. We assert
// this by checking the error is NOT an empty-prompt error (proving the
// guard is bypassed cleanly without erasing the prompt the caller
// supplied).
//
// The downstream failure here is the same as the empty-prompt-accepted
// test: "not inside a git repo" from resolveBareRoot. We only care that
// the layer-1+2 guard does not misclassify a supplied prompt as empty.
func TestRunSpawn_KeybindCarveOut_DoesNotSwallowSuppliedPrompt(t *testing.T) {
	cmd := buildSpawnCmdForEmptyPromptTest(t)
	_ = cmd.Flags().Set("prompt", "hello from the keybind path")

	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_KEYBIND_SPAWN", "1")
	// PRISM_SPAWN_PATH is now purely a working-directory hint. Set it to a
	// non-git temp dir so resolveBareRoot fails with "not inside a git repo"
	// rather than falling back to the real test cwd. See the equivalent
	// comment in TestRunSpawn_KeybindCarveOut_EmptyPromptAccepted.
	t.Setenv("PRISM_SPAWN_PATH", t.TempDir())
	t.Setenv("PRISM_BARE_ROOT", "")
	withNoopTmux(t)

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("runSpawn returned nil — expected a downstream failure (no git repo)")
	}
	if strings.Contains(err.Error(), "a prompt is required") ||
		strings.Contains(err.Error(), "empty string — supply a non-empty prompt") {
		t.Errorf("runSpawn misclassified a supplied prompt as empty: %v", err)
	}
}
