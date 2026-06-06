package render

import "github.com/prismatic-koi/prism/internal/mux/vt"

// HostProvider resolves a (sessionID, paneName) pair to a *vt.Host whose
// rendered cell grid is the content of the active pane area.
//
// Implementations are expected to return the same *vt.Host across
// successive calls for the same (sessionID, paneName) pair — the Model
// dereferences it on every View() call. Returning nil is fine; the active
// pane area then renders an "(no PTY)" placeholder.
//
// The renderer is the only intended caller. The wiring to real PTYs lives
// in the server package (#2153); this interface is what couples the two
// without a direct import.
type HostProvider interface {
	Host(sessionID, paneName string) *vt.Host
}

// HostFunc adapts a plain function to the HostProvider interface — handy
// for tests and one-off wiring.
type HostFunc func(sessionID, paneName string) *vt.Host

// Host implements HostProvider.
func (f HostFunc) Host(sessionID, paneName string) *vt.Host { return f(sessionID, paneName) }
