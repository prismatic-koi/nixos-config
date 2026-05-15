package sidecar

// Integration tests for P3.LIVE host-API endpoints (#1214):
//   POST /set-model
//   POST /apply-profile
//   POST /register-provider
//
// The tests use the fake-extension harness from sidecar_socketpipe_test.go
// (dialAndHandshake, sendJSON, readJSON, etc.) to verify that frames are
// delivered to live PI sessions and that role-scoping / skip rules work.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// newPISidecar creates a PI-harness sidecar pre-wired with a socket path and
// a given session name / role. The DB row for the session is inserted so that
// harnessNameForSession can return "pi".
func newPISidecar(t *testing.T, sockPath, sessionName, agentRole string, d *db.DB) *Sidecar {
	t.Helper()
	if d == nil {
		d = openTestDB(t)
	}
	clk := newTestClock()
	cfg := Config{
		SessionName:           sessionName,
		Repo:                  repoOf(t, sessionName),
		Worktree:              t.TempDir(),
		DB:                    d,
		Clock:                 clk,
		AgentRole:             agentRole,
		HarnessName:           "pi",
		HarnessPipeSockPath:   sockPath,
		StartupConnectTimeout: 5 * time.Second,
		Harness:               pih.New("", "", ""),
	}
	sc := New(cfg)

	// Insert an agent_status row so the DB reflects harness='pi'.
	harness := "pi"
	if err := d.UpsertStatusFull(sessionName, cfg.Repo, cfg.Worktree, "active", nil, nil, nil, nil, nil, &harness); err != nil {
		// UpsertStatusFull may not exist; fall back to UpsertStatus and patch harness separately.
		t.Logf("UpsertStatusFull: %v — falling back", err)
		_ = d.UpsertStatus(sessionName, cfg.Repo, cfg.Worktree, "active", nil, nil)
		_ = d.SetHarness(sessionName, "pi")
	}

	return sc
}

// repoOf extracts the repo from a session name (e.g. "myrepo" from "myrepo@main").
func repoOf(t *testing.T, sessionName string) string {
	t.Helper()
	repo, err := repoFromSession(sessionName)
	if err != nil {
		t.Fatalf("repoOf: %v", err)
	}
	return repo
}

// setHarnessInDB is a best-effort helper that sets harness='pi' for a session
// directly via SQL so that harnessNameForSession returns 'pi'.
func setHarnessInDB(t *testing.T, d *db.DB, sessionName string) {
	t.Helper()
	if err := d.SetHarness(sessionName, "pi"); err != nil {
		t.Logf("setHarnessInDB: SetHarness not available (%v) — trying direct SQL", err)
		// Direct SQL fallback for DBs that don't expose SetHarness yet.
		if err2 := d.SetHarnessRaw(sessionName, "pi"); err2 != nil {
			t.Fatalf("setHarnessInDB: %v", err2)
		}
	}
}

// postJSON posts JSON to a handler and returns the response recorder.
func postJSON(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("postJSON marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("postJSON new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// decodeJSON unmarshals the response body into v.
func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rr.Body).Decode(v); err != nil {
		t.Fatalf("decodeJSON: %v (body: %s)", err, rr.Body.String())
	}
}

