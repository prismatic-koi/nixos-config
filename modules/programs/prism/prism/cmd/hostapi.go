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
	"errors"
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

	"github.com/prismatic-koi/prism/internal/promptdelivery"
	"github.com/prismatic-koi/prism/internal/sidecar"
)

// newHostAPIClient returns an *http.Client that dials sockPath over a Unix
// socket. Used when PRISM_HOST_API begins with "unix://".
//
// Tombstone hygiene: when the dial returns ECONNREFUSED and the socket file
// still exists on disk, the sidecar process has exited abnormally without
// removing the socket. In that case we surface a clearer diagnostic so the
// user sees "sidecar has exited" rather than the raw "connection refused" with
// no additional context. This is the client-side branch of the tombstone
// hygiene described in issue #1486.
func newHostAPIClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := &net.Dialer{Timeout: 5 * time.Second}
				conn, dialErr := d.DialContext(ctx, "unix", sockPath)
				if dialErr != nil {
					// Check for the tombstone case: socket file exists on disk
					// but connect() returned ECONNREFUSED — the sidecar has
					// exited abnormally without cleaning up the socket file.
					// Surface a more informative error than the raw syscall
					// message so diagnostics are immediately actionable.
					if isStaleTombstoneSocket(sockPath, dialErr) {
						return nil, fmt.Errorf("host-API socket at %s is a stale tombstone — sidecar has exited without cleanup (ECONNREFUSED on existing socket file): %w", sockPath, dialErr)
					}
					return nil, fmt.Errorf("host-API socket not available at %s: %w", sockPath, dialErr)
				}
				return conn, nil
			},
		},
		Timeout: 60 * time.Second,
	}
}

// isStaleTombstoneSocket returns true when the dial error is ECONNREFUSED and
// the socket file at sockPath exists on disk. This indicates the sidecar
// exited abnormally, leaving a stale socket file — the "tombstone" case.
func isStaleTombstoneSocket(sockPath string, dialErr error) bool {
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return false
	}
	_, statErr := os.Stat(sockPath)
	return statErr == nil
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
	// Build url.Values from the single-valued map and delegate to the
	// repeated-key form. q.Encode() sorts keys, so the encoded query is
	// deterministic regardless of Go's map iteration order.
	q := url.Values{}
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	return proxyGetValuesFromHostAPI(apiURL, endpoint, q, respDst)
}

