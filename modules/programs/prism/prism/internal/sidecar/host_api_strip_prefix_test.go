package sidecar

// Tests for the set_model provider-prefix normalisation fix (issue #2252).
//
// Symptom: profiles.json stores slot.Model in the prefixed form
// "anthropic/claude-fable-5" (the same form that becomes the spawn-time
// `--model <provider/model>` CLI flag). The live-swap fan-out
// (/apply-profile, /set-model, forwardSetModel) used to pass that string
// verbatim into the set_model wire frame; the extension's
// modelRegistryFind expects a bare model ID and the lookup failed with
// the doubled-prefix message `model anthropic/anthropic/claude-fable-5
// not found in registry`. Live-swap silently no-op'd.
//
// Fix: stripProviderPrefix(provider, model) removes a single leading
// "<provider>/" segment from model when it equals provider — applied at
// both the local enqueue (liveModelSwapForSession) and the peer forward
// (forwardSetModel) layers.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
)

// ── unit tests for stripProviderPrefix ────────────────────────────────────────

// TestStripProviderPrefix verifies the normalisation contract from issue #2252:
//   - strip at most ONE leading "<provider>/" segment;
//   - strip only when that segment exactly equals the supplied provider;
//   - preserve all remaining slashes (nested / openrouter-style IDs);
//   - empty inputs are returned unchanged.
func TestStripProviderPrefix(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		model    string
		want     string
	}{
		{
			name:     "strips matching provider prefix (the bug case)",
			provider: "anthropic",
			model:    "anthropic/claude-fable-5",
			want:     "claude-fable-5",
		},
		{
			name:     "bare model id is returned unchanged",
			provider: "anthropic",
			model:    "claude-sonnet-4-20250514",
			want:     "claude-sonnet-4-20250514",
		},
		{
			name:     "non-matching prefix is left intact",
			provider: "openai",
			model:    "anthropic/claude-fable-5",
			want:     "anthropic/claude-fable-5",
		},
		{
			name:     "strips at most one segment (idempotency target)",
			provider: "anthropic",
			model:    "anthropic/anthropic/claude-fable-5",
			want:     "anthropic/claude-fable-5",
		},
		{
			name:     "preserves nested slashes after the leading segment",
			provider: "openrouter",
			model:    "openrouter/anthropic/claude-3.5-sonnet",
			want:     "anthropic/claude-3.5-sonnet",
		},
		{
			name:     "preserves a model id that contains slashes but no provider prefix",
			provider: "openrouter",
			model:    "anthropic/claude-3.5-sonnet",
			want:     "anthropic/claude-3.5-sonnet",
		},
		{
			name:     "idempotent on already-stripped input",
			provider: "anthropic",
			model:    "claude-fable-5",
			want:     "claude-fable-5",
		},
		{
			name:     "empty provider returns model unchanged",
			provider: "",
			model:    "anthropic/claude-fable-5",
			want:     "anthropic/claude-fable-5",
		},
		{
			name:     "empty model returns empty",
			provider: "anthropic",
			model:    "",
			want:     "",
		},
		{
			name:     "provider that is a prefix-substring (no slash) is not stripped",
			provider: "anth",
			model:    "anthropic/claude-fable-5",
			want:     "anthropic/claude-fable-5",
		},
		{
			name:     "exact equality with no trailing slash is not stripped",
			provider: "anthropic",
			model:    "anthropic",
			want:     "anthropic",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripProviderPrefix(tc.provider, tc.model)
			if got != tc.want {
				t.Errorf("stripProviderPrefix(%q, %q) = %q, want %q",
					tc.provider, tc.model, got, tc.want)
			}
		})
	}
}

// TestStripProviderPrefix_Idempotent verifies that calling stripProviderPrefix
// twice (the dual-strip done by liveModelSwapForSession and forwardSetModel)
// produces the same result as calling it once. This is the contract that
// makes the defence-in-depth strip on the peer side safe.
func TestStripProviderPrefix_Idempotent(t *testing.T) {
	inputs := []struct {
		provider string
		model    string
	}{
		{"anthropic", "anthropic/claude-fable-5"},
		{"anthropic", "claude-fable-5"},
		{"openrouter", "openrouter/anthropic/claude-3.5-sonnet"},
		{"openai", "gpt-4o"},
		{"", "anything"},
	}
	for _, in := range inputs {
		once := stripProviderPrefix(in.provider, in.model)
		twice := stripProviderPrefix(in.provider, once)
		if once != twice {
			t.Errorf("stripProviderPrefix not idempotent for (%q, %q): once=%q twice=%q",
				in.provider, in.model, once, twice)
		}
	}
}

