// Package client is the typed Go SDK for the prism-native multiplexer
// Unix-socket API (issue #2154, programme #2147). It wraps the
// HTTP/1.1-over-Unix-socket server in internal/mux/server with a small
// set of interfaces (SessionAPI, PaneAPI) that mirror the server's
// method surface 1:1 — see the wire-shape table in
// internal/mux/server/doc.go for the canonical mapping.
//
// # Method surface
//
// The server exposes ten endpoints; the client surfaces them as Go
// methods grouped into two interfaces:
//
//	Sessions().Create(ctx, req)        POST /session/create
//	Sessions().Destroy(ctx, id)        POST /session/destroy
//	Sessions().List(ctx)               GET  /session/list
//	Sessions().Switch(ctx, id)         POST /session/switch
//
//	Panes().Create(ctx, sess, name)    POST /pane/create
//	Panes().Destroy(ctx, sess, name)   POST /pane/destroy
//	Panes().List(ctx, sess)            GET  /pane/list?session_id=...
//	Panes().Switch(ctx, req)           POST /pane/switch
//	Panes().Resize(ctx, ...)           POST /pane/resize
//	Panes().SendInput(ctx, ...)        POST /pane/send_input
//
// Both interfaces are defined as Go interfaces so consumers (CLI
// subcommands, higher-layer orchestrators) can substitute a mock in
// their own tests without touching a real socket. The concrete
// implementation returned by New satisfies MuxClient.
//
// # Connection management
//
// Each request opens a fresh Unix-socket connection — keep-alives are
// disabled by default so a daemon restart between requests is
// transparent to callers (no stale pooled connection to retry against).
// This is the moral equivalent of the "auto-reconnect" requirement in
// the spec: there is no persistent connection to reconnect, so every
// call is a fresh dial.
//
// The configurable timeout (WithTimeout) is applied to the HTTP client
// and bounds the worst-case wall-clock cost of a single call. Per-call
// context deadlines (passed via ctx.WithDeadline) take precedence and
// are the recommended way to bound an individual operation.
//
// # Error model
//
// The server returns 4xx/5xx responses with a structured JSON body of
// the shape {"code":"<stable>","message":"...","data":{...}}. The
// client decodes that body into a *ClientError and exposes it via
// errors.As. Stable code strings are re-exported as client-side
// sentinels (ErrSessionNotFound, ErrPaneExists, …) so callers branch
// with errors.Is rather than string-matching the message. This
// duplication (rather than importing the server's unexported codes) is
// deliberate: the client is the stable boundary for downstream callers,
// and the wire codes are a documented contract — not a Go symbol.
//
// Connection failures (no socket, refused, EOF before any HTTP response
// was produced) are surfaced as ErrServerUnavailable so CLI subcommands
// can give the user a clean "daemon not running" message instead of an
// obscure dial error. Wrapping is preserved — errors.Unwrap on the
// returned error yields the underlying net.OpError, so log inspection
// still works.
//
// # Stdlib-only
//
// The package imports only the Go standard library plus
// internal/mux/pane (for the shared Session/Pane types) and
// internal/mux/server (for the canonical socket path). No third-party
// HTTP clients, no codegen — net/http with a custom Unix-dialing
// transport is sufficient and mirrors the pattern already proven in
// internal/sidecar/host_api.go (the dialUnixAndPost helper).
package client
