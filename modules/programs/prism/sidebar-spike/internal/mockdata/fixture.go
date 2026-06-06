// Package mockdata provides scripted fixtures for the sidebar-spike.
// Each fixture is a (Tree, Transitions) pair the spike's main loop
// drives: the tree is the initial state, the transitions are a
// scripted timeline of state changes the animation engine applies on
// a wall-clock tick.
//
// The fixture is the spike's only data source — there is no real
// agent_events subscription. The animation runs on a deterministic
// loop so Ben can observe glyph + colour transitions without having
// to drive the mock manually.
package mockdata

import (
	"time"

	"github.com/prismatic-koi/nixos-config/modules/programs/prism/sidebar-spike/internal/model"
)

// Fixture bundles an initial tree with the scripted transitions that
// drive the animation loop.
type Fixture struct {
	Name        string
	Tree        *model.Tree
	Transitions []model.Transition
	// LoopAfter is the duration after which the transition timeline
	// restarts from the beginning. Set to 0 to disable looping.
	LoopAfter time.Duration
}

// Default returns the v1 default fixture: three repos with a realistic
// mix of session states, plus scripted transitions that exercise every
// glyph and colour in the initial mapping.
//
// The shape of the data is designed to match the hierarchy ASCII
// example from issue #2147:
//
//	nixos-config
//	├─ ●  @main                      (coordinator)
//	├─ ●  @2141-mux-spike            (worker)
//	│  ├─ ⊙  ~review-1-review-code
//	│  └─ ⊙  ~review-1-review-goal
//	└─ ●  @degender-global-instructions
//	home-ops
//	└─ ●  @main
func Default() Fixture {
	allPanes := []model.Pane{model.PaneAgent, model.PaneTerm, model.PaneEdit}
	// Review subsessions only have a single agent pane.
	reviewPanes := []model.Pane{model.PaneAgent}

	tree := &model.Tree{
		Repos: []*model.Repo{
			{
				Name:     "nixos-config",
				Expanded: true,
				Sessions: []*model.Session{
					{
						Name:  "@main",
						State: model.StateIdle,
						Panes: allPanes,
					},
					{
						Name:  "@2141-mux-spike",
						State: model.StateReviewing,
						Panes: allPanes,
						Subsessions: []*model.Session{
							{Name: "~review-1-review-code", State: model.StateActive, Panes: reviewPanes},
							{Name: "~review-1-review-goal", State: model.StateActive, Panes: reviewPanes},
							{Name: "~review-1-review-qa", State: model.StateActive, Panes: reviewPanes},
							{Name: "~review-1-review-security", State: model.StateActive, Panes: reviewPanes},
							{Name: "~review-1-review-context", State: model.StateActive, Panes: reviewPanes},
						},
					},
					{
						Name:  "@degender-global-instructions",
						State: model.StateActive,
						Panes: allPanes,
					},
					{
						Name:  "@battery-monitor-refactor",
						State: model.StateWaiting,
						Panes: allPanes,
					},
					{
						Name:  "@stale-finished-session",
						State: model.StateFinished,
						Panes: allPanes,
					},
				},
			},
			{
				Name:     "home-ops",
				Expanded: true,
				Sessions: []*model.Session{
					{
						Name:  "@main",
						State: model.StateIdle,
						Panes: allPanes,
					},
					{
						Name:  "@plex-image-bump",
						State: model.StateEscalated,
						Panes: allPanes,
					},
				},
			},
			{
				Name:     "pi-extensions",
				Expanded: false,
				Sessions: []*model.Session{
					{
						Name:  "@main",
						State: model.StateActive,
						Panes: allPanes,
					},
				},
			},
		},
	}

	// Scripted timeline. Times are wall-clock from spike start.
	// Two distinct transition patterns per the AC in #2148:
	//
	//   • degender-global-instructions cycles
	//     active → reviewing → finished
	//   • plex-image-bump cycles
	//     escalated → active → escalated
	//
	// Plus a couple of mid-tier transitions on the review group to
	// demonstrate subsessions animating independently of their parent.
	transitions := []model.Transition{
		// degender pattern (3-step cycle)
		{At: 3 * time.Second, Target: "nixos-config/@degender-global-instructions", NewState: model.StateReviewing},
		{At: 8 * time.Second, Target: "nixos-config/@degender-global-instructions", NewState: model.StateFinished},
		{At: 14 * time.Second, Target: "nixos-config/@degender-global-instructions", NewState: model.StateActive},

		// plex pattern (escalation flip-flop)
		{At: 4 * time.Second, Target: "home-ops/@plex-image-bump", NewState: model.StateActive},
		{At: 10 * time.Second, Target: "home-ops/@plex-image-bump", NewState: model.StateEscalated},
		{At: 16 * time.Second, Target: "home-ops/@plex-image-bump", NewState: model.StateActive},

		// review-group subsession transitions — first reviewer finishes,
		// then the rest, then the parent worker leaves reviewing state.
		{At: 5 * time.Second, Target: "nixos-config/@2141-mux-spike/~review-1-review-code", NewState: model.StateFinished},
		{At: 7 * time.Second, Target: "nixos-config/@2141-mux-spike/~review-1-review-goal", NewState: model.StateFinished},
		{At: 9 * time.Second, Target: "nixos-config/@2141-mux-spike/~review-1-review-qa", NewState: model.StateFinished},
		{At: 11 * time.Second, Target: "nixos-config/@2141-mux-spike/~review-1-review-security", NewState: model.StateEscalated},
		{At: 13 * time.Second, Target: "nixos-config/@2141-mux-spike/~review-1-review-context", NewState: model.StateFinished},
		{At: 15 * time.Second, Target: "nixos-config/@2141-mux-spike", NewState: model.StateActive},
	}

	return Fixture{
		Name:        "default",
		Tree:        tree,
		Transitions: transitions,
		// Loop every 20s so Ben can keep observing without restarting.
		LoopAfter: 20 * time.Second,
	}
}

// Minimal returns a tiny fixture for quick visual checks — one repo,
// two sessions, no review subsessions, no transitions. Useful when
// debugging glyph rendering in isolation.
func Minimal() Fixture {
	tree := &model.Tree{
		Repos: []*model.Repo{
			{
				Name:     "scratch",
				Expanded: true,
				Sessions: []*model.Session{
					{Name: "@main", State: model.StateIdle, Panes: []model.Pane{model.PaneAgent, model.PaneTerm, model.PaneEdit}},
					{Name: "@hello", State: model.StateActive, Panes: []model.Pane{model.PaneAgent}},
				},
			},
		},
	}
	return Fixture{
		Name:        "minimal",
		Tree:        tree,
		Transitions: nil,
		LoopAfter:   0,
	}
}

// ByName returns the named fixture or Default() if name is empty /
// unknown.
func ByName(name string) Fixture {
	switch name {
	case "", "default":
		return Default()
	case "minimal":
		return Minimal()
	}
	return Default()
}
