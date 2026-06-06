package client

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Stable wire-code strings. These mirror the constants in
// internal/mux/server/errors.go 1:1 — the contract is the JSON wire
// shape, not a Go symbol, so duplicating them here keeps client
// callers off the server's unexported names and lets the server move
// its constants around without breaking us.
const (
	CodeMethodNotAllowed = "method_not_allowed"
	CodeBadRequest       = "bad_request"
	CodeInternal         = "internal_error"

	CodeSessionExists   = "session_exists"
	CodeSessionNotFound = "session_not_found"
	CodeParentNotFound  = "parent_not_found"
	CodeParentIsReview  = "parent_is_review"
	CodeInvalidSession  = "invalid_session"
	CodePaneExists      = "pane_exists"
	CodePaneNotFound    = "pane_not_found"
	CodeNoPanes         = "no_panes"
)

// Sentinel errors. Callers compare with errors.Is — a *ClientError
// returned by any client method matches the corresponding sentinel,
// so a CLI subcommand can write:
//
//	if errors.Is(err, client.ErrSessionNotFound) { ... }
//
// without having to type-assert and inspect the Code field by hand.
//
// The sentinels intentionally do NOT alias internal/mux/pane's typed
// errors — that would create a load-order coupling and would make
// every pane-package refactor a client-package break. The shared
// contract is the code string on the wire; that is what we match on.
var (
	// ErrServerUnavailable is returned when the daemon socket cannot be
	// reached at all — no listener, connection refused, EOF before any
	// HTTP response was received. Distinct from a structured 4xx/5xx
	// error so CLI subcommands can print a "daemon not running"
	// suggestion instead of a raw dial-error stack.
	ErrServerUnavailable = errors.New("mux/client: server unavailable")

	// ErrMethodNotAllowed is the 405 sentinel.
	ErrMethodNotAllowed = errors.New("mux/client: method not allowed")

	// ErrBadRequest is the 400 sentinel for handler-level validation
	// (missing fields, malformed JSON, unknown fields).
	ErrBadRequest = errors.New("mux/client: bad request")

	// ErrInternal is the 500 sentinel for unexpected server-side errors.
	ErrInternal = errors.New("mux/client: internal server error")

	// Typed sentinels mirroring the pane.* package's errors. Each
	// corresponds 1:1 to a Code* constant above; see ClientError.Is for
	// the mapping table.

	ErrSessionExists   = errors.New("mux/client: session already exists")
	ErrSessionNotFound = errors.New("mux/client: session not found")
	ErrParentNotFound  = errors.New("mux/client: parent session not found")
	ErrParentIsReview  = errors.New("mux/client: parent is a review subsession")
	ErrInvalidSession  = errors.New("mux/client: invalid session")
	ErrPaneExists      = errors.New("mux/client: pane already exists")
	ErrPaneNotFound    = errors.New("mux/client: pane not found")
	ErrNoPanes         = errors.New("mux/client: session has no panes")
)

// codeToSentinel maps a wire code string to its client-side sentinel.
// Used by ClientError.Is so errors.Is(err, ErrXxx) works without the
// caller knowing the wire code. Codes not in this map fall through to
// "no sentinel match" — errors.Is then only matches against a
// ClientError target with the same Code (or the generic
// ErrInternal/ErrBadRequest where applicable).
var codeToSentinel = map[string]error{
	CodeMethodNotAllowed: ErrMethodNotAllowed,
	CodeBadRequest:       ErrBadRequest,
	CodeInternal:         ErrInternal,
	CodeSessionExists:    ErrSessionExists,
	CodeSessionNotFound:  ErrSessionNotFound,
	CodeParentNotFound:   ErrParentNotFound,
	CodeParentIsReview:   ErrParentIsReview,
	CodeInvalidSession:   ErrInvalidSession,
	CodePaneExists:       ErrPaneExists,
	CodePaneNotFound:     ErrPaneNotFound,
	CodeNoPanes:          ErrNoPanes,
}

