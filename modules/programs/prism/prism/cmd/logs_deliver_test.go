package cmd

// Tests for the `prism logs --deliver=...` integration: ensures the log file
// content reaches the configured sink with the right shape and that
// readLogForDelivery honours --tail.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadLogForDelivery_FullFile(t *testing.T) {
	logFile := writeTempLogFile(t, "alpha\nbeta\ngamma\n")
	got, err := readLogForDelivery(logFile, false, 0)
	if err != nil {
		t.Fatalf("readLogForDelivery: %v", err)
	}
	if string(got) != "alpha\nbeta\ngamma\n" {
		t.Errorf("got %q", string(got))
	}
}

func TestReadLogForDelivery_TailZero_ReturnsEmpty(t *testing.T) {
	logFile := writeTempLogFile(t, "alpha\nbeta\n")
	got, err := readLogForDelivery(logFile, true, 0)
	if err != nil {
		t.Fatalf("readLogForDelivery: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty bytes, got %q", got)
	}
}

func TestReadLogForDelivery_TailN(t *testing.T) {
	logFile := writeTempLogFile(t, "alpha\nbeta\ngamma\ndelta\n")
	got, err := readLogForDelivery(logFile, true, 2)
	if err != nil {
		t.Fatalf("readLogForDelivery: %v", err)
	}
	if string(got) != "gamma\ndelta\n" {
		t.Errorf("got %q, want gamma\\ndelta\\n", string(got))
	}
}

// End-to-end: `prism logs --deliver=file:<path>` must atomically write the
// resolved log file content to <path> and print the structured success JSON
// to stdout. We exercise this by routing the deliver pipeline directly
// through readLogForDelivery + deliverContent (the same code path runLogs
// takes once flags are parsed).
func TestRunLogs_DeliverFile_EndToEnd(t *testing.T) {
	src := writeTempLogFile(t, "[prism sidecar] starting\n2026-01-01 sidecar: event\n")
	target := filepath.Join(t.TempDir(), "delivered.log")

	sink, err := parseDeliverFlag("file:" + target)
	if err != nil {
		t.Fatalf("parseDeliverFlag: %v", err)
	}
	content, err := readLogForDelivery(src, false, 0)
	if err != nil {
		t.Fatalf("readLogForDelivery: %v", err)
	}
	var status bytes.Buffer
	if err := deliverContent(sink, content, "text/plain", &status); err != nil {
		t.Fatalf("deliverContent: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("delivered content mismatch")
	}

	var resp struct {
		DeliveredTo string `json:"delivered_to"`
		Bytes       int    `json:"bytes"`
	}
	if err := json.Unmarshal(status.Bytes(), &resp); err != nil {
		t.Fatalf("status: %v", err)
	}
	if resp.Bytes != len(content) {
		t.Errorf("bytes = %d, want %d", resp.Bytes, len(content))
	}
}

// End-to-end webhook delivery: assert the body equals the file content and
// the Content-Type header matches the sink's expectations for the raw log.
func TestRunLogs_DeliverWebhook_EndToEnd(t *testing.T) {
	src := writeTempLogFile(t, "logline1\nlogline2\n")

	var (
		gotMethod      string
		gotContentType string
		gotBody        []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotBody = readAllBody(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink, _ := parseDeliverFlag("webhook:" + srv.URL)
	content, _ := readLogForDelivery(src, false, 0)
	var status bytes.Buffer
	if err := deliverContent(sink, content, "text/plain", &status); err != nil {
		t.Fatalf("deliverContent: %v", err)
	}

	if gotMethod != "POST" {
		t.Errorf("method = %q", gotMethod)
	}
	if gotContentType != "text/plain" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if string(gotBody) != "logline1\nlogline2\n" {
		t.Errorf("body = %q", gotBody)
	}
	if !strings.Contains(status.String(), `"status":200`) {
		t.Errorf("status JSON missing status:200: %s", status.String())
	}
}

func readAllBody(r *http.Request) []byte {
	buf := new(bytes.Buffer)
	if r.Body != nil {
		_, _ = buf.ReadFrom(r.Body)
		_ = r.Body.Close()
	}
	return buf.Bytes()
}
