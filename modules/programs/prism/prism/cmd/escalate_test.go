package cmd

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
)

// startEscalateHTTPStub spins up an httptest.Server that accepts the
// POST /session/<sid>/prompt_async call used by deliverViaHTTP. The handler
// records the last received request for the test to assert on.
type capturedPromptCall struct {
	body map[string]any
	url  string
}

func startEscalateHTTPStub(t *testing.T) (*httptest.Server, *capturedPromptCall) {
	t.Helper()
	captured := &capturedPromptCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(bodyBytes, &b)
		captured.body = b
		captured.url = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// TestEscalate_AutoDiscoversSingleCoordinator covers AC #1: auto-discovery of
// the same-repo default-branch coordinator and delivery as if it were a
// `prism prompt`.
func TestEscalate_AutoDiscoversSingleCoordinator(t *testing.T) {
	d := openPromptTestDB(t)

	srv, captured := startEscalateHTTPStub(t)
	port := extractTestServerPort(t, srv.URL)

	// Override httpClient to use a Unix-friendly net.Dialer (default is fine for httptest)
	// but force its timeout shorter so failed tests fail fast.
	httpClient = &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
	}}

	// Coordinator session in the same repo.
	seedSession(t, d, "repo@main", "active", intPtr(port), strPtr("oc-coord-sid"),
		strPtr("coordinator"), strPtr("anthropic/claude-sonnet-4.6"))
	// Worker session that is calling escalate.
	seedSession(t, d, "repo@feature", "active", nil, nil,
		strPtr("worker"), strPtr("anthropic/claude-sonnet-4.6"))

	if err := runEscalateForSession(d, "repo@feature", "", "I'm stuck — please advise."); err != nil {
		t.Fatalf("runEscalateForSession: %v", err)
	}

	// Calling session must now be in 'escalated' state.
	st, err := d.CurrentStatus("repo@feature")
	if err != nil || st == nil {
		t.Fatalf("CurrentStatus(repo@feature): err=%v st=%v", err, st)
	}
	if st.State != string(agent.StateEscalated) {
		t.Errorf("state = %q, want %q", st.State, agent.StateEscalated)
	}

	// Coordinator must have received the prompt.
	if captured.body == nil {
		t.Fatalf("coordinator received no prompt")
	}
	parts, ok := captured.body["parts"].([]any)
	if !ok || len(parts) == 0 {
		t.Fatalf("prompt body parts: got %v", captured.body["parts"])
	}
	if !strings.Contains(captured.url, "oc-coord-sid") {
		t.Errorf("URL = %q, want path containing %q", captured.url, "oc-coord-sid")
	}
}

// TestEscalate_StateTransitionEscalated covers AC #2: list-sessions sees
// "escalated" rather than active or finished after the call.
func TestEscalate_StateTransitionEscalated(t *testing.T) {
	d := openPromptTestDB(t)

	srv, _ := startEscalateHTTPStub(t)
	port := extractTestServerPort(t, srv.URL)

	httpClient = &http.Client{Timeout: 2 * time.Second}

	seedSession(t, d, "repo@main", "active", intPtr(port), strPtr("oc-sid-coord"),
		strPtr("coordinator"), nil)
	seedSession(t, d, "repo@feature", "active", nil, nil, strPtr("worker"), nil)

	if err := runEscalateForSession(d, "repo@feature", "", "help"); err != nil {
		t.Fatalf("runEscalateForSession: %v", err)
	}

	st, _ := d.CurrentStatus("repo@feature")
	if st.State != "escalated" {
		t.Errorf("state = %q, want %q", st.State, "escalated")
	}
}

