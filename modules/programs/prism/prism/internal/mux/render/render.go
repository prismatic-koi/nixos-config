// Package render is the bubbletea-based UI for the prism-native multiplexer.
//
// It owns the visible TUI: a herdr-shape sidebar driven by an
// internal/mux/pane.SessionTree on the left, an active pane area rendering
// real PTY output via internal/mux/vt.Host on the right (or, in narrow
// mode, a single-row status bar plus a popover-able sidebar). The
// implementation is prescriptive — every glyph, colour, tree character,
// truncation rule, and key binding is codified in §3.1 of
// docs/multiplexer-proposal.md ("UI reference: sidebar"). The package
// implements against §3.1, not against its own design notes.
//
// Surface boundaries:
//
//   - Tree data — *pane.SessionTree (from internal/mux/pane). The
//     renderer never mutates it directly; mutations (add/remove session,
//     activate pane) flow through the tree's own API or through the
//     server in #2153.
//
//   - Session state (active / idle / waiting / reviewing / escalated /
//     finished) — read through a StateProvider. The state vocabulary
//     lives in this package as State; the wiring to sidecar agent_status
//     is the job of #2155.
//
//   - PTY output — read through a HostProvider. The active pane area
//     renders the host's RenderRows() output. The wiring to real PTYs is
//     the job of #2153.
//
// Both providers are optional. A Model constructed without them renders
// the sidebar with every session in the "idle" state and an
// "(no PTY)" placeholder in the pane area — useful for tests and for
// the initial wiring step before #2153 lands.
package render
