// Package pane is the typed data model shared by every other layer of the
// prism-native multiplexer (server, render, state, persist). It carries the
// repo cluster → session → review subsession hierarchy codified in §3.1 of
// docs/multiplexer-proposal.md and the per-session flat pane model
// (N named panes, one visible at a time).
//
// Design notes:
//
//   - Pure data model. No I/O, no PTY interaction, no rendering. The package
//     does not import any other internal/* package — the four parallel
//     consumer packages (#2152 render, #2153 server, #2155 state, #2156
//     persist) all sit on top of this without cycles.
//
//   - Mutex-guarded reads and writes. The pattern mirrors
//     internal/mux/vt.Host — a single mutex serialises every state
//     transition and every read so the tree never observes a partially
//     applied write. Readers get deep-copied snapshots; the package never
//     hands out a pointer into its own state.
//
//   - JSON round-trip is a first-class operation. The snapshot/restore layer
//     in PR #2156 uses encoding/json directly on *SessionTree —
//     MarshalJSON serialises the inner state, UnmarshalJSON revalidates
//     every invariant so a tampered or malformed blob fails closed instead
//     of leaving the tree partially populated.
//
//   - No panic / log.Fatal. Every failure path returns a wrapped sentinel
//     error so the server layer (#2153) can surface it to the CLI client
//     without unwinding the goroutine.
package pane

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Sentinel errors. Callers compare with errors.Is so the server can map
// specific failure modes to specific socket-protocol responses without
// string-matching.
var (
	// ErrSessionExists is returned by AddSession when the provided ID is
	// already registered in the tree.
	ErrSessionExists = errors.New("pane: session already exists")

	// ErrSessionNotFound is returned by any operation that takes a session
	// ID for a session that is not in the tree.
	ErrSessionNotFound = errors.New("pane: session not found")

	// ErrParentNotFound is returned by AddSession when ParentID is set but
	// the parent is not in the tree.
	ErrParentNotFound = errors.New("pane: parent session not found")

	// ErrParentIsReview is returned by AddSession when ParentID points at
	// a session that is itself a review subsession — review subs are
	// exactly two levels deep per the §3.1 invariant.
	ErrParentIsReview = errors.New("pane: review subsession cannot have children")

	// ErrInvalidSession is returned by AddSession when the Session struct
	// fails the basic schema check (empty ID, empty Repo on a top-level
	// session, etc.).
	ErrInvalidSession = errors.New("pane: invalid session")

	// ErrPaneExists is returned by AddPane when a pane with the given
	// name already exists on the target session.
	ErrPaneExists = errors.New("pane: pane already exists in session")

	// ErrPaneNotFound is returned by any operation that takes a pane name
	// for a pane that is not in the session.
	ErrPaneNotFound = errors.New("pane: pane not found in session")

	// ErrNoPanes is returned by NextPane / PrevPane when the session has
	// no panes to cycle through.
	ErrNoPanes = errors.New("pane: session has no panes")

	// ErrInconsistent is returned by UnmarshalJSON when the on-disk state
	// fails one of the tree invariants. The wrapped error names which
	// invariant tripped.
	ErrInconsistent = errors.New("pane: inconsistent tree state")
)

// Pane is one named pane within a session. Name is the only first-class
// attribute — keeping it a string (rather than an enum) lets the §3.1
// initial set of "agent" / "term" / "edit" grow without a model change.
//
// The package does NOT attribute any meaning to particular names; the
// renderer (#2152) and server (#2153) decide what "agent" or "term" do.
type Pane struct {
	// Name is the pane's identifier within its session. Must be non-empty
	// and unique within the owning session — AddPane enforces both.
	Name string `json:"name"`
}

