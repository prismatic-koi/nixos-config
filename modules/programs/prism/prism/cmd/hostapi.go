package cmd

// proxyToHostAPI — shared helper for container-aware proxy mode (A-3).
//
// When PRISM_HOST_API is set, prism commands running inside a container
// cannot reach tmux directly. Instead they POST their operation to the host
// sidecar over the Unix socket accessible inside the container at
// /var/run/prism-host/<sockfilename> (the session's own per-session directory
// is bind-mounted by A-2, providing socket isolation between sessions — #960).
//
// The host sidecar (A-1) listens on that socket and executes the real tmux
// operations on the host side.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prismatic-koi/prism/internal/sidecar"
)

// newHostAPIClient returns an *http.Client that dials sockPath over a Unix
// socket. Used when PRISM_HOST_API begins with "unix://".
func newHostAPIClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := &net.Dialer{Timeout: 5 * time.Second}
				conn, dialErr := d.DialContext(ctx, "unix", sockPath)
				if dialErr != nil {
					return nil, fmt.Errorf("host-API socket not available at %s: %w", sockPath, dialErr)
				}
				return conn, nil
			},
		},
		Timeout: 60 * time.Second,
	}
}

// newTCPHostAPIClient returns a standard *http.Client that dials over TCP.
// Used when PRISM_HOST_API begins with "http://". No custom DialContext is
// needed — the default transport resolves and dials the host:port from the URL.
func newTCPHostAPIClient() *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
	}
}

// readHostAPIResponse reads the response body and returns an error if the
// status code is >= 400, surfacing the JSON "error" field when present.
// On success (status < 400), it unmarshals into respDst when non-nil.
func readHostAPIResponse(endpoint string, resp *http.Response, respDst any) error {
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("host-API %s: read response: %w", endpoint, err)
	}

	if resp.StatusCode >= 400 {
		// Try to extract an "error" field from the JSON response.
		var errResp struct {
			Error string `json:"error"`
		}
		if jsonErr := json.Unmarshal(respBody, &errResp); jsonErr == nil && errResp.Error != "" {
			return fmt.Errorf("host-API %s: %s", endpoint, errResp.Error)
		}
		return fmt.Errorf("host-API %s: HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	if respDst != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, respDst); err != nil {
			return fmt.Errorf("host-API %s: unmarshal response: %w", endpoint, err)
		}
	}
	return nil
}

// proxyToHostAPI sends a request to the host-API server and returns the parsed
// response. apiURL is the value of PRISM_HOST_API — either a unix:// URL (Linux)
// or an http:// URL (Darwin TCP path). endpoint is the path (e.g. "/spawn").
// body is marshalled to JSON and sent as the request body. On success, response
// JSON is unmarshalled into respDst (may be nil).
func proxyToHostAPI(apiURL, endpoint string, body any, respDst any) error {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("proxyToHostAPI: marshal request body: %w", err)
	}

	var client *http.Client
	var reqURL string

	if strings.HasPrefix(apiURL, "http://") {
		// TCP path (Darwin): use the http:// URL directly — standard TCP dial.
		client = newTCPHostAPIClient()
		reqURL = apiURL + endpoint
	} else {
		// Unix socket path (Linux): extract socket path and use a fake host.
		sockPath, parseErr := parseUnixSocketURL(apiURL)
		if parseErr != nil {
			return fmt.Errorf("PRISM_HOST_API %q: %w", apiURL, parseErr)
		}
		client = newHostAPIClient(sockPath)
		// "prism-hostapi" is a placeholder Host header — irrelevant for Unix
		// socket connections but required for a valid HTTP/1.1 request.
		reqURL = "http://prism-hostapi" + endpoint
	}

	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("proxyToHostAPI: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	return readHostAPIResponse(endpoint, resp, respDst)
}

