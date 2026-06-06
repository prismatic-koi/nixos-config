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
type paneCreateWire struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
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

// Create posts /pane/create.
func (p *paneAPI) Create(ctx context.Context, sessionID, name string) error {
	return p.c.doPost(ctx, "/pane/create", paneCreateWire{
		SessionID: sessionID,
		Name:      name,
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

// SendInput posts /pane/send_input. data is opaque to the server at
// this layer (PTY integration lands in a later PR) but is included
// in the wire shape now so the contract is stable.
func (p *paneAPI) SendInput(ctx context.Context, sessionID, name, data string) error {
	return p.c.doPost(ctx, "/pane/send_input", paneSendInputWire{
		SessionID: sessionID,
		Name:      name,
		Data:      data,
	}, nil)
}
