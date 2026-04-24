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

// reviewHostAPIResponse holds the response from the host-API /review endpoint.
// For the async path, only Output is populated (Passed is always true because
// the review has not yet completed when the ack is returned).
type reviewHostAPIResponse struct {
	Output string `json:"output"`
	Passed bool   `json:"passed"`
}

// proxyReviewAsync proxies an async review request to the host-API sidecar
// when running inside a container (PRISM_HOST_API is set). It sends POST /review
// to the sidecar, which runs `prism review` on the host where tmux is available,
// and returns immediately with the async acknowledgement.
//
// prNumber is the PR number to review (e.g. "123"). agents is an optional
// list of agent names for --only filtering. timeout is an optional duration
// string (e.g. "10m"). Returns the acknowledgement text and any error.
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

	// Async reviews return quickly (just spawning + ack), so 30 s is plenty.
	const clientTimeout = 30 * time.Second

	// Build a custom client.
	var (
		client *http.Client
		reqURL string
	)
	if strings.HasPrefix(apiURL, "http://") {
		client = &http.Client{Timeout: clientTimeout}
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
			Timeout: clientTimeout,
		}
		reqURL = "http://prism-hostapi/review"
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("proxyReviewAsync: marshal request body: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("proxyReviewAsync: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("host-API /review: %w", err)
	}

	var reviewResp reviewHostAPIResponse
	if err := readHostAPIResponse("/review", resp, &reviewResp); err != nil {
		return "", err
	}
	return reviewResp.Output, nil
}

// proxyLogsFromHostAPI proxies a GET /logs request to the host-API server and
// streams the response body directly to w. For follow mode, it handles Ctrl-C
// gracefully by cancelling the request context. apiURL is either a unix:// or
// http:// URL depending on the platform.
func proxyLogsFromHostAPI(apiURL, sessionName string, tail int, tailSet bool, follow bool, w io.Writer) error {
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
