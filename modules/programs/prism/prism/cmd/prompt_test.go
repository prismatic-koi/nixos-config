package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// openPromptTestDB opens a temp DB, registers cleanup, and overrides the
// package-level testDBPath so openDB() uses it. It also unsets PRISM_HOST_API
// so runPrompt uses the local DB path, not the host-API proxy.
func openPromptTestDB(t *testing.T) *db.DB {
	t.Helper()
	// Ensure the host-API proxy is not active for these unit tests.
	t.Setenv("PRISM_HOST_API", "")
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	SetTestDBPath(path)
	t.Cleanup(func() {
		d.Close()
		SetTestDBPath("")
	})
	return d
}

// seedSession creates an agent_status row for testing.
func seedSession(t *testing.T, d *db.DB, sessionName, state string, port *int, opencodeSID *string, rootAgent *string, rootModel *string) {
	t.Helper()
	if err := d.UpsertStatusWithRootAgent(sessionName, "repo", "/code/repo/main", state, nil, opencodeSID, rootAgent, rootModel); err != nil {
		t.Fatalf("UpsertStatusWithRootAgent: %v", err)
	}
	if port != nil {
		var dummy int
		err := d.QueryRow(
			"UPDATE agent_status SET opencode_port = ? WHERE session_name = ? RETURNING 1",
			*port, sessionName,
		).Scan(&dummy)
		if err != nil {
			t.Fatalf("set port: %v", err)
		}
	}
}

func intPtr(n int) *int       { return &n }
func strPtr(s string) *string { return &s }

// extractTestServerPort extracts the port number from an httptest.Server URL.
func extractTestServerPort(t *testing.T, srvURL string) int {
	t.Helper()
	parts := strings.Split(srvURL, ":")
	portStr := parts[len(parts)-1]
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return port
}

// TestBuildPromptBody_WithAgentAndModel verifies the full body when both
// root_agent_name and root_model_id are set.
func TestBuildPromptBody_WithAgentAndModel(t *testing.T) {
	status := &db.Status{
		RootAgentName: strPtr("worker"),
		RootModelID:   strPtr("anthropic/claude-sonnet-4.6"),
	}

	body := buildPromptBody("hello world", status)

	parts, ok := body["parts"].([]map[string]string)
	if !ok || len(parts) != 1 {
		t.Fatalf("parts: got %v, want single text part", body["parts"])
	}
	if parts[0]["type"] != "text" || parts[0]["text"] != "hello world" {
		t.Errorf("parts[0]: got %v, want {type:text, text:hello world}", parts[0])
	}

	if agent, ok := body["agent"].(string); !ok || agent != "worker" {
		t.Errorf("agent: got %v, want \"worker\"", body["agent"])
	}

	model, ok := body["model"].(map[string]string)
	if !ok {
		t.Fatalf("model: got %T, want map[string]string", body["model"])
	}
	if model["providerID"] != "anthropic" {
		t.Errorf("providerID: got %q, want \"anthropic\"", model["providerID"])
	}
	if model["modelID"] != "claude-sonnet-4.6" {
		t.Errorf("modelID: got %q, want \"claude-sonnet-4.6\"", model["modelID"])
	}
}

// TestBuildPromptBody_FallbackToAgentName verifies that agent_name/model_id
// are used when root_agent_name/root_model_id are nil.
func TestBuildPromptBody_FallbackToAgentName(t *testing.T) {
	status := &db.Status{
		AgentName: strPtr("coordinator"),
		ModelID:   strPtr("github-copilot/gpt-4o"),
	}

	body := buildPromptBody("test", status)

	if agent, ok := body["agent"].(string); !ok || agent != "coordinator" {
		t.Errorf("agent: got %v, want \"coordinator\"", body["agent"])
	}
	model, ok := body["model"].(map[string]string)
	if !ok {
		t.Fatalf("model: got %T, want map[string]string", body["model"])
	}
	if model["providerID"] != "github-copilot" {
		t.Errorf("providerID: got %q, want \"github-copilot\"", model["providerID"])
	}
	if model["modelID"] != "gpt-4o" {
		t.Errorf("modelID: got %q, want \"gpt-4o\"", model["modelID"])
	}
}

// TestBuildPromptBody_NoAgentModel verifies the body when neither agent nor
// model are set (pre-migration sessions).
func TestBuildPromptBody_NoAgentModel(t *testing.T) {
	status := &db.Status{}

	body := buildPromptBody("test", status)

	if _, ok := body["agent"]; ok {
		t.Errorf("agent should not be set, got %v", body["agent"])
	}
	if _, ok := body["model"]; ok {
		t.Errorf("model should not be set, got %v", body["model"])
	}
}

