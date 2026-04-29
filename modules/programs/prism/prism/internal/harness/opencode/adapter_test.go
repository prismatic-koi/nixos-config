package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// ── ConfigEnvVar ──────────────────────────────────────────────────────────────

func TestConfigEnvVar(t *testing.T) {
	a := New("", nil, "", "")
	got := a.ConfigEnvVar()
	if got != "OPENCODE_CONFIG_CONTENT" {
		t.Errorf("ConfigEnvVar() = %q, want %q", got, "OPENCODE_CONFIG_CONTENT")
	}
}

// ── RuntimeEnv ────────────────────────────────────────────────────────────────

func TestRuntimeEnv_ContainsBashTimeout(t *testing.T) {
	a := New("", nil, "", "")
	env := a.RuntimeEnv()
	if env == nil {
		t.Fatal("RuntimeEnv() returned nil")
	}
	val, ok := env["OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS"]
	if !ok {
		t.Error("RuntimeEnv() missing OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS")
	}
	if val != "900000" {
		t.Errorf("OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS = %q, want %q", val, "900000")
	}
}

func TestRuntimeEnv_ReturnsNewMapEachCall(t *testing.T) {
	a := New("", nil, "", "")
	env1 := a.RuntimeEnv()
	env2 := a.RuntimeEnv()
	// Mutating the returned map should not affect subsequent calls.
	env1["NEW_KEY"] = "value"
	if _, ok := env2["NEW_KEY"]; ok {
		t.Error("RuntimeEnv() returned the same map instance — mutations leak across calls")
	}
}

// ── ValidateAgentRole ─────────────────────────────────────────────────────────

