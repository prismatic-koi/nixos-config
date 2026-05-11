package sidecar

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness/opencode"
)

// seedSessionWithHarnessPort seeds an agent_status row for sessionName with a
// harness_port and harness_session_id. Used to set up invoker sessions that
// accept HTTP-based prompt delivery.
func seedSessionWithHarnessPort(t *testing.T, database *db.DB, sessionName, repo, sid string, port int) {
	t.Helper()
	agentName := "coordinator"
	modelID := "anthropic/claude-sonnet-4-5"
	if err := database.UpsertStatusWithAgent(sessionName, repo, "/tmp/test-worktree", "active", nil, &sid, &agentName, &modelID); err != nil {
		t.Fatalf("seedSessionWithHarnessPort: UpsertStatusWithAgent(%q): %v", sessionName, err)
	}
	if err := database.QueryRow(
		"UPDATE agent_status SET harness_port = ?, harness_session_id = ? WHERE session_name = ? RETURNING session_name",
		port, sid, sessionName,
	).Scan(new(string)); err != nil {
		t.Fatalf("seedSessionWithHarnessPort: set port/sid for %q: %v", sessionName, err)
	}
}

// seedEndedSession seeds an agent_status row with ended_at set.
func seedEndedSession(t *testing.T, database *db.DB, sessionName, repo string) {
	t.Helper()
	agentName := "coordinator"
	modelID := "anthropic/claude-sonnet-4-5"
	if err := database.UpsertStatusWithAgent(sessionName, repo, "/tmp/test-worktree", "finished", nil, nil, &agentName, &modelID); err != nil {
		t.Fatalf("seedEndedSession: UpsertStatusWithAgent(%q): %v", sessionName, err)
	}
	// Use Unix millisecond timestamp so SQLite can scan it as int64.
	nowMs := time.Now().UnixMilli()
	if err := database.QueryRow(
		"UPDATE agent_status SET ended_at = ? WHERE session_name = ? RETURNING session_name",
		nowMs, sessionName,
	).Scan(new(string)); err != nil {
		t.Fatalf("seedEndedSession: set ended_at for %q: %v", sessionName, err)
	}
}

// ── investigateAgentInvokerSession ──────────────────────────────────────────

func TestInvestigateAgentInvokerSession(t *testing.T) {
	cases := []struct {
		name        string
		sessionName string
		wantInvoker string
		wantOK      bool
	}{
		{
			name:        "standard investigate session",
			sessionName: "nixos-config@main~investigate-abc123",
			wantInvoker: "nixos-config@main",
			wantOK:      true,
		},
		{
			name:        "investigate session with longer slug",
			sessionName: "myrepo@feature~investigate-task-abc",
			wantInvoker: "myrepo@feature",
			wantOK:      true,
		},
		{
			name:        "review session — not investigate",
			sessionName: "myrepo@feature~review-2-review-goal",
			wantInvoker: "",
			wantOK:      false,
		},
		{
			name:        "plain worker session",
			sessionName: "myrepo@feature",
			wantInvoker: "",
			wantOK:      false,
		},
		{
			name:        "coordinator session",
			sessionName: "myrepo@main",
			wantInvoker: "",
			wantOK:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := investigateAgentInvokerSession(tc.sessionName)
			if ok != tc.wantOK {
				t.Errorf("investigateAgentInvokerSession(%q): ok=%v want %v", tc.sessionName, ok, tc.wantOK)
			}
			if got != tc.wantInvoker {
				t.Errorf("investigateAgentInvokerSession(%q): invoker=%q want %q", tc.sessionName, got, tc.wantInvoker)
			}
		})
	}
}

// ── notifyInvestigatorTurnEnd ────────────────────────────────────────────────