// TestBuildPromptBody_ModelWithoutSlash verifies handling of a model_id
// that has no "/" separator.
func TestBuildPromptBody_ModelWithoutSlash(t *testing.T) {
	status := &db.Status{
		RootAgentName: strPtr("worker"),
		RootModelID:   strPtr("claude-sonnet-4.6"),
	}

	body := buildPromptBody("test", status)

	model, ok := body["model"].(map[string]string)
	if !ok {
		t.Fatalf("model: got %T, want map[string]string", body["model"])
	}
	if model["providerID"] != "claude-sonnet-4.6" {
		t.Errorf("providerID: got %q, want \"claude-sonnet-4.6\"", model["providerID"])
	}
	if model["modelID"] != "" {
		t.Errorf("modelID: got %q, want empty", model["modelID"])
	}
}

// TestDeliverViaHTTP_Success verifies that deliverViaHTTP sends the correct
// request to the opencode HTTP API.
func TestDeliverViaHTTP_Success(t *testing.T) {
	var receivedBody map[string]any
	var receivedPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		bodyBytes, _ := io.ReadAll(r.Body)
		json.Unmarshal(bodyBytes, &receivedBody)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	port := extractTestServerPort(t, srv.URL)

	oldClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = oldClient }()

	status := &db.Status{
		RootAgentName: strPtr("worker"),
		RootModelID:   strPtr("anthropic/claude-sonnet-4.6"),
	}

	err := deliverViaHTTP(port, "test-sid-123", "hello prompt", status)
	if err != nil {
		t.Fatalf("deliverViaHTTP: %v", err)
	}

	if receivedPath != "/session/test-sid-123/prompt_async" {
		t.Errorf("path: got %q, want \"/session/test-sid-123/prompt_async\"", receivedPath)
	}

	if receivedBody == nil {
		t.Fatal("received body is nil")
	}

	// Verify the body contains the expected fields.
	bodyJSON, _ := json.Marshal(receivedBody)
	bodyStr := string(bodyJSON)
	if !strings.Contains(bodyStr, "hello prompt") {
		t.Errorf("body should contain prompt text: got %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "worker") {
		t.Errorf("body should contain agent name: got %s", bodyStr)
	}
}

// TestDeliverViaHTTP_ServerError verifies that deliverViaHTTP returns an error
// when the server responds with a non-2xx status.
func TestDeliverViaHTTP_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	port := extractTestServerPort(t, srv.URL)

	oldClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = oldClient }()

	status := &db.Status{}
	err := deliverViaHTTP(port, "sid", "test", status)
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500: got %q", err.Error())
	}
}

// TestDeliverViaHTTP_ConnectionRefused verifies that deliverViaHTTP returns
// an error when the server is not running.
func TestDeliverViaHTTP_ConnectionRefused(t *testing.T) {
	status := &db.Status{}
	err := deliverViaHTTP(19999, "sid", "test", status)
	if err == nil {
		t.Fatal("expected error for connection refused, got nil")
	}
}

// TestRunPrompt_HTTPDelivery verifies the full runPrompt flow via HTTP delivery.
func TestRunPrompt_HTTPDelivery(t *testing.T) {
	var receivedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		json.Unmarshal(bodyBytes, &receivedBody)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	port := extractTestServerPort(t, srv.URL)

	oldClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = oldClient }()

	d := openPromptTestDB(t)
	seedSession(t, d, "repo@main", "active", intPtr(port), strPtr("oc-sid-1"), strPtr("worker"), strPtr("anthropic/claude-sonnet-4.6"))

	rootCmd.SetArgs([]string{"prompt", "repo@main", "--prompt", "do the mahi"})
	output := captureStdout(t, func() {
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "HTTP") {
		t.Errorf("output should mention HTTP: got %q", output)
	}

	// Verify an audit bus_messages row was written with delivered_at set.
	pending, err := d.PendingMessages("repo@main", "normal", "")
	if err != nil {
		t.Fatalf("PendingMessages: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending messages should be 0 (delivered_at set): got %d", len(pending))
	}

	// Verify the audit row exists (delivered).
	var count int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE to_session = ?", "repo@main").Scan(&count); err != nil {
		t.Fatalf("count audit row: %v", err)
	}
	if count != 1 {
		t.Errorf("audit row count: got %d, want 1", count)
	}
}

