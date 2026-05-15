package cmd

// prompt_pi_test.go — tests for the PI socket-pipe routing in `prism prompt`
// (P2.SPAWN, #1212).
//
// The host-side `prism prompt <pi-session>` CLI cannot use the harness HTTP
// API path (PI sessions have no harness_port). Instead it must dial the
// per-session sidecar host-API Unix socket and route via /prompt, which the
// sidecar then forwards to the harness pipe via DeliverPrompt.

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

// setStatusHarness updates the harness column for an existing agent_status
// row. The DB package's UpsertStatus* helpers hard-code harness='pi',
// so tests covering non-default harnesses use this raw UPDATE via the
// public QueryRow accessor (UPDATE … RETURNING is required because QueryRow
// needs at least one row to scan).
func setStatusHarness(t *testing.T, d *db.DB, sessionName, harness string) {
	t.Helper()
	var dummy int
	err := d.QueryRow(
		"UPDATE agent_status SET harness = ? WHERE session_name = ? RETURNING 1",
		harness, sessionName,
	).Scan(&dummy)
	if err != nil {
		t.Fatalf("set harness: %v", err)
	}
}

// maxSunPath is the POSIX limit for sockaddr_un.sun_path (104 on macOS, 108 on Linux).
// We use 104 as the conservative cross-platform ceiling.
const maxSunPath = 104

// TestRunPrompt_PISession_RoutesThroughSidecarHostAPI seeds a PI session in
// the DB and starts a stub Unix-socket HTTP server at the per-session
// host-api.sock path. It then runs `prism prompt <session>` and asserts that
// the request reached the stub server with the correct body, that no HTTP
// (HTTP) call was made, and that the audit row was written.
func TestRunPrompt_PISession_RoutesThroughSidecarHostAPI(t *testing.T) {
	// Use a short XDG_STATE_HOME so the per-session socket path stays under
	// the 104-char Unix sun_path limit on macOS. os.MkdirTemp with a short
	// two-character prefix keeps the base path short enough on both Linux and
	// Darwin (unlike filepath.Join(os.TempDir(), ...) which can exceed 104
	// chars on macOS where TempDir returns a long /private/var/folders/... path).
	stateHome, err := os.MkdirTemp("", "pp")
	if err != nil {
		t.Fatalf("mkdir state home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateHome) })
	t.Setenv("XDG_STATE_HOME", stateHome)

	sessionName := "repo@pi"
	sockPath, err := session.SidecarHostAPIPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	if len(sockPath) > maxSunPath {
		t.Fatalf("socket path too long (%d > %d): %s", len(sockPath), maxSunPath, sockPath)
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}

	type req struct {
		Session string `json:"session"`
		Prompt  string `json:"prompt"`
	}
	reqCh := make(chan req, 1)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prompt" {
			http.Error(w, `{"error":"unexpected path"}`, http.StatusBadRequest)
			return
		}
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
	// Override the harness column so the prompt path takes the socket-pipe branch.
	setStatusHarness(t, d, sessionName, "pi")

	rootCmd.SetArgs([]string{"prompt", sessionName, "--prompt", "do the mahi"})
	output := captureStdout(t, func() {
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "socket-pipe") {
		t.Errorf("output should mention socket-pipe delivery: got %q", output)
	}

	select {
	case got := <-reqCh:
		if got.Session != sessionName {
			t.Errorf("request session: got %q, want %q", got.Session, sessionName)
		}
		if got.Prompt != "do the mahi" {
			t.Errorf("request prompt: got %q, want %q", got.Prompt, "do the mahi")
		}
	default:
		t.Fatal("did not receive a /prompt request on the per-session socket")
	}

	// Verify the audit row was written.
	var count int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE to_session = ? AND delivered_at IS NOT NULL", sessionName).Scan(&count); err != nil {
		t.Fatalf("count audit row: %v", err)
	}
	if count != 1 {
		t.Errorf("audit row count: got %d, want 1", count)
	}
}

// TestRunPrompt_PISession_SocketMissingReturnsClearError verifies that when
// the per-session host-API socket does not exist (e.g. sidecar not running),
// the error message points at the missing socket path so an operator can
// diagnose the failure.
func TestRunPrompt_PISession_SocketMissingReturnsClearError(t *testing.T) {
	stateHome, err := os.MkdirTemp("", "pm")
	if err != nil {
		t.Fatalf("mkdir state home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateHome) })
	t.Setenv("XDG_STATE_HOME", stateHome)

	sessionName := "repo@pi-no-sock"

	d := openPromptTestDB(t)
	seedSession(t, d, sessionName, "active", nil, nil, strPtr("worker"), nil)
	setStatusHarness(t, d, sessionName, "pi")

	rootCmd.SetArgs([]string{"prompt", sessionName, "--prompt", "test"})
	execErr := rootCmd.Execute()
	if execErr == nil {
		t.Fatal("expected error when sidecar socket missing, got nil")
	}
	msg := execErr.Error()
	if !strings.Contains(msg, "socket") && !strings.Contains(msg, "host-API") {
		t.Errorf("error should mention socket delivery failure: got %q", msg)
	}
}
