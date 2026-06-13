// Tests for the /cleanup host-API endpoint's stdout/stderr forwarding
// behaviour (issue #1527). Without this, the container path was silent on
// success because the previous handler captured CombinedOutput and discarded
// the captured bytes before returning {}.
//
// These tests use a stub binary that writes deterministic content to stdout
// and/or stderr and either exits 0 (success) or non-zero (failure) so we can
// assert the response shape independently of a real `prism cleanup` run.

package sidecar

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// newSidecarWithCleanupStub returns a Sidecar whose host-API /cleanup handler
// will exec a shell script we control instead of the real prism binary. The
// caller supplies the script body (which is wrapped in a `#!/bin/sh` header).
func newSidecarWithCleanupStub(t *testing.T, sessionName, repo, role, scriptBody string) *Sidecar {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	d := openTestDB(t)
	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	if err := os.WriteFile(stubPath, []byte("#!/bin/sh\n"+scriptBody+"\n"), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
	cfg := Config{
		SessionName:     sessionName,
		Repo:            repo,
		Worktree:        "/tmp/" + sessionName,
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           newTestClock(),
		AgentRole:       role,
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	}
	return New(cfg)
}

// TestHostAPI_Cleanup_ForwardsStdoutAndStderr_Success verifies that the
// /cleanup handler captures stdout and stderr from the spawned subprocess
// separately and surfaces both in the JSON response. Issue #1527 AC #1.
func TestHostAPI_Cleanup_ForwardsStdoutAndStderr_Success(t *testing.T) {
	// Stub writes the AC-required progress lines to stdout and a warning to
	// stderr, then exits 0. The handler must put the stdout content into the
	// response's "stdout" field and the stderr content into "stderr".
	script := `printf 'removing worktree /tmp/wt...\n'
printf 'deleting branch some-branch\n'
printf 'killing session myrepo@some-branch\n'
printf 'done\n'
printf '[prism] warning: branch delete: stale ref\n' 1>&2
exit 0`

	sc := newSidecarWithCleanupStub(t, "myrepo@main", "myrepo", "coordinator", script)
	rr := doHostAPI(t, sc, http.MethodPost, "/cleanup",
		`{"session":"myrepo@some-branch","yes":true}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}
	decodeJSONBody(t, rr, &resp)
	wantStdout := "removing worktree /tmp/wt...\n" +
		"deleting branch some-branch\n" +
		"killing session myrepo@some-branch\n" +
		"done\n"
	if resp.Stdout != wantStdout {
		t.Errorf("stdout mismatch:\n  got:  %q\n  want: %q", resp.Stdout, wantStdout)
	}
	wantStderr := "[prism] warning: branch delete: stale ref\n"
	if resp.Stderr != wantStderr {
		t.Errorf("stderr mismatch:\n  got:  %q\n  want: %q", resp.Stderr, wantStderr)
	}
}

// TestHostAPI_Cleanup_ForwardsStdoutAndStderr_Failure verifies that even on
// non-zero exit the captured stdout and stderr are forwarded alongside the
// error string. This addresses the "error message names the wrong layer"
// observation in the comment on issue #1527: the agent must see the underlying
// cause (e.g. "archive directory already exists") rather than just the outer
// transport's "exit status 1".
func TestHostAPI_Cleanup_ForwardsStdoutAndStderr_Failure(t *testing.T) {
	script := `printf 'removing worktree /tmp/wt...\n'
printf 'archive directory already exists: /home/u/.local/share/prism/archive/myrepo/abc\n' 1>&2
exit 1`

	sc := newSidecarWithCleanupStub(t, "myrepo@main", "myrepo", "coordinator", script)
	rr := doHostAPI(t, sc, http.MethodPost, "/cleanup",
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
		t.Errorf("expected non-empty error field; got empty")
	}
	if resp.Stdout != "removing worktree /tmp/wt...\n" {
		t.Errorf("stdout mismatch on failure: got %q", resp.Stdout)
	}
	if resp.Stderr == "" || resp.Stderr != "archive directory already exists: /home/u/.local/share/prism/archive/myrepo/abc\n" {
		t.Errorf("stderr mismatch on failure: got %q", resp.Stderr)
	}
}

// TestHostAPI_Cleanup_ForwardsJSONFlag verifies that the json flag from the
// request body is propagated to the spawned subprocess as --json, so that
// container-side `prism cleanup --yes --json` reaches the host-side process
// with --json set.
func TestHostAPI_Cleanup_ForwardsJSONFlag(t *testing.T) {
	// Stub echoes its argv to stdout so we can assert --json was forwarded.
	script := `printf '%s\n' "$@"
exit 0`

	sc := newSidecarWithCleanupStub(t, "myrepo@main", "myrepo", "coordinator", script)
	rr := doHostAPI(t, sc, http.MethodPost, "/cleanup",
		`{"session":"myrepo@some-branch","yes":true,"json":true}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Stdout string `json:"stdout"`
	}
	decodeJSONBody(t, rr, &resp)
	want := "cleanup\n--session\nmyrepo@some-branch\n--yes\n--json\n"
	if resp.Stdout != want {
		t.Errorf("forwarded argv mismatch:\n  got:  %q\n  want: %q", resp.Stdout, want)
	}
}
