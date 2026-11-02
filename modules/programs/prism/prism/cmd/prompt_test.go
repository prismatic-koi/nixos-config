package cmd

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

// openPromptTestDB opens a temp DB, registers cleanup, and overrides the
// package-level testDBPath so openDB() uses it. It also unsets PRISM_HOST_API
// so runPrompt uses the local DB path, not the host-API proxy.
//
// As a deflake measure it also calls resetRootCmdFlags so any
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

// seedSession creates the agent_status row of a pi session for testing.
func seedSession(t *testing.T, d *db.DB, sessionName, state string, harnessSessionID *string, rootAgent *string, rootModel *string) {
	t.Helper()
	if err := d.UpsertStatusWithRootAgent(sessionName, "repo", "/code/repo/main", state, nil, harnessSessionID, rootAgent, rootModel); err != nil {
		t.Fatalf("UpsertStatusWithRootAgent: %v", err)
	}
}

func intPtr(n int) *int       { return &n }
func strPtr(s string) *string { return &s }

// TestRunPrompt_NoSocketPipeHarness_ReturnsError verifies that a prompt to a
// session whose harness has no socket pipe returns an error and writes no
// audit row.
func TestRunPrompt_NoSocketPipeHarness_ReturnsError(t *testing.T) {
	d := openPromptTestDB(t)
	seedSession(t, d, "repo@legacy", "active", nil, nil, nil)
	setStatusHarness(t, d, "repo@legacy", "")

	rootCmd.SetArgs([]string{"prompt", "repo@legacy", "--prompt", "test"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error when the harness has no socket pipe, got nil")
	}
	if !strings.Contains(err.Error(), "has no socket pipe") {
		t.Errorf("error should say that the harness has no socket pipe: got %q", err.Error())
	}
	var count int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE to_session = ?", "repo@legacy").Scan(&count); err != nil {
		t.Fatalf("count bus_messages: %v", err)
	}
	if count != 0 {
		t.Errorf("bus_messages rows = %d, want 0", count)
	}
}

// TestRunPrompt_WaitingStateGuard verifies that prism prompt refuses to send
// when the target session is in "waiting" state.
func TestRunPrompt_WaitingStateGuard(t *testing.T) {
	d := openPromptTestDB(t)
	seedSession(t, d, "repo@waiting", "waiting", nil, nil, nil)

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
	seedSession(t, d, "repo@ended", "finished", nil, nil, nil)
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
	seedSession(t, d, sessionName, "active", nil, strPtr("worker"), nil)
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
	seedSession(t, d, sessionName, "active", nil, strPtr("worker"), nil)
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
	seedSession(t, d, sessionName, "active", nil, strPtr("worker"), nil)
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
// exits non-zero with a clear error before any request is made.
func TestRunPrompt_DeliverAs_InvalidValueRejected(t *testing.T) {
	// Ensure the host-API proxy is not active.
	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("XDG_STATE_HOME", stateguardtest.ShortTempDir(t, "da-"))

	// The stub of the host-API socket verifies that no request is made.
	startHostAPIStub(t, "repo@bogus", func(string, []byte) int {
		t.Errorf("host-API socket should not receive a request for an invalid --deliver-as value")
		return http.StatusInternalServerError
	})

	d := openPromptTestDB(t)
	seedSession(t, d, "repo@bogus", "active", strPtr("oc-sid-x"), strPtr("worker"), strPtr("anthropic/claude-sonnet-4.6"))

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
// a row with delivered_at set (audit trail for delivered prompts).
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