// ── integration: /set-model round-trip strips a prefixed model field ─────────

// TestHostAPI_SetModel_StripsProviderPrefix_OwnSession is the round-trip the
// issue calls out as the canonical fix signal: a /set-model call carrying a
// provider-prefixed model lands at the extension as a bare-ID set_model
// frame. Without the fix, the frame would carry the doubled prefix.
func TestHostAPI_SetModel_StripsProviderPrefix_OwnSession(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	sc.cfg.HarnessName = "pi"
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	rd := bufio.NewReader(conn)

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/set-model", map[string]any{
		"session":  sc.cfg.SessionName,
		"provider": "anthropic",
		"model":    "anthropic/claude-fable-5", // prefixed form
		"thinking": "auto",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /set-model status %d, body: %s", rr.Code, rr.Body.String())
	}

	frame := readFrameWithDeadline(t, rd, conn)
	if got := frame["type"]; got != "set_model" {
		t.Fatalf("frame type = %v, want set_model", got)
	}
	if got := frame["provider"]; got != "anthropic" {
		t.Errorf("frame provider = %v, want anthropic", got)
	}
	// The crux: the frame carries the bare ID, not the doubled-prefix form
	// (`anthropic/anthropic/claude-fable-5`) and not the input prefixed form
	// (`anthropic/claude-fable-5`).
	if got := frame["model"]; got != "claude-fable-5" {
		t.Errorf("frame model = %v, want claude-fable-5 (bare ID, no provider prefix)", got)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestHostAPI_ApplyProfile_StripsProviderPrefix_SessionScope verifies the
// /apply-profile fan-out path also strips the provider prefix from slot.Model
// before the set_model frame reaches the extension. This is the primary
// real-world trigger from issue #2252 (every Nix-generated profile slot
// stores the prefixed form).
func TestHostAPI_ApplyProfile_StripsProviderPrefix_SessionScope(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	sc.cfg.HarnessName = "pi"
	sc.cfg.AgentRole = "coordinator"
	// Pre-set root agent name so resolveRoleForSession returns "worker".
	_ = sc.cfg.DB.UpsertStatusWithRootAgent(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil, strPtr("worker"), nil)
	if err := sc.cfg.DB.SetHarness(sc.cfg.SessionName, "pi"); err != nil {
		t.Skipf("SetHarness not available: %v", err)
	}

	wait := runSocketPipeSidecar(sc)
	conn, _ := dialAndHandshake(t, sockPath)
	rd := bufio.NewReader(conn)

	// Install a profile loader whose worker slot uses the prefixed form
	// `anthropic/claude-fable-5` — the shape that triggered #2252 in
	// production profiles.json.
	origLoader := hostAPILoadProfiles
	hostAPILoadProfiles = func() (*config.ProfilesFile, error) {
		return &config.ProfilesFile{
			Default: "fable",
			Profiles: map[string]config.ProfileEntry{
				"fable": {
					"worker": {Provider: "anthropic", Model: "anthropic/claude-fable-5", Thinking: "high"},
				},
			},
		}, nil
	}
	defer func() { hostAPILoadProfiles = origLoader }()

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/apply-profile", map[string]any{
		"profile": "fable",
		"scope":   "session",
		"session": sc.cfg.SessionName,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /apply-profile status %d, body: %s", rr.Code, rr.Body.String())
	}

	frame := readFrameWithDeadline(t, rd, conn)
	if got := frame["type"]; got != "set_model" {
		t.Fatalf("frame type = %v, want set_model", got)
	}
	if got := frame["provider"]; got != "anthropic" {
		t.Errorf("frame provider = %v, want anthropic", got)
	}
	if got := frame["model"]; got != "claude-fable-5" {
		t.Errorf("frame model = %v, want claude-fable-5 (bare, no provider prefix)", got)
	}
	if got := frame["thinking"]; got != "high" {
		t.Errorf("frame thinking = %v, want high", got)
	}

	var result map[string]any
	decodeJSON(t, rr, &result)
	results, _ := result["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r0, _ := results[0].(map[string]any)
	if r0["status"] != "applied" {
		t.Errorf("result[0].status = %v, want applied", r0["status"])
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestHostAPI_ForwardSetModel_StripsProviderPrefix verifies the peer-forward
// path (forwardSetModel → dialUnixAndPostFn) normalises the model field before
// it goes on the wire. The peer's /set-model handler will also normalise on
// receipt; the AC requires both layers strip identically. This test pins the
// sender-side strip so the wire body is bare-ID even against an older peer
// that does not normalise.
func TestHostAPI_ForwardSetModel_StripsProviderPrefix(t *testing.T) {
	// Capture the POST body that forwardSetModel sends.
	type captured struct {
		Session  string `json:"session"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Thinking string `json:"thinking"`
	}
	gotCh := make(chan captured, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/set-model" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		var c captured
		_ = json.NewDecoder(r.Body).Decode(&c)
		gotCh <- c
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"applied"}`))
	}))
	defer srv.Close()

	// Replace the socket-path resolver and the unix-post function so
	// forwardSetModel routes to our test server via TCP.
	origSocketPath := hostAPISocketPath
	hostAPISocketPath = func(_ string) (string, error) {
		return srv.Listener.Addr().String(), nil
	}
	defer func() { hostAPISocketPath = origSocketPath }()

	origDialFn := dialUnixAndPostFn
	dialUnixAndPostFn = func(addr, path string, body any) error {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		resp, err := http.Post("http://"+addr+path, "application/json", bytes.NewReader(b))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("http %d", resp.StatusCode)
		}
		return nil
	}
	defer func() { dialUnixAndPostFn = origDialFn }()

	if err := forwardSetModel("otherrepo@worker1", "anthropic", "anthropic/claude-fable-5", "off"); err != nil {
		t.Fatalf("forwardSetModel: %v", err)
	}

	select {
	case got := <-gotCh:
		if got.Provider != "anthropic" {
			t.Errorf("body.provider = %q, want anthropic", got.Provider)
		}
		if got.Model != "claude-fable-5" {
			t.Errorf("body.model = %q, want claude-fable-5 (bare, no provider prefix)", got.Model)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for forwarded /set-model POST")
	}
}

