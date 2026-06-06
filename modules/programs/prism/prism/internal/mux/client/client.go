package client

import (
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
	"time"

	"github.com/prismatic-koi/prism/internal/mux/server"
)

// defaultTimeout bounds a single round-trip's worst-case wall-clock
// cost. 5 s mirrors the sidecar host-API client's choice
// (internal/sidecar/host_api.go::dialUnixAndPost) so operator
// intuition stays consistent across both clients. Per-call
// context.WithDeadline takes precedence when set.
const defaultTimeout = 5 * time.Second

// baseURL is a fake HTTP host. We dial a Unix socket via the
// transport's DialContext hook so the URL's host field is irrelevant
// — but http.Request still requires a valid host, so we pin one
// here. "prism-mux" mirrors "prism-sidecar" from the host-API client
// so log lines from either dialer are visually distinguishable.
const baseURL = "http://prism-mux"

// Client is the concrete MuxClient implementation. It is safe for
// concurrent use — every public method either reads immutable fields
// (sockPath, http) or delegates to net/http, which has its own
// concurrency guarantees.
//
// The zero value is NOT ready for use; construct with New.
type Client struct {
	// sockPath is the absolute path to the daemon's Unix socket.
	// Resolved at New time so we never have to redo the $HOME /
	// XDG_STATE_HOME lookup on the hot path.
	sockPath string

	// http is the configured HTTP client whose Transport dials
	// sockPath. Each request gets a fresh connection (keep-alives are
	// disabled) so a daemon restart between calls is transparent.
	http *http.Client

	// sessions and panes are the cached subclient handles. They hold
	// a back-reference to the parent Client so methods route through
	// the same do() helper.
	sessions *sessionAPI
	panes    *paneAPI
}

// Option configures a Client constructed by New.
type Option func(*config)

// config is the internal collector for Option values. Kept private so
// the public Option type is fully opaque — callers cannot reach in
// and mutate fields directly.
type config struct {
	sockPath  string
	timeout   time.Duration
	transport http.RoundTripper // overrides the default Unix-dialing transport
}

// WithSocketPath overrides the default socket path
// (server.DefaultSocketPath). Useful in tests against a t.TempDir()
// socket and in any future deployment that puts the daemon's socket
// somewhere other than the canonical XDG location.
func WithSocketPath(path string) Option {
	return func(c *config) { c.sockPath = path }
}

// WithTimeout sets the per-request HTTP-client timeout. Zero means
// "no client-level timeout" — callers must then bound the call via
// ctx.WithDeadline or risk hanging forever on a stuck server. The
// default is 5 s; in practice callers pass a context with a deadline
// and ignore this knob.
func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d }
}

// WithTransport injects a custom http.RoundTripper. This is the
// escape hatch for tests that already own a httptest.Server-style
// listener and want to drive the client against it without going
// through a real Unix socket. Production callers should not need
// this.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *config) { c.transport = rt }
}

// New returns a Client configured for the supplied options. When no
// WithSocketPath is given, server.DefaultSocketPath is consulted so
// the client lands on the same path the daemon binds.
//
// Resolving the default socket path requires $XDG_STATE_HOME or $HOME
// — if neither is available, New returns an error so the caller can
// surface a clean diagnostic rather than failing on first request
// with an obscure dial error. Tests in the Nix homeless-shelter
// sandbox MUST pass WithSocketPath; relying on the env-derived
// default would fail with $HOME=/homeless-shelter.
func New(opts ...Option) (*Client, error) {
	cfg := config{
		timeout: defaultTimeout,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	sockPath := cfg.sockPath
	if sockPath == "" {
		p, err := server.DefaultSocketPath()
		if err != nil {
			return nil, fmt.Errorf("mux/client: resolve default socket path: %w", err)
		}
		sockPath = p
	}

	transport := cfg.transport
	if transport == nil {
		transport = newUnixTransport(sockPath)
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   cfg.timeout,
	}

	c := &Client{
		sockPath: sockPath,
		http:     httpClient,
	}
	c.sessions = &sessionAPI{c: c}
	c.panes = &paneAPI{c: c}
	return c, nil
}

// newUnixTransport returns the default Unix-dialing http.Transport.
// Keep-alives are disabled so each request opens a fresh socket —
// this gives the "auto-reconnect on daemon restart" semantic for
// free: there is no stale pooled connection to fail through.
//
// Idle-connection limits and DialContext mirror the sidecar's
// dialUnixAndPost client.
func newUnixTransport(sockPath string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
		DisableKeepAlives: true,
	}
}