// Session represents one prism session — either a top-level worker /
// coordinator session (ParentID == "") or a review subsession
// (ParentID set to its parent's ID).
//
// All fields except ID and Repo are advisory carry-through for the
// consumers; the model does not act on them. JSON tags use omitempty so
// snapshots are compact and review subsessions (which inherit most fields
// from their parent and typically leave them empty) round-trip without
// noise.
type Session struct {
	// ID is the session's globally unique identifier. Prism's existing
	// naming convention is "<repo>@<branch>" for top-level sessions and
	// "<parent>~review-<N>-<agent>" for review subsessions, but the model
	// only requires the string be non-empty and unique within the tree —
	// it does not parse or interpret the structure.
	ID string `json:"id"`

	// Repo is the cluster name (e.g. "nixos-config"). Top-level sessions
	// MUST set this; review subsessions inherit their parent's Repo and
	// MAY leave it empty on input (AddSession backfills from the parent).
	Repo string `json:"repo,omitempty"`

	// Branch is the git branch the session's worktree is checked out at.
	// Carry-through for consumers; never inspected by the model.
	Branch string `json:"branch,omitempty"`

	// Worktree is the absolute path to the git worktree. Review
	// subsessions share their parent's worktree and typically leave this
	// empty.
	Worktree string `json:"worktree,omitempty"`

	// AgentRole is the prism agent role string (e.g. "worker",
	// "coordinator", "review-code"). Carry-through.
	AgentRole string `json:"agent_role,omitempty"`

	// SidecarAddr is the address of the sidecar host-API socket that
	// serves this session. Carry-through.
	SidecarAddr string `json:"sidecar_addr,omitempty"`

	// ParentID is empty for top-level sessions and set to the parent's ID
	// for review subsessions. The §3.1 invariant is that review
	// subsessions are exactly two levels deep and never have children of
	// their own — AddSession enforces this.
	ParentID string `json:"parent_id,omitempty"`

	// Panes is the ordered list of panes in this session. The order is
	// the AddPane insertion order, which is the order NextPane / PrevPane
	// cycle through.
	Panes []Pane `json:"panes,omitempty"`

	// ActivePane is the name of the currently visible pane, or "" if the
	// session has no panes. Always refers to a pane in Panes or is empty.
	ActivePane string `json:"active_pane,omitempty"`
}

// IsReview reports whether this session is a review subsession.
func (s Session) IsReview() bool { return s.ParentID != "" }

// clone returns a deep copy of the session — used by Snapshot and the
// per-session accessors so callers can mutate the returned value without
// racing against the tree.
func (s Session) clone() Session {
	out := s
	if len(s.Panes) > 0 {
		out.Panes = make([]Pane, len(s.Panes))
		copy(out.Panes, s.Panes)
	}
	return out
}

// SessionTree is the concurrent-safe data model. Every method acquires the
// internal mutex; callers never need to lock externally.
//
// The zero value is NOT ready for use — construct with New. The zero value
// of treeState contains nil maps, and operations would panic on map writes;
// the constructor exists to keep that failure mode out of the API surface.
type SessionTree struct {
	mu    sync.RWMutex
	state treeState
}

// treeState is the inner, JSON-serialisable shape. It is deliberately not
// exported so callers cannot bypass the SessionTree mutex to mutate it
// directly. JSON ser/de routes through *SessionTree.MarshalJSON /
// UnmarshalJSON, which acquire the lock and (on unmarshal) revalidate.
type treeState struct {
	// Sessions is the canonical store: every session in the tree, keyed
	// by its ID.
	Sessions map[string]*Session `json:"sessions"`

	// RepoOrder is the display order of repo clusters — the order they
	// appear in the sidebar's §3.1 layout. Repos are added the first
	// time a top-level session in that repo is added and removed when
	// the last top-level session in that repo is removed.
	RepoOrder []string `json:"repo_order"`

	// SessionOrder maps a repo cluster name to the ordered list of
	// top-level session IDs in that cluster. The order matches the
	// AddSession insertion order.
	SessionOrder map[string][]string `json:"session_order"`

	// ChildOrder maps a top-level session ID to the ordered list of its
	// review subsession IDs. The order matches the AddSession insertion
	// order.
	ChildOrder map[string][]string `json:"child_order"`

	// ActiveSession is the tree-level focus pointer — the session
	// currently selected in the sidebar. Either references a session in
	// Sessions or is empty.
	ActiveSession string `json:"active_session,omitempty"`
}

