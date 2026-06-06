package server

// /pane/read_output — pull-shape snapshot endpoint for the renderer.
//
// The renderer (internal/mux/render) repaints on a tick and wants the
// latest cell-grid frame for the active pane. We expose two surfaces:
//
//  1. In-process: Server.HostFor(sessionID, paneName) returns the live
//     *vt.Host so a renderer running in the same process (the
//     prismd-mux daemon plus an embedded TUI) can read RenderRows()
//     directly with no per-frame allocation.
//
//  2. Out-of-process (the soak shape): GET /pane/read_output returns
//     the rendered rows + dimensions + cursor position as a JSON
//     PaneFrame. A separate `prism attach` style client builds a
//     synthetic vt.Host from this on each poll. Lossier than the
//     in-process path but sufficient for the post-#2158 phase-3 soak.
//
// The endpoint is a GET because reads are idempotent and the client
// can cache the response on retry. Request shape: ?session_id=X&name=Y.

import (
	"net/http"

	"github.com/prismatic-koi/prism/internal/mux/vt"
)

// paneReadOutputResponse is the wire shape for GET /pane/read_output.
// Rows are the emulator's per-row Render() output, padded to the
// configured row count (matches vt.Host.RenderRows()). Cols / Rows are
// the engine's current dimensions; CursorX / CursorY are the cursor
// position inside that grid.
//
// AltScreen reports whether the emulator is currently on the alt
// screen — useful for clients that want to skip status-bar overlays
// when a full-screen TUI is running.
type paneReadOutputResponse struct {
	Cols      int      `json:"cols"`
	Rows      int      `json:"rows"`
	CursorX   int      `json:"cursor_x"`
	CursorY   int      `json:"cursor_y"`
	AltScreen bool     `json:"alt_screen"`
	Lines     []string `json:"lines"`
}

func (s *Server) handlePaneReadOutput(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	sessID := r.URL.Query().Get("session_id")
	name := r.URL.Query().Get("name")
	if sessID == "" || name == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"session_id and name query parameters are required",
			map[string]any{"session_id": sessID, "name": name})
		return
	}
	if err := s.checkPaneExists(sessID, name); err != nil {
		writePaneErr(w, err, map[string]any{
			"session_id": sessID,
			"name":       name,
		})
		return
	}

	host, ok := s.ptys.get(sessID, name)
	if !ok || host == nil || host.Host() == nil {
		// Pane exists in the model but has no PTY (e.g. created with
		// an empty argv). Return an empty frame rather than a 404 —
		// the renderer treats this as "no content yet" and shows the
		// placeholder. Dimensions are reported as zero so the client
		// can distinguish this from a 0-row pane.
		writeJSON(w, paneReadOutputResponse{
			Cols:  0,
			Rows:  0,
			Lines: []string{},
		})
		return
	}

	vtHost := host.Host()
	cols, rows := vtHost.Size()
	snap := vtHost.Snapshot()
	lines := vtHost.RenderRows()
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, paneReadOutputResponse{
		Cols:      cols,
		Rows:      rows,
		CursorX:   snap.CursorX,
		CursorY:   snap.CursorY,
		AltScreen: snap.AltScreen,
		Lines:     lines,
	})
}

// HostFor returns the live *vt.Host for (sessionID, paneName), or nil
// when no PTY is registered (pane absent, or created with an empty
// argv). Exposed for in-process renderer wiring — the returned value
// satisfies the structural shape of render.HostProvider's Host method
// without this package needing to import render.
//
// The returned host is shared with the server's pump goroutines; do
// NOT mutate it. Concurrent reads (RenderRows / Snapshot) are safe per
// the vt.Host contract.
func (s *Server) HostFor(sessionID, paneName string) *vt.Host {
	if s == nil || s.ptys == nil {
		return nil
	}
	host, ok := s.ptys.get(sessionID, paneName)
	if !ok || host == nil {
		return nil
	}
	return host.Host()
}

// HostProviderFunc adapts Server.HostFor into a stand-alone function
// value. The renderer's HostProvider interface is a single-method
// "Host(sessionID, paneName string) *vt.Host" — a *Server already
// satisfies that structurally, but a function value is sometimes
// easier to pass around in wiring code.
//
// Returns nil when s is nil so callers can write
// `render.WithHosts(render.HostFunc(srv.HostProviderFunc()))` without
// nil-checking.
func (s *Server) HostProviderFunc() func(string, string) *vt.Host {
	if s == nil {
		return nil
	}
	return s.HostFor
}

