package podmanproxy

import (
	"encoding/json"
	"net/http"
)

// errorEnvelope is the docker-compatible JSON response body used for
// every synthesised response. The single "message" field is the lowest
// common denominator across docker CLI, podman, fsouza/go-dockerclient,
// containers/buildah, etc.
type errorEnvelope struct {
	Message string `json:"message"`
}

// writeJSONError writes status and a JSON {"message": msg} body. The
// content-type is set to application/json. Any error returned by
// http.ResponseWriter.Write is intentionally ignored — there is no
// useful recovery path at the top of an HTTP handler.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	body, err := json.Marshal(errorEnvelope{Message: msg})
	if err != nil {
		// json.Marshal of a struct with a string field cannot fail
		// in practice, but fall back to a safe minimal envelope.
		body = []byte(`{"message":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeUnavailable writes the friendly upstream-unavailable envelope.
// The tests in proxy_security_test.go assert that the message names the
// platform recovery commands. Keep the "podman machine start" and
// "systemctl --user status podman.socket" substrings when you edit it.
func writeUnavailable(w http.ResponseWriter, reason string) {
	msg := "podman socket unavailable: " + reason +
		"; on macOS run 'podman machine start', on Linux check 'systemctl --user status podman.socket'"
	writeJSONError(w, http.StatusServiceUnavailable, msg)
}