// TestEscalate_BusEventDistinct covers ACs #4 and #5: the bus event has a new
// type session.escalated distinct from session.finished, and its payload
// carries the metadata fields.
func TestEscalate_BusEventDistinct(t *testing.T) {
	d := openPromptTestDB(t)

	srv, _ := startEscalateHTTPStub(t)
	port := extractTestServerPort(t, srv.URL)
	httpClient = &http.Client{Timeout: 2 * time.Second}

	seedSession(t, d, "repo@main", "active", intPtr(port), strPtr("oc-sid"), strPtr("coordinator"), nil)
	seedSession(t, d, "repo@feature", "active", nil, nil, strPtr("worker"), nil)

	if err := runEscalateForSession(d, "repo@feature", "", "stuck on review"); err != nil {
		t.Fatalf("runEscalateForSession: %v", err)
	}

	// session.escalated must exist for the source session.
	events, err := d.QueryEvents("repo@feature", 0, nil, nil, []string{"session.escalated"})
	if err != nil {
		t.Fatalf("QueryEvents(session.escalated): %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("session.escalated event count = %d, want 1", len(events))
	}

	// session.finished must NOT exist (escalation is the notification).
	finished, _ := d.QueryEvents("repo@feature", 0, nil, nil, []string{"session.finished"})
	if len(finished) != 0 {
		t.Errorf("session.finished event count = %d, want 0", len(finished))
	}

	// Payload contains the expected fields.
	var p map[string]any
	if err := json.Unmarshal([]byte(events[0].Payload), &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p["source"] != "repo@feature" {
		t.Errorf("payload.source = %v, want %q", p["source"], "repo@feature")
	}
	if p["target"] != "repo@main" {
		t.Errorf("payload.target = %v, want %q", p["target"], "repo@main")
	}
	if p["prompt"] != "stuck on review" {
		t.Errorf("payload.prompt = %v, want %q", p["prompt"], "stuck on review")
	}
	if _, ok := p["occurred_at"].(string); !ok {
		t.Errorf("payload.occurred_at missing or not string: %v", p["occurred_at"])
	}
}

// TestEscalate_MultipleCandidatesNoTo covers AC #7: multiple coordinator
// candidates and no --to → exits non-zero, state does NOT transition.
//
// Multiple candidates can arise when a pre-migration `<repo>@main` row exists
// (root_agent_name NULL) alongside a freshly-spawned coordinator in the same
// repo. The unique index permits that pairing because it filters
// `WHERE root_agent_name = 'coordinator'` only.
func TestEscalate_MultipleCandidatesNoTo(t *testing.T) {
	d := openPromptTestDB(t)

	httpClient = &http.Client{Timeout: 2 * time.Second}

	// Pre-migration legacy row: name matches <repo>@main, root_agent_name NULL.
	seedSession(t, d, "repo@main", "active", nil, nil, nil, nil)
	// Fresh coordinator with explicit root_agent_name='coordinator' on a
	// different branch. The unique index permits this because it filters on
	// root_agent_name='coordinator' only — the legacy row does not match.
	seedSession(t, d, "repo@coord-2", "active", nil, nil, strPtr("coordinator"), nil)
	seedSession(t, d, "repo@feature", "active", nil, nil, strPtr("worker"), nil)

	err := runEscalateForSession(d, "repo@feature", "", "halp")
	if err == nil {
		t.Fatalf("expected error for ambiguous coordinator, got nil")
	}
	if !strings.Contains(err.Error(), "multiple coordinator candidates") {
		t.Errorf("error message missing 'multiple coordinator candidates': %v", err)
	}
	// State must remain active — we did NOT transition.
	st, _ := d.CurrentStatus("repo@feature")
	if st.State != "active" {
		t.Errorf("state = %q, want %q (state must not transition on discovery error)", st.State, "active")
	}
}

// TestEscalate_NoCoordinatorStillEscalates covers AC #8: zero candidates →
// session still transitions to escalated, bus event still fires (target empty),
// and a self-marker is recorded in the worker's own log.
func TestEscalate_NoCoordinatorStillEscalates(t *testing.T) {
	d := openPromptTestDB(t)

	httpClient = &http.Client{Timeout: 2 * time.Second}

	// Only the worker — no coordinator anywhere in the repo.
	seedSession(t, d, "repo@feature", "active", nil, nil, strPtr("worker"), nil)

	if err := runEscalateForSession(d, "repo@feature", "", "lonely worker"); err != nil {
		t.Fatalf("runEscalateForSession: %v", err)
	}

	st, _ := d.CurrentStatus("repo@feature")
	if st.State != "escalated" {
		t.Errorf("state = %q, want %q", st.State, "escalated")
	}

	// session.escalated event must still fire.
	events, _ := d.QueryEvents("repo@feature", 0, nil, nil, []string{"session.escalated"})
	if len(events) != 1 {
		t.Fatalf("session.escalated event count = %d, want 1", len(events))
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(events[0].Payload), &p)
	if target, ok := p["target"].(string); ok && target != "" {
		t.Errorf("payload.target = %q, want \"\" (no coordinator)", target)
	}

	// A self-echo "escalation" event must exist so prism checkin shows it.
	echoes, _ := d.QueryEvents("repo@feature", 0, nil, nil, []string{"escalation"})
	if len(echoes) != 1 {
		t.Errorf("escalation echo event count = %d, want 1", len(echoes))
	}
}

// TestEscalate_ExplicitToMissingSession covers AC #9: --to <nonexistent> exits
// non-zero without transitioning state.
func TestEscalate_ExplicitToMissingSession(t *testing.T) {
	d := openPromptTestDB(t)

	seedSession(t, d, "repo@feature", "active", nil, nil, strPtr("worker"), nil)

	err := runEscalateForSession(d, "repo@feature", "ghost-session", "halp")
	if err == nil {
		t.Fatalf("expected error for missing --to target")
	}
	if !strings.Contains(err.Error(), "ghost-session") {
		t.Errorf("error message missing target name: %v", err)
	}

	st, _ := d.CurrentStatus("repo@feature")
	if st.State != "active" {
		t.Errorf("state = %q, want %q (must not transition on bad --to)", st.State, "active")
	}
}

// TestEscalate_PromptEchoedToSelf covers AC #10: the prompt body is recorded
// in the calling session's own conversation log.
func TestEscalate_PromptEchoedToSelf(t *testing.T) {
	d := openPromptTestDB(t)

	srv, _ := startEscalateHTTPStub(t)
	port := extractTestServerPort(t, srv.URL)
	httpClient = &http.Client{Timeout: 2 * time.Second}

	seedSession(t, d, "repo@main", "active", intPtr(port), strPtr("oc-sid"), strPtr("coordinator"), nil)
	seedSession(t, d, "repo@feature", "active", nil, nil, strPtr("worker"), nil)

	const body = "the question I need answered"
	if err := runEscalateForSession(d, "repo@feature", "", body); err != nil {
		t.Fatalf("runEscalateForSession: %v", err)
	}

	echoes, err := d.QueryEvents("repo@feature", 0, nil, nil, []string{"escalation"})
	if err != nil {
		t.Fatalf("QueryEvents(escalation): %v", err)
	}
	if len(echoes) != 1 {
		t.Fatalf("escalation echo count = %d, want 1", len(echoes))
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(echoes[0].Payload), &p)
	if got, _ := p["prompt"].(string); got != body {
		t.Errorf("echoed prompt = %q, want %q", got, body)
	}
}

// TestEscalate_ResolveTarget_OneCandidate exercises the resolver directly.
func TestEscalate_ResolveTarget_OneCandidate(t *testing.T) {
	d := openPromptTestDB(t)
	seedSession(t, d, "repo@main", "active", nil, nil, strPtr("coordinator"), nil)

	target, err := resolveEscalationTarget(d, "repo", "")
	if err != nil {
		t.Fatalf("resolveEscalationTarget: %v", err)
	}
	if target == nil || target.SessionName != "repo@main" {
		t.Errorf("target = %+v, want repo@main", target)
	}
}

// TestEscalate_ResolveTarget_LegacyAtMainRow exercises the discovery
// fallback for pre-migration rows where root_agent_name is NULL but the
// session is named <repo>@main.
func TestEscalate_ResolveTarget_LegacyAtMainRow(t *testing.T) {
	d := openPromptTestDB(t)
	// Note: seedSession passes nil rootAgent → root_agent_name stays NULL.
	seedSession(t, d, "repo@main", "active", nil, nil, nil, nil)

	target, err := resolveEscalationTarget(d, "repo", "")
	if err != nil {
		t.Fatalf("resolveEscalationTarget: %v", err)
	}
	if target == nil || target.SessionName != "repo@main" {
		t.Errorf("legacy fallback target = %+v, want repo@main", target)
	}
}

// TestEscalate_ResolveTarget_ExplicitTo exercises the --to override path.
func TestEscalate_ResolveTarget_ExplicitTo(t *testing.T) {
	d := openPromptTestDB(t)
	seedSession(t, d, "repo@main", "active", nil, nil, strPtr("coordinator"), nil)
	// Use a non-coordinator role for the second target so the unique index
	// (one coordinator per repo) is not violated. --to allows targeting any
	// session by name regardless of role.
	seedSession(t, d, "repo@coord-2", "active", nil, nil, strPtr("worker"), nil)

	target, err := resolveEscalationTarget(d, "repo", "repo@coord-2")
	if err != nil {
		t.Fatalf("resolveEscalationTarget(repo@coord-2): %v", err)
	}
	if target == nil || target.SessionName != "repo@coord-2" {
		t.Errorf("--to target = %+v, want repo@coord-2", target)
	}
}
