package podmanproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"sync"
	"syscall"
	"time"
)

// Config holds the proxy's runtime configuration. All public fields are
// inputs; the proxy never mutates Config after NewProxy returns.
type Config struct {
	// ListenerPath is the absolute path of the Unix socket the proxy
	// listens on. Required. The file is created by Serve and removed
	// when Serve returns.
	ListenerPath string

	// UpstreamPath is the absolute path of the docker/podman Unix socket
	// the proxy forwards to. Required. The file does not need to exist
	// at NewProxy time — when it is missing, every request returns the
	// friendly upstream-unavailable 503 envelope.
	UpstreamPath string

	// AllowedBindSources is the set of host path prefixes that may appear
	// as the source side of a HostConfig.Bind, the Source side of a
	// HostConfig.Mounts entry of Type "bind", or the `path` query on
	// PUT /containers/{id}/archive. A path is allowed iff, after
	// filepath.Clean, it equals an entry exactly or is a strict child of
	// one (i.e., starts with entry + "/"). Substring matches are NOT
	// permitted: "/srv" does NOT allow "/srv-other".
	//
	// The set MAY contain "/" — this is intentional so the security
	// test suite can use a negative-control profile that opens every
	// path, proving the positive tests are not no-ops because of an
	// unrelated denial code path.
	AllowedBindSources []string

	// AllowedCaps is the set of Linux capabilities that may appear in
	// HostConfig.CapAdd. Comparison is case-insensitive; callers may
	// pass either "CAP_NET_BIND_SERVICE" or "NET_BIND_SERVICE" and the
	// proxy normalises both ends of the comparison.
	AllowedCaps []string

	// MaxMemoryBytes is the inclusive upper bound for HostConfig.Memory.
	// Zero disables the cap (any memory value passes).
	MaxMemoryBytes int64

	// MaxCPUQuota is the inclusive upper bound for HostConfig.CpuQuota
	// (CFS quota in microseconds). Zero disables the cap.
	MaxCPUQuota int64

	// AuditWriter receives exactly one JSON line per accepted or
	// rejected request. Nil silently drops audit events.
	AuditWriter io.Writer

	// MaxBodyBytes caps the size of a body-bearing request that the
	// proxy will buffer in memory for inspection. Zero uses
	// DefaultMaxBodyBytes. Bodies exceeding the cap are denied with
	// 413 Request Entity Too Large and never forwarded.
	MaxBodyBytes int64

	// DialTimeout caps how long the upstream Unix-socket dial may take
	// before the proxy synthesises a 503 upstream-unavailable. Zero
	// uses DefaultDialTimeout.
	DialTimeout time.Duration

	// Clock is the time source used to stamp audit events. nil uses
	// time.Now. Tests override this to assert exact timestamps.
	Clock func() time.Time
}

// Default values used by Config when zero-valued.
const (
	// DefaultMaxBodyBytes is the request-body cap used when Config.MaxBodyBytes
	// is zero. 16 MiB is generous enough for containers/create bodies that
	// carry a moderately-sized Env or Labels map without forcing callers
	// to override the default.
	DefaultMaxBodyBytes int64 = 16 << 20

	// DefaultDialTimeout is the upstream dial timeout used when
	// Config.DialTimeout is zero.
	DefaultDialTimeout = 5 * time.Second

	// shutdownTimeout is how long Serve gives the http.Server to drain
	// in-flight requests on context cancellation before returning.
	shutdownTimeout = 2 * time.Second
)

// Proxy is a default-deny HTTP filter proxy in front of a docker/podman
// REST API Unix socket. Construct with NewProxy and run with Serve.
type Proxy struct {
	cfg      Config
	upstream *httputil.ReverseProxy

	// mu guards listener and server while Serve is initialising; once
	// Serve enters its select loop, listener and server are read-only.
	mu       sync.Mutex
	listener net.Listener
	server   *http.Server

	// auditMu serialises writes to AuditWriter. The AC requires exactly
	// one JSON line per request, so interleaved writes from concurrent
	// requests must not corrupt each other.
	auditMu sync.Mutex
}