// New returns a freshly initialised, empty SessionTree.
func New() *SessionTree {
	return &SessionTree{state: newTreeState()}
}

func newTreeState() treeState {
	return treeState{
		Sessions:     make(map[string]*Session),
		RepoOrder:    []string{},
		SessionOrder: make(map[string][]string),
		ChildOrder:   make(map[string][]string),
	}
}

// ---------------------------------------------------------------------------
// Mutating operations
// ---------------------------------------------------------------------------

// AddSession inserts a session into the tree.
//
// For a top-level session: s.ID and s.Repo MUST be non-empty; s.ParentID
// MUST be empty. The repo is added to RepoOrder if this is the first
// session in it; otherwise the session is appended to the repo's existing
// SessionOrder slice.
//
// For a review subsession: s.ID and s.ParentID MUST be non-empty. The
// parent MUST exist and MUST NOT itself be a review subsession (the §3.1
// "exactly two levels" invariant). s.Repo is backfilled from the parent if
// left empty on input; if set, it MUST match the parent's Repo or
// ErrInvalidSession is returned.
//
// s.Panes is preserved as-is; if s.ActivePane is non-empty it MUST name a
// pane in s.Panes, or ErrInvalidSession is returned. If s.ActivePane is
// empty and s.Panes is non-empty, the first pane is auto-selected as
// active so callers do not have to write the same two lines after every
// AddSession.
//
// Returns ErrSessionExists if s.ID is already in the tree.
func (t *SessionTree) AddSession(s Session) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if s.ID == "" {
		return fmt.Errorf("%w: empty ID", ErrInvalidSession)
	}
	if _, exists := t.state.Sessions[s.ID]; exists {
		return fmt.Errorf("%w: %q", ErrSessionExists, s.ID)
	}
	if s.ParentID == "" {
		// Top-level session.
		if s.Repo == "" {
			return fmt.Errorf("%w: top-level session %q has empty Repo", ErrInvalidSession, s.ID)
		}
	} else {
		// Review subsession.
		parent, ok := t.state.Sessions[s.ParentID]
		if !ok {
			return fmt.Errorf("%w: %q", ErrParentNotFound, s.ParentID)
		}
		if parent.IsReview() {
			return fmt.Errorf("%w: parent %q is itself a review subsession", ErrParentIsReview, s.ParentID)
		}
		switch {
		case s.Repo == "":
			s.Repo = parent.Repo
		case s.Repo != parent.Repo:
			return fmt.Errorf("%w: review subsession %q has Repo %q but parent has Repo %q",
				ErrInvalidSession, s.ID, s.Repo, parent.Repo)
		}
	}

	// Validate the pane set: unique names, and ActivePane (if set) must
	// resolve to a pane in the slice.
	seen := make(map[string]struct{}, len(s.Panes))
	for i, p := range s.Panes {
		if p.Name == "" {
			return fmt.Errorf("%w: pane %d has empty name", ErrInvalidSession, i)
		}
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("%w: duplicate pane name %q", ErrInvalidSession, p.Name)
		}
		seen[p.Name] = struct{}{}
	}
	if s.ActivePane != "" {
		if _, ok := seen[s.ActivePane]; !ok {
			return fmt.Errorf("%w: ActivePane %q is not in Panes", ErrInvalidSession, s.ActivePane)
		}
	} else if len(s.Panes) > 0 {
		s.ActivePane = s.Panes[0].Name
	}

	// Defensive copy so callers cannot mutate the slice we now own.
	copied := s.clone()
	t.state.Sessions[s.ID] = &copied

	if s.ParentID == "" {
		if _, ok := t.state.SessionOrder[s.Repo]; !ok {
			t.state.RepoOrder = append(t.state.RepoOrder, s.Repo)
		}
		t.state.SessionOrder[s.Repo] = append(t.state.SessionOrder[s.Repo], s.ID)
	} else {
		t.state.ChildOrder[s.ParentID] = append(t.state.ChildOrder[s.ParentID], s.ID)
	}
	return nil
}

