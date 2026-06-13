package cmd

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

// openPromptTestDB opens a temp DB, registers cleanup, and overrides the
// package-level testDBPath so openDB() uses it. It also unsets PRISM_HOST_API
// so runPrompt uses the local DB path, not the host-API proxy.
//
// As a deflake measure (#1521) it also calls resetRootCmdFlags so any
// rootCmd flag values left behind by a previous test (or a previous iteration
// under `go test -count=N`) are wiped before this test drives the cobra tree
// via rootCmd.SetArgs / rootCmd.Execute.
func openPromptTestDB(t *testing.T) *db.DB {
	t.Helper()
	resetRootCmdFlags(t)
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
func seedSession(t *testing.T, d *db.DB, sessionName, state string, port *int, harnessSessionID *string, rootAgent *string, rootModel *string) {
	t.Helper()
	if err := d.UpsertStatusWithRootAgent(sessionName, "repo", "/code/repo/main", state, nil, harnessSessionID, rootAgent, rootModel); err != nil {
		t.Fatalf("UpsertStatusWithRootAgent: %v", err)
	}
	// Clear harness for test sessions so HTTP fallback path is used.
	if err := d.QueryRow("UPDATE agent_status SET harness = '' WHERE session_name = ? RETURNING 1", sessionName).Scan(new(int)); err != nil {
		t.Fatalf("clear harness: %v", err)
	}
	if port != nil {
		var dummy int
		err := d.QueryRow(
			// Clear harness so HTTP fallback path is used in tests.
			"UPDATE agent_status SET harness_port = ?, harness = '' WHERE session_name = ? RETURNING 1",
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

// TestDeliverViaHTTP_ConnectionRefused verifies that deliverViaHTTP returns

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

	if !strings.Contains(output, "prompt delivered") {
		t.Errorf("output should mention delivery: got %q", output)
	}

	// Verify the audit row exists (delivered_at set).
	var count int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE to_session = ?", "repo@main").Scan(&count); err != nil {
		t.Fatalf("count audit row: %v", err)
	}
	if count != 1 {
		t.Errorf("audit row count: got %d, want 1", count)
	}
}

// TestRunPrompt_NoPort_ReturnsError verifies that prompts return an error
// when harness_port is NULL (no HTTP delivery possible).
func TestRunPrompt_NoPort_ReturnsError(t *testing.T) {
	d := openPromptTestDB(t)
	seedSession(t, d, "repo@legacy", "active", nil, nil, nil, nil)

	rootCmd.SetArgs([]string{"prompt", "repo@legacy", "--prompt", "test"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error when session has no port, got nil")
	}
	if !strings.Contains(err.Error(), "no harness port") {
		t.Errorf("error should mention missing port: got %q", err.Error())
	}
}

// TestRunPrompt_HTTPError_ReturnsError verifies that when HTTP delivery fails,
// an error is returned (no fallback to bus).
func TestRunPrompt_HTTPError_ReturnsError(t *testing.T) {
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
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error when HTTP delivery fails, got nil")
	}
	if !strings.Contains(err.Error(), "500") && !strings.Contains(err.Error(), "http status") {
		t.Errorf("error should mention HTTP failure: got %q", err.Error())
	}

	// Verify no undelivered bus_messages row was written.
	var count int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE to_session = ? AND delivered_at IS NULL", "repo@fail").Scan(&count); err != nil {
		t.Fatalf("count undelivered: %v", err)
	}
	if count != 0 {
		t.Errorf("no undelivered bus messages expected after HTTP failure, got %d", count)
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

// TestRunPrompt_DeliverAs_DefaultIsSteer verifies that prism prompt with no
// --deliver-as flag sends deliver_as=steer in the sidecar host-API request body.
func TestRunPrompt_DeliverAs_DefaultIsSteer(t *testing.T) {
	stateHome, err := os.MkdirTemp("", "da")
	if err != nil {
		t.Fatalf("mkdir state home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateHome) })
	t.Setenv("XDG_STATE_HOME", stateHome)

	sessionName := "repo@da-steer"
	sockPath, err := session.SidecarHostAPIPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}

	type req struct {
		DeliverAs string `json:"deliver_as"`
	}
	reqCh := make(chan req, 1)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got req
		_ = json.Unmarshal(body, &got)
		reqCh <- got
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	d := openPromptTestDB(t)
	seedSession(t, d, sessionName, "active", nil, nil, strPtr("worker"), nil)
	setStatusHarness(t, d, sessionName, "pi")

	// No --deliver-as flag — default should be steer.
	rootCmd.SetArgs([]string{"prompt", sessionName, "--prompt", "mid-flight correction"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	select {
	case got := <-reqCh:
		if got.DeliverAs != "steer" {
			t.Errorf("deliver_as = %q, want \"steer\" (default)", got.DeliverAs)
		}
	default:
		t.Fatal("did not receive a /prompt request on the per-session socket")
	}
}

// TestRunPrompt_DeliverAs_ExplicitFollowUp verifies that --deliver-as followUp
// sends deliver_as=followUp in the request body.
func TestRunPrompt_DeliverAs_ExplicitFollowUp(t *testing.T) {
	stateHome, err := os.MkdirTemp("", "da")
	if err != nil {
		t.Fatalf("mkdir state home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateHome) })
	t.Setenv("XDG_STATE_HOME", stateHome)

	sessionName := "repo@da-follow"
	sockPath, err := session.SidecarHostAPIPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}

	type req struct {
		DeliverAs string `json:"deliver_as"`
	}
	reqCh := make(chan req, 1)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got req
		_ = json.Unmarshal(body, &got)
		reqCh <- got
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	d := openPromptTestDB(t)
	seedSession(t, d, sessionName, "active", nil, nil, strPtr("worker"), nil)
	setStatusHarness(t, d, sessionName, "pi")

	rootCmd.SetArgs([]string{"prompt", sessionName, "--prompt", "follow-up message", "--deliver-as", "followUp"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	select {
	case got := <-reqCh:
		if got.DeliverAs != "followUp" {
			t.Errorf("deliver_as = %q, want \"followUp\"", got.DeliverAs)
		}
	default:
		t.Fatal("did not receive a /prompt request on the per-session socket")
	}
}

// TestRunPrompt_DeliverAs_ExplicitNextTurn verifies that --deliver-as nextTurn
// sends deliver_as=nextTurn in the request body.
func TestRunPrompt_DeliverAs_ExplicitNextTurn(t *testing.T) {
	stateHome, err := os.MkdirTemp("", "da")
	if err != nil {
		t.Fatalf("mkdir state home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateHome) })
	t.Setenv("XDG_STATE_HOME", stateHome)

	sessionName := "repo@da-next"
	sockPath, err := session.SidecarHostAPIPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}

	type req struct {
		DeliverAs string `json:"deliver_as"`
	}
	reqCh := make(chan req, 1)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got req
		_ = json.Unmarshal(body, &got)
		reqCh <- got
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	d := openPromptTestDB(t)
	seedSession(t, d, sessionName, "active", nil, nil, strPtr("worker"), nil)
	setStatusHarness(t, d, sessionName, "pi")

	rootCmd.SetArgs([]string{"prompt", sessionName, "--prompt", "queued message", "--deliver-as", "nextTurn"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	select {
	case got := <-reqCh:
		if got.DeliverAs != "nextTurn" {
			t.Errorf("deliver_as = %q, want \"nextTurn\"", got.DeliverAs)
		}
	default:
		t.Fatal("did not receive a /prompt request on the per-session socket")
	}
}

// TestRunPrompt_DeliverAs_InvalidValueRejected verifies that --deliver-as bogus
// exits non-zero with a clear error before any HTTP request is made.
func TestRunPrompt_DeliverAs_InvalidValueRejected(t *testing.T) {
	// Ensure the host-API proxy is not active.
	t.Setenv("PRISM_HOST_API", "")

	// We use an HTTP server to verify no request is made.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not receive a request for an invalid --deliver-as value")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	port := extractTestServerPort(t, srv.URL)
	oldClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = oldClient }()

	d := openPromptTestDB(t)
	seedSession(t, d, "repo@bogus", "active", intPtr(port), strPtr("oc-sid-x"), strPtr("worker"), strPtr("anthropic/claude-sonnet-4.6"))

	rootCmd.SetArgs([]string{"prompt", "repo@bogus", "--prompt", "test", "--deliver-as", "bogus"})
	execErr := rootCmd.Execute()
	if execErr == nil {
		t.Fatal("expected error for invalid --deliver-as value, got nil")
	}
	if !strings.Contains(execErr.Error(), "bogus") {
		t.Errorf("error should mention the invalid value: got %q", execErr.Error())
	}
	// Error must name the accepted values.
	for _, m := range []string{"steer", "followUp", "nextTurn"} {
		if !strings.Contains(execErr.Error(), m) {
			t.Errorf("error should mention accepted value %q: got %q", m, execErr.Error())
		}
	}
}

// TestWriteBusMessageDelivered verifies that WriteBusMessageDelivered inserts
// a row with delivered_at set (audit trail for HTTP-delivered prompts).
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

	// Verify the row exists with delivered_at set.
	var deliveredAt *int64
	err := d.QueryRow("SELECT delivered_at FROM bus_messages WHERE id = ?", "test-delivered-1").Scan(&deliveredAt)
	if err != nil {
		t.Fatalf("query delivered_at: %v", err)
	}
	if deliveredAt == nil {
		t.Error("delivered_at: got nil, want non-nil")
	}
}
