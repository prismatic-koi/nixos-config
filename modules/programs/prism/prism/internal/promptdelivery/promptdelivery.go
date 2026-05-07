// Package promptdelivery provides a harness-aware prompt delivery helper used
// by all out-of-process callers that need to send a follow-up prompt to a
// running agent session.
//
// # Problem
//
// Three delivery paths in prism (review-complete monitor, coordinator notify,
// merge-queue watcher) originally always POSTed to the opencode HTTP API
// (http://localhost:<port>/session/<sid>/prompt_async). That path only works
// for opencode sessions — pi sessions do not have an opencode HTTP server.
//
// # Solution
//
// DeliverToSession inspects the target session's harness field and routes:
//
//   - harness = "pi" (TransportSocketPipe) → POST {"session":…,"prompt":…} to
//     the session's host-API Unix socket at /prompt. The sidecar's /prompt
//     handler detects the same-session socket-pipe shape and forwards the text
//     to the pi process via DeliverPrompt (stdin pipe write).
//   - harness = "opencode" (or anything else) → POST to the opencode HTTP API
//     at http://localhost:<port>/session/<sid>/prompt_async (unchanged path).
//
// All callers must pass the result of db.CurrentStatus for the target session.
// The caller is responsible for pre-flight checks (session found, not ended, etc).
package promptdelivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/session"
)

// DeliverToSession delivers text to the agent session described by status,
// routing based on the session's harness field.
//
// For pi sessions (TransportSocketPipe), it dials the session's host-API Unix
// socket and POSTs /prompt. The socket path is derived from sessionName via
// session.SidecarHostAPIPath.
//
// For opencode sessions (or when the harness is unknown / unset), it POSTs
// to http://localhost:<HarnessPort>/session/<HarnessSessionID>/prompt_async.
//
// status must be non-nil. The caller is expected to have already verified that
// the session is active (EndedAt == nil) before calling this function.
//
// buildHTTPBody is called for the opencode path to construct the POST body.
// For the pi path, only the plain text is sent (the sidecar handles wrapping).
// When buildHTTPBody is nil, a minimal body with just a "parts" array is used.
//
// source identifies the logical origin of the delivery. When non-empty it is
// forwarded to the sidecar's /prompt handler via the "source" JSON field. The
// sidecar uses this to gate reviewingInFlight clearing: only
// source="review-complete" (the monitor's delivery) clears the flag; all other
// deliveries leave it unchanged. Pass "" for non-monitor callers.
//
// deliverAs controls the prompt delivery mode for pi (TransportSocketPipe)
// sessions. It is forwarded as the "deliver_as" JSON field to the sidecar's
// /prompt handler, which validates and passes it through to DeliverPrompt.
// Accepted values: "steer", "followUp", "nextTurn". Empty string defaults to
// "nextTurn" (current behaviour, for backward-compatible callers).
// For opencode sessions the parameter is ignored (opencode uses prompt_async).
func DeliverToSession(sessionName string, status *db.Status, text string, buildHTTPBody func(string, *db.Status) map[string]any, source string, deliverAs string) error {
	// Determine the transport shape from the harness field.
	if status.Harness != nil && *status.Harness != "" {
		shape, ok := harness.ShapeOf(*status.Harness)
		if ok && shape == harness.TransportSocketPipe {
			return deliverViaSidecarSocket(sessionName, text, source, deliverAs)
		}
	}

	// Default: opencode HTTP path.
	return deliverViaHTTP(status, text, buildHTTPBody)
}

// deliverViaSidecarSocket dials the target session's host-API Unix socket and
// POSTs /prompt with {"session": sessionName, "prompt": text, "source": source,
// "deliver_as": deliverAs}.
//
// The sidecar's /prompt handler checks that req.Session == s.cfg.SessionName
// and that the harness shape is TransportSocketPipe, then calls s.DeliverPrompt
// to write the text to the pi process's stdin pipe.
//
// source is forwarded as the "source" JSON field. The sidecar uses it to gate
// reviewingInFlight clearing: only source="review-complete" clears the flag.
// Pass "" for non-monitor callers (the field is omitted when empty).
//
// deliverAs is forwarded as the "deliver_as" JSON field. Pass "followUp" for
// post-turn notifications (coordinator notify, merge-queue outcome, startup
// failure). Pass "" to default to "nextTurn" (existing caller behaviour).
// The sidecar validates the value and rejects unknown strings with HTTP 400.
//
// Returns an error if the socket does not exist (session ended / socket cleaned
// up) or the HTTP request fails.
func deliverViaSidecarSocket(sessionName, text, source, deliverAs string) error {
	sockPath, err := session.SidecarHostAPIPath(sessionName)
	if err != nil {
		return fmt.Errorf("resolve host-API socket path for %q: %w", sessionName, err)
	}

	if _, statErr := os.Stat(sockPath); statErr != nil {
		return fmt.Errorf("host-API socket not found at %s (session may have ended): %w", sockPath, statErr)
	}

	client := newUnixClient(sockPath)

	bodyMap := map[string]string{
		"session": sessionName,
		"prompt":  text,
	}
	if source != "" {
		bodyMap["source"] = source
	}
	if deliverAs != "" {
		bodyMap["deliver_as"] = deliverAs
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		return fmt.Errorf("marshal prompt body: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, "http://prism-hostapi/prompt", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dial host-API socket at %s: %w", sockPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("host-API /prompt returned HTTP %d for session %q", resp.StatusCode, sessionName)
	}
	return nil
}

// deliverViaHTTP sends text to the opencode HTTP API at
// http://localhost:<HarnessPort>/session/<HarnessSessionID>/prompt_async.
func deliverViaHTTP(status *db.Status, text string, buildBody func(string, *db.Status) map[string]any) error {
	if status.HarnessPort == nil || status.HarnessSessionID == nil {
		return fmt.Errorf("session %q has no harness port or session ID — cannot deliver prompt", status.SessionName)
	}

	var body map[string]any
	if buildBody != nil {
		body = buildBody(text, status)
	} else {
		body = map[string]any{
			"parts": []map[string]string{
				{"type": "text", "text": text},
			},
		}
	}

	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal prompt body: %w", err)
	}

	url := fmt.Sprintf("http://localhost:%d/session/%s/prompt_async", *status.HarnessPort, *status.HarnessSessionID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(jsonBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http status %d from %s", resp.StatusCode, url)
	}
	return nil
}

// newUnixClient returns an *http.Client that dials sockPath over a Unix socket.
func newUnixClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := &net.Dialer{Timeout: 5 * time.Second}
				conn, err := d.DialContext(ctx, "unix", sockPath)
				if err != nil {
					return nil, fmt.Errorf("host-API socket not available at %s: %w", sockPath, err)
				}
				return conn, nil
			},
		},
		Timeout: 10 * time.Second,
	}
}
