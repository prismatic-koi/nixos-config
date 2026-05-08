package cmd

// Tests for the --deliver=<sink> plumbing used by `prism logs` (and any
// future commands that opt in). Covers parsing, atomic file write, webhook
// success / failure, and the unknown-scheme refusal.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── parseDeliverFlag ─────────────────────────────────────────────────────────

func TestParseDeliverFlag_DefaultsToStdout(t *testing.T) {
	cases := []string{"", "stdout"}
	for _, raw := range cases {
		s, err := parseDeliverFlag(raw)
		if err != nil {
			t.Errorf("raw=%q: unexpected error: %v", raw, err)
		}
		if s.kind != "stdout" {
			t.Errorf("raw=%q: kind = %q, want stdout", raw, s.kind)
		}
	}
}

func TestParseDeliverFlag_File(t *testing.T) {
	s, err := parseDeliverFlag("file:./out.log")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.kind != "file" || s.dest != "./out.log" {
		t.Errorf("got %+v, want kind=file dest=./out.log", s)
	}
}

func TestParseDeliverFlag_FileMissingPath(t *testing.T) {
	_, err := parseDeliverFlag("file:")
	if err == nil {
		t.Fatal("expected error for empty file path")
	}
	if !strings.Contains(err.Error(), "requires a path") {
		t.Errorf("err = %v; want path-required message", err)
	}
}

func TestParseDeliverFlag_Webhook(t *testing.T) {
	s, err := parseDeliverFlag("webhook:https://example.com/hook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.kind != "webhook" || s.dest != "https://example.com/hook" {
		t.Errorf("got %+v, want kind=webhook dest=https://example.com/hook", s)
	}
}

func TestParseDeliverFlag_WebhookHTTP(t *testing.T) {
	s, err := parseDeliverFlag("webhook:http://localhost:8080/hook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.kind != "webhook" || s.dest != "http://localhost:8080/hook" {
		t.Errorf("got %+v", s)
	}
}

func TestParseDeliverFlag_WebhookMissingURL(t *testing.T) {
	_, err := parseDeliverFlag("webhook:")
	if err == nil {
		t.Fatal("expected error for empty webhook URL")
	}
	if !strings.Contains(err.Error(), "requires a URL") {
		t.Errorf("err = %v; want URL-required message", err)
	}
}

func TestParseDeliverFlag_WebhookBadScheme(t *testing.T) {
	_, err := parseDeliverFlag("webhook:ftp://example.com")
	if err == nil {
		t.Fatal("expected error for ftp:// webhook URL")
	}
	if !strings.Contains(err.Error(), "http://") {
		t.Errorf("err = %v; want http://-required message", err)
	}
}

func TestParseDeliverFlag_UnknownScheme(t *testing.T) {
	_, err := parseDeliverFlag("s3://bucket/key")
	if err == nil {
		t.Fatal("expected error for unknown scheme")
	}
	if !strings.Contains(err.Error(), "unknown sink scheme") {
		t.Errorf("err = %v; want 'unknown sink scheme'", err)
	}
	// Refusal must enumerate the valid set (principle 3).
	for _, want := range []string{"stdout", "file:<path>", "webhook:<url>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

// ── deliverContent — stdout sink ─────────────────────────────────────────────

func TestDeliverContent_Stdout_WritesContentVerbatim(t *testing.T) {
	var buf bytes.Buffer
	sink, _ := parseDeliverFlag("stdout")
	if err := deliverContent(sink, []byte("hello world\n"), "text/plain", &buf); err != nil {
		t.Fatalf("deliverContent: %v", err)
	}
	if buf.String() != "hello world\n" {
		t.Errorf("got %q, want %q", buf.String(), "hello world\n")
	}
}

// ── deliverContent — file sink ───────────────────────────────────────────────

func TestDeliverContent_File_WritesAtomicallyAndReportsBytes(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.log")
	sink, err := parseDeliverFlag("file:" + target)
	if err != nil {
		t.Fatalf("parseDeliverFlag: %v", err)
	}
	content := []byte("line1\nline2\nline3\n")

	var status bytes.Buffer
	if err := deliverContent(sink, content, "text/plain", &status); err != nil {
		t.Fatalf("deliverContent: %v", err)
	}

	// File contents match input.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("file content = %q, want %q", got, content)
	}

	// Status JSON has the expected shape.
	var resp struct {
		DeliveredTo string `json:"delivered_to"`
		Bytes       int    `json:"bytes"`
	}
	if err := json.Unmarshal(status.Bytes(), &resp); err != nil {
		t.Fatalf("status JSON parse: %v (raw: %s)", err, status.String())
	}
	if resp.DeliveredTo != "file:"+target {
		t.Errorf("delivered_to = %q, want %q", resp.DeliveredTo, "file:"+target)
	}
	if resp.Bytes != len(content) {
		t.Errorf("bytes = %d, want %d", resp.Bytes, len(content))
	}

	// No leftover tempfile in the destination directory.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".prism-deliver-") {
			t.Errorf("tempfile leftover: %s", e.Name())
		}
	}
}