// RemoveSession deletes a session from the tree.
//
// If the session is top-level, all of its review subsessions are removed
// as well (the §3.1 invariant: review subs cannot exist without a parent).
// If removing the last top-level session in a repo cluster, the cluster is
// also dropped from RepoOrder.
//
// If the removed session (or one of its cascaded review subs) was the
// tree's ActiveSession, ActiveSession is cleared.
//
// Returns ErrSessionNotFound if id is not in the tree.
func (t *SessionTree) RemoveSession(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.removeSessionLocked(id)
}

func (t *SessionTree) removeSessionLocked(id string) error {
	s, ok := t.state.Sessions[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}

	if !s.IsReview() {
		// Cascade-remove every review subsession first.
		for _, childID := range t.state.ChildOrder[id] {
			delete(t.state.Sessions, childID)
			if t.state.ActiveSession == childID {
				t.state.ActiveSession = ""
			}
		}
		delete(t.state.ChildOrder, id)

		// Remove from the repo's SessionOrder.
		repo := s.Repo
		order := t.state.SessionOrder[repo]
		t.state.SessionOrder[repo] = removeString(order, id)
		if len(t.state.SessionOrder[repo]) == 0 {
			delete(t.state.SessionOrder, repo)
			t.state.RepoOrder = removeString(t.state.RepoOrder, repo)
		}
	} else {
		// Review subsession — just unhook from the parent's child order.
		parentID := s.ParentID
		t.state.ChildOrder[parentID] = removeString(t.state.ChildOrder[parentID], id)
		if len(t.state.ChildOrder[parentID]) == 0 {
			delete(t.state.ChildOrder, parentID)
		}
	}

	delete(t.state.Sessions, id)
	if t.state.ActiveSession == id {
		t.state.ActiveSession = ""
	}
	return nil
}

// AddPane appends a pane to a session. Pane names must be unique within
// the session. If the session previously had no panes, the new pane is
// auto-activated.
//
// Returns ErrSessionNotFound if sessionID is unknown, ErrPaneExists if a
// pane with the same name already exists in the session, or
// ErrInvalidSession if p.Name is empty.
func (t *SessionTree) AddPane(sessionID string, p Pane) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if p.Name == "" {
		return fmt.Errorf("%w: empty pane name", ErrInvalidSession)
	}
	s, ok := t.state.Sessions[sessionID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}
	for _, existing := range s.Panes {
		if existing.Name == p.Name {
			return fmt.Errorf("%w: session %q already has pane %q",
				ErrPaneExists, sessionID, p.Name)
		}
	}
	s.Panes = append(s.Panes, p)
	if s.ActivePane == "" {
		s.ActivePane = p.Name
	}
	return nil
}

// RemovePane deletes a pane from a session. If the removed pane was the
// session's ActivePane, ActivePane is set to whichever pane now occupies
// the same position in the slice (or the new last pane, if the removed
// one was last), or "" if the session is now empty.
//
// Returns ErrSessionNotFound or ErrPaneNotFound on lookup failures.
func (t *SessionTree) RemovePane(sessionID, paneName string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	s, ok := t.state.Sessions[sessionID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}
	idx := -1
	for i, existing := range s.Panes {
		if existing.Name == paneName {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: session %q has no pane %q",
			ErrPaneNotFound, sessionID, paneName)
	}
	s.Panes = append(s.Panes[:idx], s.Panes[idx+1:]...)
	if s.ActivePane == paneName {
		switch {
		case len(s.Panes) == 0:
			s.ActivePane = ""
		case idx < len(s.Panes):
			// Pane that took the removed pane's index becomes active.
			s.ActivePane = s.Panes[idx].Name
		default:
			// Removed the trailing pane — fall back to the new last.
			s.ActivePane = s.Panes[len(s.Panes)-1].Name
		}
	}
	return nil
}

// ActivatePane sets the session's ActivePane. The pane MUST exist in the
// session, or ErrPaneNotFound is returned.
func (t *SessionTree) ActivatePane(sessionID, paneName string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	s, ok := t.state.Sessions[sessionID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}
	for _, existing := range s.Panes {
		if existing.Name == paneName {
			s.ActivePane = paneName
			return nil
		}
	}
	return fmt.Errorf("%w: session %q has no pane %q",
		ErrPaneNotFound, sessionID, paneName)
}

