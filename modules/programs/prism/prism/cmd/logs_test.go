package cmd

// Tests for the prism logs subcommand.
//
// Covers:
//   - tailLinesFromReader: edge cases (empty, fewer lines than n, more lines, n=0)
//   - runLogsFull: reads and writes the full file content
//   - runLogsTail: reads the last N lines correctly
//   - runLogsFollow: exits immediately when session is already in a terminal state
//   - proxyLogsFromHostAPI: forwards correct query params, streams response body,
//     surfaces 404 errors from the host API

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// ── tailLinesFromReader ──────────────────────────────────────────────────────

func TestTailLinesFromReader_Empty(t *testing.T) {
	r := strings.NewReader("")
	lines, err := tailLinesFromReader(r, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("expected 0 lines, got %d: %v", len(lines), lines)
	}
}

func TestTailLinesFromReader_FewerLinesThanN(t *testing.T) {
	content := "line1\nline2\nline3\n"
	r := strings.NewReader(content)
	lines, err := tailLinesFromReader(r, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d: %v", len(lines), lines)
	}
	if lines[0] != "line1" || lines[1] != "line2" || lines[2] != "line3" {
		t.Errorf("unexpected lines: %v", lines)
	}
}

func TestTailLinesFromReader_ExactN(t *testing.T) {
	content := "a\nb\nc\n"
	r := strings.NewReader(content)
	lines, err := tailLinesFromReader(r, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d: %v", len(lines), lines)
	}
}

func TestTailLinesFromReader_MoreLinesThanN(t *testing.T) {
	content := "a\nb\nc\nd\ne\n"
	r := strings.NewReader(content)
	lines, err := tailLinesFromReader(r, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(lines), lines)
	}
	if lines[0] != "c" || lines[1] != "d" || lines[2] != "e" {
		t.Errorf("wrong lines: %v (want c, d, e)", lines)
	}
}

func TestTailLinesFromReader_NoTrailingNewline(t *testing.T) {
	content := "a\nb\nc"
	r := strings.NewReader(content)
	lines, err := tailLinesFromReader(r, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(lines), lines)
	}
	if lines[0] != "b" || lines[1] != "c" {
		t.Errorf("wrong lines: %v (want b, c)", lines)
	}
}

func TestTailLinesFromReader_NIsOne(t *testing.T) {
	content := "a\nb\nc\n"
	r := strings.NewReader(content)
	lines, err := tailLinesFromReader(r, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d: %v", len(lines), lines)
	}
	if lines[0] != "c" {
		t.Errorf("line = %q, want %q", lines[0], "c")
	}
}

// ── runLogsFull ──────────────────────────────────────────────────────────────

func TestRunLogsFull_WritesFullContent(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")

	content := "2026-01-01 sidecar: starting\n2026-01-01 sidecar: event: session.created\n"
	logFile := writeTempLogFile(t, content)

	out := captureStdoutFn(t, func() {
		if err := runLogsFull(logFile); err != nil {
			t.Errorf("runLogsFull: %v", err)
		}
	})
	if out != content {
		t.Errorf("output = %q, want %q", out, content)
	}
}

func TestRunLogsFull_EmptyFile(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	logFile := writeTempLogFile(t, "")

	out := captureStdoutFn(t, func() {
		if err := runLogsFull(logFile); err != nil {
			t.Errorf("runLogsFull: %v", err)
		}
	})
	if out != "" {
		t.Errorf("expected empty output for empty file, got %q", out)
	}
}

// ── runLogsTail ──────────────────────────────────────────────────────────────

func TestRunLogsTail_Zero_NoOutput(t *testing.T) {
	logFile := writeTempLogFile(t, "a\nb\nc\n")
	out := captureStdoutFn(t, func() {
		if err := runLogsTail(logFile, 0); err != nil {
			t.Errorf("runLogsTail: %v", err)
		}
	})
	if out != "" {
		t.Errorf("expected empty output for tail=0, got %q", out)
	}
}