// TestNotifyInvestigatorTurnEnd_Delivery verifies that a body-bearing per-turn
// notification is delivered to the invoker session when the turn ends with a
// non-empty assistant text block. The notification body must contain the
// sender label, the text block verbatim, and the steering-channel hint.
func TestNotifyInvestigatorTurnEnd_Delivery(t *testing.T) {
	d := openTestDB(t)
	repo := "nixos-config"
	invokerSession := repo + "@main"
	investigatorSession := invokerSession + "~investigate-testslug"
	invokerSID := "invoker-sid-001"

	var mu sync.Mutex
	var receivedBodies []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/session" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id":"` + invokerSID + `"}]`))
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/prompt_async") {
			body := make([]byte, 4096)
			n, _ := r.Body.Read(body)
			mu.Lock()
			receivedBodies = append(receivedBodies, string(body[:n]))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	srvPort := parseSrvPort(t, srv.URL)
	seedSessionWithHarnessPort(t, d, invokerSession, repo, invokerSID, srvPort)

	clk := newTestClock()
	cfg := Config{
		SessionName: investigatorSession,
		Repo:        repo,
		Worktree:    "/tmp/investigate-test-wt",
		DB:          d,
		Clock:       clk,
		HTTPClient:  srv.Client(),
		Harness:     opencode.New("http://localhost:0", srv.Client(), "", ""),
	}
	s := New(cfg)

	const turnText = "I have found the root cause. The issue is in package X."
	s.notifyInvestigatorTurnEnd(turnText)

	// Allow async HTTP call to complete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(receivedBodies)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(receivedBodies) != 1 {
		t.Fatalf("want 1 delivery, got %d", len(receivedBodies))
	}
	body := receivedBodies[0]

	// The body is a JSON-encoded prompt_async payload; the text is inside.
	// Check that all required elements are present in the delivered payload.
	if !strings.Contains(body, "From investigator session: "+investigatorSession) {
		t.Errorf("notification body missing sender label; got: %s", body)
	}
	if !strings.Contains(body, turnText) {
		t.Errorf("notification body missing verbatim text block; got: %s", body)
	}
	if !strings.Contains(body, "prism prompt "+investigatorSession+" --prompt") {
		t.Errorf("notification body missing steering-channel hint; got: %s", body)
	}
}

// TestNotifyInvestigatorTurnEnd_EmptyText verifies that no notification is
// delivered when the text block is empty or whitespace-only.
func TestNotifyInvestigatorTurnEnd_EmptyText(t *testing.T) {
	d := openTestDB(t)
	repo := "nixos-config"
	invokerSession := repo + "@main"
	investigatorSession := invokerSession + "~investigate-testslug"

	var delivered int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			delivered++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	srvPort := parseSrvPort(t, srv.URL)
	seedSessionWithHarnessPort(t, d, invokerSession, repo, "sid-empty", srvPort)

	clk := newTestClock()
	cfg := Config{
		SessionName: investigatorSession,
		Repo:        repo,
		Worktree:    "/tmp/investigate-test-empty",
		DB:          d,
		Clock:       clk,
		HTTPClient:  srv.Client(),
		Harness:     opencode.New("http://localhost:0", srv.Client(), "", ""),
	}
	s := New(cfg)

	// Empty string — no delivery.
	s.notifyInvestigatorTurnEnd("")
	// Whitespace-only — no delivery.
	s.notifyInvestigatorTurnEnd("   \n\t  ")

	// Give any async delivery a chance (there should be none).
	time.Sleep(50 * time.Millisecond)

	if delivered > 0 {
		t.Errorf("expected no delivery for empty/whitespace text, got %d", delivered)
	}
}

// TestNotifyInvestigatorTurnEnd_InvokerEnded verifies that the notification is
// dropped silently (no panic, no delivery) when the invoker session has ended.
func TestNotifyInvestigatorTurnEnd_InvokerEnded(t *testing.T) {
	d := openTestDB(t)
	repo := "nixos-config"
	invokerSession := repo + "@main"
	investigatorSession := invokerSession + "~investigate-ended"

	var delivered int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			delivered++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	seedEndedSession(t, d, invokerSession, repo)

	clk := newTestClock()
	cfg := Config{
		SessionName: investigatorSession,
		Repo:        repo,
		Worktree:    "/tmp/investigate-test-ended",
		DB:          d,
		Clock:       clk,
		HTTPClient:  srv.Client(),
		Harness:     opencode.New("http://localhost:0", srv.Client(), "", ""),
	}
	s := New(cfg)

	s.notifyInvestigatorTurnEnd("some findings here")
	time.Sleep(50 * time.Millisecond)

	if delivered > 0 {
		t.Errorf("expected no delivery when invoker has ended, got %d", delivered)
	}
}

// TestNotifyInvestigatorTurnEnd_NotInvestigateSession verifies that a session
// NOT named with ~investigate does NOT trigger any delivery.
func TestNotifyInvestigatorTurnEnd_NotInvestigateSession(t *testing.T) {
	d := openTestDB(t)
	repo := "nixos-config"

	var delivered int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			delivered++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	clk := newTestClock()
	cfg := Config{
		// A plain worker session — no ~investigate in the name.
		SessionName: repo + "@feature",
		Repo:        repo,
		Worktree:    "/tmp/investigate-test-not-investigate",
		DB:          d,
		Clock:       clk,
		HTTPClient:  srv.Client(),
		Harness:     opencode.New("http://localhost:0", srv.Client(), "", ""),
	}
	s := New(cfg)

	s.notifyInvestigatorTurnEnd("some text that should not be delivered")
	time.Sleep(50 * time.Millisecond)

	if delivered > 0 {
		t.Errorf("expected no delivery for non-investigate session, got %d", delivered)
	}
}

// TestNotifyCoordinator_InvestigateAgentSuppressed verifies that an
// investigate-agent session does NOT emit a bare "has finished" notification
// to the coordinator.
func TestNotifyCoordinator_InvestigateAgentSuppressed(t *testing.T) {
	d := openTestDB(t)
	repo := "nixos-config"
	coordSID := "coord-sid-investigate-suppressed"

	var notifyCount int
	var notifyMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/session" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id":"` + coordSID + `"}]`))
			return
		}
		notifyMu.Lock()
		notifyCount++
		notifyMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	srvPort := parseSrvPort(t, srv.URL)
	seedCoordinatorWithPort(t, d, repo, srvPort, coordSID)

	clk := newTestClock()
	investigatorSession := repo + "@main~investigate-abc"
	cfg := Config{
		SessionName: investigatorSession,
		Repo:        repo,
		Worktree:    "/tmp/investigate-coord-suppressed",
		DB:          d,
		Clock:       clk,
		HTTPClient:  srv.Client(),
		Harness:     opencode.New("http://localhost:0", srv.Client(), "", ""),
	}
	s := New(cfg)

	// Directly call notifyCoordinator — it should be suppressed.
	s.notifyCoordinator()

	time.Sleep(100 * time.Millisecond)

	notifyMu.Lock()
	count := notifyCount
	notifyMu.Unlock()

	if count != 0 {
		t.Errorf("investigate-agent should not emit bare_finish notification; got %d notifications", count)
	}
}