// TestHostAPI_ForwardSetModel_PreservesNestedSlashes verifies the nested-ID
// guarantee from the issue: when the leading segment matches the provider,
// only that segment is removed; any further '/' characters in the model ID
// are preserved verbatim on the wire.
func TestHostAPI_ForwardSetModel_PreservesNestedSlashes(t *testing.T) {
	type captured struct {
		Model string `json:"model"`
	}
	gotCh := make(chan captured, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var c captured
		_ = json.NewDecoder(r.Body).Decode(&c)
		gotCh <- c
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"applied"}`))
	}))
	defer srv.Close()

	origSocketPath := hostAPISocketPath
	hostAPISocketPath = func(_ string) (string, error) {
		return srv.Listener.Addr().String(), nil
	}
	defer func() { hostAPISocketPath = origSocketPath }()

	origDialFn := dialUnixAndPostFn
	dialUnixAndPostFn = func(addr, path string, body any) error {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		resp, err := http.Post("http://"+addr+path, "application/json", bytes.NewReader(b))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return nil
	}
	defer func() { dialUnixAndPostFn = origDialFn }()

	// Openrouter-style nested ID: provider=openrouter, model carries the
	// leading "openrouter/" segment plus a further "anthropic/" segment that
	// must NOT be stripped (it isn't the frame's provider).
	if err := forwardSetModel("otherrepo@worker1", "openrouter", "openrouter/anthropic/claude-3.5-sonnet", ""); err != nil {
		t.Fatalf("forwardSetModel: %v", err)
	}

	select {
	case got := <-gotCh:
		if got.Model != "anthropic/claude-3.5-sonnet" {
			t.Errorf("body.model = %q, want anthropic/claude-3.5-sonnet (only leading provider segment stripped)", got.Model)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for forwarded /set-model POST")
	}
}
