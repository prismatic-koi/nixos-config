package sidecar

// Tests for the host-API /event endpoint added to fix the shadow-DB issue for
// prism event subcommands running inside containers (issue #1254).
//
// These tests exercise the hostAPIHandler() method directly without spinning
// up a real Unix socket server. The shape mirrors sidecar_merge_test.go from
// #1043.
//
// Note: the success path (kind + session both valid, event written to host DB)
// is covered at the cmd level in cmd/event_proxy_test.go via a fake HTTP
// server. Sidecar-level tests here focus on the validation rejection paths
// that the handler enforces before shelling out to prism event.

import (
	"net/http"
	"strings"
	"testing"
)

// ── /event — validation rejections ───────────────────────────────────────────

// TestHostAPI_Event_UnknownKindReturns400 verifies that posting an unknown
// event kind to /event returns HTTP 400 with a message listing the valid kinds.
func TestHostAPI_Event_UnknownKindReturns400(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "worker", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/event",
		`{"kind":"not-a-real-kind","session":"myrepo@main"}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "unknown event kind") {
		t.Errorf("body %q: want message containing 'unknown event kind'", body)
	}
	// The error should list valid kinds so the caller can diagnose the problem.
	if !strings.Contains(body, "compaction") {
		t.Errorf("body %q: want valid-kind list containing 'compaction'", body)
	}
}

// TestHostAPI_Event_EmptySessionReturns400 verifies that posting a valid kind
// but an empty session string to /event returns HTTP 400.
func TestHostAPI_Event_EmptySessionReturns400(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "worker", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/event",
		`{"kind":"compaction","session":""}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "session is required") {
		t.Errorf("body %q: want message containing 'session is required'", body)
	}
}

// TestHostAPI_Event_WhitespaceOnlySessionReturns400 verifies that a session
// containing only whitespace is also rejected (TrimSpace guard).
func TestHostAPI_Event_WhitespaceOnlySessionReturns400(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "worker", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/event",
		`{"kind":"compaction","session":"   "}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Event_MalformedJSONReturns400 verifies that a malformed request
// body is rejected with HTTP 400.
func TestHostAPI_Event_MalformedJSONReturns400(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "worker", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/event", `{not valid json`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
}