// SocketPath returns the resolved socket path. Useful in CLI
// diagnostics ("dialed <path>") and in tests that need to assert
// which socket the client is talking to.
func (c *Client) SocketPath() string { return c.sockPath }

// Sessions returns the SessionAPI subclient. Always non-nil for a
// Client constructed by New.
func (c *Client) Sessions() SessionAPI { return c.sessions }

// Panes returns the PaneAPI subclient. Always non-nil for a Client
// constructed by New.
func (c *Client) Panes() PaneAPI { return c.panes }

// Close releases the transport's idle connections. Currently a
// best-effort cleanup — with DisableKeepAlives we hold no pooled
// connections, but calling CloseIdleConnections is harmless and
// makes the contract forward-compatible with a future
// connection-pooled transport.
func (c *Client) Close() error {
	if tr, ok := c.http.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Internal request helpers — all methods funnel through these so
// error decoding, body handling, and ctx propagation are uniform.
// ---------------------------------------------------------------------------

// doPost issues a POST request with a JSON body and decodes the
// response into out (when non-nil). Returns nil on 2xx, a
// *ClientError on a structured 4xx/5xx, ErrServerUnavailable on a
// connection failure, or a wrapped error otherwise.
func (c *Client) doPost(ctx context.Context, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("mux/client: marshal request: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("mux/client: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, out)
}

// doGet issues a GET request and decodes the response into out (when
// non-nil). query is appended as the query string when non-empty.
func (c *Client) doGet(ctx context.Context, path string, query url.Values, out any) error {
	full := baseURL + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return fmt.Errorf("mux/client: build request: %w", err)
	}
	return c.do(req, out)
}

// do executes req, decodes the body, and routes errors. The split
// between doPost/doGet and do keeps the verb-specific code (body
// encoding, query string assembly) separate from the verb-agnostic
// error-handling logic.
func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		// Connection-class failures are reported as
		// ErrServerUnavailable so CLI callers can give a clean
		// "daemon not running" message. We classify by inspecting
		// the wrapped error chain — net.OpError for connect-time
		// failures, os.ErrNotExist for a missing socket file,
		// syscall.ECONNREFUSED for an unbound path.
		if isUnavailable(err) {
			return fmt.Errorf("%w: %w", ErrServerUnavailable, err)
		}
		return fmt.Errorf("mux/client: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("mux/client: read response body: %w", readErr)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeErrorBody(resp.StatusCode, body)
	}

	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("mux/client: decode response body (status %d): %w", resp.StatusCode, err)
	}
	return nil
}

// isUnavailable inspects an http.Client.Do error chain for the
// connection-class failures we want to surface as
// ErrServerUnavailable. The candidate set:
//
//   - any wrapped os.ErrNotExist (the socket file does not exist)
//   - any wrapped net.OpError on dial — this catches
//     ECONNREFUSED, EACCES on a stale socket, and the
//     bare-Unix-path "no such file" case on Linux
//   - io.EOF after a partial dial (rare: the daemon Accept()ed and
//     then immediately closed the connection without sending a
//     response)
//
// We deliberately do NOT classify HTTP-level errors as unavailable —
// a 500 from a running server is a ClientError, not a connect
// failure.
func isUnavailable(err error) bool {
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// Only treat dial-time op-errors as unavailable; "read" /
		// "write" op-errors mid-request indicate the daemon went
		// away after we already started — surface those as the raw
		// error so the caller knows it was not a clean refusal.
		if opErr.Op == "dial" {
			return true
		}
	}
	return false
}
