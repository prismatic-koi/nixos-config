package cmd

// Tests for the container-side escalate proxy (proxyEscalate /
// proxyEscalateWithWriters). These tests stand up a mock host-API on a Unix
// socket, send an escalate request through proxyEscalateWithWriters, and
// assert that the stdout/stderr returned by the host are forwarded
// byte-for-byte to the caller's writers — closing the silent-success gap:
// without this round-trip, the container path is silent on success.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestProxyEscalate_ForwardsStdoutAndStderr verifies that on a 200 OK
// response carrying stdout/stderr fields, the proxy writes both streams
// to the caller's writers verbatim. This is the Part B AC for the
// container code path.
func TestProxyEscalate_ForwardsStdoutAndStderr(t *testing.T) {
	wantStdout := "prism escalate: OK delivered to myrepo@main (delivery_id=abc-123-def)\n"
	wantStderr := "prism escalate: OK delivered to myrepo@main (delivery_id=abc-123-def)\n"

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
	if err := proxyEscalateWithWriters(srv.apiURL(), "", "halp", false, "", &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proxyEscalateWithWriters: %v", err)
	}

	if stdoutBuf.String() != wantStdout {
		t.Errorf("stdout forwarded mismatch:\n  got:  %q\n  want: %q", stdoutBuf.String(), wantStdout)
	}
	if stderrBuf.String() != wantStderr {
		t.Errorf("stderr forwarded mismatch:\n  got:  %q\n  want: %q", stderrBuf.String(), wantStderr)
	}

	// Body shape: prompt is required, json/dedup_window are omitted when not set.
	select {
	case body := <-gotBody:
		if body["prompt"] != "halp" {
			t.Errorf("prompt = %v, want halp", body["prompt"])
		}
		if _, ok := body["json"]; ok {
			t.Errorf("json field present in body when jsonOut=false: %v", body["json"])
		}
		if _, ok := body["dedup_window"]; ok {
			t.Errorf("dedup_window present when not overridden: %v", body["dedup_window"])
		}
	default:
		t.Fatal("no request body received")
	}
}

// TestProxyEscalate_ForwardsJSONFlag verifies that --json is forwarded to
// the host-side child so its stdout carries the JSON envelope (and stderr
// the human mirror). The proxy then forwards both streams verbatim to the
// container's local stdout/stderr — preserving the human/json mutual
// exclusion on stdout.
func TestProxyEscalate_ForwardsJSONFlag(t *testing.T) {
	wantStdout := `{"delivered_to":"myrepo@main","delivery_id":"abc-123","replayed":false}` + "\n"
	wantStderr := "prism escalate: OK delivered to myrepo@main (delivery_id=abc-123)\n"

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
	if err := proxyEscalateWithWriters(srv.apiURL(), "", "halp", true, "", &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proxyEscalateWithWriters: %v", err)
	}

	if stdoutBuf.String() != wantStdout {
		t.Errorf("stdout forwarded mismatch:\n  got:  %q\n  want: %q", stdoutBuf.String(), wantStdout)
	}
	if stderrBuf.String() != wantStderr {
		t.Errorf("stderr forwarded mismatch:\n  got:  %q\n  want: %q", stderrBuf.String(), wantStderr)
	}
	// The forwarded stdout in --json mode must NOT contain the human line
	// (mutual exclusion held end-to-end, including across the proxy).
	if strings.Contains(stdoutBuf.String(), "prism escalate: OK") {
		t.Errorf("forwarded stdout contains human line in --json mode: %q", stdoutBuf.String())
	}

	// Body shape carries json=true.
	select {
	case body := <-gotBody:
		if body["json"] != true {
			t.Errorf("json field = %v, want true", body["json"])
		}
	default:
		t.Fatal("no request body received")
	}
}

// TestProxyEscalate_ForwardsToFlag verifies that --to is forwarded.
func TestProxyEscalate_ForwardsToFlag(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"stdout":"","stderr":""}`))
	})

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := proxyEscalateWithWriters(srv.apiURL(), "myrepo@coord-2", "halp", false, "", &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proxyEscalateWithWriters: %v", err)
	}
	select {
	case body := <-gotBody:
		if body["to"] != "myrepo@coord-2" {
			t.Errorf("to = %v, want myrepo@coord-2", body["to"])
		}
	default:
		t.Fatal("no request body received")
	}
}

// TestProxyEscalate_ForwardsDedupWindow verifies that --dedup-window is
// forwarded when explicitly set, and omitted otherwise.
func TestProxyEscalate_ForwardsDedupWindow(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"stdout":"","stderr":""}`))
	})

	var stdoutBuf, stderrBuf bytes.Buffer
	if err := proxyEscalateWithWriters(srv.apiURL(), "", "halp", false, "10m", &stdoutBuf, &stderrBuf); err != nil {
		t.Fatalf("proxyEscalateWithWriters: %v", err)
	}
	select {
	case body := <-gotBody:
		if body["dedup_window"] != "10m" {
			t.Errorf("dedup_window = %v, want 10m", body["dedup_window"])
		}
	default:
		t.Fatal("no request body received")
	}
}

// TestProxyEscalate_ForwardsErrorWithStdoutStderr verifies that on a non-2xx
// response carrying stdout/stderr alongside an error, the proxy forwards
// both streams to the caller's writers AND returns an error containing the
// underlying message. Parity with proxyCleanupToHostAPIWithWriters.
func TestProxyEscalate_ForwardsErrorWithStdoutStderr(t *testing.T) {
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{` +
			`"error":"escalate failed: exit status 1",` +
			`"stdout":"",` +
			`"stderr":"prism escalate: deliver prompt to myrepo@main: connection refused\n"` +
			`}`))
	})

	var stdoutBuf, stderrBuf bytes.Buffer
	err := proxyEscalateWithWriters(srv.apiURL(), "", "halp", false, "", &stdoutBuf, &stderrBuf)
	if err == nil {
		t.Fatal("expected error from proxyEscalateWithWriters on 500 response")
	}
	if !strings.Contains(err.Error(), "escalate failed: exit status 1") {
		t.Errorf("error %q should contain underlying cause", err.Error())
	}
	if !strings.Contains(stderrBuf.String(), "deliver prompt to myrepo@main") {
		t.Errorf("stderr on error not forwarded: got %q", stderrBuf.String())
	}
}