// ClientError is the typed error returned for any 4xx/5xx response
// from the server. The fields mirror the wire shape from
// internal/mux/server/errors.go's errorResponse so callers can
// inspect Code, Message, and Data without re-parsing the body.
//
// ClientError implements errors.Is by checking the target against the
// client-side sentinel for its Code (see codeToSentinel) AND against
// any other *ClientError with the same Code — callers may either
// branch on a sentinel (preferred) or build their own ClientError to
// match against in tests.
type ClientError struct {
	// HTTPStatus is the raw HTTP status code (e.g. 404, 409, 500).
	// Useful for logging and for callers that care about the
	// status-class even when the wire Code is missing.
	HTTPStatus int

	// Code is the stable wire identifier (e.g. "session_not_found").
	// Empty if the server returned a non-JSON error body — see
	// decodeErrorBody below for how that fallback is constructed.
	Code string

	// Message is the human-readable message field from the JSON body.
	Message string

	// Data is the optional extra-context map from the JSON body. Held
	// as json.RawMessage so the original bytes are preserved verbatim
	// and callers that need typed fields can json.Unmarshal into a
	// struct of their own.
	Data json.RawMessage
}

// Error renders the ClientError as a single line suitable for log
// output. Format: "mux: <code> (HTTP <status>): <message>". The Data
// field is intentionally omitted from this rendering because it can
// be arbitrary structured content; callers who want it should
// inspect the field directly.
func (e *ClientError) Error() string {
	if e == nil {
		return "<nil ClientError>"
	}
	if e.Code == "" {
		return fmt.Sprintf("mux: HTTP %d: %s", e.HTTPStatus, e.Message)
	}
	return fmt.Sprintf("mux: %s (HTTP %d): %s", e.Code, e.HTTPStatus, e.Message)
}

// Is implements the errors.Is contract. It returns true when:
//
//   - target is the client-side sentinel for e.Code (e.g. e.Code ==
//     "session_not_found" and target == ErrSessionNotFound), OR
//   - target is itself a *ClientError whose Code matches e.Code.
//
// Status-only matches (e.g. "any 5xx") are NOT supported via Is —
// callers who want that level of detail should inspect HTTPStatus
// directly.
func (e *ClientError) Is(target error) bool {
	if e == nil {
		return false
	}
	if other, ok := target.(*ClientError); ok {
		return other.Code != "" && other.Code == e.Code
	}
	if sentinel, ok := codeToSentinel[e.Code]; ok && errors.Is(target, sentinel) {
		return true
	}
	return false
}

// decodeErrorBody parses a 4xx/5xx response body into a *ClientError.
// When the body is not valid JSON (e.g. a proxy-injected HTML error
// page) we still return a usable *ClientError carrying the HTTP
// status and the raw body as Message, with Code empty — that lets
// callers handle "server returned garbage" uniformly with structured
// errors.
//
// The HTTPStatus is set by the caller; this function only owns the
// body-decoding half.
func decodeErrorBody(status int, body []byte) *ClientError {
	out := &ClientError{HTTPStatus: status}

	// Empty body — render a synthetic message so the Error() output
	// stays informative.
	if len(body) == 0 {
		out.Message = fmt.Sprintf("empty error body (HTTP %d)", status)
		return out
	}

	// Wire shape mirrors internal/mux/server/errors.go's errorResponse.
	// We accept extra unknown fields silently (no DisallowUnknownFields)
	// so the server can grow the envelope without breaking older
	// clients — the documented fields are sufficient for branching.
	var wire struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		// Not JSON — preserve the raw body as the message so a CLI
		// caller still sees what the server (or an intervening proxy)
		// said.
		out.Message = fmt.Sprintf("non-JSON error body (HTTP %d): %s", status, string(body))
		return out
	}
	out.Code = wire.Code
	out.Message = wire.Message
	out.Data = wire.Data
	return out
}
