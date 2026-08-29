// Tests for the /close host-API endpoint. The endpoint mirrors
// /cleanup: a coordinator-only POST that shells out to a host-side prism
// binary and forwards stdout/stderr verbatim. These tests use a shell-script
// stub bound to PrismBinaryPath in place of the real `prism close` so we can
// assert the argv-forwarding contract without exercising the full close flow.

package sidecar

import (
	"net/http"
	"testing"
)

// TestHostAPI_Close_ForwardsAllFlags verifies that --yes, --json,
// --keep-worktree, and --remove-worktree are all forwarded to the spawned
// `prism close` subprocess when their request-body counterparts are set.
func TestHostAPI_Close_ForwardsAllFlags(t *testing.T) {
	// Stub echoes its argv (one per line) so we can match against it.
	script := `printf '%s\n' "$@"
exit 0`

	sc := newSidecarWithCleanupStub(t, "myrepo@main", "myrepo", "coordinator", script)
	rr := doHostAPI(t, sc, http.MethodPost, "/close",
		`{"session":"myrepo@some-branch","yes":true,"json":true,"keep_worktree":true}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}
	decodeJSONBody(t, rr, &resp)
	want := "close\n--session\nmyrepo@some-branch\n--yes\n--json\n--keep-worktree\n"
	if resp.Stdout != want {
		t.Errorf("forwarded argv mismatch:\n  got:  %q\n  want: %q", resp.Stdout, want)
	}
}

// TestHostAPI_Close_ForwardsRemoveWorktreeFlag verifies the symmetric
// --remove-worktree case.
func TestHostAPI_Close_ForwardsRemoveWorktreeFlag(t *testing.T) {
	script := `printf '%s\n' "$@"
exit 0`

	sc := newSidecarWithCleanupStub(t, "myrepo@main", "myrepo", "coordinator", script)
	rr := doHostAPI(t, sc, http.MethodPost, "/close",
		`{"session":"myrepo@some-branch","yes":true,"remove_worktree":true}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Stdout string `json:"stdout"`
	}
	decodeJSONBody(t, rr, &resp)
	want := "close\n--session\nmyrepo@some-branch\n--yes\n--remove-worktree\n"
	if resp.Stdout != want {
		t.Errorf("forwarded argv mismatch:\n  got:  %q\n  want: %q", resp.Stdout, want)
	}
}

// TestHostAPI_Close_DefaultOmitsForceFlags verifies that when neither force
// flag is set, the host-side argv carries only --yes/--json (matching the
// proxy's "omit when false" body shape).
func TestHostAPI_Close_DefaultOmitsForceFlags(t *testing.T) {
	script := `printf '%s\n' "$@"
exit 0`

	sc := newSidecarWithCleanupStub(t, "myrepo@main", "myrepo", "coordinator", script)
	rr := doHostAPI(t, sc, http.MethodPost, "/close",
		`{"session":"myrepo@some-branch","yes":true}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Stdout string `json:"stdout"`
	}
	decodeJSONBody(t, rr, &resp)
	want := "close\n--session\nmyrepo@some-branch\n--yes\n"
	if resp.Stdout != want {
		t.Errorf("forwarded argv mismatch:\n  got:  %q\n  want: %q", resp.Stdout, want)
	}
}

// TestHostAPI_Close_ForwardsStdoutAndStderrOnSuccess verifies the success
// path of the /close handler: the spawned subprocess's stdout and stderr are
// captured separately and surfaced in the JSON response, matching /cleanup.
func TestHostAPI_Close_ForwardsStdoutAndStderrOnSuccess(t *testing.T) {
	script := `printf 'closing session myrepo@some-branch...\n'
printf '[prism] info: keeping worktree (open PR detected)\n' 1>&2
exit 0`

	sc := newSidecarWithCleanupStub(t, "myrepo@main", "myrepo", "coordinator", script)
	rr := doHostAPI(t, sc, http.MethodPost, "/close",
		`{"session":"myrepo@some-branch","yes":true}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}
	decodeJSONBody(t, rr, &resp)
	if resp.Stdout != "closing session myrepo@some-branch...\n" {
		t.Errorf("stdout mismatch: got %q", resp.Stdout)
	}
	if resp.Stderr != "[prism] info: keeping worktree (open PR detected)\n" {
		t.Errorf("stderr mismatch: got %q", resp.Stderr)
	}
}