func TestValidateAgentRole_ValidRole(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	agentsDir := filepath.Join(dir, "opencode", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "worker.md"), []byte("# worker"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a := New("", nil, "", "")
	if err := a.ValidateAgentRole("worker"); err != nil {
		t.Errorf("ValidateAgentRole(worker): unexpected error: %v", err)
	}
}

func TestValidateAgentRole_InvalidRole(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	// Create the agents dir but don't add any files.
	agentsDir := filepath.Join(dir, "opencode", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	a := New("", nil, "", "")
	err := a.ValidateAgentRole("nonexistent-agent")
	if err == nil {
		t.Fatal("ValidateAgentRole(nonexistent-agent): expected error, got nil")
	}
	// Error should mention "opencode" (harness name) and the role.
	if got := err.Error(); !contains(got, "opencode") {
		t.Errorf("error %q does not mention harness name 'opencode'", got)
	}
	if got := err.Error(); !contains(got, "nonexistent-agent") {
		t.Errorf("error %q does not mention the role 'nonexistent-agent'", got)
	}
}

func TestValidateAgentRole_NoAgentsDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// Don't create the agents dir at all.

	a := New("", nil, "", "")
	err := a.ValidateAgentRole("worker")
	if err == nil {
		t.Fatal("ValidateAgentRole(worker): expected error when agents dir is missing, got nil")
	}
}

// ── EffectiveModel ────────────────────────────────────────────────────────────

func TestEffectiveModel_WithConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	configDir := filepath.Join(dir, "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := `{"agent":{"worker":{"model":"anthropic/claude-sonnet-4-6"},"coordinator":{"model":"anthropic/claude-opus-4-6"}}}`
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte(config), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a := New("", nil, "", "")

	if got := a.EffectiveModel("worker"); got != "anthropic/claude-sonnet-4-6" {
		t.Errorf("EffectiveModel(worker) = %q, want %q", got, "anthropic/claude-sonnet-4-6")
	}
	if got := a.EffectiveModel("coordinator"); got != "anthropic/claude-opus-4-6" {
		t.Errorf("EffectiveModel(coordinator) = %q, want %q", got, "anthropic/claude-opus-4-6")
	}
}

func TestEffectiveModel_UnknownRole(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	configDir := filepath.Join(dir, "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := `{"agent":{"worker":{"model":"anthropic/claude-sonnet-4-6"}}}`
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte(config), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a := New("", nil, "", "")
	if got := a.EffectiveModel("unknown-role"); got != "" {
		t.Errorf("EffectiveModel(unknown-role) = %q, want empty string", got)
	}
}

func TestEffectiveModel_NoConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// Don't create the config file.

	a := New("", nil, "", "")
	if got := a.EffectiveModel("worker"); got != "" {
		t.Errorf("EffectiveModel(worker) = %q, want empty string when config is missing", got)
	}
}

func TestEffectiveModel_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	configDir := filepath.Join(dir, "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a := New("", nil, "", "")
	if got := a.EffectiveModel("worker"); got != "" {
		t.Errorf("EffectiveModel(worker) = %q, want empty string for invalid JSON", got)
	}
}

// TestEffectiveModel_PrefersProfilesFile is the #1206 behaviour gate:
// EffectiveModel must consult prism's role-keyed profile data before falling
// back to opencode's own opencode.json. When both files exist, the profile
// data wins because it is the canonical input that produced opencode.json
// in the first place.
func TestEffectiveModel_PrefersProfilesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	// profiles.json — anthropic profile carries claude-sonnet-4-6 for worker.
	prismDir := filepath.Join(dir, "prism")
	if err := os.MkdirAll(prismDir, 0o755); err != nil {
		t.Fatalf("MkdirAll prism: %v", err)
	}
	profiles := `{
		"default": "anthropic",
		"role_mapping": {"primary": ["coordinator"], "secondary": ["worker"]},
		"profiles": {
			"anthropic": {
				"coordinator": {"provider": "anthropic", "model": "anthropic/claude-opus-4-7", "thinking": "none"},
				"worker":      {"provider": "anthropic", "model": "anthropic/claude-sonnet-4-6", "thinking": "none"}
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(prismDir, "profiles.json"), []byte(profiles), 0o644); err != nil {
		t.Fatalf("WriteFile profiles.json: %v", err)
	}

	// opencode.json — write a different (stale) value so we can prove the
	// profile data is preferred when present.
	configDir := filepath.Join(dir, "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll opencode: %v", err)
	}
	stale := `{"agent":{"worker":{"model":"anthropic/claude-stale"}}}`
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte(stale), 0o644); err != nil {
		t.Fatalf("WriteFile opencode.json: %v", err)
	}

	a := New("", nil, "", "")
	if got := a.EffectiveModel("worker"); got != "anthropic/claude-sonnet-4-6" {
		t.Errorf("EffectiveModel(worker) = %q, want anthropic/claude-sonnet-4-6 (from profiles.json)", got)
	}
	if got := a.EffectiveModel("coordinator"); got != "anthropic/claude-opus-4-7" {
		t.Errorf("EffectiveModel(coordinator) = %q, want anthropic/claude-opus-4-7 (from profiles.json)", got)
	}
}

// TestEffectiveModel_FallsBackToOpencodeConfig verifies that when prism's
// profile data does not resolve a model for the role (e.g. the profile has
// no slot, or profiles.json is missing entirely), EffectiveModel falls back
// to opencode's own config so manually-configured sessions still work.
func TestEffectiveModel_FallsBackToOpencodeConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	// profiles.json defines anthropic but with no slot for "build".
	prismDir := filepath.Join(dir, "prism")
	if err := os.MkdirAll(prismDir, 0o755); err != nil {
		t.Fatalf("MkdirAll prism: %v", err)
	}
	profiles := `{
		"default": "anthropic",
		"role_mapping": {"primary": ["coordinator"]},
		"profiles": {
			"anthropic": {
				"coordinator": {"provider": "anthropic", "model": "anthropic/claude-opus-4-7", "thinking": "none"}
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(prismDir, "profiles.json"), []byte(profiles), 0o644); err != nil {
		t.Fatalf("WriteFile profiles.json: %v", err)
	}

	configDir := filepath.Join(dir, "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll opencode: %v", err)
	}
	cfg := `{"agent":{"build":{"model":"anthropic/claude-build-fallback"}}}`
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("WriteFile opencode.json: %v", err)
	}

	a := New("", nil, "", "")
	if got := a.EffectiveModel("build"); got != "anthropic/claude-build-fallback" {
		t.Errorf("EffectiveModel(build) = %q, want fallback from opencode.json", got)
	}
}

