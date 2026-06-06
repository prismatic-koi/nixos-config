package cmd

// Tests for the prism launch PRISM_USE_MUX=1 renderer wiring (#2176).
//
// These tests pin the state-adapter mapping (agent.AgentState →
// render.State) and the host-provider's empty-frame contract. Both
// are leaf wiring pieces that the runLaunchMux entry point depends
// on; getting them right pre-empts a class of "the renderer shows
// the wrong glyph" failures the soak would otherwise surface.

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/mux/pane"
	"github.com/prismatic-koi/prism/internal/mux/render"
	"github.com/prismatic-koi/prism/internal/mux/state"
)

// TestStateAdapter_MapsAgentStateToRenderState pins the canonical
// agent.AgentState → render.State translation table the launch
// renderer relies on. The agent vocabulary is wider than the
// renderer's (idle / compacting / error / interrupted / deleted all
// collapse to StateIdle for the sidebar glyph) so the mapping
// deserves an explicit table-driven test.
func TestStateAdapter_MapsAgentStateToRenderState(t *testing.T) {
	cases := []struct {
		in   agent.AgentState
		want render.State
	}{
		{agent.StateActive, render.StateActive},
		{agent.StateWaiting, render.StateWaiting},
		{agent.StateReviewing, render.StateReviewing},
		{agent.StateEscalated, render.StateEscalated},
		{agent.StateFinished, render.StateFinished},
		{agent.StateIdle, render.StateIdle},
		// The vocabulary outside the renderer's enum collapses to
		// idle. This is the desired UX — the sidebar glyph stays
		// neutral rather than showing an "unknown" character.
		{agent.StateCompacting, render.StateIdle},
		{agent.StateError, render.StateIdle},
		{agent.StateInterrupted, render.StateIdle},
		{agent.StateDeleted, render.StateIdle},
	}
	for _, tc := range cases {
		t.Run(string(tc.in), func(t *testing.T) {
			store := state.New(nil)
			store.SetSessionState("test", tc.in)
			a := newStateAdapter(store)
			got := a.State("test")
			if got != tc.want {
				t.Errorf("agent.AgentState(%q) → render.State = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestStateAdapter_UnknownSessionIsIdle pins the "no event yet"
// path: a session the Store has never seen reports StateIdle (the
// renderer's zero value) so the sidebar renders cleanly on first
// paint, before any agent_events row has flowed.
func TestStateAdapter_UnknownSessionIsIdle(t *testing.T) {
	store := state.New(nil)
	a := newStateAdapter(store)
	if got := a.State("never-seen"); got != render.StateIdle {
		t.Errorf("unknown session → %d, want StateIdle (%d)", got, render.StateIdle)
	}
}

// TestStateAdapter_NilSafe pins the defensive nil-check inside the
// adapter. The launch wire-up should never pass a nil store, but a
// future refactor that accidentally does should fail soft (every
// session reports idle) rather than panic.
func TestStateAdapter_NilSafe(t *testing.T) {
	var a *stateAdapter // nil receiver
	if got := a.State("anything"); got != render.StateIdle {
		t.Errorf("nil adapter → %d, want StateIdle (%d)", got, render.StateIdle)
	}
	a = newStateAdapter(nil)
	if got := a.State("anything"); got != render.StateIdle {
		t.Errorf("adapter with nil store → %d, want StateIdle (%d)", got, render.StateIdle)
	}
}

// TestClientHostProvider_HostKey_RoundTrips pins the (sessionID,
// paneName) ↔ cacheKey encoding. The NUL separator is documented to
// prevent collisions between a pane named "a/b" in session "x" and
// a pane named "b" in session "x/a"; this test enforces the round-
// trip so a future refactor that swaps NUL for a different
// separator does not silently re-introduce the collision class.
func TestClientHostProvider_HostKey_RoundTrips(t *testing.T) {
	cases := []struct {
		session, pane string
	}{
		{"repo@feat", "agent"},
		{"repo@feat", "edit"},
		{"repo@feat~review-1-code", "agent"},
		// Cases with slashes / colons / dashes in either field —
		// the NUL separator should make all of these unambiguous.
		{"repo/sub@feat", "agent:1"},
		{"x", "a/b"},
		{"x/a", "b"},
	}
	for _, tc := range cases {
		key := hostKey(tc.session, tc.pane)
		sess, name, ok := splitHostKey(key)
		if !ok {
			t.Errorf("splitHostKey(%q): ok=false; want true", key)
			continue
		}
		if sess != tc.session || name != tc.pane {
			t.Errorf("round-trip(%q,%q) = (%q,%q); want unchanged", tc.session, tc.pane, sess, name)
		}
	}
}

// TestClientHostProvider_HostKey_CollisionResistant pins the
// non-collision invariant for the case the NUL separator solves:
// session "x" + pane "a/b" must map to a different key than session
// "x/a" + pane "b".
func TestClientHostProvider_HostKey_CollisionResistant(t *testing.T) {
	a := hostKey("x", "a/b")
	b := hostKey("x/a", "b")
	if a == b {
		t.Errorf("hostKey collisions: hostKey(x, a/b) = hostKey(x/a, b) = %q", a)
	}
}

// TestClientHostProvider_Host_NilOnEmptyInput pins the defensive
// short-circuit for empty (sessionID, paneName). The renderer's
// renderActivePane already filters these out at the row-resolution
// step, but the provider should return nil rather than fabricating
// an empty host that would never be polled.
func TestClientHostProvider_Host_NilOnEmptyInput(t *testing.T) {
	p := newClientHostProvider(nil) // mc unused on the nil-arg branch
	if got := p.Host("", "agent"); got != nil {
		t.Errorf("Host(\"\", \"agent\") = %v, want nil", got)
	}
	if got := p.Host("repo@feat", ""); got != nil {
		t.Errorf("Host(\"repo@feat\", \"\") = %v, want nil", got)
	}
}

// Ensure the unused imports (pane / render) report no
// "imported and not used" errors at build time. The references
// here are no-ops at runtime but keep the test file honest about
// the type contracts it depends on.
var _ render.StateProvider = (*stateAdapter)(nil)
var _ = pane.New