// ActivateSession sets the tree-level ActiveSession pointer. The session
// MUST exist, or ErrSessionNotFound is returned. Passing "" clears the
// pointer — useful for "nothing focused" states.
func (t *SessionTree) ActivateSession(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if id == "" {
		t.state.ActiveSession = ""
		return nil
	}
	if _, ok := t.state.Sessions[id]; !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	t.state.ActiveSession = id
	return nil
}

// NextPane cycles the session's ActivePane forward by one position
// (wrapping at the end of Panes) and returns the new ActivePane name.
// Returns ErrSessionNotFound if sessionID is unknown, or ErrNoPanes if
// the session has no panes to cycle.
//
// If the session has panes but no ActivePane (e.g. an externally
// constructed Session with ActivePane left empty), the first pane is
// selected.
func (t *SessionTree) NextPane(sessionID string) (string, error) {
	return t.cyclePane(sessionID, +1)
}

// PrevPane cycles the session's ActivePane backward by one position
// (wrapping at the start of Panes) and returns the new ActivePane name.
// Returns ErrSessionNotFound if sessionID is unknown, or ErrNoPanes if
// the session has no panes to cycle.
func (t *SessionTree) PrevPane(sessionID string) (string, error) {
	return t.cyclePane(sessionID, -1)
}

func (t *SessionTree) cyclePane(sessionID string, delta int) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s, ok := t.state.Sessions[sessionID]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}
	if len(s.Panes) == 0 {
		return "", fmt.Errorf("%w: session %q", ErrNoPanes, sessionID)
	}

	idx := 0
	if s.ActivePane != "" {
		for i, p := range s.Panes {
			if p.Name == s.ActivePane {
				idx = i
				break
			}
		}
		n := len(s.Panes)
		// (idx + delta + n) % n keeps the result non-negative for delta = -1.
		idx = ((idx+delta)%n + n) % n
	}
	s.ActivePane = s.Panes[idx].Name
	return s.ActivePane, nil
}

// ---------------------------------------------------------------------------
// Read accessors — all return deep copies so callers cannot race the tree.
// ---------------------------------------------------------------------------

// Session returns a deep copy of the named session and true if it exists,
// or the zero Session and false otherwise.
func (t *SessionTree) Session(id string) (Session, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.state.Sessions[id]
	if !ok {
		return Session{}, false
	}
	return s.clone(), true
}

// HasSession reports whether a session with the given ID is in the tree.
// Cheaper than Session when callers only need an existence check.
func (t *SessionTree) HasSession(id string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.state.Sessions[id]
	return ok
}

// Sessions returns deep copies of every session in the tree. Order is
// repo-cluster-major (matching RepoOrder), then top-level session within
// each cluster (matching SessionOrder), then review subsession within
// each top-level (matching ChildOrder). This is the natural iteration
// order for the sidebar renderer in #2152.
func (t *SessionTree) Sessions() []Session {
	t.mu.RLock()
	defer t.mu.RUnlock()

	out := make([]Session, 0, len(t.state.Sessions))
	for _, repo := range t.state.RepoOrder {
		for _, id := range t.state.SessionOrder[repo] {
			s, ok := t.state.Sessions[id]
			if !ok {
				continue
			}
			out = append(out, s.clone())
			for _, childID := range t.state.ChildOrder[id] {
				if child, ok := t.state.Sessions[childID]; ok {
					out = append(out, child.clone())
				}
			}
		}
	}
	return out
}

// Repos returns the repo cluster names in display order.
func (t *SessionTree) Repos() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, len(t.state.RepoOrder))
	copy(out, t.state.RepoOrder)
	return out
}

// RepoSessions returns the top-level session IDs in the named repo
// cluster, in display order. Returns nil if the repo is not known.
func (t *SessionTree) RepoSessions(repo string) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ids := t.state.SessionOrder[repo]
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	copy(out, ids)
	return out
}

