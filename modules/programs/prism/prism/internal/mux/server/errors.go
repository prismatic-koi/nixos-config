package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/prismatic-koi/prism/internal/mux/pane"
)

// errorResponse is the canonical JSON shape every error response carries.
// `Code` is a stable identifier suitable for programmatic branching;
// `Message` is human-readable; `Data` is an optional map of extra
// context (e.g. {"session_id":"foo"}). The fields use snake_case to
// match the prism convention.
type errorResponse struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

// Error code constants — stable identifiers used by clients to branch
// on failure modes without string-matching the message.
const (
	codeMethodNotAllowed = "method_not_allowed"
	codeBadRequest       = "bad_request"
	codeInternal         = "internal_error"

	codeSessionExists   = "session_exists"
	codeSessionNotFound = "session_not_found"
	codeParentNotFound  = "parent_not_found"
	codeParentIsReview  = "parent_is_review"
	codeInvalidSession  = "invalid_session"
	codePaneExists      = "pane_exists"
	codePaneNotFound    = "pane_not_found"
	codeNoPanes         = "no_panes"
)

// statusAndCodeForPaneErr maps a typed sentinel from internal/mux/pane
// to (http_status, stable_code). Callers should pass err already
// wrapped as the pane.* sentinel — errors.Is is used to detect the
// match. When err matches none of the typed sentinels, the caller's
// fallback (500 / internal_error) applies.
func statusAndCodeForPaneErr(err error) (int, string, bool) {
	switch {
	case errors.Is(err, pane.ErrSessionExists):
		return http.StatusConflict, codeSessionExists, true
	case errors.Is(err, pane.ErrSessionNotFound):
		return http.StatusNotFound, codeSessionNotFound, true
	case errors.Is(err, pane.ErrParentNotFound):
		return http.StatusNotFound, codeParentNotFound, true
	case errors.Is(err, pane.ErrParentIsReview):
		return http.StatusBadRequest, codeParentIsReview, true
	case errors.Is(err, pane.ErrInvalidSession):
		return http.StatusBadRequest, codeInvalidSession, true
	case errors.Is(err, pane.ErrPaneExists):
		return http.StatusConflict, codePaneExists, true
	case errors.Is(err, pane.ErrPaneNotFound):
		return http.StatusNotFound, codePaneNotFound, true
	case errors.Is(err, pane.ErrNoPanes):
		return http.StatusBadRequest, codeNoPanes, true
	}
	return 0, "", false
}

// writeError serialises an errorResponse and writes it with the given
// HTTP status. The Content-Type is forced to application/json so the
// CLI client never has to sniff. Marshalling failures are extremely
// unlikely (the type is fully concrete) but if one occurs we fall back
// to a hardcoded JSON literal so the client still gets a parseable
// body.
func writeError(w http.ResponseWriter, status int, code, message string, data map[string]any) {
	body, err := json.Marshal(errorResponse{Code: code, Message: message, Data: data})
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal_error","message":"marshal error response"}`))
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writePaneErr inspects err for a typed pane.* sentinel and writes the
// matching structured response. When no typed sentinel matches, it
// writes a 500/internal_error carrying the underlying error message.
// The optional data map is included verbatim in the response body.
func writePaneErr(w http.ResponseWriter, err error, data map[string]any) {
	if status, code, ok := statusAndCodeForPaneErr(err); ok {
		writeError(w, status, code, err.Error(), data)
		return
	}
	writeError(w, http.StatusInternalServerError, codeInternal, err.Error(), data)
}