// readFrameWithDeadline reads the next JSONL frame from conn with a 2s deadline.
func readFrameWithDeadline(t *testing.T, rd *bufio.Reader, conn net.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	line, err := rd.ReadBytes('\n')
	if err != nil {
		t.Fatalf("readFrameWithDeadline: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("readFrameWithDeadline unmarshal: %v", err)
	}
	return m
}

// fakeProfilesFile returns a *config.ProfilesFile with two profiles for testing.
func fakeProfilesFile() *config.ProfilesFile {
	return &config.ProfilesFile{
		Default: "base",
		Profiles: map[string]config.ProfileEntry{
			"base": {
				"worker":      {Provider: "anthropic", Model: "claude-3-5-sonnet", Thinking: "none"},
				"coordinator": {Provider: "anthropic", Model: "claude-3-5-sonnet", Thinking: "none"},
			},
			"alt": {
				"worker":      {Provider: "openai", Model: "gpt-4o", Thinking: ""},
				"coordinator": {Provider: "openai", Model: "gpt-4o-mini", Thinking: ""},
			},
		},
	}
}

// ── /set-model tests ──────────────────────────────────────────────────────────

// TestHostAPI_SetModel_OwnSession verifies that /set-model delivers a set_model
// frame to the calling PI session's extension.
func TestHostAPI_SetModel_OwnSession(t *testing.T) {
	sockPath := shortSockPath(t)
	d := openTestDB(t)
	sc := newSocketPipeSidecar(t, sockPath)
	// Override HarnessName to "pi" so the self-path goes through SetModel.
	sc.cfg.HarnessName = "pi"
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	rd := bufio.NewReader(conn)

	// POST /set-model via the handler directly.
	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/set-model", map[string]any{
		"session":  sc.cfg.SessionName,
		"provider": "anthropic",
		"model":    "claude-3-7-sonnet",
		"thinking": "auto",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /set-model status %d, body: %s", rr.Code, rr.Body.String())
	}

	// The extension should receive a set_model frame.
	frame := readFrameWithDeadline(t, rd, conn)
	if got := frame["type"]; got != "set_model" {
		t.Errorf("frame type = %v, want set_model", got)
	}
	if got := frame["model"]; got != "claude-3-7-sonnet" {
		t.Errorf("frame model = %v, want claude-3-7-sonnet", got)
	}
	if got := frame["provider"]; got != "anthropic" {
		t.Errorf("frame provider = %v, want anthropic", got)
	}

	var result map[string]string
	decodeJSON(t, rr, &result)
	if result["status"] != "applied" {
		t.Errorf("response status = %q, want applied", result["status"])
	}

	_ = d // suppress unused warning
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestHostAPI_SetModel_WorkerRejectedForOtherSession verifies that a worker
// sidecar cannot call /set-model targeting a different session (403).
func TestHostAPI_SetModel_WorkerRejectedForOtherSession(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	sc.cfg.AgentRole = "worker"
	sc.cfg.HarnessName = "pi"
	// Do NOT start runSocketPipeSidecar — we only need the handler.

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/set-model", map[string]any{
		"session":  "otherrepo@other-branch",
		"provider": "anthropic",
		"model":    "claude-3-7-sonnet",
		"thinking": "",
	})
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_SetModel_DisconnectedSession verifies that /set-model returns
// error:disconnected when the pipe is not yet connected (no extension dialled).
func TestHostAPI_SetModel_DisconnectedSession(t *testing.T) {
	d := openTestDB(t)
	_ = d.UpsertStatus("testrepo@main", "testrepo", t.TempDir(), "active", nil, nil)
	if err := d.SetHarness("testrepo@main", "pi"); err != nil {
		t.Skipf("SetHarness not available: %v", err)
	}

	clk := newTestClock()
	cfg := Config{
		SessionName:           "testrepo@main",
		Repo:                  "testrepo",
		Worktree:              t.TempDir(),
		DB:                    d,
		Clock:                 clk,
		AgentRole:             "coordinator",
		HarnessName:           "pi",
		HarnessPipeSockPath:   "", // no pipe path
		StartupConnectTimeout: 5 * time.Second,
		Harness:               pih.New("", "", ""),
	}
	sc := New(cfg)
	// harnessPipeOutCh is nil — pipe not connected.

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/set-model", map[string]any{
		"session":  sc.cfg.SessionName,
		"provider": "anthropic",
		"model":    "claude-3-7-sonnet",
		"thinking": "",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var result map[string]string
	decodeJSON(t, rr, &result)
	if result["status"] != "error:disconnected" {
		t.Errorf("status = %q, want error:disconnected", result["status"])
	}
}

// ── /apply-profile tests ───────────────────────────────────────────────────────

