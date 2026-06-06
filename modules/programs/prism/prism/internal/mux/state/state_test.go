// state_test.go — unit tests for the Store half of internal/mux/state.
//
// The Subscriber's wire behaviour is exercised by integration_test.go via a
// fake sidecar; the tests here focus on the Store's contract (apply, listener
// firing order, snapshot copy semantics, no-op gating).

package state

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/mux/pane"
)

func TestStore_SetSessionState_Roundtrip(t *testing.T) {
	s := New(nil)
	s.SetSessionState("alpha", agent.StateActive)
	got, ok := s.SessionState("alpha")
	if !ok || got != agent.StateActive {
		t.Fatalf("SessionState(alpha) = (%q, %v), want (active, true)", got, ok)
	}
}

func TestStore_SetSessionState_EmptyClears(t *testing.T) {
	s := New(nil)
	s.SetSessionState("alpha", agent.StateActive)
	s.SetSessionState("alpha", "")
	if _, ok := s.SessionState("alpha"); ok {
		t.Fatalf("SessionState(alpha) still present after clear")
	}
}

func TestStore_Snapshot_IsCopy(t *testing.T) {
	s := New(nil)
	s.SetSessionState("alpha", agent.StateActive)
	snap := s.Snapshot()
	snap["alpha"] = agent.StateError
	got, _ := s.SessionState("alpha")
	if got != agent.StateActive {
		t.Fatalf("mutation of snapshot leaked into store: got %q, want active", got)
	}
}

func TestStore_ApplyEvent_StateChange(t *testing.T) {
	s := New(nil)
	evt := Event{
		Type:        "state_change",
		SessionName: "alpha",
		Payload:     mustJSON(t, map[string]string{"state": "waiting"}),
	}
	if !s.ApplyEvent(evt) {
		t.Fatalf("ApplyEvent returned false for first state change")
	}
	got, _ := s.SessionState("alpha")
	if got != agent.StateWaiting {
		t.Fatalf("after ApplyEvent: %q, want waiting", got)
	}
}

func TestStore_ApplyEvent_SnapshotEqualsChange(t *testing.T) {
	// The Store must treat state_snapshot the same way as state_change so
	// the reconnect-resync path lands on identical state without a
	// special branch.
	s := New(nil)
	evt := Event{
		Type:        "state_snapshot",
		SessionName: "alpha",
		Payload:     mustJSON(t, map[string]string{"state": "reviewing"}),
		Snapshot:    true,
	}
	if !s.ApplyEvent(evt) {
		t.Fatalf("ApplyEvent(state_snapshot) returned false")
	}
	got, _ := s.SessionState("alpha")
	if got != agent.StateReviewing {
		t.Fatalf("snapshot apply: got %q, want reviewing", got)
	}
}

func TestStore_ApplyEvent_DuplicateNoOp(t *testing.T) {
	s := New(nil)
	evt := Event{
		Type:        "state_change",
		SessionName: "alpha",
		Payload:     mustJSON(t, map[string]string{"state": "active"}),
	}
	if !s.ApplyEvent(evt) {
		t.Fatalf("first ApplyEvent returned false")
	}
	if s.ApplyEvent(evt) {
		t.Fatalf("duplicate ApplyEvent returned true, want false (no-op)")
	}
}

func TestStore_ApplyEvent_IgnoresUnknownType(t *testing.T) {
	// The package is intentionally narrow — non-state events flow through
	// other consumers. ApplyEvent must drop them rather than write
	// garbage into the Store.
	s := New(nil)
	evt := Event{
		Type:        "message_updated",
		SessionName: "alpha",
		Payload:     mustJSON(t, map[string]string{"role": "assistant"}),
	}
	if s.ApplyEvent(evt) {
		t.Fatalf("ApplyEvent(message_updated) returned true; should be no-op")
	}
	if _, ok := s.SessionState("alpha"); ok {
		t.Fatalf("Store gained an alpha entry from an unrelated event")
	}
}

func TestStore_ApplyEvent_IgnoresMalformedPayload(t *testing.T) {
	s := New(nil)
	evt := Event{
		Type:        "state_change",
		SessionName: "alpha",
		Payload:     json.RawMessage([]byte("not json")),
	}
	if s.ApplyEvent(evt) {
		t.Fatalf("ApplyEvent on malformed payload returned true")
	}
	if _, ok := s.SessionState("alpha"); ok {
		t.Fatalf("Store gained an alpha entry from a malformed event")
	}
}

func TestStore_Listener_FiresAfterWrite(t *testing.T) {
	s := New(nil)

	var (
		calls      atomic.Int32
		lastSeen   string
		lastSeenMu sync.Mutex
	)
	s.AddListener(func() {
		calls.Add(1)
		lastSeenMu.Lock()
		defer lastSeenMu.Unlock()
		// Reading from inside the listener proves the Store is
		// readable while a write is in flight (listeners fire outside
		// the lock).
		if v, ok := s.SessionState("alpha"); ok {
			lastSeen = string(v)
		}
	})

	s.SetSessionState("alpha", agent.StateActive)
	if got := calls.Load(); got != 1 {
		t.Fatalf("listener calls: got %d, want 1", got)
	}
	lastSeenMu.Lock()
	defer lastSeenMu.Unlock()
	if lastSeen != string(agent.StateActive) {
		t.Fatalf("listener saw %q, want active", lastSeen)
	}
}

func TestStore_SessionTree_Reference(t *testing.T) {
	tree := pane.New()
	s := New(tree)
	if s.SessionTree() != tree {
		t.Fatalf("Store did not retain the supplied SessionTree")
	}
	if New(nil).SessionTree() != nil {
		t.Fatalf("nil SessionTree should round-trip as nil")
	}
}

func TestStore_AllSidebarStates_RoundTrip(t *testing.T) {
	// AC: the Store must accept every state from prism's eight-state enum
	// — six "real" states from §3.1 plus the two transitional ones
	// (compacting, interrupted). Implicit error and deleted are also
	// produced by the sidecar; they pass through unchanged.
	s := New(nil)
	want := []agent.AgentState{
		agent.StateActive,
		agent.StateIdle,
		agent.StateWaiting,
		agent.StateReviewing,
		agent.StateEscalated,
		agent.StateFinished,
		agent.StateCompacting,
		agent.StateInterrupted,
		agent.StateError,
		agent.StateDeleted,
	}
	for i, st := range want {
		s.SetSessionState("alpha", st)
		got, ok := s.SessionState("alpha")
		if !ok || got != st {
			t.Fatalf("iteration %d: got (%q, %v), want (%q, true)", i, got, ok, st)
		}
	}
}

func TestBuildQuery_NoSessions_EmptyString(t *testing.T) {
	if got := buildQuery(nil); got != "" {
		t.Fatalf("buildQuery(nil) = %q, want empty (wildcard)", got)
	}
}

func TestBuildQuery_SkipsEmptyEntries(t *testing.T) {
	got := buildQuery([]string{"alpha", "", "beta"})
	// url.Values uses url.QueryEscape, which preserves alphanumerics
	// — so the order is deterministic and the empty value is skipped.
	want1 := "session=alpha&session=beta"
	want2 := "session=beta&session=alpha"
	if got != want1 && got != want2 {
		t.Fatalf("buildQuery = %q, want %q or %q", got, want1, want2)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
