// Package model defines the mock session-tree data model used by the
// sidebar-spike. The shapes mirror the planned prism mux model
// (repo cluster → session → review subsessions) but contain only the
// fields the sidebar render layer needs to read.
//
// The real model (planned PR #2 of issue #2147) will live under
// internal/mux/pane/ and will carry the full pane lifecycle, focus
// state, and persistence schema. The shapes here are intentionally
// minimal — the spike validates rendering, not data design.
package model

import "time"

// State enumerates the prism session states the sidebar must visually
// distinguish. The set mirrors prism's actual agent-state vocabulary
// (active / idle / waiting / reviewing / escalated / finished) so the
// spike can be smoke-tested against intuitions a real prism user
// already has.
//
// Initial glyph + colour mappings live in internal/sidebar; both are
// revisable in iteration (see design-notes.md).
type State int

const (
	StateActive    State = iota // worker mid-turn
	StateIdle                   // no in-flight turn
	StateWaiting                // paused for user input (prism's `waiting` state)
	StateReviewing              // review group in progress
	StateEscalated              // escalated to coordinator / error
	StateFinished               // terminal — clean exit
)

// String returns the lowercase prism-state token. Used for tooltips,
// debug logging, and the label rendered next to the glyph.
func (s State) String() string {
	switch s {
	case StateActive:
		return "active"
	case StateIdle:
		return "idle"
	case StateWaiting:
		return "waiting"
	case StateReviewing:
		return "reviewing"
	case StateEscalated:
		return "escalated"
	case StateFinished:
		return "finished"
	}
	return "unknown"
}

// Pane is a named pane within a session. The MVP scope decision in
// issue #2147 names three: agent, term, edit (mirroring today's tmux
// layout). The set is open-ended in shape; this type is just a string
// alias to avoid premature enum-ification.
type Pane string

const (
	PaneAgent Pane = "agent"
	PaneTerm  Pane = "term"
	PaneEdit  Pane = "edit"
)

// Session is one prism session — coordinator, worker, or review
// subsession.
type Session struct {
	// Name is the session identifier as the user sees it
	// (e.g. "@main", "@2141-mux-spike", "~review-1-review-code").
	// The leading sigil distinguishes top-level sessions (`@`) from
	// review subsessions (`~`); the sidebar renders the sigil as-is.
	Name string

	// State drives glyph + colour selection.
	State State

	// Panes is the fixed ordered list of named panes for this session.
	// Review subsessions typically have a single `agent` pane; workers
	// and coordinators have the full agent/term/edit triple.
	Panes []Pane

	// ActivePane is the index into Panes of the currently-visible pane.
	// Tab / Shift-Tab cycles this in the inner ring (interaction model
	// in #2148).
	ActivePane int

	// Subsessions are review subsessions nested under a worker. The
	// sidebar renders them indented under their parent. Subsessions
	// never have their own subsessions in the MVP — the hierarchy is
	// exactly two levels deep.
	Subsessions []*Session

	// ExpandedReviews controls whether the sidebar shows this
	// session's review subsessions. Default is false to mirror
	// prism's existing convention (`prism sessions list` without
	// `--all` hides review subsessions; the dashboard does the
	// same). Left / Right on the session row toggles it.
	ExpandedReviews bool
}

// Repo is a repo cluster — a group of prism sessions that share a
// working repository.
type Repo struct {
	// Name is the bare repo name (e.g. "nixos-config", "home-ops").
	Name string

	// Sessions is the list of top-level sessions in this repo.
	// Review subsessions live under their parent session in
	// Session.Subsessions, not at this level.
	Sessions []*Session

	// Expanded controls whether the sidebar shows this repo's
	// children. Left / Right keystrokes toggle it.
	Expanded bool
}

// Tree is the full session-tree the sidebar renders. Ownership of the
// slice is the model's; the sidebar reads it but never mutates it
// directly. State changes flow through ApplyTransition.
type Tree struct {
	Repos []*Repo
}

// Transition is a scripted state change that the mock data driver
// applies to the tree over time. v1 uses these to demonstrate that the
// glyph + colour mapping animates in a way that feels right.
//
// The Target path is "repo/session" or "repo/session/subsession" — a
// slash-delimited address the driver resolves against the tree.
type Transition struct {
	At       time.Duration
	Target   string
	NewState State
}