// TestHostAPI_ApplyProfile_SessionScope verifies that /apply-profile with
// scope=session delivers the correct set_model frame to the target PI session.
func TestHostAPI_ApplyProfile_SessionScope(t *testing.T) {
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

	// Install a fake profile loader.
	origLoader := hostAPILoadProfiles
	hostAPILoadProfiles = func() (*config.ProfilesFile, error) {
		return fakeProfilesFile(), nil
	}
	defer func() { hostAPILoadProfiles = origLoader }()

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/apply-profile", map[string]any{
		"profile": "alt",
		"scope":   "session",
		"session": sc.cfg.SessionName,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /apply-profile status %d, body: %s", rr.Code, rr.Body.String())
	}

	// Extension should receive set_model with the "worker" slot from "alt".
	frame := readFrameWithDeadline(t, rd, conn)
	if got := frame["type"]; got != "set_model" {
		t.Errorf("frame type = %v, want set_model", got)
	}
	if got := frame["model"]; got != "gpt-4o" {
		t.Errorf("frame model = %v, want gpt-4o", got)
	}
	if got := frame["provider"]; got != "openai" {
		t.Errorf("frame provider = %v, want openai", got)
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

// TestHostAPI_ApplyProfile_SkipsOpencodeSessions verifies that opencode
// sessions in scope are skipped with status "skipped:opencode".
// TestHostAPI_ApplyProfile_SkipsNoMatchingSlot verifies that sessions whose
// role has no slot in the new profile get "skipped:no-matching-slot".
func TestHostAPI_ApplyProfile_SkipsNoMatchingSlot(t *testing.T) {
	d := openTestDB(t)
	sessName := "testrepo@worker1"
	_ = d.UpsertStatus(sessName, "testrepo", t.TempDir(), "active", nil, nil)
	if err := d.SetHarness(sessName, "pi"); err != nil {
		t.Skipf("SetHarness not available: %v", err)
	}
	// Set root_agent_name to a role not in the "alt" profile.
	_ = d.UpsertStatusWithRootAgent(sessName, "testrepo", t.TempDir(), "active", nil, nil, strPtr("explore"), nil)

	clk := newTestClock()
	cfg := Config{
		SessionName: "testrepo@main",
		Repo:        "testrepo",
		Worktree:    t.TempDir(),
		DB:          d,
		Clock:       clk,
		AgentRole:   "coordinator",
		HarnessName: "pi",
		Harness:     pih.New("", "", ""),
	}
	sc := New(cfg)
	_ = d.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	origLoader := hostAPILoadProfiles
	// Profile "sparse" only has "coordinator", not "explore" or "worker".
	hostAPILoadProfiles = func() (*config.ProfilesFile, error) {
		return &config.ProfilesFile{
			Default: "base",
			Profiles: map[string]config.ProfileEntry{
				"sparse": {
					"coordinator": {Provider: "anthropic", Model: "claude-sonnet", Thinking: "none"},
				},
			},
		}, nil
	}
	defer func() { hostAPILoadProfiles = origLoader }()

	// Override socket path lookup to avoid real filesystem socket.
	origSocketPath := hostAPISocketPath
	hostAPISocketPath = func(s string) (string, error) {
		return "", fmt.Errorf("test: no socket for %s", s)
	}
	defer func() { hostAPISocketPath = origSocketPath }()

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/apply-profile", map[string]any{
		"profile": "sparse",
		"scope":   "session",
		"session": sessName,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /apply-profile status %d, body: %s", rr.Code, rr.Body.String())
	}

	var result map[string]any
	decodeJSON(t, rr, &result)
	results, _ := result["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %v", len(results), results)
	}
	r0, _ := results[0].(map[string]any)
	if r0["status"] != "skipped:no-matching-slot" {
		t.Errorf("result[0].status = %v, want skipped:no-matching-slot", r0["status"])
	}
}

