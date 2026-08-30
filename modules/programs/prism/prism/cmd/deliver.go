package cmd

// deliver.go implements the `--deliver=<sink>` flag plumbing used by
// `prism logs` (raw sidecar log and `--harness-events` JSONL frames).
//
// Three sinks are supported:
//
//	stdout                — default, write content directly to w
//	file:<path>           — atomic write (tempfile + rename) so a partial
//	                        deliver cannot leave a half-written file
//	webhook:<url>         — HTTP POST with a configurable Content-Type;
//	                        success prints {"delivered_to":..., "status":...}
//	                        and 4xx/5xx are reported as a non-zero exit
//
// Unknown schemes return a structured refusal naming the valid set.
// The caller
// (cmd/logs.go, cmd/logs_harness.go) is responsible for buffering the
// content into a []byte before invoking deliver — both surfaces today
// already read from the on-disk log or the DB into a buffer.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DeliverSinkStdout is the literal value for `--deliver=stdout`.
const DeliverSinkStdout = "stdout"

// validDeliverSchemes lists the accepted scheme prefixes for `--deliver`.
// Used by the unknown-scheme refusal message so the supported set is
// always in sync with the parser.
var validDeliverSchemes = []string{"stdout", "file:<path>", "webhook:<url>"}

// deliverSink is the parsed form of a `--deliver=...` flag value.
type deliverSink struct {
	// kind is one of "stdout", "file", or "webhook". Empty on a parse error.
	kind string
	// dest is the file path (kind=="file") or URL (kind=="webhook"). Empty
	// for kind=="stdout".
	dest string
}

// parseDeliverFlag parses a `--deliver=<value>` flag value into a deliverSink.
//
// An empty value is treated as the default (stdout) so callers can pass the
// raw flag value without checking for emptiness first.
//
// Unknown schemes (including a missing path/URL after a known scheme) return
// a structured error naming the valid set, consistent with principle 3.
func parseDeliverFlag(raw string) (deliverSink, error) {
	if raw == "" || raw == DeliverSinkStdout {
		return deliverSink{kind: "stdout"}, nil
	}
	if strings.HasPrefix(raw, "file:") {
		path := strings.TrimPrefix(raw, "file:")
		if path == "" {
			return deliverSink{}, fmt.Errorf(
				"--deliver=file: requires a path (e.g. --deliver=file:./out.log); valid sinks: %s",
				strings.Join(validDeliverSchemes, ", "),
			)
		}
		return deliverSink{kind: "file", dest: path}, nil
	}
	if strings.HasPrefix(raw, "webhook:") {
		dest := strings.TrimPrefix(raw, "webhook:")
		if dest == "" {
			return deliverSink{}, fmt.Errorf(
				"--deliver=webhook: requires a URL (e.g. --deliver=webhook:https://example.com/hook); valid sinks: %s",
				strings.Join(validDeliverSchemes, ", "),
			)
		}
		if !strings.HasPrefix(dest, "http://") && !strings.HasPrefix(dest, "https://") {
			return deliverSink{}, fmt.Errorf(
				"--deliver=webhook:%s: URL must start with http:// or https://",
				dest,
			)
		}
		return deliverSink{kind: "webhook", dest: dest}, nil
	}
	return deliverSink{}, fmt.Errorf(
		"--deliver=%s: unknown sink scheme; valid sinks are: %s",
		raw, strings.Join(validDeliverSchemes, ", "),
	)
}

// deliverHTTPClient is the HTTP client used for webhook delivery. Overridden
// in tests via the package-level variable so test webservers can intercept
// the request without monkey-patching net/http.
var deliverHTTPClient = &http.Client{Timeout: 30 * time.Second}