// Children returns the review subsession IDs of the given top-level
// session in display order. Returns nil if the parent is not known or has
// no review subsessions.
func (t *SessionTree) Children(parentID string) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ids := t.state.ChildOrder[parentID]
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	copy(out, ids)
	return out
}

// ActiveSessionID returns the tree-level ActiveSession pointer, or "" if
// nothing is focused.
func (t *SessionTree) ActiveSessionID() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state.ActiveSession
}

// ActivePaneName returns the named session's ActivePane and true if the
// session exists, or "" and false otherwise. The empty string is
// ambiguous on its own ("no active" vs "no session") — pair the bool with
// the string to distinguish.
func (t *SessionTree) ActivePaneName(sessionID string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.state.Sessions[sessionID]
	if !ok {
		return "", false
	}
	return s.ActivePane, true
}

// Len returns the total number of sessions in the tree (top-level +
// review subsessions). Cheap snapshot for the §3.1 sidebar header's
// `prism · N sessions` count — note though that the §3.1 header counts
// only top-level sessions; consumers wanting that figure should walk
// Repos() and sum RepoSessions().
func (t *SessionTree) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.state.Sessions)
}

// ---------------------------------------------------------------------------
// JSON serialisation — first-class for the snapshot/restore layer in #2156.
// ---------------------------------------------------------------------------

// MarshalJSON implements json.Marshaler. The encoded form is the inner
// treeState, fully sufficient to reconstruct the tree via UnmarshalJSON.
func (t *SessionTree) MarshalJSON() ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return json.Marshal(t.state)
}

// UnmarshalJSON implements json.Unmarshaler. The decoded state is
// revalidated against every tree invariant — a malformed or tampered blob
// fails closed with ErrInconsistent rather than silently producing a
// half-built tree.
//
// Empty / missing maps in the source JSON are accepted; nil maps are
// re-initialised to empty so subsequent writes do not panic.
func (t *SessionTree) UnmarshalJSON(data []byte) error {
	var s treeState
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s.Sessions == nil {
		s.Sessions = make(map[string]*Session)
	}
	if s.RepoOrder == nil {
		s.RepoOrder = []string{}
	}
	if s.SessionOrder == nil {
		s.SessionOrder = make(map[string][]string)
	}
	if s.ChildOrder == nil {
		s.ChildOrder = make(map[string][]string)
	}
	if err := validate(&s); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = s
	return nil
}