// proxyGetFromHostAPI sends a GET request to the host-API server with optional
// query parameters and returns the parsed response. apiURL is the value of
// PRISM_HOST_API — either unix:// or http://. endpoint is the path (e.g.
// "/list-sessions"). params are appended as properly-encoded URL query
// parameters. On success, response JSON is unmarshalled into respDst (may be nil).
func proxyGetFromHostAPI(apiURL, endpoint string, params map[string]string, respDst any) error {
	var client *http.Client
	var rawURL string

	if strings.HasPrefix(apiURL, "http://") {
		client = newTCPHostAPIClient()
		rawURL = apiURL + endpoint
	} else {
		sockPath, parseErr := parseUnixSocketURL(apiURL)
		if parseErr != nil {
			return fmt.Errorf("PRISM_HOST_API %q: %w", apiURL, parseErr)
		}
		client = newHostAPIClient(sockPath)
		rawURL = "http://prism-hostapi" + endpoint
	}

	// Append properly percent-encoded query parameters.
	if len(params) > 0 {
		q := url.Values{}
		for k, v := range params {
			if v != "" {
				q.Set(k, v)
			}
		}
		if len(q) > 0 {
			rawURL += "?" + q.Encode()
		}
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("proxyGetFromHostAPI: build request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	return readHostAPIResponse(endpoint, resp, respDst)
}

// proxyListSessions proxies a list-sessions request to the host-API sidecar.
// apiURL is the value of PRISM_HOST_API. showAll controls whether the all=true
// query parameter is sent. Returns the raw JSON output for the caller to render.
func proxyListSessions(apiURL string, showAll bool) ([]byte, error) {
	params := map[string]string{}
	if showAll {
		params["all"] = "true"
	}
	var raw json.RawMessage
	if err := proxyGetFromHostAPI(apiURL, "/list-sessions", params, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// proxyCheckin proxies a checkin request to the host-API sidecar.
// apiURL is the value of PRISM_HOST_API. Returns a struct with the session
// state and raw events JSON.
func proxyCheckin(apiURL, session string, limit int, before, after *string, types []string, _ bool) ([]byte, error) {
	params := map[string]string{
		"session": session,
		"last":    fmt.Sprintf("%d", limit),
	}
	if before != nil {
		params["before"] = *before
	}
	if after != nil {
		params["from"] = *after
	}
	if len(types) > 0 {
		params["types"] = strings.Join(types, ",")
	}
	var raw json.RawMessage
	if err := proxyGetFromHostAPI(apiURL, "/checkin", params, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// proxyPrompt proxies a prompt delivery request to the host-API sidecar.
// apiURL is the value of PRISM_HOST_API.
func proxyPrompt(apiURL, session, prompt string) error {
	return proxyToHostAPI(apiURL, "/prompt", map[string]any{
		"session": session,
		"prompt":  prompt,
	}, nil)
}

// proxyReviewAsync proxies an async review request to the host-API sidecar
// when running inside a container (PRISM_HOST_API is set). It sends POST /review
// to the sidecar, which runs `prism review` on the host where tmux is available,
// streams each output line to os.Stdout as it arrives, and returns once the
// subprocess completes (signalled by a sentinel line in the response body).
//
// prNumber is the PR number to review (e.g. "123"). agents is an optional
// list of agent names for --only filtering. timeout is an optional duration
// string (e.g. "10m"). Returns an error if the host-side subprocess failed or
// the connection was lost mid-stream.
//
// The response from the sidecar /review endpoint is a plain-text chunked stream
// terminated by one of two sentinel lines:
//
//	sidecar.ReviewSentinelPassed — subprocess exited 0
//	sidecar.ReviewSentinelFailed — subprocess exited non-zero
//
// This function consumes the sentinel and does NOT print it to stdout.
func proxyReviewAsync(apiURL, prNumber string, agents []string, timeout string) (string, error) {
	// Build request body.
	body := map[string]any{
		"pr_number": prNumber,
	}
	if len(agents) > 0 {
		body["agents"] = agents
	}
	if timeout != "" {
		body["timeout"] = timeout
	}

	// The sidecar streams output as it arrives and closes the response body
	// when the sentinel is written, so we need a longer timeout than the 30 s
	// that sufficed for the old buffered path. Use 60 s — the spawn phase
	// (the only non-trivial work in async prism review) completes well within
	// that window even on a slow host.
	const clientTimeout = 60 * time.Second

	// Build a custom client without a read timeout so the streaming response
	// body can remain open for the full duration of the subprocess.
	var (
		client *http.Client
		reqURL string
	)
	if strings.HasPrefix(apiURL, "http://") {
		// TCP path: use a transport with no read deadline on the connection.
		// We set Timeout:0 on the client and rely on the context deadline instead.
		client = &http.Client{
			Transport: &http.Transport{},
			Timeout:   0, // no global timeout; context deadline governs
		}
		reqURL = apiURL + "/review"
	} else {
		sockPath, parseErr := parseUnixSocketURL(apiURL)
		if parseErr != nil {
			return "", fmt.Errorf("PRISM_HOST_API %q: %w", apiURL, parseErr)
		}
		client = &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					d := &net.Dialer{Timeout: 5 * time.Second}
					conn, dialErr := d.DialContext(ctx, "unix", sockPath)
					if dialErr != nil {
						return nil, fmt.Errorf("host-API socket not available at %s: %w", sockPath, dialErr)
					}
					return conn, nil
				},
			},
			Timeout: 0, // no global timeout; context deadline governs
		}
		reqURL = "http://prism-hostapi/review"
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("proxyReviewAsync: marshal request body: %w", err)
	}

	// Attach a context deadline so the request cannot hang indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("proxyReviewAsync: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("host-API /review: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Error responses from the sidecar are still JSON (written before the
		// streaming body begins), so read and surface the error field.
		respBody, _ := io.ReadAll(resp.Body)
		var errResp struct {
			Error string `json:"error"`
		}
		if jsonErr := json.Unmarshal(respBody, &errResp); jsonErr == nil && errResp.Error != "" {
			return "", fmt.Errorf("host-API /review: %s", errResp.Error)
		}
		return "", fmt.Errorf("host-API /review: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	// Read the streaming response line-by-line. Each line is printed to stdout
	// immediately so the worker sees progress as it is emitted. The final line
	// is a sentinel that we consume but do not print.
	scanner := bufio.NewScanner(resp.Body)
	// Allow lines up to 1 MiB to handle very long output without truncation.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var (
		sentinelSeen bool
		passed       bool
		outputBuf    strings.Builder
	)
	for scanner.Scan() {
		line := scanner.Text()
		switch line {
		case sidecar.ReviewSentinelPassed:
			sentinelSeen = true
			passed = true
		case sidecar.ReviewSentinelFailed:
			sentinelSeen = true
			passed = false
		default:
			// Regular output line — print to stdout immediately and buffer for
			// the caller so it can be used in the return value if needed.
			fmt.Fprintln(os.Stdout, line)
			outputBuf.WriteString(line)
			outputBuf.WriteByte('\n')
		}
		// Stop after consuming the sentinel.
		if sentinelSeen {
			break
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		return outputBuf.String(), fmt.Errorf("host-API /review: read stream: %w", scanErr)
	}

	if !sentinelSeen {
		// The stream ended without a sentinel — the subprocess died mid-stream
		// or the connection was lost. Surface this as a clear error.
		return outputBuf.String(), fmt.Errorf("host-API /review: stream ended without completion sentinel (subprocess may have crashed)")
	}

	if !passed {
		return outputBuf.String(), fmt.Errorf("host-API /review: review process failed — check host logs for details")
	}

	return outputBuf.String(), nil
}

// proxyLogsFromHostAPI proxies a GET /logs request to the host-API server and
// streams the response body directly to w. For follow mode, it handles Ctrl-C
// gracefully by cancelling the request context. apiURL is either a unix:// or
// http:// URL depending on the platform. When agentRun is true, the
// source=agent-run query parameter is sent so the host reads the agent-run log
// instead of the sidecar log.
func proxyLogsFromHostAPI(apiURL, sessionName string, tail int, tailSet bool, follow bool, agentRun bool, w io.Writer) error {
	var client *http.Client
	var baseURL string

	if strings.HasPrefix(apiURL, "http://") {
		client = newTCPHostAPIClient()
		baseURL = apiURL
	} else {
		sockPath, parseErr := parseUnixSocketURL(apiURL)
		if parseErr != nil {
			return fmt.Errorf("PRISM_HOST_API %q: %w", apiURL, parseErr)
		}
		client = newHostAPIClient(sockPath)
		baseURL = "http://prism-hostapi"
	}

	// Build URL with query parameters.
	q := url.Values{}
	q.Set("session", sessionName)
	if tailSet {
		q.Set("tail", fmt.Sprintf("%d", tail))
	}
	if follow {
		q.Set("follow", "true")
	}
	if agentRun {
		q.Set("source", "agent-run")
	}
	rawURL := baseURL + "/logs?" + q.Encode()

	// For follow mode, use a cancellable context so Ctrl-C exits cleanly.
	ctx := context.Background()
	var cancel context.CancelFunc
	if follow {
		ctx, cancel = context.WithCancel(ctx)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			select {
			case <-sigCh:
				cancel()
			case <-ctx.Done():
			}
		}()
		defer signal.Stop(sigCh)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("proxyLogsFromHostAPI: build request: %w", err)
	}

	// For follow mode (streaming), override the default 60 s timeout to 0
	// (no timeout) so the connection is not cut while waiting for new log lines.
	if follow {
		client.Timeout = 0
	}

	resp, err := client.Do(req)
	if err != nil {
		// Ctrl-C in follow mode: treat as clean exit.
		if follow && ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("host-API /logs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		var errResp struct {
			Error string `json:"error"`
		}
		if jsonErr := json.Unmarshal(body, &errResp); jsonErr == nil && errResp.Error != "" {
			return fmt.Errorf("host-API /logs: %s", errResp.Error)
		}
		return fmt.Errorf("host-API /logs: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	_, copyErr := io.Copy(w, resp.Body)
	if copyErr != nil {
		// Ctrl-C in follow mode: treat as clean exit.
		if follow && ctx.Err() != nil {
			return nil
		}
		return copyErr
	}
	return nil
}

// parseUnixSocketURL extracts the filesystem path from a unix:///path URL.
// Returns an error for malformed values (not starting with "unix://").
func parseUnixSocketURL(apiURL string) (string, error) {
	const prefix = "unix://"
	if !strings.HasPrefix(apiURL, prefix) {
		return "", fmt.Errorf("unsupported scheme — expected unix:///path/to/sock, got %q", apiURL)
	}
	// unix:///path → strip "unix://" → "/path"
	// unix://host/path → strip "unix://" → "host/path" (unusual, but handled)
	path := strings.TrimPrefix(apiURL, prefix)
	if path == "" {
		return "", fmt.Errorf("empty socket path in %q", apiURL)
	}
	return path, nil
}