// NewProxy validates cfg and returns a Proxy ready to be Served. The
// proxy does not bind its listener until Serve is called — the only
// failure modes of NewProxy are missing required Config fields.
func NewProxy(cfg Config) (*Proxy, error) {
	if cfg.ListenerPath == "" {
		return nil, errors.New("podmanproxy: Config.ListenerPath is required")
	}
	if cfg.UpstreamPath == "" {
		return nil, errors.New("podmanproxy: Config.UpstreamPath is required")
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.MaxBodyBytes < 0 {
		return nil, fmt.Errorf("podmanproxy: Config.MaxBodyBytes must be >= 0, got %d", cfg.MaxBodyBytes)
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = DefaultDialTimeout
	}
	if cfg.DialTimeout < 0 {
		return nil, fmt.Errorf("podmanproxy: Config.DialTimeout must be >= 0, got %v", cfg.DialTimeout)
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}

	p := &Proxy{cfg: cfg}

	dialer := &net.Dialer{Timeout: cfg.DialTimeout}
	transport := &http.Transport{
		// All connections route through the upstream Unix socket
		// regardless of the request URL — we set Director below to
		// rewrite the scheme/host so net/http does not try DNS.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", cfg.UpstreamPath)
		},
		// Compression is irrelevant for a local Unix socket and would
		// only obscure body inspection during debugging.
		DisableCompression: true,
		// Allow long-lived streaming connections (attach, exec, logs
		// follow=1) without a forced idle close.
		IdleConnTimeout: 0,
	}

	p.upstream = &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = "http"
			// The host is irrelevant for a Unix-socket dial but the
			// Go HTTP client still requires a non-empty URL.Host and
			// Host header for HTTP/1.1 request-line composition.
			r.URL.Host = "podman.sock"
			r.Host = "podman.sock"
		},
		Transport:    transport,
		ErrorHandler: p.upstreamErrorHandler,
		// FlushInterval -1 flushes immediately on every Write, which
		// matters for streaming endpoints (attach / exec start / logs
		// follow). Buffering would stall the agent's interactive
		// session.
		FlushInterval: -1,
	}

	return p, nil
}

// Serve binds the listener at ListenerPath, serves HTTP, and blocks
// until ctx is cancelled. On cancellation it shuts the http.Server down
// (giving in-flight requests up to shutdownTimeout to drain) and removes
// the listener socket file before returning.
//
// The error returned by Serve is nil on clean ctx-cancellation shutdown,
// or a wrapped non-nil error if the initial bind failed or the server
// returned an unexpected error during accept.
func (p *Proxy) Serve(ctx context.Context) error {
	// Remove any leftover socket file from a previous run before bind.
	// On a fresh boot ListenerPath does not exist; on a crashed-and-
	// restarted process it may exist as a dead socket file and bind
	// would fail with EADDRINUSE. Best-effort: ignore the result.
	_ = os.Remove(p.cfg.ListenerPath)

	l, err := net.Listen("unix", p.cfg.ListenerPath)
	if err != nil {
		return fmt.Errorf("podmanproxy: listen %q: %w", p.cfg.ListenerPath, err)
	}
	// Restrict the socket to the owning user. The umask of the calling
	// process is the floor here; chmod 0600 hardens against an over-
	// permissive umask.
	_ = os.Chmod(p.cfg.ListenerPath, 0o600)

	srv := &http.Server{
		Handler:           http.HandlerFunc(p.serveHTTP),
		ReadHeaderTimeout: 30 * time.Second,
	}
	p.mu.Lock()
	p.listener = l
	p.server = srv
	p.mu.Unlock()

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(l)
		if errors.Is(err, http.ErrServerClosed) {
			serveErr <- nil
			return
		}
		serveErr <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		<-serveErr
		_ = os.Remove(p.cfg.ListenerPath)
		return nil

	case err := <-serveErr:
		_ = os.Remove(p.cfg.ListenerPath)
		if err != nil {
			return fmt.Errorf("podmanproxy: serve: %w", err)
		}
		return nil
	}
}

// upstreamErrorHandler runs when the reverse proxy fails to dial or
// receive a usable response from the upstream socket. It synthesises
// the friendly 503 envelope.
//
// No audit line is emitted here: by the time the upstream is contacted,
// the policy decision (allow) was already audited at serveHTTP time.
// The AC requires exactly one audit line per request.
func (p *Proxy) upstreamErrorHandler(w http.ResponseWriter, _ *http.Request, err error) {
	writeUnavailable(w, classifyUpstreamErr(err))
}

// classifyUpstreamErr maps a net/http upstream error to a short reason
// string embedded in the friendly 503 envelope. The reason is used
// verbatim in the message body, so it MUST be safe to render to an
// untrusted client (no host paths or internal IDs).
func classifyUpstreamErr(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ENOENT), errors.Is(err, os.ErrNotExist):
		return "socket missing"
	case errors.Is(err, context.DeadlineExceeded):
		return "dial timeout"
	case errors.Is(err, syscall.EACCES), errors.Is(err, os.ErrPermission):
		return "permission denied"
	default:
		return "dial failed"
	}
}
