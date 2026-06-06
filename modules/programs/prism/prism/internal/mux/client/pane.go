package client

import (
	"context"
	"net/url"

	"github.com/prismatic-koi/prism/internal/mux/pane"
)

// paneAPI implements PaneAPI. Mirrors sessionAPI in shape.
type paneAPI struct {
	c *Client
}

// paneCreateWire matches server.paneCreateRequest.
//
// Argv (when non-empty) instructs the server to spawn the process
// under a PTY and host its output in a vt.Host. Cwd is the child's
// working directory; Env (when non-nil) replaces the daemon's
// environment. Cols / Rows are the initial PTY geometry — zero falls
// back to a conventional 80x24.
type paneCreateWire struct {
	SessionID string            `json:"session_id"`
	Name      string            `json:"name"`
	Argv      []string          `json:"argv,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Cols      uint16            `json:"cols,omitempty"`
	Rows      uint16            `json:"rows,omitempty"`
}

// paneDestroyWire matches server.paneDestroyRequest.
type paneDestroyWire struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
}

// paneSwitchResponseWire matches server.paneSwitchResponse.
type paneSwitchResponseWire struct {
	ActivePane string `json:"active_pane"`
}

// paneResizeWire matches server.paneResizeRequest.
type paneResizeWire struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

// paneSendInputWire matches server.paneSendInputRequest.
type paneSendInputWire struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Data      string `json:"data"`
}

// Create posts /pane/create. The opts struct carries the runtime-side
// fields the server needs to spawn a process under a PTY — Argv, Cwd,
// Env, Cols, Rows. Callers that only want a model-side row (no PTY)
// pass a zero-value PaneCreateOptions, matching the pre-#2158 shape.
//
// Returns *ClientError on a structured server error (model-side
// failures: ErrSessionNotFound, ErrPaneExists), or ErrServerUnavailable
// on a connection failure. A PTY-spawn failure surfaces as a 500 with
// the underlying os/exec error in Message.
func (p *paneAPI) Create(ctx context.Context, sessionID, name string, opts PaneCreateOptions) error {
	return p.c.doPost(ctx, "/pane/create", paneCreateWire{
		SessionID: sessionID,
		Name:      name,
		Argv:      opts.Argv,
		Cwd:       opts.Cwd,
		Env:       opts.Env,
		Cols:      opts.Cols,
		Rows:      opts.Rows,
	}, nil)
}

// Destroy posts /pane/destroy.
func (p *paneAPI) Destroy(ctx context.Context, sessionID, name string) error {
	return p.c.doPost(ctx, "/pane/destroy", paneDestroyWire{
		SessionID: sessionID,
		Name:      name,
	}, nil)
}

// List returns the panes of sessionID and the active-pane pointer.
// As with SessionAPI.List, nil slices are promoted to [] so callers
// can range without a nil check.
func (p *paneAPI) List(ctx context.Context, sessionID string) (PaneList, error) {
	q := url.Values{}
	q.Set("session_id", sessionID)

	var resp PaneList
	if err := p.c.doGet(ctx, "/pane/list", q, &resp); err != nil {
		return PaneList{}, err
	}
	if resp.Panes == nil {
		resp.Panes = []pane.Pane{}
	}
	return resp, nil
}

// Switch posts /pane/switch. The XOR validation (exactly one of Name
// or Direction) is enforced by the server; the client passes the
// request through verbatim so the structured 400/CodeBadRequest is
// what callers see on misuse.
func (p *paneAPI) Switch(ctx context.Context, req PaneSwitchRequest) (string, error) {
	var resp paneSwitchResponseWire
	if err := p.c.doPost(ctx, "/pane/switch", req, &resp); err != nil {
		return "", err
	}
	return resp.ActivePane, nil
}

// Resize posts /pane/resize. cols and rows must be non-negative —
// the server rejects negatives with 400/CodeBadRequest.
func (p *paneAPI) Resize(ctx context.Context, sessionID, name string, cols, rows int) error {
	return p.c.doPost(ctx, "/pane/resize", paneResizeWire{
		SessionID: sessionID,
		Name:      name,
		Cols:      cols,
		Rows:      rows,
	}, nil)
}

// SendInput posts /pane/send_input. The bytes are written verbatim to
// the PTY master FD (the kernel presents this to the child as stdin).
// When the pane has no PTY (created with an empty Argv), the call is a
// no-op and returns nil so the model side stays consistent.
func (p *paneAPI) SendInput(ctx context.Context, sessionID, name, data string) error {
	return p.c.doPost(ctx, "/pane/send_input", paneSendInputWire{
		SessionID: sessionID,
		Name:      name,
		Data:      data,
	}, nil)
}

// paneReadOutputResponseWire matches server.paneReadOutputResponse.
type paneReadOutputResponseWire struct {
	Cols      int      `json:"cols"`
	Rows      int      `json:"rows"`
	CursorX   int      `json:"cursor_x"`
	CursorY   int      `json:"cursor_y"`
	AltScreen bool     `json:"alt_screen"`
	Lines     []string `json:"lines"`
}

// ReadOutput posts GET /pane/read_output. Returns a snapshot of the
// pane's rendered cell grid suitable for the renderer's polling tick.
//
// The Lines slice is the vt.Host's RenderRows() output — one string
// per visible row, padded to the row count. Cursor coordinates are
// in row-major form (CursorY is the row index, CursorX the column).
// Cols / Rows are the engine's current dimensions.
//
// When the pane has no PTY (created with an empty Argv), the returned
// PaneFrame carries Cols=0 and Rows=0 with an empty Lines slice — the
// model row exists but there is no rendered content.
func (p *paneAPI) ReadOutput(ctx context.Context, sessionID, name string) (PaneFrame, error) {
	q := url.Values{}
	q.Set("session_id", sessionID)
	q.Set("name", name)

	var resp paneReadOutputResponseWire
	if err := p.c.doGet(ctx, "/pane/read_output", q, &resp); err != nil {
		return PaneFrame{}, err
	}
	if resp.Lines == nil {
		resp.Lines = []string{}
	}
	return PaneFrame{
		Cols:      resp.Cols,
		Rows:      resp.Rows,
		CursorX:   resp.CursorX,
		CursorY:   resp.CursorY,
		AltScreen: resp.AltScreen,
		Lines:     resp.Lines,
	}, nil
}