// validate runs every invariant check on a treeState. Used by
// UnmarshalJSON; also exported via the public Validate method below so
// tests and consumers can spot-check trees they have built up through
// the API.
func validate(s *treeState) error {
	// 1. Every entry in SessionOrder / ChildOrder references a session
	//    that exists in Sessions.
	for repo, ids := range s.SessionOrder {
		for _, id := range ids {
			sess, ok := s.Sessions[id]
			if !ok {
				return fmt.Errorf("%w: SessionOrder[%q] references unknown session %q",
					ErrInconsistent, repo, id)
			}
			if sess.IsReview() {
				return fmt.Errorf("%w: SessionOrder[%q] contains review subsession %q (must be top-level)",
					ErrInconsistent, repo, id)
			}
			if sess.Repo != repo {
				return fmt.Errorf("%w: session %q has Repo %q but is listed under SessionOrder[%q]",
					ErrInconsistent, id, sess.Repo, repo)
			}
		}
	}
	for parentID, ids := range s.ChildOrder {
		parent, ok := s.Sessions[parentID]
		if !ok {
			return fmt.Errorf("%w: ChildOrder references unknown parent %q",
				ErrInconsistent, parentID)
		}
		if parent.IsReview() {
			return fmt.Errorf("%w: ChildOrder[%q] — parent is itself a review subsession",
				ErrInconsistent, parentID)
		}
		for _, id := range ids {
			child, ok := s.Sessions[id]
			if !ok {
				return fmt.Errorf("%w: ChildOrder[%q] references unknown child %q",
					ErrInconsistent, parentID, id)
			}
			if child.ParentID != parentID {
				return fmt.Errorf("%w: child %q has ParentID %q but is listed under ChildOrder[%q]",
					ErrInconsistent, id, child.ParentID, parentID)
			}
		}
	}

	// 2. Every session in Sessions is reachable via either SessionOrder
	//    (top-level) or ChildOrder (review subs) — no orphans.
	reachable := make(map[string]struct{}, len(s.Sessions))
	for _, ids := range s.SessionOrder {
		for _, id := range ids {
			reachable[id] = struct{}{}
		}
	}
	for _, ids := range s.ChildOrder {
		for _, id := range ids {
			reachable[id] = struct{}{}
		}
	}
	for id, sess := range s.Sessions {
		if _, ok := reachable[id]; !ok {
			return fmt.Errorf("%w: session %q is orphaned (not in SessionOrder or ChildOrder)",
				ErrInconsistent, id)
		}
		if sess.IsReview() {
			// Review subs must point at an existing top-level parent.
			parent, ok := s.Sessions[sess.ParentID]
			if !ok {
				return fmt.Errorf("%w: review subsession %q references missing parent %q",
					ErrInconsistent, id, sess.ParentID)
			}
			if parent.IsReview() {
				return fmt.Errorf("%w: review subsession %q has review-subsession parent %q",
					ErrInconsistent, id, sess.ParentID)
			}
		} else {
			if sess.Repo == "" {
				return fmt.Errorf("%w: top-level session %q has empty Repo",
					ErrInconsistent, id)
			}
		}
		// 3. Pane invariants: unique names, ActivePane present in Panes
		//    if non-empty.
		seen := make(map[string]struct{}, len(sess.Panes))
		for _, p := range sess.Panes {
			if p.Name == "" {
				return fmt.Errorf("%w: session %q has a pane with empty name",
					ErrInconsistent, id)
			}
			if _, dup := seen[p.Name]; dup {
				return fmt.Errorf("%w: session %q has duplicate pane %q",
					ErrInconsistent, id, p.Name)
			}
			seen[p.Name] = struct{}{}
		}
		if sess.ActivePane != "" {
			if _, ok := seen[sess.ActivePane]; !ok {
				return fmt.Errorf("%w: session %q has ActivePane %q not in Panes",
					ErrInconsistent, id, sess.ActivePane)
			}
		}
	}

	// 4. RepoOrder is exactly the key set of SessionOrder.
	if len(s.RepoOrder) != len(s.SessionOrder) {
		return fmt.Errorf("%w: RepoOrder length %d != SessionOrder size %d",
			ErrInconsistent, len(s.RepoOrder), len(s.SessionOrder))
	}
	seenRepo := make(map[string]struct{}, len(s.RepoOrder))
	for _, repo := range s.RepoOrder {
		if _, dup := seenRepo[repo]; dup {
			return fmt.Errorf("%w: RepoOrder contains duplicate %q",
				ErrInconsistent, repo)
		}
		seenRepo[repo] = struct{}{}
		if _, ok := s.SessionOrder[repo]; !ok {
			return fmt.Errorf("%w: RepoOrder contains %q with no SessionOrder entry",
				ErrInconsistent, repo)
		}
	}

	// 5. ActiveSession (if set) references an existing session.
	if s.ActiveSession != "" {
		if _, ok := s.Sessions[s.ActiveSession]; !ok {
			return fmt.Errorf("%w: ActiveSession %q does not exist",
				ErrInconsistent, s.ActiveSession)
		}
	}
	return nil
}

// Validate runs the full invariant check on the tree's current state.
// Returns nil if every invariant holds, or a wrapped ErrInconsistent
// describing the first violation. Cheap defence-in-depth for tests and
// consumers that want to assert the tree is well-formed after a sequence
// of operations.
func (t *SessionTree) Validate() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return validate(&t.state)
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

// removeString returns a new slice with the first occurrence of v removed.
// Returns the input unchanged (but in a freshly allocated slice) if v is
// not present; the freshly allocated slice keeps the caller from holding
// on to a backing array that may have been reused.
func removeString(in []string, v string) []string {
	for i, s := range in {
		if s == v {
			return append(in[:i:i], in[i+1:]...)
		}
	}
	return in
}
