package client

import (
	"context"

	"github.com/prismatic-koi/prism/internal/mux/pane"
)

// MuxClient is the top-level interface to the mux daemon. The
// concrete implementation is returned by New; tests substituting a
// mock implement MuxClient and pass the mock wherever a real *Client
// would be used.
//
// The interface deliberately splits the two namespaces (Sessions,
// Panes) into sub-interfaces so a test can replace one half without
// having to satisfy the other — e.g. a "session-only" mock can return
// nil for Panes() if the unit under test never touches pane methods.
type MuxClient interface {
	// Sessions returns the SessionAPI subclient. Always non-nil for
	// the implementation returned by New.
	Sessions() SessionAPI

	// Panes returns the PaneAPI subclient. Always non-nil for the
	// implementation returned by New.
	Panes() PaneAPI

	// Close releases any underlying transport resources. Safe to call
	// multiple times. The current implementation has no persistent
	// connection to release (keep-alives are disabled) so Close is a
	// no-op, but it is part of the interface so future transports
	// (e.g. a connection-pooled variant) can drop into the same shape
	// without an API break.
	Close() error
}

// SessionAPI exposes the session.* method namespace from the server.
// Method names mirror the server's URL paths in PascalCase.
type SessionAPI interface {
	// Create posts /session/create with the supplied session, and
	// returns the canonical post-insert view (which may include
	// server-applied defaults such as ActivePane auto-selecting the
	// first pane). Returns *ClientError on a structured server error
	// and ErrServerUnavailable on a connection failure.
	Create(ctx context.Context, sess pane.Session) (pane.Session, error)

	// Destroy posts /session/destroy. Cascades to any review
	// subsessions of the target (server-side behaviour from
	// pane.SessionTree.RemoveSession).
	Destroy(ctx context.Context, id string) error

	// List returns every session in the tree, in the server's
	// canonical order, alongside the active-session pointer. An empty
	// tree returns a SessionList with an empty Sessions slice (never
	// nil) and an empty ActiveSession string.
	List(ctx context.Context) (SessionList, error)

	// Switch sets the active session and returns the resulting
	// active-session ID. Passing an empty id clears the focus
	// pointer (matches the server's documented behaviour).
	Switch(ctx context.Context, id string) (string, error)
}

// PaneAPI exposes the pane.* method namespace from the server.
type PaneAPI interface {
	// Create posts /pane/create.
	Create(ctx context.Context, sessionID, name string) error

	// Destroy posts /pane/destroy.
	Destroy(ctx context.Context, sessionID, name string) error

	// List returns the panes for sessionID alongside the
	// session-level active-pane pointer. An empty pane set returns a
	// PaneList with an empty Panes slice (never nil).
	List(ctx context.Context, sessionID string) (PaneList, error)

	// Switch posts /pane/switch. Exactly one of req.Name and
	// req.Direction must be set; the server returns
	// 400/CodeBadRequest otherwise. Returns the resulting active
	// pane name.
	Switch(ctx context.Context, req PaneSwitchRequest) (string, error)

	// Resize posts /pane/resize. cols and rows must be non-negative.
	// The server currently validates the (session, pane) tuple and
	// returns 200 without effecting the resize — actual geometry
	// dispatch lands in a later PR — but the wire contract is stable
	// from this PR onward.
	Resize(ctx context.Context, sessionID, name string, cols, rows int) error

	// SendInput posts /pane/send_input. As with Resize, the server
	// currently validates and returns 200 without effecting input
	// delivery; the wire contract is the stable part.
	SendInput(ctx context.Context, sessionID, name, data string) error
}

// SessionList is the typed response shape for SessionAPI.List. The
// Sessions slice is in the server's canonical order (repo-cluster
// major, then insertion order within each repo, then review
// subsessions after their parent).
type SessionList struct {
	Sessions      []pane.Session `json:"sessions"`
	ActiveSession string         `json:"active_session,omitempty"`
}

// PaneList is the typed response shape for PaneAPI.List.
type PaneList struct {
	Panes      []pane.Pane `json:"panes"`
	ActivePane string      `json:"active_pane,omitempty"`
}

// PaneSwitchRequest is the input shape for PaneAPI.Switch. Exactly
// one of Name and Direction must be set:
//
//   - Name selects a specific pane by name.
//   - Direction is "next" or "prev"; the server cycles through panes
//     in insertion order and wraps at the ends.
//
// The Direction* constants below are the only valid values; passing
// anything else results in a *ClientError wrapping ErrBadRequest.
type PaneSwitchRequest struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name,omitempty"`
	Direction string `json:"direction,omitempty"`
}

// Direction* constants for PaneSwitchRequest.Direction.
const (
	DirectionNext = "next"
	DirectionPrev = "prev"
)

// Static-assertion compile-time checks. If any of these go bad the
// build fails immediately rather than at first use of the interface
// — a small but reliable safety net during refactors.
var (
	_ MuxClient  = (*Client)(nil)
	_ SessionAPI = (*sessionAPI)(nil)
	_ PaneAPI    = (*paneAPI)(nil)
)
