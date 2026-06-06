package client

import (
	"context"

	"github.com/prismatic-koi/prism/internal/mux/pane"
)

// sessionAPI implements SessionAPI. It holds a back-reference to the
// parent Client so requests route through the shared do() helper.
//
// The type is intentionally unexported — callers reach it via
// Client.Sessions(), never construct it directly.
type sessionAPI struct {
	c *Client
}

// sessionCreateWire is the wire shape for POST /session/create. It
// mirrors internal/mux/server.sessionCreateRequest field-for-field —
// keeping a private wire-shape struct (rather than sending
// pane.Session directly) means a future divergence between the model
// and the wire is a one-file change on each side.
//
// The struct is constructed inside Create from the supplied
// pane.Session so callers see a clean Go-typed surface.
type sessionCreateWire struct {
	ID          string             `json:"id"`
	Repo        string             `json:"repo,omitempty"`
	Branch      string             `json:"branch,omitempty"`
	Worktree    string             `json:"worktree,omitempty"`
	AgentRole   string             `json:"agent_role,omitempty"`
	SidecarAddr string             `json:"sidecar_addr,omitempty"`
	ParentID    string             `json:"parent_id,omitempty"`
	Panes       []sessionPaneInput `json:"panes,omitempty"`
	ActivePane  string             `json:"active_pane,omitempty"`
}

// sessionPaneInput mirrors internal/mux/server.paneInput — the wire
// shape for embedded panes in a /session/create payload. We use a
// private wire type rather than pane.Pane so the wire format is
// pinned independently of the model.
type sessionPaneInput struct {
	Name string `json:"name"`
}

// sessionResponseWire is the wire shape for /session/create's success
// body. The single field's JSON tag matches server.sessionResponse.
type sessionResponseWire struct {
	Session pane.Session `json:"session"`
}

// sessionSwitchWire matches server.sessionSwitchRequest.
type sessionSwitchWire struct {
	ID string `json:"id"`
}

// sessionSwitchResponseWire matches server.sessionSwitchResponse.
type sessionSwitchResponseWire struct {
	ActiveSession string `json:"active_session"`
}

// sessionDestroyWire matches server.sessionDestroyRequest.
type sessionDestroyWire struct {
	ID string `json:"id"`
}

// Create posts /session/create with the supplied session and returns
// the canonical post-insert view. The pane.Session zero value is NOT
// accepted by the server (ID is required); the client passes the
// request through verbatim and lets the server's validation produce
// the structured error.
func (s *sessionAPI) Create(ctx context.Context, sess pane.Session) (pane.Session, error) {
	wire := sessionCreateWire{
		ID:          sess.ID,
		Repo:        sess.Repo,
		Branch:      sess.Branch,
		Worktree:    sess.Worktree,
		AgentRole:   sess.AgentRole,
		SidecarAddr: sess.SidecarAddr,
		ParentID:    sess.ParentID,
		ActivePane:  sess.ActivePane,
	}
	if len(sess.Panes) > 0 {
		wire.Panes = make([]sessionPaneInput, len(sess.Panes))
		for i, p := range sess.Panes {
			wire.Panes[i] = sessionPaneInput{Name: p.Name}
		}
	}

	var resp sessionResponseWire
	if err := s.c.doPost(ctx, "/session/create", wire, &resp); err != nil {
		return pane.Session{}, err
	}
	return resp.Session, nil
}

// Destroy posts /session/destroy. Empty id is intentionally not
// short-circuited client-side; the server returns
// 400/CodeBadRequest for it and we want that error class to be
// visible through one code path, not two.
func (s *sessionAPI) Destroy(ctx context.Context, id string) error {
	return s.c.doPost(ctx, "/session/destroy", sessionDestroyWire{ID: id}, nil)
}

// List returns every session in the tree. An empty tree returns a
// SessionList with an empty Sessions slice — the server normalises
// nil to [] on the wire so the client never has to special-case
// "null" decoding.
func (s *sessionAPI) List(ctx context.Context) (SessionList, error) {
	var resp SessionList
	if err := s.c.doGet(ctx, "/session/list", nil, &resp); err != nil {
		return SessionList{}, err
	}
	// Defensive normalisation: the server already returns [] for an
	// empty tree, but a future server bug or an old build could
	// return null. Promote nil to [] so callers can range without a
	// nil check.
	if resp.Sessions == nil {
		resp.Sessions = []pane.Session{}
	}
	return resp, nil
}

// Switch sets the active session pointer. The returned string is
// the server's echo of the new active-session ID — typically equal
// to id, but pinned in the wire contract so callers can confirm
// without a follow-up List.
func (s *sessionAPI) Switch(ctx context.Context, id string) (string, error) {
	var resp sessionSwitchResponseWire
	if err := s.c.doPost(ctx, "/session/switch", sessionSwitchWire{ID: id}, &resp); err != nil {
		return "", err
	}
	return resp.ActiveSession, nil
}

// Compile-time check that sessionAPI's exposed surface matches the
// interface — duplicated from interfaces.go for in-file
// discoverability when reading session.go in isolation.
var _ SessionAPI = (*sessionAPI)(nil)