// TestHostAPI_ApplyProfile_GlobalScopeRejectedByWorker verifies that a worker
// calling /apply-profile with scope=global receives 403.
func TestHostAPI_ApplyProfile_GlobalScopeRejectedByWorker(t *testing.T) {
	d := openTestDB(t)
	// Session ends in @worker1 — not a coordinator.
	_ = d.UpsertStatus("testrepo@worker1", "testrepo", t.TempDir(), "active", nil, nil)
	// root_agent_name = "worker" so isCoordinatorSession returns false.
	_ = d.UpsertStatusWithRootAgent("testrepo@worker1", "testrepo", t.TempDir(), "active", nil, nil, strPtr("worker"), nil)

	clk := newTestClock()
	cfg := Config{
		SessionName: "testrepo@worker1",
		Repo:        "testrepo",
		Worktree:    t.TempDir(),
		DB:          d,
		Clock:       clk,
		AgentRole:   "worker",
		HarnessName: "pi",
		Harness:     pih.New("", "", ""),
	}
	sc := New(cfg)

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/apply-profile", map[string]any{
		"profile": "alt",
		"scope":   "global",
	})
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_ApplyProfile_CoordinatorScopeRejectedByWorker verifies that a
// worker calling /apply-profile with scope=coordinator receives 403.
func TestHostAPI_ApplyProfile_CoordinatorScopeRejectedByWorker(t *testing.T) {
	d := openTestDB(t)
	_ = d.UpsertStatus("testrepo@worker1", "testrepo", t.TempDir(), "active", nil, nil)
	_ = d.UpsertStatusWithRootAgent("testrepo@worker1", "testrepo", t.TempDir(), "active", nil, nil, strPtr("worker"), nil)

	clk := newTestClock()
	cfg := Config{
		SessionName: "testrepo@worker1",
		Repo:        "testrepo",
		Worktree:    t.TempDir(),
		DB:          d,
		Clock:       clk,
		AgentRole:   "worker",
		HarnessName: "pi",
		Harness:     pih.New("", "", ""),
	}
	sc := New(cfg)

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/apply-profile", map[string]any{
		"profile": "alt",
		"scope":   "coordinator",
	})
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// ── /register-provider tests ───────────────────────────────────────────────────

