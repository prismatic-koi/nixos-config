// fixture_test.go relocates the converged mock-data fixture from the
// design-iteration spike landed for #2148 (the spike directory itself
// is deleted in this PR; the design it produced is the §3.1
// prescription in docs/multiplexer-proposal.md). Three repo clusters,
// one review group on the in-progress worker, a sampling of states
// across the spectrum so the golden tests exercise every glyph +
// colour cell in the §3.1 table.
//
// The original fixture targeted a standalone mock model. Here we build
// a pane.SessionTree (the post-#2163 production model) + a StateMap
// for the StateProvider, which together carry the same information.
package render

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/mux/pane"
)

// fixturePanes are the three §3.1 named panes that every non-review
// session in the fixture carries. Reviews have a single agent pane
// (mirroring the spike's reviewPanes).
var fixturePanes = []pane.Pane{{Name: "agent"}, {Name: "term"}, {Name: "edit"}}

var fixtureReviewPanes = []pane.Pane{{Name: "agent"}}

// buildFixtureTree constructs a fresh SessionTree populated with the
// spike's `Default` fixture. The session IDs follow prism's
// "<repo>@<branch>" convention so the renderer's name-stripping logic
// in sessionDisplayName matches the §3.1 examples ("@main",
// "@2141-mux-spike", …).
//
// Review subsessions follow the "<parent>~review-N-<agent>" convention
// the pane package's docstring documents.
func buildFixtureTree(t *testing.T) *pane.SessionTree {
	t.Helper()
	tree := pane.New()

	// --- nixos-config ---
	addSess := func(s pane.Session) {
		t.Helper()
		if err := tree.AddSession(s); err != nil {
			t.Fatalf("AddSession(%q): %v", s.ID, err)
		}
	}

	addSess(pane.Session{
		ID: "nixos-config@main", Repo: "nixos-config",
		Panes: fixturePanes,
	})
	addSess(pane.Session{
		ID: "nixos-config@2141-mux-spike", Repo: "nixos-config",
		Panes: fixturePanes,
	})
	// Five review subsessions under @2141-mux-spike.
	for _, agent := range []string{"code", "goal", "qa", "security", "context"} {
		addSess(pane.Session{
			ID:       "nixos-config@2141-mux-spike~review-1-review-" + agent,
			ParentID: "nixos-config@2141-mux-spike",
			Panes:    fixtureReviewPanes,
		})
	}
	addSess(pane.Session{
		ID: "nixos-config@degender-global-instructions", Repo: "nixos-config",
		Panes: fixturePanes,
	})
	addSess(pane.Session{
		ID: "nixos-config@battery-monitor-refactor", Repo: "nixos-config",
		Panes: fixturePanes,
	})
	addSess(pane.Session{
		ID: "nixos-config@stale-finished-session", Repo: "nixos-config",
		Panes: fixturePanes,
	})

	// --- home-ops ---
	addSess(pane.Session{
		ID: "home-ops@main", Repo: "home-ops",
		Panes: fixturePanes,
	})
	addSess(pane.Session{
		ID: "home-ops@plex-image-bump", Repo: "home-ops",
		Panes: fixturePanes,
	})

	// --- pi-extensions ---
	addSess(pane.Session{
		ID: "pi-extensions@main", Repo: "pi-extensions",
		Panes: fixturePanes,
	})

	return tree
}

// fixtureStates is the StateMap counterpart to buildFixtureTree. The
// state choices mirror the spike's `Default()` initial states — every
// glyph in the §3.1 table appears at least once, with the review group
// frozen at "active across the board" so the canonical-expanded golden
// shows the active glyph for all five reviewers.
func fixtureStates() StateMap {
	return StateMap{
		"nixos-config@main":                                    StateIdle,
		"nixos-config@2141-mux-spike":                          StateReviewing,
		"nixos-config@2141-mux-spike~review-1-review-code":     StateActive,
		"nixos-config@2141-mux-spike~review-1-review-goal":     StateActive,
		"nixos-config@2141-mux-spike~review-1-review-qa":       StateActive,
		"nixos-config@2141-mux-spike~review-1-review-security": StateEscalated,
		"nixos-config@2141-mux-spike~review-1-review-context":  StateActive,
		"nixos-config@degender-global-instructions":            StateActive,
		"nixos-config@battery-monitor-refactor":                StateWaiting,
		"nixos-config@stale-finished-session":                  StateFinished,
		"home-ops@main":                                        StateIdle,
		"home-ops@plex-image-bump":                             StateEscalated,
		"pi-extensions@main":                                   StateActive,
	}
}
