package sidecar

// Tests for the host-API /escalate endpoint, focused on the cross-session
// integrity check that prevents a worker from escalating "from" any session
// other than its own. The check mirrors the rule applied by /prompt and
// /set-model in the same file. A regression here would let a non-coordinator
// mutate `agent_status.state`, emit a `session.escalated` bus event
// attributed to a victim, and pin that victim in `escalated` so legitimate
// `has finished` notifications are suppressed (review-security finding,
// PR #1524 round 1).

import (
	"net/http"
	"strings"
	"testing"
)

// TestHostAPI_Escalate_WorkerCannotImpersonate verifies that a worker session
// is rejected with HTTP 403 when it sets `from` to any session other than its
// own. The shell-out path must NOT be reached.
func TestHostAPI_Escalate_WorkerCannotImpersonate(t *testing.T) {
	d := openTestDB(t)
	// Use the role-and-binary helper so that even if the auth check failed
	// (regression) the shell-out would exit code 1, not block the test.
	sc := newSidecarWithRoleAndBinary(t, "myrepo@feature", "myrepo", "worker", d)

	body := `{"prompt":"halp","from":"myrepo@victim"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", body)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %q, want 403", rr.Code, rr.Body.String())
	}
	// The error message should name both the legitimate session and the
	// rejected `from` value so the caller can diagnose the mistake.
	if !strings.Contains(rr.Body.String(), "myrepo@feature") {
		t.Errorf("body %q: want message naming the calling session", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "myrepo@victim") {
		t.Errorf("body %q: want message naming the rejected from value", rr.Body.String())
	}
}

// TestHostAPI_Escalate_WorkerOwnSessionAccepted verifies the legitimate path:
// a worker setting `from` to its own session name is accepted (the auth check
// passes; the request then reaches the shell-out which fails with 500 on the
// stub binary — that's expected and proves we got past the 403 gate).
func TestHostAPI_Escalate_WorkerOwnSessionAccepted(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRoleAndBinary(t, "myrepo@feature", "myrepo", "worker", d)

	body := `{"prompt":"halp","from":"myrepo@feature"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", body)

	// 500 means the auth check passed and the stub binary rejected the call.
	// 403 would mean the auth check rejected it (regression).
	if rr.Code == http.StatusForbidden {
		t.Fatalf("worker rejected for its own session: %q", rr.Body.String())
	}
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %q, want 500 (stub binary fails) or 200", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Escalate_WorkerEmptyFromAccepted verifies that the common path
// (worker omits `from`, sidecar substitutes its own session name) is accepted.
// Same shape as the own-session test: 500 from the stub binary, never 403.
func TestHostAPI_Escalate_WorkerEmptyFromAccepted(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRoleAndBinary(t, "myrepo@feature", "myrepo", "worker", d)

	body := `{"prompt":"halp"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", body)

	if rr.Code == http.StatusForbidden {
		t.Fatalf("worker rejected when from was empty: %q", rr.Body.String())
	}
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %q, want 500 (stub binary fails)", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Escalate_CoordinatorCanProxy verifies that a coordinator session
// IS allowed to escalate on behalf of a different session. This is needed so
// that future tooling (e.g. an admin UI or an automated supervisor) can drive
// escalations through a coordinator's host-API socket.
func TestHostAPI_Escalate_CoordinatorCanProxy(t *testing.T) {
	d := openTestDB(t)
	// Seed the coordinator row so isCoordinatorSession reads back the role.
	role := "coordinator"
	if err := d.UpsertStatusWithRootAgent("myrepo@main", "myrepo", "/wt", "active", nil, nil, &role, nil); err != nil {
		t.Fatalf("seed coordinator: %v", err)
	}
	sc := newSidecarWithRoleAndBinary(t, "myrepo@main", "myrepo", "coordinator", d)

	body := `{"prompt":"halp","from":"myrepo@feature"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", body)

	if rr.Code == http.StatusForbidden {
		t.Fatalf("coordinator rejected when proxying for another session: %q", rr.Body.String())
	}
	// Either OK or 500 (from the stub binary) is fine — we only care that the
	// auth gate did NOT 403.
	if rr.Code != http.StatusOK && rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %q, want 200 or 500", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Escalate_EmptyPromptRejected verifies the existing 400 guard
// for missing prompt remains intact alongside the new auth check.
func TestHostAPI_Escalate_EmptyPromptRejected(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRoleAndBinary(t, "myrepo@feature", "myrepo", "worker", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", `{"prompt":"   "}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "prompt is required") {
		t.Errorf("body %q: want 'prompt is required'", rr.Body.String())
	}
}

// TestHostAPI_Escalate_MalformedJSONRejected verifies the existing 400 guard
// for malformed JSON remains intact.
func TestHostAPI_Escalate_MalformedJSONRejected(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRoleAndBinary(t, "myrepo@feature", "myrepo", "worker", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/escalate", `{not valid`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Escalate_GetMethodRejected verifies the requirePost guard is
// applied (consistent with sibling endpoints).
func TestHostAPI_Escalate_GetMethodRejected(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRoleAndBinary(t, "myrepo@feature", "myrepo", "worker", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/escalate", "")

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}
