package promptdelivery_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	_ "github.com/prismatic-koi/prism/internal/harness/pi" // register pi harness for ShapeOf("pi") in tests
	"github.com/prismatic-koi/prism/internal/promptdelivery"
	"github.com/prismatic-koi/prism/internal/session"
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

	err := promptdelivery.DeliverToSession("myrepo@feature", status, "hello opencode", nil, "", "")
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

	err := promptdelivery.DeliverToSession("myrepo@feature", status, "hello", nil, "", "")
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
	err := promptdelivery.DeliverToSession("myrepo@feature", status, "hello", nil, "", "")
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

	err := promptdelivery.DeliverToSession("myrepo@feature", status, "hello nil harness", nil, "", "")
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

	err := promptdelivery.DeliverToSession("myrepo@feature", status, "custom body test", customBuilder, "", "")
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

// TestDeliverToSession_PiPath_DeliverAsForwarded verifies that DeliverToSession
// forwards the deliverAs parameter as the "deliver_as" JSON field in the
// /prompt POST body when the session uses the pi (TransportSocketPipe) harness.
// This ensures callers that pass "followUp" (e.g. notifyCoordinator) have their
// intent preserved end-to-end — not overridden with a hardcoded "nextTurn".
func TestDeliverToSession_PiPath_DeliverAsForwarded(t *testing.T) {
	tests := []struct {
		deliverAs string
		want      string
	}{
		{deliverAs: "followUp", want: "followUp"},
		{deliverAs: "steer", want: "steer"},
		{deliverAs: "nextTurn", want: "nextTurn"},
		{deliverAs: "", want: ""}, // empty → field omitted from body; sidecar defaults to nextTurn
	}

	for _, tc := range tests {
		t.Run("deliverAs="+tc.deliverAs, func(t *testing.T) {
			// Redirect XDG_STATE_HOME into a per-subtest temp dir so that
			// SidecarHostAPIPath never falls back to $HOME — which is
			// /homeless-shelter (unwritable) inside the Nix build sandbox.
			// Use os.MkdirTemp with a short prefix rather than t.TempDir() to
			// keep the resulting socket path under the 108-byte sun_path limit
			// (t.TempDir embeds the full subtest name which pushes it over).
			xdgTmp, mkTmpErr := os.MkdirTemp("", "prism-pd-*")
			if mkTmpErr != nil {
				t.Fatalf("create temp dir: %v", mkTmpErr)
			}
			t.Cleanup(func() { _ = os.RemoveAll(xdgTmp) })
			t.Setenv("XDG_STATE_HOME", xdgTmp)

			// Derive a session name that maps to a socket path we can control.
			sessionName := "myrepo@feature"
			sockPath, err := session.SidecarHostAPIPath(sessionName)
			if err != nil {
				t.Fatalf("resolve socket path: %v", err)
			}

			// Create the parent directory so the socket can be created there.
			dir := sockPath[:strings.LastIndex(sockPath, "/")]
			if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
				t.Fatalf("mkdir socket dir: %v", mkErr)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })

			// Start a Unix-socket HTTP server that captures the /prompt body.
			var gotBody map[string]string
			lns, listenErr := net.Listen("unix", sockPath)
			if listenErr != nil {
				t.Fatalf("listen on socket: %v", listenErr)
			}
			srv := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					b, _ := io.ReadAll(r.Body)
					_ = json.Unmarshal(b, &gotBody)
					w.WriteHeader(http.StatusOK)
				}),
			}
			go func() { _ = srv.Serve(lns) }()
			t.Cleanup(func() { _ = srv.Close() })

			piHarness := "pi"
			status := &db.Status{
				SessionName: sessionName,
				Harness:     &piHarness,
			}

			if deliverErr := promptdelivery.DeliverToSession(sessionName, status, "hello pi", nil, "", tc.deliverAs); deliverErr != nil {
				t.Fatalf("DeliverToSession: %v", deliverErr)
			}

			// Verify deliver_as in the captured body.
			if gotBody == nil {
				t.Fatal("server received no request body")
			}
			if tc.want == "" {
				// Empty deliverAs → field must be absent from the body.
				if _, ok := gotBody["deliver_as"]; ok {
					t.Errorf("deliver_as = %q in body, want absent (empty deliverAs omitted)",
						gotBody["deliver_as"])
				}
			} else {
				if got := gotBody["deliver_as"]; got != tc.want {
					t.Errorf("deliver_as = %q, want %q", got, tc.want)
				}
			}
		})
	}
}
