package dashboard_test

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/dashboard"
)

// TestFilterAgentSessions_AlwaysNonNil locks down the nil-vs-empty wire
// contract documented on Shared.ApplySessionsMsg: FilterAgentSessions must
// return a non-nil slice on every successful call, including the case where
// every input session is a meta session and the result is empty. See #1859.
func TestFilterAgentSessions_AlwaysNonNil(t *testing.T) {
	t.Run("nil input returns empty non-nil", func(t *testing.T) {
		out := dashboard.FilterAgentSessions(nil)
		if out == nil {
			t.Fatal("FilterAgentSessions(nil) returned nil; want empty non-nil slice")
		}
		if len(out) != 0 {
			t.Errorf("len = %d, want 0", len(out))
		}
	})

	t.Run("empty input returns empty non-nil", func(t *testing.T) {
		out := dashboard.FilterAgentSessions([]dashboard.AgentSession{})
		if out == nil {
			t.Fatal("FilterAgentSessions([]) returned nil; want empty non-nil slice")
		}
		if len(out) != 0 {
			t.Errorf("len = %d, want 0", len(out))
		}
	})

	t.Run("only meta sessions returns empty non-nil", func(t *testing.T) {
		// This is the regression case from #1859: every prism session was
		// cleaned up, only scratchpad and prism-dashboard remain. Before the
		// fix, FilterAgentSessions returned a nil slice here, which
		// ApplySessionsMsg then conflated with the DB-error signal and
		// preserved stale rows.
		in := []dashboard.AgentSession{
			{Name: "scratchpad"},
			{Name: "prism-dashboard"},
		}
		out := dashboard.FilterAgentSessions(in)
		if out == nil {
			t.Fatal("FilterAgentSessions(meta-only) returned nil; want empty non-nil slice (would cause ghost rows in dashboard)")
		}
		if len(out) != 0 {
			t.Errorf("len = %d, want 0 (meta sessions must be filtered out)", len(out))
		}
	})

	t.Run("mixed input filters meta and returns rest", func(t *testing.T) {
		in := []dashboard.AgentSession{
			{Name: "scratchpad"},
			{Name: "nixos-config@main"},
			{Name: "prism-dashboard"},
			{Name: "nixos-config@feature"},
		}
		out := dashboard.FilterAgentSessions(in)
		if out == nil {
			t.Fatal("returned nil")
		}
		if len(out) != 2 {
			t.Fatalf("len = %d, want 2", len(out))
		}
		if out[0].Name != "nixos-config@main" || out[1].Name != "nixos-config@feature" {
			t.Errorf("unexpected order/contents: %+v", out)
		}
	})
}

// TestApplySessionsMsg_ThreeStates exercises every signal the
// FetchSessionsFromDB → ApplySessionsMsg wire carries:
//
//   - (a) non-empty success: Sessions is a non-empty slice → list updates.
//   - (b) empty success:     Sessions is an empty non-nil slice → list clears.
//   - (c) DB error:          Sessions is nil                  → list preserved.
//
// This is the central regression test for #1859. Before the fix, state (b)
// and state (c) were indistinguishable because FilterAgentSessions returned a
// nil slice when zero non-meta sessions survived filtering — meaning empty
// success was treated as a DB error and the dashboard never updated to "no
// active sessions" after the last prism session was cleaned up.
func TestApplySessionsMsg_ThreeStates(t *testing.T) {
	seed := func() dashboard.Shared {
		// Build a Shared with two pre-existing sessions, as if a prior
		// successful fetch had populated the list.
		d := dashboard.Shared{
			Sessions: []dashboard.AgentSession{
				{Name: "nixos-config@main"},
				{Name: "nixos-config@feature"},
			},
		}
		return d
	}

	t.Run("a: non-empty success updates list", func(t *testing.T) {
		d := seed()
		fresh := []dashboard.AgentSession{
			{Name: "nixos-config@main"},
			{Name: "nixos-config@feature"},
			{Name: "nixos-config@bugfix"},
		}
		d2, _ := d.ApplySessionsMsg(dashboard.SessionsMsg{Sessions: fresh}, "")
		if len(d2.Sessions) != 3 {
			t.Fatalf("want 3 sessions after non-empty success, got %d", len(d2.Sessions))
		}
		if d2.Sessions[2].Name != "nixos-config@bugfix" {
			t.Errorf("new session not present: %+v", d2.Sessions)
		}
	})

	t.Run("b: empty success clears list", func(t *testing.T) {
		d := seed()
		// An empty-but-non-nil slice models a successful fetch that returned
		// zero non-meta sessions (e.g. every prism session was cleaned up,
		// only scratchpad and prism-dashboard remain).
		empty := []dashboard.AgentSession{}
		d2, _ := d.ApplySessionsMsg(dashboard.SessionsMsg{Sessions: empty}, "")
		if len(d2.Sessions) != 0 {
			t.Errorf("want 0 sessions after empty success (no ghost rows), got %d: %+v", len(d2.Sessions), d2.Sessions)
		}
		// Displayed must also reflect the empty list.
		if len(d2.Displayed) != 0 {
			t.Errorf("want Displayed empty after empty success, got %d: %+v", len(d2.Displayed), d2.Displayed)
		}
	})

	t.Run("c: DB error preserves list", func(t *testing.T) {
		d := seed()
		// A SessionsMsg with nil Sessions models a DB error in
		// FetchSessionsFromDB (openDB or AllActiveStatus failed).
		d2, _ := d.ApplySessionsMsg(dashboard.SessionsMsg{Sessions: nil}, "")
		if len(d2.Sessions) != 2 {
			t.Errorf("want 2 sessions preserved after DB error, got %d: %+v", len(d2.Sessions), d2.Sessions)
		}
	})
}
