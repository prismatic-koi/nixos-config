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
	// Create posts /pane/create. opts.Argv (when non-empty) instructs the
	// server to spawn the process under a PTY and host its output in a
	// vt.Host. opts.Cwd is the child's working directory; opts.Env (when
	// non-nil) replaces the daemon's environment; opts.Cols/Rows are the
	// initial PTY geometry. A zero-value opts creates a model-only row
	// with no PTY — useful for tests and for the legacy validate-only
	// shape.
	Create(ctx context.Context, sessionID, name string, opts PaneCreateOptions) error

	// Destroy posts /pane/destroy. When a PTY is registered for the
	// pane, the server signals the child (SIGTERM → grace → SIGKILL),
	// waits for exit, and unregisters the host. Destroy returns once
	// the model row is removed; PTY teardown is best-effort and never
	// fails the API call.
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
	// The server forwards the resize to the PTY (kernel will SIGWINCH
	// the child) and updates the emulator's grid dimensions. Panes
	// without a PTY accept the call as a no-op for wire-contract
	// stability.
	Resize(ctx context.Context, sessionID, name string, cols, rows int) error

	// SendInput posts /pane/send_input. The bytes are written verbatim
	// to the PTY master FD (which the kernel presents to the child as
	// stdin). Panes without a PTY accept the call as a no-op.
	SendInput(ctx context.Context, sessionID, name, data string) error

	// ReadOutput posts GET /pane/read_output. Returns a snapshot of
	// the rendered cell grid for the renderer's polling tick. Panes
	// without a PTY return a zero-dimension PaneFrame with an empty
	// Lines slice.
	ReadOutput(ctx context.Context, sessionID, name string) (PaneFrame, error)
}

// PaneCreateOptions carries the runtime-side fields the server uses
// to spawn a PTY for the new pane. The zero value is valid: it
// produces a model-only row with no PTY.
type PaneCreateOptions struct {
	// Argv is the executable + arguments to spawn. argv[0] is the
	// program to exec. An empty Argv creates a model-only row with
	// no PTY.
	Argv []string

	// Cwd is the child's working directory. Empty means "inherit the
	// daemon's cwd" — in practice the user's $HOME, not what the
	// CLI wants. Pass an explicit absolute path.
	Cwd string

	// Env is the child's environment. Nil means "inherit the daemon's
	// env"; a non-nil but empty map means "start with an empty env".
	Env map[string]string

	// Cols and Rows are the initial PTY geometry. Zero falls back to
	// a conventional 80x24; the renderer resizes on first paint.
	Cols uint16
	Rows uint16
}

// PaneFrame is the typed response shape for PaneAPI.ReadOutput. It is
// a polling-shape snapshot of the pane's rendered cell grid — one
// string per visible row, padded to the row count, with cursor
// coordinates and the alt-screen flag for renderer chrome.
//
// When the pane has no PTY (created with an empty Argv) the returned
// PaneFrame carries Cols=0 and Rows=0 with an empty Lines slice; the
// renderer treats this as "no content yet" and shows the placeholder.
type PaneFrame struct {
	Cols      int      `json:"cols"`
	Rows      int      `json:"rows"`
	CursorX   int      `json:"cursor_x"`
	CursorY   int      `json:"cursor_y"`
	AltScreen bool     `json:"alt_screen"`
	Lines     []string `json:"lines"`
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