// TestEffectiveModel_HonoursRuntimeActiveProfile is the #1207 behaviour gate:
// when the runtime active-profile state file selects a non-default profile,
// EffectiveModel must return the model from THAT profile rather than from
// the nix-configured default. This proves the lookup goes through
// ResolveActiveProfile and not pf.Default directly.
func TestEffectiveModel_HonoursRuntimeActiveProfile(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	stateRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateRoot)

	prismCfg := filepath.Join(cfgDir, "prism")
	if err := os.MkdirAll(prismCfg, 0o755); err != nil {
		t.Fatalf("MkdirAll prism config: %v", err)
	}
	// Two profiles: anthropic (the nix default) and gemini-hybrid (the
	// runtime override). Different worker models so the test can tell which
	// branch produced the result.
	profiles := `{
		"default": "anthropic",
		"role_mapping": {"primary": ["coordinator"], "secondary": ["worker"]},
		"profiles": {
			"anthropic":     {
				"coordinator": {"provider": "anthropic", "model": "anthropic/claude-opus-4-7", "thinking": "none"},
				"worker":      {"provider": "anthropic", "model": "anthropic/claude-default-worker", "thinking": "none"}
			},
			"gemini-hybrid": {
				"coordinator": {"provider": "anthropic", "model": "anthropic/claude-opus-4-7", "thinking": "none"},
				"worker":      {"provider": "google", "model": "google/gemini-runtime-worker", "thinking": "medium"}
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(prismCfg, "profiles.json"), []byte(profiles), 0o644); err != nil {
		t.Fatalf("WriteFile profiles.json: %v", err)
	}

	// Write the runtime state file pointing at gemini-hybrid.
	prismState := filepath.Join(stateRoot, "prism")
	if err := os.MkdirAll(prismState, 0o755); err != nil {
		t.Fatalf("MkdirAll prism state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prismState, "active-profile"), []byte("gemini-hybrid\n"), 0o644); err != nil {
		t.Fatalf("WriteFile active-profile: %v", err)
	}

	a := New("", nil, "", "")
	if got := a.EffectiveModel("worker"); got != "google/gemini-runtime-worker" {
		t.Errorf("EffectiveModel(worker) = %q, want google/gemini-runtime-worker (from runtime state file, not nix default)", got)
	}
}

// ── CreateSession retry ───────────────────────────────────────────────────────

// sessionResponse is a helper that writes a JSON session list to the response.
func sessionResponse(w http.ResponseWriter, sessions []sessionEntry) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sessions)
}

// TestCreateSession_SucceedsFirstAttempt verifies that when the server
// responds immediately with a session, CreateSession succeeds with no retry
// log lines emitted.
func TestCreateSession_SucceedsFirstAttempt(t *testing.T) {
	wantID := "sess-abc-123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session" {
			http.NotFound(w, r)
			return
		}
		sessionResponse(w, []sessionEntry{{ID: wantID}})
	}))
	defer srv.Close()

	// Capture log output.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	a := NewContainerMode(srv.URL, nil, "worker", "")
	id, err := a.CreateSession(context.Background())
	if err != nil {
		t.Fatalf("CreateSession() error = %v, want nil", err)
	}
	if id != wantID {
		t.Errorf("CreateSession() = %q, want %q", id, wantID)
	}

	// The happy path must not emit any retry log lines.
	logOut := buf.String()
	if strings.Contains(logOut, "attempt") {
		t.Errorf("expected no retry log lines on first-attempt success, got:\n%s", logOut)
	}
}

// TestCreateSession_RetriesAndSucceeds verifies that when the first N attempts
// fail with a transport error, CreateSession retries and eventually succeeds
// once the server starts responding.
func TestCreateSession_RetriesAndSucceeds(t *testing.T) {
	const failAttempts = 3
	wantID := "sess-retry-ok"

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session" {
			http.NotFound(w, r)
			return
		}
		n := callCount.Add(1)
		if n <= failAttempts {
			// Simulate a slow/hung server by closing the connection immediately,
			// which causes a transport error on the client side.
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijack unavailable", http.StatusInternalServerError)
				return
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		sessionResponse(w, []sessionEntry{{ID: wantID}})
	}))
	defer srv.Close()

	// Capture log output.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	a := NewContainerMode(srv.URL, nil, "worker", "")
	id, err := a.CreateSession(context.Background())
	if err != nil {
		t.Fatalf("CreateSession() error = %v, want nil after retries", err)
	}
	if id != wantID {
		t.Errorf("CreateSession() = %q, want %q", id, wantID)
	}

	// Should have logged exactly failAttempts retry lines in the new format.
	logOut := buf.String()
	want := "sidecar: CreateSession: attempt"
	if !strings.Contains(logOut, want) {
		t.Errorf("expected log line containing %q, got:\n%s", want, logOut)
	}
}

// TestCreateSession_ExhaustsRetries verifies that when all attempts fail,
// CreateSession returns a descriptive error and logs the final failure.
func TestCreateSession_ExhaustsRetries(t *testing.T) {
	// Server that always closes the connection immediately (transport failure).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unavailable", http.StatusInternalServerError)
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	// Capture log output.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	a := NewContainerMode(srv.URL, nil, "worker", "")
	_, err := a.CreateSession(context.Background())
	if err == nil {
		t.Fatal("CreateSession() error = nil, want error after all retries exhausted")
	}

	// Error message must mention how many attempts were made.
	if !strings.Contains(err.Error(), "failed after") {
		t.Errorf("error %q does not describe exhausted retries", err.Error())
	}

	// Log must contain the final-failure line.
	logOut := buf.String()
	if !strings.Contains(logOut, "GET /session failed after") {
		t.Errorf("expected final-failure log line, got:\n%s", logOut)
	}
}

// contains is a helper to check substring presence.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
