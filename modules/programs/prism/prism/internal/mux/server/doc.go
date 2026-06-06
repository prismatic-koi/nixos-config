// Package server is the Unix-socket API layer of the prism-native
// multiplexer (issue #2153, programme #2147). It exposes the
// internal/mux/pane.SessionTree to CLI clients over a Unix domain socket
// so that prism subcommands (and eventually the 33 callers currently
// using internal/tmux) can mutate the workspace model without sharing
// process memory.
//
// # Wire shape
//
// HTTP/1.1 over Unix socket with JSON request and response bodies. This
// mirrors the existing prism host-API server in
// internal/sidecar/host_api.go — the same client transport
// (http.Client with a custom net.Dialer that dials a Unix path) works
// against either socket. The §3 architecture table in
// docs/multiplexer-proposal.md sketches a JSONL framing as one option;
// the issue #2153 AC explicitly says "JSON-RPC over newline-delimited
// frames OR the equivalent prism convention used by the sidecar host-API
// today" — we pick the latter so the CLI client gets to reuse the
// idioms already proven in the host-API path and there is only one wire
// shape for reviewers to think about.
//
// The "method surface" requested by the ACs (session.create,
// pane.send_input, …) maps to URL paths by replacing the dot with a
// slash: session.create → POST /session/create, pane.send_input →
// POST /pane/send_input. List endpoints use GET; mutating endpoints use
// POST.
//
// # Socket path
//
// Default location:
//
//	$XDG_STATE_HOME/prism/run/<12-hex-of-sha256("prism-mux")>/mux.sock
//
// The hashed-directory layout mirrors
// internal/session.SidecarHostAPIPath — both for path-length budgeting
// against sun_path (Linux 108 bytes, Darwin 104) and so existing prism
// tooling that walks $XDG_STATE_HOME/prism/run/ keeps working. The
// daemon is singleton-per-user (no session name to disambiguate), so
// the SHA-256 input is a fixed string "prism-mux" rather than a session
// name. A hypothetical real session literally named "prism-mux" would
// produce a different hash because real sessions are conventionally
// "<repo>@<branch>"; even if a collision did occur, the file names
// (hostapi.sock vs mux.sock) inside the directory differ, so the two
// sockets coexist cleanly.
//
// # Concurrency
//
// One socket connection per CLI invocation; the net/http server accepts
// many connections in parallel (one goroutine per connection). All
// mutations on the SessionTree go through its existing mutex (see
// internal/mux/pane), so the server itself holds no additional locks.
//
// # Errors
//
// Every error response carries a structured JSON body:
//
//	{"code":"<stable_code>","message":"<human readable>","data":{...}}
//
// The stable codes (see errors.go) map 1:1 to the typed sentinels
// exported by internal/mux/pane (ErrSessionExists, ErrPaneNotFound, …)
// so a client can branch on `code` without string-matching the
// `message`. HTTP status codes are set in parallel — 404 for not-found,
// 409 for conflicts, 400 for invalid input, 405 for wrong method, 500
// for unexpected internal failures.
package server
