package promptdelivery_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
)

// TestDeliverToSession_OpencodePath verifies that DeliverToSession routes
// sessions with harness="opencode" through the HTTP prompt_async endpoint,
// leaving the host-API socket path unused.
func TestDeliverToSession_OpencodePath(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Extract the port from the test server URL.
	addr := srv.Listener.Addr().(*net.TCPAddr)
	port := addr.Port

	harness := "opencode"
	sid := "session-123"
	status := &db.Status{
		SessionName:      "myrepo@feature",
		Harness:          &harness,
		HarnessPort:      &port,
		HarnessSessionID: &sid,
	}

	err := promptdelivery.DeliverToSession("myrepo@feature", status, "hello opencode", nil, "")
	if err != nil {
		t.Fatalf("DeliverToSession: %v", err)
	}

	// Verify the body was sent to prompt_async.
	if !strings.Contains(string(gotBody), "hello opencode") {
		t.Errorf("expected body to contain 'hello opencode', got %q", gotBody)
	}
}

// TestDeliverToSession_OpencodePath_NoPort verifies that DeliverToSession
// returns an error for opencode sessions that have no harness port.
func TestDeliverToSession_OpencodePath_NoPort(t *testing.T) {
	harness := "opencode"
	status := &db.Status{
		SessionName: "myrepo@feature",
		Harness:     &harness,
		// HarnessPort is nil — cannot deliver.
	}

	err := promptdelivery.DeliverToSession("myrepo@feature", status, "hello", nil, "")
	if err == nil {
		t.Fatal("expected error for missing harness port, got nil")
	}
	if !strings.Contains(err.Error(), "harness port") {
		t.Errorf("error %q should mention harness port", err.Error())
	}
}

// TestDeliverToSession_PiPath_MissingSocket verifies that DeliverToSession
// returns a clear error when the host-API socket does not exist (session ended
// or socket cleaned up).
func TestDeliverToSession_PiPath_MissingSocket(t *testing.T) {
	piHarness := "pi"
	status := &db.Status{
		SessionName: "myrepo@feature",
		Harness:     &piHarness,
	}

	// The socket path doesn't exist — we expect a clear error, not a hang.
	err := promptdelivery.DeliverToSession("myrepo@feature", status, "hello", nil, "")
	if err == nil {
		t.Fatal("expected error for missing socket, got nil")
	}
	// Error should mention the socket or session.
	errStr := err.Error()
	if !strings.Contains(errStr, "socket") && !strings.Contains(errStr, "session") {
		t.Errorf("error %q should mention socket or session", errStr)
	}
}

// TestDeliverToSession_NilHarness verifies that DeliverToSession falls back to
// the HTTP path when status.Harness is nil (pre-migration rows).
func TestDeliverToSession_NilHarness(t *testing.T) {
	var gotRequest bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequest = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := srv.Listener.Addr().(*net.TCPAddr)
	port := addr.Port
	sid := "session-456"

	status := &db.Status{
		SessionName:      "myrepo@feature",
		Harness:          nil, // pre-migration: no harness recorded
		HarnessPort:      &port,
		HarnessSessionID: &sid,
	}

	err := promptdelivery.DeliverToSession("myrepo@feature", status, "hello nil harness", nil, "")
	if err != nil {
		t.Fatalf("DeliverToSession: %v", err)
	}
	if !gotRequest {
		t.Error("expected HTTP request to be made for nil harness, but none was sent")
	}
}

// TestDeliverToSession_CustomBodyBuilder verifies that a custom buildHTTPBody
// function is called for the opencode HTTP path and its output is used as the
// POST body.
func TestDeliverToSession_CustomBodyBuilder(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := srv.Listener.Addr().(*net.TCPAddr)
	port := addr.Port
	sid := "session-789"
	harness := "opencode"

	status := &db.Status{
		SessionName:      "myrepo@feature",
		Harness:          &harness,
		HarnessPort:      &port,
		HarnessSessionID: &sid,
	}

	customBuilder := func(text string, _ *db.Status) map[string]any {
		return map[string]any{"custom_text": text, "extra": "field"}
	}

	err := promptdelivery.DeliverToSession("myrepo@feature", status, "custom body test", customBuilder, "")
	if err != nil {
		t.Fatalf("DeliverToSession: %v", err)
	}
	if gotBody["custom_text"] != "custom body test" {
		t.Errorf("custom_text = %v, want 'custom body test'", gotBody["custom_text"])
	}
	if gotBody["extra"] != "field" {
		t.Errorf("extra = %v, want 'field'", gotBody["extra"])
	}
}
