package sidecar

// Tests for the host-API /feedback endpoint (issue #1644).
//
// The endpoint accepts a feedback.Entry JSON body, appends it to the host
// feedback.jsonl, and returns the resolved path. These tests exercise the
// handler directly via hostAPIHandler() without a real Unix socket.
//
// The store path is redirected to a t.TempDir() via XDG_STATE_HOME so the
// tests pass inside the nix-build sandbox where HOME=/homeless-shelter is
// intentionally unwritable.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/feedback"
)

// TestHostAPI_Feedback_AppendsEntryAndReturnsPath verifies that a valid POST
// to /feedback appends the entry to the host store and returns the resolved
// path in the response body.
func TestHostAPI_Feedback_AppendsEntryAndReturnsPath(t *testing.T) {
	// Redirect the feedback store to a tempdir so the handler writes there
	// rather than to the real home directory.
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	d := openTestDB(t)
	sc := newSidecarWithRole(t, "prism-test@worker-feedback", "prism-test", "worker", d)

	entry := feedback.Entry{
		Timestamp:    "2026-05-15T10:00:00Z",
		Text:         "some feedback text",
		Session:      "prism-test@worker-feedback",
		PrismVersion: "abc123",
	}
	body, _ := json.Marshal(entry)

	rr := doHostAPI(t, sc, http.MethodPost, "/feedback", string(body))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	// Response must include the path.
	var resp struct {
		Path string `json:"path"`
	}
	decodeJSONBody(t, rr, &resp)
	if resp.Path == "" {
		t.Fatalf("response missing path: %q", rr.Body.String())
	}

	// The entry must actually be on disk at the returned path.
	data, readErr := os.ReadFile(resp.Path)
	if readErr != nil {
		t.Fatalf("ReadFile %s: %v", resp.Path, readErr)
	}
	if !strings.Contains(string(data), "some feedback text") {
		t.Errorf("store missing entry text: %s", data)
	}

	// Verify the path is under the tempdir (not the real home).
	if !strings.HasPrefix(resp.Path, dir) {
		t.Errorf("path %q should be under tempdir %q", resp.Path, dir)
	}
}

// TestHostAPI_Feedback_MalformedBodyReturns400 verifies that malformed JSON
// is rejected with HTTP 400.
func TestHostAPI_Feedback_MalformedBodyReturns400(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	d := openTestDB(t)
	sc := newSidecarWithRole(t, "prism-test@worker-feedback", "prism-test", "worker", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/feedback", `{not valid json`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "invalid JSON") {
		t.Errorf("body %q: want 'invalid JSON'", rr.Body.String())
	}
}

// TestHostAPI_Feedback_EmptyTextReturns400 verifies that an entry with no
// text is rejected with HTTP 400.
func TestHostAPI_Feedback_EmptyTextReturns400(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	d := openTestDB(t)
	sc := newSidecarWithRole(t, "prism-test@worker-feedback", "prism-test", "worker", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/feedback",
		`{"timestamp":"2026-05-15T10:00:00Z","text":"","prism_version":"v1"}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "text is required") {
		t.Errorf("body %q: want 'text is required'", rr.Body.String())
	}
}

// TestHostAPI_Feedback_WorkerRoleAllowed verifies that worker-role sidecars
// (not just coordinators) are permitted to call /feedback.
func TestHostAPI_Feedback_WorkerRoleAllowed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	d := openTestDB(t)
	// Explicitly a worker.
	sc := newSidecarWithRole(t, "prism-test@worker-feedback", "prism-test", "worker", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/feedback",
		`{"timestamp":"2026-05-15T10:00:00Z","text":"worker note","prism_version":"v1"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("worker should be allowed; status = %d, body = %q", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Feedback_GetMethodNotAllowed verifies that GET /feedback returns
// HTTP 405 (the handler is POST-only).
func TestHostAPI_Feedback_GetMethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	d := openTestDB(t)
	sc := newSidecarWithRole(t, "prism-test@worker-feedback", "prism-test", "worker", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/feedback", "")

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, body = %q, want 405", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Feedback_XDGStateHomeHonoured verifies that the handler
// honours $XDG_STATE_HOME as the store path override — the same mechanism
// used by tests to avoid writing to the real home directory.
func TestHostAPI_Feedback_XDGStateHomeHonoured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	d := openTestDB(t)
	sc := newSidecarWithRole(t, "prism-test@worker-xdg", "prism-test", "worker", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/feedback",
		`{"timestamp":"2026-05-15T10:00:00Z","text":"xdg test","prism_version":"v1"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rr.Code, rr.Body.String())
	}

	var resp struct{ Path string `json:"path"` }
	decodeJSONBody(t, rr, &resp)

	expectedPath := filepath.Join(dir, "prism", "feedback.jsonl")
	if resp.Path != expectedPath {
		t.Errorf("path = %q, want %q", resp.Path, expectedPath)
	}
}