func TestRunLogsTail_LastN(t *testing.T) {
	logFile := writeTempLogFile(t, "alpha\nbeta\ngamma\ndelta\n")
	out := captureStdoutFn(t, func() {
		if err := runLogsTail(logFile, 2); err != nil {
			t.Errorf("runLogsTail: %v", err)
		}
	})
	want := "gamma\ndelta\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestRunLogsTail_MoreThanAvailable(t *testing.T) {
	content := "only\ntwo\n"
	logFile := writeTempLogFile(t, content)
	out := captureStdoutFn(t, func() {
		if err := runLogsTail(logFile, 100); err != nil {
			t.Errorf("runLogsTail: %v", err)
		}
	})
	want := content
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

// ── runLogsFollow (host-side, already-finished case) ────────────────────────

func TestRunLogsFollow_AlreadyFinished_PrintsAndExits(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")

	// Create a temp DB and seed a finished session.
	dbPath := filepath.Join(t.TempDir(), "prism.db")
	SetTestDBPath(dbPath)
	t.Cleanup(func() { SetTestDBPath("") })

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()
	_ = d.UpsertStatus("myrepo@feat", "myrepo", "/wt", "finished", nil, nil)
	d.Close()

	content := "line1\nline2\nline3\n"
	logFile := writeTempLogFile(t, content)

	out := captureStdoutFn(t, func() {
		if err := runLogsFollow("myrepo@feat", logFile); err != nil {
			t.Errorf("runLogsFollow: %v", err)
		}
	})
	if out != content {
		t.Errorf("output = %q, want %q", out, content)
	}
}

// ── proxyLogsFromHostAPI ─────────────────────────────────────────────────────

// TestProxyLogsFromHostAPI_SendsCorrectQueryParams verifies that the proxy
// function sends the correct session and tail query parameters as a GET request.
func TestProxyLogsFromHostAPI_SendsCorrectQueryParams(t *testing.T) {
	var capturedPath string
	var capturedQuery string

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("log line 1\nlog line 2\n"))
	})

	var buf bytes.Buffer
	err := proxyLogsFromHostAPI(srv.apiURL(), "myrepo@main", 5, true, false, &buf)
	if err != nil {
		t.Fatalf("proxyLogsFromHostAPI: %v", err)
	}

	if capturedPath != "/logs" {
		t.Errorf("path = %q, want /logs", capturedPath)
	}
	// session param should be percent-encoded.
	if !strings.Contains(capturedQuery, "session=myrepo%40main") {
		t.Errorf("query %q should contain session=myrepo%%40main", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "tail=5") {
		t.Errorf("query %q should contain tail=5", capturedQuery)
	}
	if strings.Contains(capturedQuery, "follow") {
		t.Errorf("query %q should not contain follow (not set)", capturedQuery)
	}

	got := buf.String()
	if got != "log line 1\nlog line 2\n" {
		t.Errorf("body = %q, want log content", got)
	}
}

// TestProxyLogsFromHostAPI_NoTailParam verifies that when tailSet=false,
// the tail parameter is omitted from the query.
func TestProxyLogsFromHostAPI_NoTailParam(t *testing.T) {
	var capturedQuery string

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("full log\n"))
	})

	var buf bytes.Buffer
	err := proxyLogsFromHostAPI(srv.apiURL(), "myrepo@main", 0, false, false, &buf)
	if err != nil {
		t.Fatalf("proxyLogsFromHostAPI: %v", err)
	}

	if strings.Contains(capturedQuery, "tail") {
		t.Errorf("query %q should not contain tail when tailSet=false", capturedQuery)
	}
}

// TestProxyLogsFromHostAPI_Returns404AsError verifies that a 404 from the
// host API is surfaced as an error with the error message.
func TestProxyLogsFromHostAPI_Returns404AsError(t *testing.T) {
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		body, _ := json.Marshal(map[string]string{"error": "no log file for session myrepo@nonexistent"})
		_, _ = w.Write(body)
	})

	var buf bytes.Buffer
	err := proxyLogsFromHostAPI(srv.apiURL(), "myrepo@nonexistent", 0, false, false, &buf)
	if err == nil {
		t.Fatal("expected non-nil error for 404 response")
	}
	if !strings.Contains(err.Error(), "no log file for session") {
		t.Errorf("error %q should contain 'no log file for session'", err.Error())
	}
}

// TestProxyLogsFromHostAPI_FollowSendsFollowParam verifies that follow=true
// adds follow=true to the query.
func TestProxyLogsFromHostAPI_FollowSendsFollowParam(t *testing.T) {
	var capturedQuery string

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("streamed content\n"))
	})

	var buf bytes.Buffer
	// follow=true but server immediately closes: should not block.
	err := proxyLogsFromHostAPI(srv.apiURL(), "myrepo@main", 0, false, true, &buf)
	if err != nil {
		t.Fatalf("proxyLogsFromHostAPI: %v", err)
	}

	if !strings.Contains(capturedQuery, "follow=true") {
		t.Errorf("query %q should contain follow=true", capturedQuery)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// writeTempLogFile creates a temp file with the given content and returns its path.
func writeTempLogFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-sidecar-*.log")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if content != "" {
		if _, err := io.WriteString(f, content); err != nil {
			t.Fatalf("write temp log: %v", err)
		}
	}
	f.Close()
	return f.Name()
}

// captureStdoutFn captures stdout for the duration of fn and returns what was
// written. Named differently from captureStdout (checkin_test.go) to avoid a
// duplicate symbol in the same package.
func captureStdoutFn(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	return buf.String()
}