func TestDeliverContent_File_CreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "deeper", "out.log")
	sink, _ := parseDeliverFlag("file:" + target)
	if err := deliverContent(sink, []byte("x"), "text/plain", io.Discard); err != nil {
		t.Fatalf("deliverContent: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("destination not created: %v", err)
	}
}

// ── deliverContent — webhook sink ────────────────────────────────────────────

func TestDeliverContent_Webhook_Success(t *testing.T) {
	var (
		gotMethod      string
		gotContentType string
		gotBody        []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	sink, _ := parseDeliverFlag("webhook:" + srv.URL)
	var status bytes.Buffer
	if err := deliverContent(sink, []byte("payload"), "text/plain", &status); err != nil {
		t.Fatalf("deliverContent: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotContentType != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", gotContentType)
	}
	if string(gotBody) != "payload" {
		t.Errorf("body = %q, want payload", gotBody)
	}

	var resp struct {
		DeliveredTo string `json:"delivered_to"`
		Status      int    `json:"status"`
	}
	if err := json.Unmarshal(status.Bytes(), &resp); err != nil {
		t.Fatalf("parse status: %v (raw: %s)", err, status.String())
	}
	if resp.Status != http.StatusAccepted {
		t.Errorf("status = %d, want %d", resp.Status, http.StatusAccepted)
	}
	if resp.DeliveredTo != "webhook:"+srv.URL {
		t.Errorf("delivered_to = %q, want %q", resp.DeliveredTo, "webhook:"+srv.URL)
	}
}

func TestDeliverContent_Webhook_NDJSONContentType(t *testing.T) {
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink, _ := parseDeliverFlag("webhook:" + srv.URL)
	if err := deliverContent(sink, []byte(`{"f":1}`+"\n"), "application/x-ndjson", io.Discard); err != nil {
		t.Fatalf("deliverContent: %v", err)
	}
	if gotContentType != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", gotContentType)
	}
}

func TestDeliverContent_Webhook_4xxIsNonZeroExit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request: missing field"))
	}))
	defer srv.Close()

	sink, _ := parseDeliverFlag("webhook:" + srv.URL)
	err := deliverContent(sink, []byte("x"), "text/plain", io.Discard)
	if err == nil {
		t.Fatal("expected error for 4xx response")
	}
	// Error payload should be a JSON object with status + body.
	var resp struct {
		DeliveredTo string `json:"delivered_to"`
		Status      int    `json:"status"`
		Body        string `json:"body"`
	}
	if jsonErr := json.Unmarshal([]byte(err.Error()), &resp); jsonErr != nil {
		t.Fatalf("error not JSON: %v (raw: %s)", jsonErr, err.Error())
	}
	if resp.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.Status)
	}
	if !strings.Contains(resp.Body, "bad request") {
		t.Errorf("body = %q, want to contain 'bad request'", resp.Body)
	}
}

func TestDeliverContent_Webhook_5xxIsNonZeroExit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sink, _ := parseDeliverFlag("webhook:" + srv.URL)
	err := deliverContent(sink, []byte("x"), "text/plain", io.Discard)
	if err == nil {
		t.Fatal("expected error for 5xx response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v; want to mention 500", err)
	}
}

func TestDeliverContent_Webhook_BodyTruncated(t *testing.T) {
	// Server returns a body larger than webhookErrorBodyMax.
	huge := strings.Repeat("A", webhookErrorBodyMax*2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()

	sink, _ := parseDeliverFlag("webhook:" + srv.URL)
	err := deliverContent(sink, []byte("x"), "text/plain", io.Discard)
	if err == nil {
		t.Fatal("expected error")
	}
	var resp struct {
		Body      string `json:"body"`
		Truncated bool   `json:"truncated"`
	}
	if jsonErr := json.Unmarshal([]byte(err.Error()), &resp); jsonErr != nil {
		t.Fatalf("error not JSON: %v (raw: %s)", jsonErr, err.Error())
	}
	if !resp.Truncated {
		t.Errorf("expected truncated=true")
	}
	if len(resp.Body) != webhookErrorBodyMax {
		t.Errorf("body length = %d, want %d", len(resp.Body), webhookErrorBodyMax)
	}
}