// TestRunPrompt_BusFallback_NoPort verifies that prompts fall back to
// bus_messages when opencode_port is NULL.
func TestRunPrompt_BusFallback_NoPort(t *testing.T) {
	d := openPromptTestDB(t)
	seedSession(t, d, "repo@legacy", "active", nil, nil, nil, nil)

	rootCmd.SetArgs([]string{"prompt", "repo@legacy", "--prompt", "fallback test"})
	output := captureStdout(t, func() {
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "queued") {
		t.Errorf("output should mention queued: got %q", output)
	}

	pending, err := d.PendingMessages("repo@legacy", "normal", "")
	if err != nil {
		t.Fatalf("PendingMessages: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("pending count: got %d, want 1", len(pending))
	}
}

// TestRunPrompt_BusFallback_HTTPError verifies that when HTTP delivery fails,
// the prompt falls back to bus_messages.
func TestRunPrompt_BusFallback_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	port := extractTestServerPort(t, srv.URL)

	oldClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = oldClient }()

	d := openPromptTestDB(t)
	seedSession(t, d, "repo@fail", "active", intPtr(port), strPtr("oc-sid-2"), strPtr("worker"), strPtr("anthropic/claude-sonnet-4.6"))

	rootCmd.SetArgs([]string{"prompt", "repo@fail", "--prompt", "retry me"})
	output := captureStdout(t, func() {
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "queued") {
		t.Errorf("output should mention queued: got %q", output)
	}

	pending, err := d.PendingMessages("repo@fail", "normal", "")
	if err != nil {
		t.Fatalf("PendingMessages: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("pending count: got %d, want 1", len(pending))
	}
}

// TestRunPrompt_WaitingStateGuard verifies that prism prompt refuses to send
// when the target session is in "waiting" state.
func TestRunPrompt_WaitingStateGuard(t *testing.T) {
	d := openPromptTestDB(t)
	seedSession(t, d, "repo@waiting", "waiting", nil, nil, nil, nil)

	rootCmd.SetArgs([]string{"prompt", "repo@waiting", "--prompt", "should fail"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for waiting session, got nil")
	}
	if !strings.Contains(err.Error(), "waiting") {
		t.Errorf("error should mention waiting: got %q", err.Error())
	}
}

// TestRunPrompt_EndedSession verifies that prism prompt refuses to send
// when the target session has ended.
func TestRunPrompt_EndedSession(t *testing.T) {
	d := openPromptTestDB(t)
	seedSession(t, d, "repo@ended", "finished", nil, nil, nil, nil)
	if err := d.SetEnded("repo@ended"); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	rootCmd.SetArgs([]string{"prompt", "repo@ended", "--prompt", "should fail"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for ended session, got nil")
	}
	if !strings.Contains(err.Error(), "ended") {
		t.Errorf("error should mention ended: got %q", err.Error())
	}
}

// TestRunPrompt_SessionNotFound verifies that prism prompt errors when the
// target session doesn't exist.
func TestRunPrompt_SessionNotFound(t *testing.T) {
	_ = openPromptTestDB(t)

	rootCmd.SetArgs([]string{"prompt", "repo@nonexistent", "--prompt", "should fail"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for nonexistent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found: got %q", err.Error())
	}
}

// TestRunPrompt_UrgentFlagAccepted verifies that --urgent is accepted without
// error (backward compatibility) even though it's now a no-op.
func TestRunPrompt_UrgentFlagAccepted(t *testing.T) {
	d := openPromptTestDB(t)
	seedSession(t, d, "repo@urgent", "active", nil, nil, nil, nil)

	rootCmd.SetArgs([]string{"prompt", "repo@urgent", "--urgent", "--prompt", "urgent test"})
	output := captureStdout(t, func() {
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "queued") {
		t.Errorf("output should mention queued: got %q", output)
	}
}

// TestWriteBusMessageDelivered verifies that WriteBusMessageDelivered inserts
// a row with delivered_at set (not pending).
func TestWriteBusMessageDelivered(t *testing.T) {
	d := openPromptTestDB(t)

	msg := db.BusMessage{
		ID:          "test-delivered-1",
		FromSession: "repo@sender",
		ToSession:   "repo@receiver",
		Repo:        "repo",
		Text:        "delivered test",
		Urgency:     "normal",
		SentAt:      time.Now(),
	}

	if err := d.WriteBusMessageDelivered(msg); err != nil {
		t.Fatalf("WriteBusMessageDelivered: %v", err)
	}

	// Must NOT appear in pending messages (delivered_at is set).
	pending, err := d.PendingMessages("repo@receiver", "normal", "")
	if err != nil {
		t.Fatalf("PendingMessages: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending count: got %d, want 0 (message is already delivered)", len(pending))
	}

	// Verify the row exists with delivered_at set.
	var deliveredAt *int64
	err = d.QueryRow("SELECT delivered_at FROM bus_messages WHERE id = ?", "test-delivered-1").Scan(&deliveredAt)
	if err != nil {
		t.Fatalf("query delivered_at: %v", err)
	}
	if deliveredAt == nil {
		t.Error("delivered_at: got nil, want non-nil")
	}
}