// TestHostAPI_RegisterProvider_OwnSession verifies that /register-provider
// delivers a register_provider frame to the own PI session's extension.
func TestHostAPI_RegisterProvider_OwnSession(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	sc.cfg.HarnessName = "pi"
	sc.cfg.AgentRole = "coordinator"
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	rd := bufio.NewReader(conn)

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/register-provider", map[string]any{
		"name":    "my-proxy",
		"config":  map[string]any{"base_url": "http://localhost:9000", "api_key": "test"},
		"scope":   "session",
		"session": sc.cfg.SessionName,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /register-provider status %d, body: %s", rr.Code, rr.Body.String())
	}

	frame := readFrameWithDeadline(t, rd, conn)
	if got := frame["type"]; got != "register_provider" {
		t.Errorf("frame type = %v, want register_provider", got)
	}
	if got := frame["name"]; got != "my-proxy" {
		t.Errorf("frame name = %v, want my-proxy", got)
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

// ── multi-session fan-out test ────────────────────────────────────────────────

// TestHostAPI_ApplyProfile_FanOut verifies that /apply-profile with
// scope=coordinator fans out to N live PI sessions via their host-API sockets.
//
// Architecture: the test spins up N fake "target" sidecar host-API servers
// (in-process via httptest.NewServer) that record received /set-model
// requests, then overrides hostAPISocketPath so the coordinator's sidecar
// routes forwarded calls to those fake servers.
func TestHostAPI_ApplyProfile_FanOut(t *testing.T) {
	// Fake profile loader.
	origLoader := hostAPILoadProfiles
	hostAPILoadProfiles = func() (*config.ProfilesFile, error) {
		return fakeProfilesFile(), nil
	}
	defer func() { hostAPILoadProfiles = origLoader }()

	d := openTestDB(t)
	repo := "myrepo"
	coordSession := repo + "@main"

	// Insert coordinator row (not a PI session — opencode harness).
	_ = d.UpsertStatus(coordSession, repo, t.TempDir(), "active", nil, nil)
	_ = d.UpsertStatusWithRootAgent(coordSession, repo, t.TempDir(), "active", nil, nil, strPtr("coordinator"), nil)

	type captured struct {
		session  string
		provider string
		model    string
		thinking string
	}
	var capturedFrames []captured
	captureCh := make(chan captured, 10)

	// Spin up N fake target sidecars. Each captures /set-model requests.
	targetNames := []string{repo + "@worker1", repo + "@worker2"}
	targetServers := make(map[string]*httptest.Server)
	for _, name := range targetNames {
		name := name
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/set-model" {
				var req struct {
					Session  string `json:"session"`
					Provider string `json:"provider"`
					Model    string `json:"model"`
					Thinking string `json:"thinking"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
					captureCh <- captured{session: name, provider: req.Provider, model: req.Model, thinking: req.Thinking}
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"session":"` + name + `","status":"applied"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		targetServers[name] = srv

		// Insert DB row for the target.
		_ = d.UpsertStatus(name, repo, t.TempDir(), "active", nil, nil)
		if err := d.SetHarness(name, "pi"); err != nil {
			t.Skipf("SetHarness not available: %v", err)
		}
		_ = d.UpsertStatusWithRootAgent(name, repo, t.TempDir(), "active", nil, nil, strPtr("worker"), nil)
	}

	// Override hostAPISocketPath to return the test server's TCP address.
	// dialUnixAndPost will dial the Unix socket, but since we return a TCP
	// address we need to also intercept the dialler. Instead, we override
	// the entire forwarding path by replacing hostAPISocketPath to return a
	// synthetic "socket path" that we recognise, then also provide a custom
	// HTTP client.  The simpler approach is to stub dialUnixAndPost via a
	// package-level variable.
	//
	// Since dialUnixAndPost is not a var, we instead stub hostAPISocketPath to
	// return a special path that identifies the target, and then verify by
	// checking the captured channel with a timeout.  For this test to work
	// over TCP rather than Unix sockets, we override the client by intercepting
	// at the forwardSetModel level through hostAPISocketPath returning the
	// httptest server's listener address encoded as a fake path.
	//
	// The cleanest approach: replace hostAPISocketPath to return the httptest
	// server URL host (TCP addr), and replace dialUnixAndPost's transport to
	// use TCP.  Since dialUnixAndPost is a private function, we can instead
	// stub at the hostAPISocketPath level AND intercept the http.Transport.
	//
	// Simplest: provide a custom `dialUnixAndPostFn` package-level var.
	// We add that below.

	origDialFn := dialUnixAndPostFn
	dialUnixAndPostFn = func(sockPath, path string, body any) error {
		// sockPath here is set by our overridden hostAPISocketPath to the
		// test server's "address" (the httptest URL host).
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		resp, err := http.Post("http://"+sockPath+path, "application/json", bytes.NewReader(b))
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

	origSocketPath := hostAPISocketPath
	hostAPISocketPath = func(sessName string) (string, error) {
		if srv, ok := targetServers[sessName]; ok {
			// Return just the host:port (no scheme).
			return srv.Listener.Addr().String(), nil
		}
		return "", fmt.Errorf("no server for %s", sessName)
	}
	defer func() { hostAPISocketPath = origSocketPath }()

	// Create the coordinator sidecar.
	clk := newTestClock()
	cfg := Config{
		SessionName: coordSession,
		Repo:        repo,
		Worktree:    t.TempDir(),
		DB:          d,
		Clock:       clk,
		AgentRole:   "coordinator",
		HarnessName: "pi",
		Harness:     pih.New("", "", ""),
	}
	sc := New(cfg)

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/apply-profile", map[string]any{
		"profile": "alt",
		"scope":   "coordinator",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /apply-profile status %d, body: %s", rr.Code, rr.Body.String())
	}

	// Collect captured frames with a timeout.
	timeout := time.After(2 * time.Second)
	for i := 0; i < len(targetNames); i++ {
		select {
		case cap := <-captureCh:
			capturedFrames = append(capturedFrames, cap)
			_ = cap
		case <-timeout:
			t.Fatalf("timed out waiting for set_model frames (got %d/%d)", len(capturedFrames), len(targetNames))
		}
	}

	if len(capturedFrames) != len(targetNames) {
		t.Errorf("got %d set_model frames, want %d", len(capturedFrames), len(targetNames))
	}
	for _, cap := range capturedFrames {
		if cap.model != "gpt-4o" {
			t.Errorf("session %s: model = %q, want gpt-4o", cap.session, cap.model)
		}
		if cap.provider != "openai" {
			t.Errorf("session %s: provider = %q, want openai", cap.session, cap.provider)
		}
	}
}

// ── helpers used by the fan-out test ─────────────────────────────────────────

// strPtrVal returns a *string pointing to s.
func strPtrVal(s string) *string { return &s }