// TestHostAPI_Close_ForwardsStdoutAndStderrOnFailure verifies the failure
// path: a non-zero exit still produces stdout/stderr in the JSON response
// alongside the error field. Mirrors /cleanup's contract.
func TestHostAPI_Close_ForwardsStdoutAndStderrOnFailure(t *testing.T) {
	script := `printf 'closing session myrepo@some-branch...\n'
printf '[prism] error: tmux session not found\n' 1>&2
exit 1`

	sc := newSidecarWithCleanupStub(t, "myrepo@main", "myrepo", "coordinator", script)
	rr := doHostAPI(t, sc, http.MethodPost, "/close",
		`{"session":"myrepo@some-branch","yes":true}`)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error  string `json:"error"`
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}
	decodeJSONBody(t, rr, &resp)
	if resp.Error == "" {
		t.Errorf("expected non-empty error field on 500; got empty")
	}
	if resp.Stdout != "closing session myrepo@some-branch...\n" {
		t.Errorf("stdout mismatch on failure: got %q", resp.Stdout)
	}
	if resp.Stderr != "[prism] error: tmux session not found\n" {
		t.Errorf("stderr mismatch on failure: got %q", resp.Stderr)
	}
}

// TestHostAPI_Close_RejectsMissingSession verifies the request-validation
// guard: an empty "session" field is a 400, matching /cleanup.
func TestHostAPI_Close_RejectsMissingSession(t *testing.T) {
	script := `exit 0`
	sc := newSidecarWithCleanupStub(t, "myrepo@main", "myrepo", "coordinator", script)
	rr := doHostAPI(t, sc, http.MethodPost, "/close", `{"session":"","yes":true}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Close_RequiresPost verifies that GET /close is rejected with
// 405 Method Not Allowed (matches the requirePost guard on /cleanup).
func TestHostAPI_Close_RequiresPost(t *testing.T) {
	script := `exit 0`
	sc := newSidecarWithCleanupStub(t, "myrepo@main", "myrepo", "coordinator", script)
	rr := doHostAPI(t, sc, http.MethodGet, "/close", "")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body=%s", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Close_RequiresCoordinator verifies that worker-role sidecars
// cannot invoke /close (parity with /cleanup).
func TestHostAPI_Close_RequiresCoordinator(t *testing.T) {
	script := `exit 0`
	sc := newSidecarWithCleanupStub(t, "myrepo@feature", "myrepo", "worker", script)
	rr := doHostAPI(t, sc, http.MethodPost, "/close",
		`{"session":"myrepo@feature","yes":true}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Close_ForwardsKeepWorktreeToCleanup verifies the /cleanup
// endpoint also accepts and forwards keep_worktree (the parity surface for
// coordinators that want to soft-close via /cleanup without going through
// /close's decision tree).
func TestHostAPI_Close_ForwardsKeepWorktreeToCleanup(t *testing.T) {
	script := `printf '%s\n' "$@"
exit 0`

	sc := newSidecarWithCleanupStub(t, "myrepo@main", "myrepo", "coordinator", script)
	rr := doHostAPI(t, sc, http.MethodPost, "/cleanup",
		`{"session":"myrepo@some-branch","yes":true,"keep_worktree":true}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Stdout string `json:"stdout"`
	}
	decodeJSONBody(t, rr, &resp)
	want := "cleanup\n--session\nmyrepo@some-branch\n--yes\n--keep-worktree\n"
	if resp.Stdout != want {
		t.Errorf("forwarded argv mismatch:\n  got:  %q\n  want: %q", resp.Stdout, want)
	}
}