// TestNotifyInvestigatorTurnEnd_ConcurrentDelivery verifies that two concurrent
// investigator sessions can deliver to the same invoker without mangling each
// other's sender labels.
func TestNotifyInvestigatorTurnEnd_ConcurrentDelivery(t *testing.T) {
	d := openTestDB(t)
	repo := "nixos-config"
	invokerSession := repo + "@main"
	investigator1 := invokerSession + "~investigate-alpha"
	investigator2 := invokerSession + "~investigate-beta"
	invokerSID := "invoker-sid-concurrent"

	var mu sync.Mutex
	var receivedBodies []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/session" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id":"` + invokerSID + `"}]`))
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/prompt_async") {
			body := make([]byte, 4096)
			n, _ := r.Body.Read(body)
			mu.Lock()
			receivedBodies = append(receivedBodies, string(body[:n]))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	srvPort := parseSrvPort(t, srv.URL)
	seedSessionWithHarnessPort(t, d, invokerSession, repo, invokerSID, srvPort)

	clk := newTestClock()
	cfg1 := Config{
		SessionName: investigator1,
		Repo:        repo,
		Worktree:    "/tmp/investigate-concurrent-1",
		DB:          d,
		Clock:       clk,
		HTTPClient:  srv.Client(),
		Harness:     opencode.New("http://localhost:0", srv.Client(), "", ""),
	}
	cfg2 := Config{
		SessionName: investigator2,
		Repo:        repo,
		Worktree:    "/tmp/investigate-concurrent-2",
		DB:          d,
		Clock:       clk,
		HTTPClient:  srv.Client(),
		Harness:     opencode.New("http://localhost:0", srv.Client(), "", ""),
	}
	s1 := New(cfg1)
	s2 := New(cfg2)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s1.notifyInvestigatorTurnEnd("alpha findings") }()
	go func() { defer wg.Done(); s2.notifyInvestigatorTurnEnd("beta findings") }()
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(receivedBodies)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	bodies := make([]string, len(receivedBodies))
	copy(bodies, receivedBodies)
	mu.Unlock()

	if len(bodies) != 2 {
		t.Fatalf("want 2 deliveries, got %d", len(bodies))
	}

	// Each body must contain the correct sender label.
	foundAlpha, foundBeta := false, false
	for _, body := range bodies {
		if strings.Contains(body, "From investigator session: "+investigator1) {
			foundAlpha = true
		}
		if strings.Contains(body, "From investigator session: "+investigator2) {
			foundBeta = true
		}
	}
	if !foundAlpha {
		t.Errorf("alpha investigator label not found in any delivery; bodies: %v", bodies)
	}
	if !foundBeta {
		t.Errorf("beta investigator label not found in any delivery; bodies: %v", bodies)
	}
}
