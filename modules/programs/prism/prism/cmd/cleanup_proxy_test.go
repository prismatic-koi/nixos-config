// Tests for the container-side cleanup proxy (proxyCleanupToHostAPI).
//
// These tests stand up a mock host-API on a Unix socket, send a cleanup
// request through proxyCleanupToHostAPIWithWriters, and assert that the
// stdout/stderr returned by the host are forwarded byte-for-byte to the
// caller's writers. This is the inverse direction of the host-side test in
// internal/sidecar/host_api_cleanup_test.go: together they cover both ends
// of the wire (issue #1527 AC #1).

package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestProxyCleanupToHostAPI_ForwardsStdoutAndStderr verifies that on a 200 OK
// response carrying stdout/stderr fields, the proxy writes both to the
// caller's writers verbatim.
func TestProxyCleanupToHostAPI_ForwardsStdoutAndStderr(t *testing.T) {
	wantStdout := "removing worktree /tmp/wt...\n" +
		"deleting branch some-branch\n" +
		"killing session myrepo@some-branch\n" +
		"done\n"
	wantStderr := "[prism] warning: branch delete: stale ref\n"

	// Capture the request so we can assert the body shape too.
	gotBody := make(chan map[string]any, 1)
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case gotBody <- body:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"stdout":` + jsonStringMust(wantStdout) +
			`,"stderr":` + jsonStringMust(wantStderr) + `}`))
	})

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := proxyCleanupToHostAPIWithWriters(srv.apiURL(), "myrepo@some-branch", true, false, false, &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proxyCleanupToHostAPI: %v", err)
	}
	if stdoutBuf.String() != wantStdout {
		t.Errorf("stdout forwarded mismatch:\n  got:  %q\n  want: %q", stdoutBuf.String(), wantStdout)
	}
	if stderrBuf.String() != wantStderr {
		t.Errorf("stderr forwarded mismatch:\n  got:  %q\n  want: %q", stderrBuf.String(), wantStderr)
	}

	// Body shape: yes=true, json=false, session=...
	select {
	case body := <-gotBody:
		if body["session"] != "myrepo@some-branch" {
			t.Errorf("session = %v, want myrepo@some-branch", body["session"])
		}
		if body["yes"] != true {
			t.Errorf("yes = %v, want true", body["yes"])
		}
		if body["json"] != false {
			t.Errorf("json = %v, want false", body["json"])
		}
	default:
		t.Fatal("no request body received")
	}
}

// TestProxyCleanupToHostAPI_ForwardsErrorWithStdoutStderr verifies that on a
// non-2xx response carrying stdout/stderr alongside an error, the proxy
// forwards both streams to the caller's writers AND returns an error
// containing the underlying message. This addresses the
// "error message names the wrong layer" observation in the comment on
// issue #1527: the agent must see the underlying cause, not just the outer
// transport's exit shape.
func TestProxyCleanupToHostAPI_ForwardsErrorWithStdoutStderr(t *testing.T) {
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{` +
			`"error":"cleanup failed: exit status 1",` +
			`"stdout":"removing worktree /tmp/wt...\n",` +
			`"stderr":"archive directory already exists: /home/u/abc\n"` +
			`}`))
	})

	var stdoutBuf, stderrBuf bytes.Buffer
	err := proxyCleanupToHostAPIWithWriters(srv.apiURL(), "myrepo@some-branch", true, false, false, &stdoutBuf, &stderrBuf)
	if err == nil {
		t.Fatal("expected error from proxyCleanupToHostAPI on 500 response")
	}
	if !strings.Contains(err.Error(), "cleanup failed: exit status 1") {
		t.Errorf("error %q should contain underlying cause", err.Error())
	}
	if stdoutBuf.String() != "removing worktree /tmp/wt...\n" {
		t.Errorf("stdout on error: got %q, want %q", stdoutBuf.String(), "removing worktree /tmp/wt...\n")
	}
	if stderrBuf.String() != "archive directory already exists: /home/u/abc\n" {
		t.Errorf("stderr on error: got %q", stderrBuf.String())
	}
}

// TestProxyCleanupToHostAPI_PassesJSONFlag verifies the proxy sends the
// json flag through the request body so the host invokes `prism cleanup`
// with --json.
func TestProxyCleanupToHostAPI_PassesJSONFlag(t *testing.T) {
	gotBody := make(chan map[string]any, 1)
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case gotBody <- body:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Mimic a JSON-mode cleanup result on stdout.
		_, _ = w.Write([]byte(`{"stdout":"{\"session\":\"myrepo@b\",\"worktree_removed\":\"/tmp/wt\",\"branch_deleted\":\"b\",\"session_killed\":true}\n","stderr":""}`))
	})

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := proxyCleanupToHostAPIWithWriters(srv.apiURL(), "myrepo@b", true, true, false, &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proxyCleanupToHostAPI: %v", err)
	}
	select {
	case body := <-gotBody:
		if body["json"] != true {
			t.Errorf("json flag = %v, want true", body["json"])
		}
	default:
		t.Fatal("no request body received")
	}
	// And the JSON body the host emitted on stdout is forwarded verbatim.
	want := `{"session":"myrepo@b","worktree_removed":"/tmp/wt","branch_deleted":"b","session_killed":true}` + "\n"
	if stdoutBuf.String() != want {
		t.Errorf("forwarded stdout mismatch:\n  got:  %q\n  want: %q", stdoutBuf.String(), want)
	}
}

// jsonStringMust returns a JSON-encoded string literal of s. Used by the
// inline string-builder responses in this file so callers don't need to
// worry about escaping the embedded newlines.
func jsonStringMust(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Compile-time guard: io.Discard must satisfy io.Writer (used by some helpers
// elsewhere). Keeps the import live without affecting test behaviour.
var _ io.Writer = io.Discard