// deliverContent routes content to the configured sink. On success, a JSON
// status object is written to statusW (typically os.Stdout) so the caller
// gets a structured confirmation regardless of where the artifact landed.
//
// For sink.kind == "stdout", content is written directly to statusW (the
// default behaviour preserves backwards compatibility — no JSON wrapper).
//
// contentType applies only to webhook delivery (e.g. "text/plain" for the
// raw sidecar log, "application/x-ndjson" for `--harness-events`).
func deliverContent(sink deliverSink, content []byte, contentType string, statusW io.Writer) error {
	switch sink.kind {
	case "stdout", "":
		_, err := statusW.Write(content)
		return err
	case "file":
		return deliverToFile(sink.dest, content, statusW)
	case "webhook":
		return deliverToWebhook(sink.dest, content, contentType, statusW)
	default:
		return fmt.Errorf(
			"--deliver: unknown sink kind %q; valid sinks are: %s",
			sink.kind, strings.Join(validDeliverSchemes, ", "),
		)
	}
}

// deliverToFile writes content to path atomically: write to a sibling
// tempfile and rename on success. A partial deliver therefore cannot leave
// a half-written destination file. On success a JSON object describing the
// delivery is written to statusW.
func deliverToFile(path string, content []byte, statusW io.Writer) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("--deliver=file:%s: resolve path: %w", path, err)
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("--deliver=file:%s: create parent directory: %w", path, err)
	}
	tmp, err := os.CreateTemp(dir, ".prism-deliver-*.tmp")
	if err != nil {
		return fmt.Errorf("--deliver=file:%s: create tempfile: %w", path, err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if anything below fails before the rename.
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("--deliver=file:%s: write tempfile: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("--deliver=file:%s: close tempfile: %w", path, err)
	}
	if err := os.Rename(tmpPath, abs); err != nil {
		return fmt.Errorf("--deliver=file:%s: rename: %w", path, err)
	}
	cleanup = false
	resp := struct {
		DeliveredTo string `json:"delivered_to"`
		Bytes       int    `json:"bytes"`
	}{
		DeliveredTo: "file:" + path,
		Bytes:       len(content),
	}
	enc := json.NewEncoder(statusW)
	return enc.Encode(resp)
}

// webhookErrorBodyMax bounds how many bytes of a 4xx/5xx response body we
// echo back to the caller. The body is truncated for safety — endpoints
// can return arbitrarily large HTML error pages and we don't want to flood
// the operator's terminal with them.
const webhookErrorBodyMax = 4 * 1024

// deliverToWebhook POSTs content to url with the given Content-Type and
// reports the response status. 4xx / 5xx responses become a non-zero exit
// (a returned error), with the status code and a truncated body included
// in the structured error message so the operator can see what the server
// said. The local content is not lost — both prism-logs callers read on
// demand and a webhook failure does not delete or modify the source.
func deliverToWebhook(url string, content []byte, contentType string, statusW io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("--deliver=webhook:%s: build request: %w", url, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := deliverHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("--deliver=webhook:%s: POST: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, webhookErrorBodyMax+1))
		truncated := len(body) > webhookErrorBodyMax
		if truncated {
			body = body[:webhookErrorBodyMax]
		}
		errResp := struct {
			DeliveredTo string `json:"delivered_to"`
			Status      int    `json:"status"`
			Body        string `json:"body"`
			Truncated   bool   `json:"truncated,omitempty"`
		}{
			DeliveredTo: "webhook:" + url,
			Status:      resp.StatusCode,
			Body:        string(body),
			Truncated:   truncated,
		}
		marshalled, _ := json.Marshal(errResp)
		return errors.New(string(marshalled))
	}
	// Drain remaining body so the connection can be reused and we don't leak
	// goroutines on the server side.
	_, _ = io.Copy(io.Discard, resp.Body)
	out := struct {
		DeliveredTo string `json:"delivered_to"`
		Status      int    `json:"status"`
	}{
		DeliveredTo: "webhook:" + url,
		Status:      resp.StatusCode,
	}
	enc := json.NewEncoder(statusW)
	return enc.Encode(out)
}
