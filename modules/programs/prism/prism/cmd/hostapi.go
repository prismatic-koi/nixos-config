package cmd

// proxyToHostAPI — shared helper for container-aware proxy mode (A-3).
//
// When PRISM_HOST_API is set, prism commands running inside a container
// cannot reach tmux directly. Instead they POST their operation to the host
// sidecar over the Unix socket that A-2 mounts at /var/run/prism-hostapi.sock.
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
	"strings"
	"time"
)

// proxyToHostAPI sends a request to the host-API Unix socket server and returns
// the parsed response. apiURL is the value of PRISM_HOST_API
// (e.g. "unix:///var/run/prism-hostapi.sock"). endpoint is the path
// (e.g. "/spawn"). body is marshalled to JSON and sent as the request body.
// On success, response JSON is unmarshalled into respDst (may be nil).
func proxyToHostAPI(apiURL, endpoint string, body any, respDst any) error {
	sockPath, err := parseUnixSocketURL(apiURL)
	if err != nil {
		return fmt.Errorf("PRISM_HOST_API %q: %w", apiURL, err)
	}

	client := &http.Client{
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

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("proxyToHostAPI: marshal request body: %w", err)
	}

	// Use "prism-hostapi" as the Host header — it is irrelevant for Unix socket
	// connections but required by http.Client for a valid HTTP/1.1 request.
	req, err := http.NewRequest(http.MethodPost, "http://prism-hostapi"+endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("proxyToHostAPI: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("proxyToHostAPI: read response: %w", err)
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
			return fmt.Errorf("proxyToHostAPI: unmarshal response: %w", err)
		}
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