// proxyGetValuesFromHostAPI is the url.Values form of proxyGetFromHostAPI. It
// supports repeated query keys (e.g. id=A&id=B for /stats?view=compare), which
// the map-based form cannot express. apiURL is either a unix:// or http:// URL;
// endpoint is the path; query carries the (possibly repeated) parameters. On
// success the response JSON is unmarshalled into respDst (may be nil).
func proxyGetValuesFromHostAPI(apiURL, endpoint string, query url.Values, respDst any) error {
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

	if enc := query.Encode(); enc != "" {
		rawURL += "?" + enc
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

// proxyReadToHostAPI sends a GET request to the host-API server for a read
// operation (stats, sessions, checkin). apiURL is the value of PRISM_HOST_API.
// endpoint is the URL path (e.g. "/stats"). params are appended as URL query
// parameters. The raw JSON response body is returned for the caller to unmarshal
// and render. This is the shared helper analogous to proxyEventToHostAPI for
// read paths — callers do not repeat the env-var check or the GET plumbing.
//
// On connection failure the error message includes the socket path and the
// underlying error (per the edge-case AC: clear error, not silent fallback).
func proxyReadToHostAPI(apiURL, endpoint string, params map[string]string) ([]byte, error) {
	var raw json.RawMessage
	if err := proxyGetFromHostAPI(apiURL, endpoint, params, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// proxyStats proxies a stats request to the host-API sidecar.
// apiURL is the value of PRISM_HOST_API. view selects the data set
// (summary, doomloops, denials, asks, detail). sessionFilter, days, repoFilter,
// and sinceMs are the corresponding query parameters. Returns raw JSON.
func proxyStats(apiURL, view, sessionFilter string, days int, repoFilter string, sinceMs int64) ([]byte, error) {
	params := map[string]string{
		"view": view,
	}
	if sessionFilter != "" {
		params["session"] = sessionFilter
	}
	if days > 0 {
		params["days"] = fmt.Sprintf("%d", days)
	}
	if repoFilter != "" {
		params["repo"] = repoFilter
	}
	if sinceMs > 0 {
		params["since"] = fmt.Sprintf("%d", sinceMs)
	}
	return proxyReadToHostAPI(apiURL, "/stats", params)
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

// cleanupResponse is the host-API /cleanup response shape. stdout/stderr are
// the captured byte streams of the host-side `prism cleanup` subprocess. The
// container-side caller writes them verbatim to its own stdout/stderr so that
// the byte content is identical to a host invocation (modulo trailing
// whitespace from the buffer). On success Error is empty; on failure the
// host-API still returns stdout/stderr alongside Error so the caller can
// surface the underlying cause. See issue #1527.
type cleanupResponse struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Error  string `json:"error,omitempty"`
}

// proxyCleanupToHostAPI sends a cleanup request to the host-API sidecar and
// forwards the host-side stdout and stderr to the caller's stdout/stderr.
//
// This makes the container-path output of `prism cleanup` byte-equivalent to
// the host-path output (modulo trailing whitespace), per issue #1527 AC #1.
// Without this forwarding, the container path was silent on success because
// the previous handler discarded the captured CombinedOutput.
//
// `keepWorktree`, when true, forwards `keep_worktree:true` to the host so the
// host-side `prism cleanup` runs with --keep-worktree (issue #2179). Passing
// false preserves the pre-#2179 destructive default.
func proxyCleanupToHostAPI(apiURL, session string, yes, jsonMode, keepWorktree bool) error {
	return proxyCleanupToHostAPIWithWriters(apiURL, session, yes, jsonMode, keepWorktree, os.Stdout, os.Stderr)
}

// proxyCleanupToHostAPIWithWriters is the testable form of
// proxyCleanupToHostAPI: stdout/stderr destinations are injectable so unit
// tests can capture forwarded output without redirecting os.Stdout.
func proxyCleanupToHostAPIWithWriters(apiURL, session string, yes, jsonMode, keepWorktree bool, stdout, stderr io.Writer) error {
	body := map[string]any{
		"session": session,
		"yes":     yes,
		"json":    jsonMode,
	}
	if keepWorktree {
		body["keep_worktree"] = true
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("proxyCleanupToHostAPI: marshal request body: %w", err)
	}

	var client *http.Client
	var reqURL string
	if strings.HasPrefix(apiURL, "http://") {
		client = newTCPHostAPIClient()
		reqURL = apiURL + "/cleanup"
	} else {
		sockPath, parseErr := parseUnixSocketURL(apiURL)
		if parseErr != nil {
			return fmt.Errorf("PRISM_HOST_API %q: %w", apiURL, parseErr)
		}
		client = newHostAPIClient(sockPath)
		reqURL = "http://prism-hostapi/cleanup"
	}

	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("proxyCleanupToHostAPI: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("host-API /cleanup: read response: %w", readErr)
	}

	var parsed cleanupResponse
	// Tolerate empty bodies; we'll surface a clearer error below.
	if len(respBody) > 0 {
		if unmarshalErr := json.Unmarshal(respBody, &parsed); unmarshalErr != nil {
			return fmt.Errorf("host-API /cleanup: unmarshal response: %w (body=%s)", unmarshalErr, strings.TrimSpace(string(respBody)))
		}
	}

	// Forward the host-side streams unconditionally — even on error — so the
	// caller can see partial-success progress lines and stderr warnings
	// (e.g. archive collision). This addresses both the silent-success defect
	// and the error-message-names-the-wrong-layer issue noted in the comment
	// on issue #1527.
	if parsed.Stdout != "" {
		_, _ = io.WriteString(stdout, parsed.Stdout)
	}
	if parsed.Stderr != "" {
		_, _ = io.WriteString(stderr, parsed.Stderr)
	}

	if resp.StatusCode >= 400 {
		if parsed.Error != "" {
			return fmt.Errorf("host-API /cleanup: %s", parsed.Error)
		}
		return fmt.Errorf("host-API /cleanup: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// proxyCloseToHostAPI sends a `prism close` request to the host-API sidecar
// and forwards the host-side stdout and stderr to the caller's stdout/stderr
// (issue #2179). Mirrors proxyCleanupToHostAPI but targets the /close endpoint
// so the host runs `prism close` (with its smart-decide logic) rather than
// `prism cleanup` (always destructive).
func proxyCloseToHostAPI(apiURL, session string, yes, jsonMode, keepWorktree, removeWorktree bool) error {
	return proxyCloseToHostAPIWithWriters(apiURL, session, yes, jsonMode, keepWorktree, removeWorktree, os.Stdout, os.Stderr)
}

// proxyCloseToHostAPIWithWriters is the testable form of
// proxyCloseToHostAPI: stdout/stderr destinations are injectable so unit
// tests can capture forwarded output without redirecting os.Stdout.
func proxyCloseToHostAPIWithWriters(apiURL, session string, yes, jsonMode, keepWorktree, removeWorktree bool, stdout, stderr io.Writer) error {
	body := map[string]any{
		"session": session,
		"yes":     yes,
		"json":    jsonMode,
	}
	if keepWorktree {
		body["keep_worktree"] = true
	}
	if removeWorktree {
		body["remove_worktree"] = true
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("proxyCloseToHostAPI: marshal request body: %w", err)
	}

	var client *http.Client
	var reqURL string
	if strings.HasPrefix(apiURL, "http://") {
		client = newTCPHostAPIClient()
		reqURL = apiURL + "/close"
	} else {
		sockPath, parseErr := parseUnixSocketURL(apiURL)
		if parseErr != nil {
			return fmt.Errorf("PRISM_HOST_API %q: %w", apiURL, parseErr)
		}
		client = newHostAPIClient(sockPath)
		reqURL = "http://prism-hostapi/close"
	}

	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("proxyCloseToHostAPI: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("host-API /close: read response: %w", readErr)
	}

	var parsed cleanupResponse
	if len(respBody) > 0 {
		if unmarshalErr := json.Unmarshal(respBody, &parsed); unmarshalErr != nil {
			return fmt.Errorf("host-API /close: unmarshal response: %w (body=%s)", unmarshalErr, strings.TrimSpace(string(respBody)))
		}
	}

	// Forward unconditionally on success and error (parity with /cleanup).
	if parsed.Stdout != "" {
		_, _ = io.WriteString(stdout, parsed.Stdout)
	}
	if parsed.Stderr != "" {
		_, _ = io.WriteString(stderr, parsed.Stderr)
	}

	if resp.StatusCode >= 400 {
		if parsed.Error != "" {
			return fmt.Errorf("host-API /close: %s", parsed.Error)
		}
		return fmt.Errorf("host-API /close: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// proxyPrompt proxies a prompt delivery request to the host-API sidecar.
// apiURL is the value of PRISM_HOST_API. deliverAs controls the delivery mode
// ("steer", "followUp", or "nextTurn") and is forwarded as the "deliver_as"
// JSON field so the sidecar can inject the prompt at the right time.
func proxyPrompt(apiURL, session, prompt, deliverAs string) error {
	return proxyToHostAPI(apiURL, "/prompt", map[string]any{
		"session":    session,
		"prompt":     prompt,
		"deliver_as": deliverAs,
	}, nil)
}

// proxyPromptWithOutcome is the outcome-aware variant of proxyPrompt (issue
// #2359 Gap B). It parses the sidecar's response envelope ({"buffered": true}
// / {"replayed": true}) and forwards the same human-readable / --json
// output as the non-proxied path, so a container-side `prism prompt` call
// carries the buffered outcome all the way back to the operator.
//
// The response from the sidecar's host-API /prompt endpoint is a JSON
// object; the fields we look for are optional and default to false. An
// empty response body is treated as a synchronous delivery.
func proxyPromptWithOutcome(apiURL, session, prompt, deliverAs string, jsonOut bool) error {
	var respEnv struct {
		Buffered bool `json:"buffered"`
		Replayed bool `json:"replayed"`
	}
	if err := proxyToHostAPI(apiURL, "/prompt", map[string]any{
		"session":    session,
		"prompt":     prompt,
		"deliver_as": deliverAs,
	}, &respEnv); err != nil {
		return err
	}
	// The proxied path does not have visibility into the delivery_id — the
	// host-side sidecar's /prompt handler shells to `prism prompt` which
	// mints a fresh UUID, and the sidecar returns after the child exits.
	// Callers who need the ID must inspect the host-side bus_messages
	// audit trail. Surface an empty string in the JSON envelope so the
	// field is present but distinguishable from a real ID.
	return emitPromptOutcome(os.Stdout, jsonOut, session, "host-api", promptdeliveryOutcomeForProxy(respEnv.Buffered, respEnv.Replayed))
}

// promptdeliveryOutcomeForProxy is a small adapter to build a
// promptdelivery.DeliveryOutcome without pulling the whole struct into every
// callsite here. Kept as a helper so the proxy path's outcome shape stays
// aligned with the direct path's.
func promptdeliveryOutcomeForProxy(buffered, replayed bool) promptdelivery.DeliveryOutcome {
	return promptdelivery.DeliveryOutcome{
		Buffered: buffered,
		Replayed: replayed,
	}
}

// proxyReviewAsync proxies an async review request to the host-API sidecar
// when running inside a container (PRISM_HOST_API is set). It sends POST /review
// to the sidecar, which runs `prism review` on the host where tmux is available,
// streams each output line to os.Stdout as it arrives, and returns once the
// subprocess completes (signalled by a sentinel line in the response body).
//
// prNumber is the PR number to review (e.g. "123"). agents is an optional
// list of agent names for --only filtering. timeout is an optional duration
// string (e.g. "10m"). rebase requests an inline rebase onto origin/main
// before the review (issue #1518). Returns an error if the host-side
// subprocess failed or the connection was lost mid-stream.
//
// The response from the sidecar /review endpoint is a plain-text chunked stream
// terminated by one of two sentinel lines:
//
//	sidecar.ReviewSentinelPassed — subprocess exited 0
//	sidecar.ReviewSentinelFailed — subprocess exited non-zero
//
// This function consumes the sentinel and does NOT print it to stdout.
//
// quietStdout suppresses the per-line streaming print to os.Stdout while
// still buffering the output for the caller's return value. It is used by
// the in-sandbox `prism review --wait --json` path to honour the
// JSON-exclusive contract (#1500): otherwise the streamed Ack lines would
// land on stdout before waitForReviewTerminal emits its JSON object.
func proxyReviewAsync(apiURL, prNumber string, agents []string, timeout string, rebase bool, quietStdout bool) (string, error) {
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
	if rebase {
		body["rebase"] = true
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
						if isStaleTombstoneSocket(sockPath, dialErr) {
							return nil, fmt.Errorf("host-API socket at %s is a stale tombstone — sidecar has exited without cleanup (ECONNREFUSED on existing socket file): %w", sockPath, dialErr)
						}
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
			// Regular output line — buffer for the caller's return value, and
			// (unless quietStdout is set) stream to stdout immediately so the
			// worker sees progress as it arrives.
			if !quietStdout {
				fmt.Fprintln(os.Stdout, line)
			}
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
