package podmanproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every escape vector in the threat table (docs/podman-proxy.md,
// "Threat model") gets an explicit security test. The tests share a small scaffold
// that stands up a real podmanproxy in front of a fake upstream so we
// can assert both the synthesised 4xx (no upstream forward) AND the
// upstream-forwarded path.
//
// The negative-control test (TestSecurity_NegativeControl_RootAllowlistPasses)
// at the bottom of this file is the load-bearing meta-test that proves
// the positive denials are not no-ops resulting from an unrelated
// failure mode somewhere else in the policy code path.

// ───────────────────────────── test scaffold ─────────────────────────────

// shortSocketDir creates a temp directory with a short path suitable
// for a Unix socket bind. The Linux sockaddr_un.sun_path field is
// limited to 108 bytes; Darwin caps at 104. t.TempDir() embeds the
// test name and parallel-subtest counter, which can push the path
// past the limit when TMPDIR is the Nix sandbox
// (/dev/shm/prism-go-test.<token>/...) and the subtest name is long.
// A path of about 117 bytes fails bind with EINVAL under that layout.
//
// Pattern matches cmd/merge_proxy_test.go's startFakeHostAPIServer —
// MkdirTemp("/tmp", "p") deterministically yields a short path
// regardless of how long TMPDIR happens to be on the host. /tmp is
// writable in the Nix build sandbox (verified by the existing tests
// in cmd/ that use this pattern under the homeless-shelter gate).
//
// Use this helper for any test that creates a socket file. The
// directory is registered for cleanup via t.Cleanup so it does not
// outlive the test.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatalf("mkdir socket tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeUpstream is a minimal docker-API mock that records every request
// it sees and returns 200 OK to every well-formed request. The proxy's
// reverse-proxy path forwards to this; the proxy's deny path never
// reaches it.
type fakeUpstream struct {
	server   *http.Server
	sockPath string
	listener net.Listener

	// requests counts every request that reached the upstream. The
	// security tests assert that denied requests do NOT increment this.
	requests atomic.Int64

	// lastBody captures the body of the most recent request. Used by
	// the "forward unmodified" tests to assert the proxy didn't mangle
	// a legitimate body en route.
	lastBody atomic.Value // []byte

	// lastRawQuery captures the URL.RawQuery of the most recent
	// request. Used by the name-prefix tests that assert the `?name=`
	// channel was rewritten alongside the body Name.
	lastRawQuery atomic.Value // string

	// presentImages names the image references this upstream reports as
	// ALREADY in its store, answering the proxy's image-existence probe
	// (GET /images/{ref}/json) with 200 instead of 404.
	//
	// The default — an empty map — means "the store is empty", so a
	// pull through the proxy is a genuine new pull and lands in the
	// image ledger. Tests that need the opposite call markImagePresent.
	//
	// probeStatusOverride, when non-zero, makes the probe route answer
	// with that status instead, so a test can exercise the
	// probe-failure path.
	presentMu           sync.Mutex
	presentImages       map[string]struct{}
	probeStatusOverride int

	// probes counts image-existence probes separately from forwarded
	// requests, so `requests` keeps meaning "requests the proxy
	// forwarded on a client's behalf".
	probes atomic.Int64

	// rawPaths records the UNNORMALISED request-target of every request
	// this upstream received, captured before http.ServeMux gets a
	// chance to clean the path or answer it with a 301.
	//
	// This exists for the path-traversal tests. ServeMux collapses dot
	// segments itself, so a test that asserts on r.URL.Path inside a
	// mux handler cannot tell "the proxy never sent `..`" from "the mux
	// cleaned it away". Only the raw target answers that.
	rawMu    sync.Mutex
	rawPaths []string
}

// capturedRawPaths returns every unnormalised request-target this
// upstream has seen.
func (fu *fakeUpstream) capturedRawPaths() []string {
	fu.rawMu.Lock()
	defer fu.rawMu.Unlock()
	return append([]string(nil), fu.rawPaths...)
}

// markImagePresent makes the upstream report ref as already in its
// store when the proxy probes for it.
func (fu *fakeUpstream) markImagePresent(ref string) {
	fu.presentMu.Lock()
	defer fu.presentMu.Unlock()
	if fu.presentImages == nil {
		fu.presentImages = map[string]struct{}{}
	}
	fu.presentImages[ref] = struct{}{}
}

// setProbeStatus forces every subsequent image-existence probe to
// answer with status, so a test can drive the probe-failure branch.
func (fu *fakeUpstream) setProbeStatus(status int) {
	fu.presentMu.Lock()
	defer fu.presentMu.Unlock()
	fu.probeStatusOverride = status
}

// imageProbeRef returns the image reference an image-inspect path names
// ("/images/<ref>/json"), or "" when the path is not a probe.
func imageProbeRef(path string) string {
	const prefix = "/images/"
	const suffix = "/json"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	// shortSocketDir, not t.TempDir() — Unix socket path length is
	// kernel-bounded (108 bytes on Linux, 104 on Darwin) and
	// t.TempDir() embeds the full test/subtest name which can blow
	// the limit under the Nix sandbox.
	dir := shortSocketDir(t)
	sockPath := filepath.Join(dir, "podman.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen upstream sock: %v", err)
	}
	fu := &fakeUpstream{sockPath: sockPath, listener: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Image-existence probe: the proxy's own read-only lookup,
		// not a forwarded client request. Answer it from
		// presentImages and do NOT count it in `requests`.
		if ref := imageProbeRef(r.URL.Path); ref != "" && r.Method == http.MethodGet {
			fu.probes.Add(1)
			fu.presentMu.Lock()
			override := fu.probeStatusOverride
			_, present := fu.presentImages[ref]
			fu.presentMu.Unlock()
			switch {
			case override != 0:
				w.WriteHeader(override)
			case present:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"Id":"sha256:already-here"}`))
			default:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"No such image"}`))
			}
			return
		}
		fu.requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		fu.lastBody.Store(body)
		fu.lastRawQuery.Store(r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Id":"deadbeef"}`))
	})
	// Record the raw request-target BEFORE the mux sees it. The mux
	// cleans dot segments and answers with a 301, so a capture inside a
	// handler would never observe a traversal attempt.
	recordRaw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fu.rawMu.Lock()
		fu.rawPaths = append(fu.rawPaths, r.RequestURI)
		fu.rawMu.Unlock()
		mux.ServeHTTP(w, r)
	})
	fu.server = &http.Server{Handler: recordRaw}
	go func() { _ = fu.server.Serve(ln) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = fu.server.Shutdown(shutdownCtx)
	})
	return fu
}

// captured returns the recorded request count. Helper so tests read
// fluently as "want zero upstream calls".
func (fu *fakeUpstream) captured() int64 {
	return fu.requests.Load()
}

// proxyHarness is the per-test return value of startProxy /
// startProxyWithCaps. It packages the listener socket path, the
// audit buffer, and the REAL allowed directories the test should
// bind to in positive-path tests.
//
// allowedDir and scratchDir are real, existing directories in the
// configured AllowedBindSources list. isAllowedBindSource calls
// filepath.EvalSymlinks, which errors on paths that do not exist — so
// a test that wants to
// exercise the positive path of the bind allowlist must use a path
// that actually exists on disk. Tests can also create subdirectories
// inside allowedDir / scratchDir for finer-grained scenarios.
type proxyHarness struct {
	sock       string
	audit      *bytes.Buffer
	allowedDir string
	scratchDir string
}

// startProxy stands a podmanproxy.Proxy on a fresh listener socket in
// front of the supplied upstream, with sensible defaults that cover
// the threat-table tests. Resource caps are intentionally LEFT
// DISABLED here: when a cap is configured the strict mode requires
// every create body to set the corresponding field, which would force
// every unrelated test (bind escapes, privileged, etc.) to also
// include placeholder Memory/CpuQuota values. Tests that specifically
// exercise the resource caps configure them in their own Config via
// startProxyWithConfig.
func startProxy(t *testing.T, fu *fakeUpstream) *proxyHarness {
	t.Helper()
	// shortSocketDir, not t.TempDir() — see newFakeUpstream for rationale.
	dir := shortSocketDir(t)
	listenPath := filepath.Join(dir, "proxy.sock")
	auditBuf := &bytes.Buffer{}

	allowedDir := mkRealDir(t, "allow")
	scratchDir := mkRealDir(t, "scratch")

	cfg := Config{
		ListenerPath:       listenPath,
		UpstreamPath:       fu.sockPath,
		AllowedBindSources: []string{allowedDir, scratchDir},
		AllowedCaps:        []string{}, // empty by default
		AuditWriter:        auditBuf,
		// No MaxMemoryBytes / MaxCPUQuota / MaxNanoCpus — see comment above.
		// No AllowedSecurityOpts — default-deny on SecurityOpt is exercised
		// by the SecurityOpt-specific tests.
	}
	startProxyWithConfig(t, cfg, auditBuf, listenPath)
	return &proxyHarness{
		sock:       listenPath,
		audit:      auditBuf,
		allowedDir: allowedDir,
		scratchDir: scratchDir,
	}
}

// startProxyWithCaps mirrors startProxy but enables the resource
// caps. Tests that exercise the cap policy (over-cap, zero, negative,
// absent) use this so the cap is actually active.
func startProxyWithCaps(t *testing.T, fu *fakeUpstream) *proxyHarness {
	t.Helper()
	dir := shortSocketDir(t)
	listenPath := filepath.Join(dir, "proxy.sock")
	auditBuf := &bytes.Buffer{}

	allowedDir := mkRealDir(t, "allow")
	scratchDir := mkRealDir(t, "scratch")

	cfg := Config{
		ListenerPath:       listenPath,
		UpstreamPath:       fu.sockPath,
		AllowedBindSources: []string{allowedDir, scratchDir},
		MaxMemoryBytes:     4 * 1024 * 1024 * 1024,
		MaxCPUQuota:        200_000,
		MaxNanoCpus:        2_000_000_000, // 2 cores
		AuditWriter:        auditBuf,
	}
	startProxyWithConfig(t, cfg, auditBuf, listenPath)
	return &proxyHarness{
		sock:       listenPath,
		audit:      auditBuf,
		allowedDir: allowedDir,
		scratchDir: scratchDir,
	}
}

// mkRealDir creates a real, existing tempdir under /tmp with the
// given prefix. Cleanup is registered with t.Cleanup. Used as
// allowlist entries (the symlink-resolution check requires allowlist
// entries to actually exist) and as bind sources in positive-path
// tests.
func mkRealDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatalf("mkdir real dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// validCapsBody returns a hostConfig fragment that satisfies all the
// active resource caps configured by startProxyWithCaps. Tests that
// want to exercise a single cap (e.g. memory-over-cap) start with
// this baseline and override the one field under test.
func validCapsBody() map[string]any {
	return map[string]any{
		"Memory":   int64(1 * 1024 * 1024 * 1024), // 1 GiB
		"CpuQuota": int64(50_000),                 // 0.5 cores
		"NanoCpus": int64(500_000_000),            // 0.5 cores
	}
}

// startProxyWithConfig is the low-level scaffold helper used by tests
// that need to mutate the policy config (e.g. the negative-control).
func startProxyWithConfig(t *testing.T, cfg Config, _ *bytes.Buffer, listenPath string) *Proxy {
	t.Helper()
	p, err := NewProxy(cfg)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serveErr := make(chan error, 1)
	go func() { serveErr <- p.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serveErr:
			if err != nil {
				t.Logf("Serve returned: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Log("Serve did not return within 3s of cancellation")
		}
	})
	waitForSocket(t, listenPath)
	return p
}

// waitForSocket polls until the proxy's listener accepts connections
// or the timeout elapses. Tests use this to avoid a race between
// Serve's listener bind and the first client request.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.Dial("unix", path)
		if err == nil {
			conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy socket %s did not become ready: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// proxyClient returns an http.Client wired to dial the proxy's Unix
// socket. Every test request goes through this client.
func proxyClient(socket string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// postCreate is a tiny helper for the most-used test shape: POST
// /v1.41/containers/create with the supplied HostConfig.
func postCreate(t *testing.T, sock string, hostConfig map[string]any) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"Image":      "alpine",
		"HostConfig": hostConfig,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost,
		"http://podman.sock/v1.41/containers/create",
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return doRequest(t, sock, req)
}

// mustNewRequest is the equivalent of postCreate for non-create
// endpoints: it marshals body as JSON and constructs the request.
// Used by the update / exec / SecurityOpt-allowlist tests where the
// URL is something other than /containers/create.
func mustNewRequest(t *testing.T, method, url string, body any) *http.Request {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func doRequest(t *testing.T, sock string, req *http.Request) *http.Response {
	t.Helper()
	client := proxyClient(sock)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

// readEnvelope reads resp.Body and unmarshals the {"message": "..."}
// envelope. Tests assert the message text contains a guidance keyword
// rather than the full string, so the wording can evolve without
// breaking the tests.
func readEnvelope(t *testing.T, resp *http.Response) errorEnvelope {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (body=%q)", err, body)
	}
	return env
}

// assertNoForward fails the test when the upstream's request count is
// non-zero. Used after every deny-path test.
func assertNoForward(t *testing.T, fu *fakeUpstream) {
	t.Helper()
	if got := fu.captured(); got != 0 {
		t.Fatalf("expected zero upstream requests, got %d (a deny path forwarded to upstream)", got)
	}
}

// ───────────────────────── threat-table tests ─────────────────────────

// host-bind escape: Binds source outside the allowlist.
//
// AC: A containers/create request with any HostConfig.Binds source
// outside the allowlist returns 403 with a docker-compatible
// {"message": "..."} body and is NOT forwarded upstream.
func TestSecurity_HostBindOutsideAllowlist_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"Binds": []string{"/etc/passwd:/host_passwd:ro"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	env := readEnvelope(t, resp)
	if env.Message == "" {
		t.Fatal("expected non-empty error message")
	}
	if !strings.Contains(env.Message, "/etc/passwd") {
		t.Errorf("error message should name the rejected source, got: %q", env.Message)
	}
	assertNoForward(t, fu)
}

// host-bind escape: Mounts source outside the allowlist (newer API).
func TestSecurity_HostMountOutsideAllowlist_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"Mounts": []map[string]any{
			{
				"Type":   "bind",
				"Source": "/Users/bensherman",
				"Target": "/host",
			},
		},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	assertNoForward(t, fu)
}

// privileged escape.
//
// AC: A containers/create request with HostConfig.Privileged: true
// returns 403 and is not forwarded.
func TestSecurity_Privileged_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"Privileged": true,
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	env := readEnvelope(t, resp)
	if !strings.Contains(strings.ToLower(env.Message), "privileged") {
		t.Errorf("envelope should mention 'privileged', got %q", env.Message)
	}
	assertNoForward(t, fu)
}

// cap-add escape: CapAdd entry outside the allowlist.
//
// AC: A containers/create request with HostConfig.CapAdd containing a
// value outside the allowlist returns 403.
func TestSecurity_CapAddDisallowed_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"CapAdd": []string{"SYS_ADMIN"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	assertNoForward(t, fu)
}

// cap-add allowlist: explicit allow-list entry passes (sanity check).
func TestSecurity_CapAddInAllowlist_Allowed(t *testing.T) {
	fu := newFakeUpstream(t)
	dir := shortSocketDir(t)
	allowedDir := mkRealDir(t, "allow")
	cfg := Config{
		ListenerPath:       filepath.Join(dir, "proxy.sock"),
		UpstreamPath:       fu.sockPath,
		AllowedBindSources: []string{allowedDir},
		AllowedCaps:        []string{"NET_BIND_SERVICE"},
		AuditWriter:        &bytes.Buffer{},
	}
	startProxyWithConfig(t, cfg, nil, cfg.ListenerPath)

	resp := postCreate(t, cfg.ListenerPath, map[string]any{
		"CapAdd": []string{"CAP_NET_BIND_SERVICE"}, // both forms
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if fu.captured() != 1 {
		t.Errorf("expected 1 upstream request, got %d", fu.captured())
	}
}

// host-network escape and the four sibling host-* modes.
//
// AC: A containers/create request with HostConfig.NetworkMode: "host"
// returns 403. Same for PidMode, IpcMode, UTSMode, UsernsMode.
func TestSecurity_HostNamespaceModes_Denied(t *testing.T) {
	modes := []string{"NetworkMode", "PidMode", "IpcMode", "UTSMode", "UsernsMode"}
	for _, field := range modes {
		t.Run(field, func(t *testing.T) {
			fu := newFakeUpstream(t)
			h := startProxy(t, fu)
			sock := h.sock
			resp := postCreate(t, sock, map[string]any{
				field: "host",
			})
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s: status got %d want %d", field, resp.StatusCode, http.StatusForbidden)
			}
			assertNoForward(t, fu)
		})
	}
}

// devices escape: non-empty Devices.
//
// AC: A containers/create request with non-empty HostConfig.Devices
// returns 403.
func TestSecurity_DevicesNonEmpty_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"Devices": []map[string]any{
			{
				"PathOnHost":        "/dev/sda1",
				"PathInContainer":   "/dev/sda1",
				"CgroupPermissions": "rwm",
			},
		},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	assertNoForward(t, fu)
}

// ───────────────── resource caps (strict mode) ─────────────────────
//
// Resource caps are enforced server-side; out-of-bounds returns 403.
// The strict interpretation is that the cap must actually be
// enforceable — docker's Memory=0 "unlimited" semantic and an absent
// Memory field both have to be denied when the cap is configured,
// otherwise the cap can be trivially bypassed.

func TestSecurity_MemoryOverCap_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock

	// Configured cap is 4 GiB; request 10 TiB. CpuQuota / NanoCpus
	// supplied so the request only trips the memory check.
	body := validCapsBody()
	body["Memory"] = int64(10) * 1024 * 1024 * 1024 * 1024
	resp := postCreate(t, sock, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	assertNoForward(t, fu)
}

func TestSecurity_CPUQuotaOverCap_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock

	body := validCapsBody()
	body["CpuQuota"] = int64(2_000_000)
	resp := postCreate(t, sock, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	assertNoForward(t, fu)
}

// MemoryZero: docker treats Memory=0 as "unlimited". When a cap is
// configured, 0 must be denied so the cap cannot be bypassed by
// stating "I want unlimited memory".
func TestSecurity_MemoryZero_DeniedWhenCapActive(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock
	body := validCapsBody()
	body["Memory"] = int64(0)
	resp := postCreate(t, sock, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Memory=0 should be denied when cap is active; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

func TestSecurity_MemoryNegative_DeniedWhenCapActive(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock
	body := validCapsBody()
	body["Memory"] = int64(-1)
	resp := postCreate(t, sock, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Memory<0 should be denied when cap is active; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// MemoryAbsent_DeniedWhenCapActive: an agent that simply omits the
// Memory field also gets the host default (unlimited) on docker /
// podman, which is the same bypass class as Memory=0. The strict
// policy denies this too so the cap is actually enforceable.
func TestSecurity_MemoryAbsent_DeniedWhenCapActive(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock
	body := validCapsBody()
	delete(body, "Memory")
	resp := postCreate(t, sock, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("absent Memory should be denied when cap is active; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// MemoryAbsent_AllowedWhenNoCap: when MaxMemoryBytes==0 (cap disabled)
// an absent Memory field must NOT be denied — the cap is opt-in and
// the default-no-cap mode preserves docker's default behaviour.
func TestSecurity_MemoryAbsent_AllowedWhenNoCap(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock
	// No Memory field, no other resource fields. With no cap active,
	// this should pass. Use the harness's real allowedDir so the
	// bind source resolves via EvalSymlinks.
	resp := postCreate(t, sock, map[string]any{
		"Binds": []string{h.allowedDir + ":/app:ro"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("absent Memory with no cap should be allowed; got %d", resp.StatusCode)
	}
}

// CpuQuotaZero / CpuQuotaAbsent: identical class to Memory.
func TestSecurity_CpuQuotaZero_DeniedWhenCapActive(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock
	body := validCapsBody()
	body["CpuQuota"] = int64(0)
	resp := postCreate(t, sock, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CpuQuota=0 should be denied when cap is active; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

func TestSecurity_CpuQuotaAbsent_DeniedWhenCapActive(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock
	body := validCapsBody()
	delete(body, "CpuQuota")
	resp := postCreate(t, sock, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("absent CpuQuota should be denied when cap is active; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// NanoCpus is a fully orthogonal way to express CPU caps from
// CpuQuota. If only CpuQuota was capped, an agent that wanted to
// burst above the cap could simply state the request as NanoCpus.
func TestSecurity_NanoCpusOverCap_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock
	body := validCapsBody()
	body["NanoCpus"] = int64(8_000_000_000) // 8 cores; cap is 2 cores
	resp := postCreate(t, sock, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("NanoCpus over cap should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

func TestSecurity_NanoCpusZero_DeniedWhenCapActive(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock
	body := validCapsBody()
	body["NanoCpus"] = int64(0)
	resp := postCreate(t, sock, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("NanoCpus=0 should be denied when cap is active; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

func TestSecurity_NanoCpusAbsent_DeniedWhenCapActive(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock
	body := validCapsBody()
	delete(body, "NanoCpus")
	resp := postCreate(t, sock, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("absent NanoCpus should be denied when cap is active; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// Sanity: when every cap is set to a value within bounds, the
// request is forwarded. Proves the strict cap suite is not just a
// blanket deny.
func TestSecurity_ResourceCapsWithinBounds_Allowed(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock
	resp := postCreate(t, sock, validCapsBody())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid resource caps should be allowed; got %d", resp.StatusCode)
	}
	if fu.captured() != 1 {
		t.Errorf("expected 1 upstream call, got %d", fu.captured())
	}
}

// ─────────── containers/update: cap fields cannot be reset ───────────
//
// AC class: an agent that creates with valid caps could otherwise
// POST update with Memory=0 to remove the cap. Update body must run
// the same per-field nonpositive / over-cap denials, but with the
// absent-field policy relaxed (an update body is partial — missing
// fields just are not being changed).

func TestSecurity_UpdateEndpoint_MemoryZero_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock
	req := mustNewRequest(t, http.MethodPost,
		"http://podman.sock/v1.41/containers/abc/update",
		map[string]any{"Memory": int64(0)})
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("update Memory=0 should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

func TestSecurity_UpdateEndpoint_MemoryOverCap_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock
	req := mustNewRequest(t, http.MethodPost,
		"http://podman.sock/v1.41/containers/abc/update",
		map[string]any{"Memory": int64(10) * 1024 * 1024 * 1024 * 1024})
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("update Memory over cap should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

func TestSecurity_UpdateEndpoint_PartialBodyAllowed(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock
	// Only setting CpuShares (not in our cap list). Absent Memory and
	// CpuQuota are allowed in update context.
	req := mustNewRequest(t, http.MethodPost,
		"http://podman.sock/v1.41/containers/abc/update",
		map[string]any{"CpuShares": 512})
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update with only non-capped fields should be allowed; got %d", resp.StatusCode)
	}
}

func TestSecurity_UpdateEndpoint_MalformedJSON_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock
	req, _ := http.NewRequest(http.MethodPost,
		"http://podman.sock/v1.41/containers/abc/update",
		bytes.NewReader([]byte(`{garbage`)))
	req.Header.Set("Content-Type", "application/json")
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed update body should be 400; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// ─────────── containers/exec: Privileged cannot be re-introduced ──────
//
// AC class: an agent that creates a non-privileged container could
// otherwise exec into it with Privileged=true — docker exec grants
// extra capabilities independent of the parent container's
// HostConfig.Privileged.

func TestSecurity_ExecPrivileged_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock
	req := mustNewRequest(t, http.MethodPost,
		"http://podman.sock/v1.41/containers/abc/exec",
		map[string]any{
			"Cmd":        []string{"id"},
			"Privileged": true,
		})
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("exec with Privileged=true should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

func TestSecurity_ExecWithoutPrivileged_Allowed(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock
	req := mustNewRequest(t, http.MethodPost,
		"http://podman.sock/v1.41/containers/abc/exec",
		map[string]any{
			"Cmd":        []string{"id"},
			"Privileged": false,
		})
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec without Privileged should be allowed; got %d", resp.StatusCode)
	}
	if fu.captured() != 1 {
		t.Errorf("expected 1 upstream call, got %d", fu.captured())
	}
}

func TestSecurity_ExecMalformedJSON_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock
	req, _ := http.NewRequest(http.MethodPost,
		"http://podman.sock/v1.41/containers/abc/exec",
		bytes.NewReader([]byte(`{garbage`)))
	req.Header.Set("Content-Type", "application/json")
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed exec body should be 400; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// ─────────── SecurityOpt default-deny ─────────────────────────────────
//
// AC class: SecurityOpt entries like "seccomp=unconfined",
// "apparmor=unconfined", "no-new-privileges=false", and
// "label=disable" disable the container's security primitives. The
// strict default-deny policy rejects every SecurityOpt entry not in
// the configured allowlist (which is empty by default).

func TestSecurity_SecurityOpt_DefaultDenyAll(t *testing.T) {
	cases := []string{
		"seccomp=unconfined",
		"apparmor=unconfined",
		"no-new-privileges=false",
		"no-new-privileges:false",
		"label=disable",
		"label:disable",
		"systempaths=unconfined",
		// A custom seccomp profile path is also denied under default-deny.
		"seccomp=/etc/custom.json",
	}
	for _, opt := range cases {
		t.Run(opt, func(t *testing.T) {
			fu := newFakeUpstream(t)
			h := startProxy(t, fu)
			sock := h.sock
			resp := postCreate(t, sock, map[string]any{
				"SecurityOpt": []string{opt},
			})
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("SecurityOpt %q should be denied by default; got %d", opt, resp.StatusCode)
			}
			assertNoForward(t, fu)
		})
	}
}

// When an entry IS in AllowedSecurityOpts, it passes (sanity check
// that the deny is allowlist-driven, not a blanket reject).
func TestSecurity_SecurityOpt_InAllowlist_Allowed(t *testing.T) {
	fu := newFakeUpstream(t)
	dir := shortSocketDir(t)
	allowedDir := mkRealDir(t, "allow")
	cfg := Config{
		ListenerPath:        filepath.Join(dir, "proxy.sock"),
		UpstreamPath:        fu.sockPath,
		AllowedBindSources:  []string{allowedDir},
		AllowedSecurityOpts: []string{"no-new-privileges:true"},
		AuditWriter:         &bytes.Buffer{},
	}
	startProxyWithConfig(t, cfg, nil, cfg.ListenerPath)

	resp := postCreate(t, cfg.ListenerPath, map[string]any{
		"SecurityOpt": []string{"no-new-privileges:true"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SecurityOpt in allowlist should be allowed; got %d", resp.StatusCode)
	}
}

// Allowlist match is exact; the deny still fires for a sibling entry
// not in the allowlist.
func TestSecurity_SecurityOpt_PartialAllowlist_ExactMatch(t *testing.T) {
	fu := newFakeUpstream(t)
	dir := shortSocketDir(t)
	allowedDir := mkRealDir(t, "allow")
	cfg := Config{
		ListenerPath:        filepath.Join(dir, "proxy.sock"),
		UpstreamPath:        fu.sockPath,
		AllowedBindSources:  []string{allowedDir},
		AllowedSecurityOpts: []string{"no-new-privileges:true"},
		AuditWriter:         &bytes.Buffer{},
	}
	startProxyWithConfig(t, cfg, nil, cfg.ListenerPath)

	resp := postCreate(t, cfg.ListenerPath, map[string]any{
		"SecurityOpt": []string{"no-new-privileges:true", "seccomp=unconfined"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sibling unconfined entry should be denied; got %d", resp.StatusCode)
	}
}

// archive exfil: PUT /containers/{id}/archive?path=/etc — host path.
//
// AC: A PUT /containers/{id}/archive request with a `path` query
// parameter outside the allowlist returns 403.
func TestSecurity_ArchivePathOutsideAllowlist_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	req, err := http.NewRequest(http.MethodPut,
		"http://podman.sock/v1.41/containers/abc123/archive?path=%2Fetc",
		bytes.NewReader([]byte("tar body would go here")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	assertNoForward(t, fu)
}

// endpoint allowlist: any endpoint not in the allowlist returns 403.
//
// AC: A request to an endpoint outside the allowlist returns 403 with
// an actionable error message identifying the rejected endpoint.
func TestSecurity_UnknownEndpoint_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	for _, tc := range []struct {
		name, method, path string
	}{
		{"unknown-top-level", http.MethodGet, "/v1.41/server/info"},
		{"unknown-libpod", http.MethodPost, "/libpod/secrets/create"},
		{"undeclared-method", http.MethodPatch, "/v1.41/containers/abc/json"},
		{"swarm-mode", http.MethodGet, "/v1.41/swarm"},
		{"unversioned-bogus", http.MethodPost, "/swarm/init"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, "http://podman.sock"+tc.path, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp := doRequest(t, sock, req)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s %s: status got %d want %d", tc.method, tc.path, resp.StatusCode, http.StatusForbidden)
			}
			env := readEnvelope(t, resp)
			if !strings.Contains(env.Message, tc.path) {
				t.Errorf("envelope should name rejected path %q, got %q", tc.path, env.Message)
			}
			if !strings.Contains(strings.ToLower(env.Message), "not permitted") {
				t.Errorf("envelope should explain rejection, got %q", env.Message)
			}
		})
	}
	assertNoForward(t, fu)
}

// malformed JSON body returns 400 and is not forwarded.
//
// AC: A malformed JSON body on a body-bearing endpoint returns 400 and
// is not forwarded.
func TestSecurity_MalformedJSONBody_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	req, err := http.NewRequest(http.MethodPost,
		"http://podman.sock/v1.41/containers/create",
		bytes.NewReader([]byte(`{this is not valid JSON`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	assertNoForward(t, fu)
}

// empty body on containers/create is treated as malformed.
func TestSecurity_EmptyCreateBody_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	req, err := http.NewRequest(http.MethodPost,
		"http://podman.sock/v1.41/containers/create",
		bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	assertNoForward(t, fu)
}

// upstream unavailable: synthesise the friendly 503 envelope.
//
// AC: When the upstream socket is unreachable (file does not exist,
// OR dial fails with ECONNREFUSED), the proxy returns 503 with the
// actionable {"message": "..."} body.
//
// This test covers the "file does not exist" branch (ENOENT). The
// ECONNREFUSED branch is covered by TestSecurity_UpstreamECONNREFUSED_Returns503Envelope
// below, which engineers a real ECONNREFUSED by binding a unix socket
// and then closing the listener while leaving the socket file behind.
func TestSecurity_UpstreamMissing_Returns503Envelope(t *testing.T) {
	// Construct a proxy whose UpstreamPath does not exist.
	dir := shortSocketDir(t)
	cfg := Config{
		ListenerPath:       filepath.Join(dir, "proxy.sock"),
		UpstreamPath:       filepath.Join(dir, "does-not-exist.sock"),
		AllowedBindSources: []string{"/workspace"},
		AuditWriter:        &bytes.Buffer{},
	}
	startProxyWithConfig(t, cfg, nil, cfg.ListenerPath)

	req, err := http.NewRequest(http.MethodGet,
		"http://podman.sock/v1.41/_ping", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp := doRequest(t, cfg.ListenerPath, req)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	env := readEnvelope(t, resp)
	if !strings.HasPrefix(env.Message, "podman socket unavailable: ") {
		t.Errorf("envelope prefix: got %q, want 'podman socket unavailable: …'", env.Message)
	}
	if !strings.Contains(env.Message, "podman machine start") {
		t.Errorf("envelope should mention macOS recovery, got %q", env.Message)
	}
	if !strings.Contains(env.Message, "systemctl --user status podman.socket") {
		t.Errorf("envelope should mention Linux recovery, got %q", env.Message)
	}
}

// upstream connection-refused: bind a listener and close it, leaving
// a dead socket file behind. A dial against that file yields
// ECONNREFUSED on every platform where unix sockets are implemented
// (Linux, Darwin, *BSD).
//
// This pairs with TestSecurity_UpstreamMissing_Returns503Envelope to
// cover both halves of the upstream-unavailable AC.
func TestSecurity_UpstreamECONNREFUSED_Returns503Envelope(t *testing.T) {
	dir := shortSocketDir(t)
	deadSocket := filepath.Join(dir, "dead.sock")

	// Bind a real unix listener and close it. Go's UnixListener.Close
	// unlinks by default for Listen-created listeners, so we recreate
	// the file as a regular file afterwards — connecting to a non-socket
	// file path produces ECONNREFUSED on Linux and ENOTSOCK on Darwin,
	// both of which our classifier maps to a 503 envelope (the Darwin
	// case falls into the "dial failed" default branch, which still
	// returns 503).
	ln, err := net.Listen("unix", deadSocket)
	if err != nil {
		t.Fatalf("bind dead listener: %v", err)
	}
	_ = ln.Close()
	// Recreate the file as a regular file so the proxy's Transport
	// gets a definite dial failure rather than ENOENT.
	if err := os.WriteFile(deadSocket, []byte{}, 0o600); err != nil {
		t.Fatalf("recreate dead file: %v", err)
	}

	cfg := Config{
		ListenerPath:       filepath.Join(dir, "proxy.sock"),
		UpstreamPath:       deadSocket,
		AllowedBindSources: []string{"/workspace"},
		AuditWriter:        &bytes.Buffer{},
	}
	startProxyWithConfig(t, cfg, nil, cfg.ListenerPath)

	req, err := http.NewRequest(http.MethodGet,
		"http://podman.sock/v1.41/_ping", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp := doRequest(t, cfg.ListenerPath, req)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	env := readEnvelope(t, resp)
	if !strings.HasPrefix(env.Message, "podman socket unavailable: ") {
		t.Errorf("envelope prefix: got %q, want 'podman socket unavailable: …'", env.Message)
	}
}

// streaming endpoints forward without parsing a body.
//
// AC: Streaming endpoints (containers/{id}/attach, exec/{id}/start,
// containers/{id}/logs?follow=1) forward without attempting to parse
// a body.
func TestSecurity_StreamingEndpoints_ForwardWithoutBodyParse(t *testing.T) {
	cases := []struct {
		name, method, path string
		// supplyBody true means we send a non-JSON body that would
		// otherwise trip the JSON parser. The upstream is forgiving;
		// any non-403 response from the proxy proves the body did
		// not go through the inspector.
		supplyBody bool
	}{
		{"attach", http.MethodPost, "/v1.41/containers/abc/attach?stream=1", false},
		{"exec-start", http.MethodPost, "/v1.41/exec/exec123/start", true},
		{"logs-follow", http.MethodGet, "/v1.41/containers/abc/logs?follow=1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fu := newFakeUpstream(t)
			h := startProxy(t, fu)
			sock := h.sock
			var body io.Reader
			if tc.supplyBody {
				// A body that is NOT valid JSON. If the proxy
				// were body-parsing this endpoint it would return
				// 400; instead we expect the upstream to be hit.
				body = bytes.NewReader([]byte("not-json-and-shouldnt-be-parsed"))
			}
			req, err := http.NewRequest(tc.method, "http://podman.sock"+tc.path, body)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp := doRequest(t, sock, req)
			if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest {
				t.Fatalf("streaming endpoint should not be policy-rejected, got %d", resp.StatusCode)
			}
			if fu.captured() == 0 {
				t.Errorf("streaming endpoint should have forwarded to upstream")
			}
		})
	}
}

// audit log: one JSON line per request with the required fields.
//
// AC: Every accepted and rejected request emits exactly one line to
// the configured audit io.Writer with fields timestamp, method,
// endpoint, decision, reason as JSON.
func TestSecurity_AuditLog_OneLinePerRequest(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock
	audit := h.audit

	// Reject: privileged container.
	r1 := postCreate(t, sock, map[string]any{"Privileged": true})
	_ = r1.Body.Close()

	// Accept: harmless containers/create with a real allowed path.
	r2 := postCreate(t, sock, map[string]any{"Binds": []string{h.allowedDir + ":/x:ro"}})
	_ = r2.Body.Close()

	// Reject: unknown endpoint.
	req, _ := http.NewRequest(http.MethodGet, "http://podman.sock/v1.41/swarm", nil)
	r3 := doRequest(t, sock, req)
	_ = r3.Body.Close()

	lines := splitNonEmptyLines(audit.String())
	if len(lines) != 3 {
		t.Fatalf("expected 3 audit lines, got %d:\n%s", len(lines), audit.String())
	}

	wantDecisions := []string{auditDeny, auditAllow, auditDeny}
	for i, line := range lines {
		var entry auditLine
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("line %d not valid JSON: %v\n%s", i, err, line)
		}
		if entry.Timestamp == "" {
			t.Errorf("line %d missing timestamp", i)
		}
		if entry.Method == "" {
			t.Errorf("line %d missing method", i)
		}
		if entry.Endpoint == "" {
			t.Errorf("line %d missing endpoint", i)
		}
		if entry.Decision != wantDecisions[i] {
			t.Errorf("line %d decision: got %q want %q", i, entry.Decision, wantDecisions[i])
		}
		if entry.Reason == "" {
			t.Errorf("line %d missing reason", i)
		}
	}
}

// Each audit line must end with a newline so log aggregators that
// expect line-delimited JSON can parse the stream without grammar
// hacks.
func TestSecurity_AuditLog_NewlineTerminated(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock
	audit := h.audit

	resp := postCreate(t, sock, map[string]any{})
	_ = resp.Body.Close()
	out := audit.String()
	if out == "" {
		t.Fatal("audit log is empty")
	}
	if out[len(out)-1] != '\n' {
		t.Errorf("audit line is not newline-terminated, last byte = %x", out[len(out)-1])
	}
}

// raw-socket-curl bypass attempt — verify there is no path from the
// agent to the underlying upstream socket directly. The proxy package
// itself cannot prove this (the bind/SBPL rules are owned by later
// steps), but it CAN prove the proxy never exposes a path query that
// returns the real socket location, and it CAN prove a request that
// claims to want to talk directly to /var/run/docker.sock just sees
// the same allowlist denial.
//
// This is a smoke test for the "the proxy is the only API surface"
// invariant.
func TestSecurity_RawSocketCurlBypass_NoPath(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	// Try the docker convention of an Engine API path that would not
	// exist on podman in any case, and a podman-machine-specific
	// shim path that would expose the underlying connection.
	for _, path := range []string{
		"/var/run/docker.sock",
		"/run/podman/podman.sock",
		"/_raw/passthrough",
	} {
		req, _ := http.NewRequest(http.MethodGet, "http://podman.sock"+path, nil)
		resp := doRequest(t, sock, req)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("path %q: status got %d want %d", path, resp.StatusCode, http.StatusForbidden)
		}
		_ = resp.Body.Close()
	}
	assertNoForward(t, fu)
}

// path-traversal in bind source: filepath.Clean (and now
// filepath.EvalSymlinks) must collapse ".." to a canonical path,
// after which the prefix check against the allowlist runs.
//
// Concrete scenario: allowlist = h.allowedDir; source = h.allowedDir
// + "/../../etc/passwd". After lexical Clean the source becomes
// "/etc/passwd"; after EvalSymlinks it stays "/etc/passwd" (exists
// on every Unix host). The prefix check against h.allowedDir fails.
// DENY.
func TestSecurity_BindSourceTraversal_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	// Build a traversal source that, after Clean, escapes the allowed
	// prefix and lands on a real host file outside the allowlist.
	traversal := h.allowedDir + "/../../../../etc/hosts"
	resp := postCreate(t, sock, map[string]any{
		"Binds": []string{traversal + ":/x:ro"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	assertNoForward(t, fu)
}

// substring trap: an allowlist entry must NOT match a sibling path
// whose name shares the prefix but adds extra characters before the
// next separator. "/foo" must NOT allow "/foo-other".
//
// We create two real sibling directories under a common parent so
// EvalSymlinks resolves both, isolating the prefix-check logic.
func TestSecurity_BindSourceSubstringTrap_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	parent := mkRealDir(t, "trap")
	allowed := filepath.Join(parent, "allow")
	sibling := filepath.Join(parent, "allow-other")
	if err := os.Mkdir(allowed, 0o755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	dir := shortSocketDir(t)
	cfg := Config{
		ListenerPath:       filepath.Join(dir, "proxy.sock"),
		UpstreamPath:       fu.sockPath,
		AllowedBindSources: []string{allowed},
		AuditWriter:        &bytes.Buffer{},
	}
	startProxyWithConfig(t, cfg, nil, cfg.ListenerPath)

	resp := postCreate(t, cfg.ListenerPath, map[string]any{
		"Binds": []string{sibling + ":/x:ro"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("substring trap: status got %d want %d", resp.StatusCode, http.StatusForbidden)
	}
	assertNoForward(t, fu)
}

// Happy path: a containers/create with allowed binds forwards the
// body to the upstream unmodified.
//
// AC: A containers/create request whose HostConfig.Binds entries all
// have sources under the configured allowlist forwards to the upstream
// socket unmodified.
func TestSecurity_AllowedBinds_ForwardsUnmodified(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	// Create a real subdirectory inside allowedDir so the bind source
	// is both inside the allowlist AND resolvable by EvalSymlinks.
	projDir := filepath.Join(h.allowedDir, "proj")
	if err := os.Mkdir(projDir, 0o755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}
	hostConfig := map[string]any{
		"Binds": []string{projDir + ":/app:rw", h.scratchDir + ":/tmp:ro"},
	}
	resp := postCreate(t, sock, hostConfig)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if fu.captured() != 1 {
		t.Errorf("expected exactly 1 upstream call, got %d", fu.captured())
	}
	// The upstream's lastBody should match what we sent — the proxy
	// must NOT have mangled the legitimate body.
	got := fu.lastBody.Load().([]byte)
	var roundtrip struct {
		Image      string         `json:"Image"`
		HostConfig map[string]any `json:"HostConfig"`
	}
	if err := json.Unmarshal(got, &roundtrip); err != nil {
		t.Fatalf("upstream body not valid JSON: %v\n%s", err, got)
	}
	if roundtrip.Image != "alpine" {
		t.Errorf("upstream Image: got %q, want alpine", roundtrip.Image)
	}
}

// ────────────────────────── negative control ──────────────────────────

// ─────────── symlink bypass of bind-source allowlist ────────────
//
// The agent has write access to a path INSIDE an allowed prefix, so
// it plants a symlink at <allowed>/key → <forbidden host file>. A
// purely lexical prefix check sees /<allowed>/key starting with
// /<allowed>/ → ALLOW. The proxy forwards to podman, runc's mount(2)
// follows the symlink at the kernel level, and the container ends up
// with the forbidden host file bind-mounted in. filepath.EvalSymlinks
// on the source before the prefix check closes this. The tests below
// pin the scenario down.

func TestSecurity_BindSymlinkToForbiddenPath_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	// Plant a symlink inside the allowed prefix pointing at /etc/hosts
	// (the canonical "forbidden host file" stand-in that exists on
	// every Unix host).
	symlinkPath := filepath.Join(h.allowedDir, "key")
	if err := os.Symlink("/etc/hosts", symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	resp := postCreate(t, sock, map[string]any{
		"Binds": []string{symlinkPath + ":/k:ro"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("symlink-to-forbidden bind should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// Same exploit shape via HostConfig.Mounts of Type=bind — the second
// call site.
func TestSecurity_MountSymlinkToForbiddenPath_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	symlinkPath := filepath.Join(h.allowedDir, "keymount")
	if err := os.Symlink("/etc/hosts", symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	resp := postCreate(t, sock, map[string]any{
		"Mounts": []map[string]any{
			{
				"Type":   "bind",
				"Source": symlinkPath,
				"Target": "/k",
			},
		},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("symlink-to-forbidden mount should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// Same exploit shape via the PUT /containers/{id}/archive path
// query — the third call site.
func TestSecurity_ArchiveSymlinkToForbiddenPath_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	symlinkPath := filepath.Join(h.allowedDir, "keyarchive")
	if err := os.Symlink("/etc/hosts", symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPut,
		"http://podman.sock/v1.41/containers/abc/archive?path="+url.QueryEscape(symlinkPath),
		bytes.NewReader([]byte("tar")))
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("symlink-to-forbidden archive path should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// Symlink that points to a location ALSO inside the allowed prefix
// must still resolve and pass. Proves the symlink-resolution code
// is not a blanket "any symlink → deny".
func TestSecurity_BindSymlinkToAllowedPath_Allowed(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	targetDir := filepath.Join(h.allowedDir, "target")
	if err := os.Mkdir(targetDir, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	symlinkPath := filepath.Join(h.allowedDir, "link")
	if err := os.Symlink(targetDir, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	resp := postCreate(t, sock, map[string]any{
		"Binds": []string{symlinkPath + ":/x:ro"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("symlink to within-allowlist target should be allowed; got %d", resp.StatusCode)
	}
}

// Non-existent source: EvalSymlinks errors → DENY. The proxy leans
// reject for non-existent paths.
func TestSecurity_BindNonExistentSource_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	// Path under the allowed prefix but the file does not exist —
	// without EvalSymlinks the lexical check would ALLOW this; with
	// EvalSymlinks it errors and we DENY.
	nonexistent := filepath.Join(h.allowedDir, "does-not-exist")
	resp := postCreate(t, sock, map[string]any{
		"Binds": []string{nonexistent + ":/x:ro"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-existent source should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// Broken symlink chain (link → missing target): EvalSymlinks errors
// on the missing target, so DENY even though the symlink itself
// exists at an allowed path. Defence-in-depth against an agent that
// plants a broken symlink as a stepping stone.
func TestSecurity_BindBrokenSymlink_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	linkPath := filepath.Join(h.allowedDir, "broken")
	if err := os.Symlink(filepath.Join(h.allowedDir, "missing-target"), linkPath); err != nil {
		t.Fatalf("create broken symlink: %v", err)
	}
	resp := postCreate(t, sock, map[string]any{
		"Binds": []string{linkPath + ":/x:ro"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("broken symlink source should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// Relative path source via HostConfig.Mounts.Source. The Docker API
// spec requires absolute source paths for Type=bind mounts; the
// proxy rejects relative sources defensively.
func TestSecurity_MountRelativeSource_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"Mounts": []map[string]any{
			{
				"Type":   "bind",
				"Source": "relative/path",
				"Target": "/x",
			},
		},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("relative Mounts source should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// macOS-on-symlinked-TMPDIR sanity: when /tmp is a symlink to
// /private/tmp (macOS default), an allowlist entry under /tmp must
// still match a source under /private/tmp (and vice versa). The
// canonicalisePath helper resolves the allowlist entry so the
// prefix comparison is done in canonical form. This is most
// relevant on Darwin, but the helper exercises the same
// canonicalisation path on Linux too.
func TestSecurity_BindSymlinkedAllowlistEntry_Matched(t *testing.T) {
	fu := newFakeUpstream(t)
	dir := shortSocketDir(t)

	real := mkRealDir(t, "real")
	sub := filepath.Join(real, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	// Symlink alias to that directory — used as the allowlist entry.
	aliasParent := mkRealDir(t, "alias")
	alias := filepath.Join(aliasParent, "link")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatalf("create alias symlink: %v", err)
	}

	cfg := Config{
		ListenerPath:       filepath.Join(dir, "proxy.sock"),
		UpstreamPath:       fu.sockPath,
		AllowedBindSources: []string{alias},
		AuditWriter:        &bytes.Buffer{},
	}
	startProxyWithConfig(t, cfg, nil, cfg.ListenerPath)

	// Bind the CANONICAL path. Without canonical-form allowlist
	// resolution this would not match (alias != real lexically);
	// with canonical resolution both sides resolve to the same
	// target and the bind is allowed.
	resp := postCreate(t, cfg.ListenerPath, map[string]any{
		"Binds": []string{sub + ":/x:ro"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("canonical source under symlinked allowlist entry should be allowed; got %d", resp.StatusCode)
	}
}

// TestSecurity_NegativeControl_RootAllowlistPasses is the load-bearing
// meta-test: if I mutate Config.AllowedBindSources to contain "/" (the
// universal-allow value), the SAME host-bind request that was rejected
// by TestSecurity_HostBindOutsideAllowlist_Denied above must NOW PASS.
//
// This proves the positive tests are not no-ops due to an unrelated
// policy code path always rejecting requests for some other reason. If
// this test fails, every other security test in this file is suspect.
//
// AC: A negative-control test that mutates Config.AllowedBindSources
// to include "/" causes a host-bind request to PASS — proving the
// positive tests are not no-ops because of unrelated policy code paths.
// ──────────────────────── HostConfig denials ───────────────────────────────
//
// The tests below pair with each HostConfig denial; the
// schema-inversion tests live below in the "unknown-field rejection"
// section.

// local-volume bind-mount bypass via volumes/create. The agent calls
// POST volumes/create with Driver=local and DriverOpts that bind a
// host path through the local driver's option language; then
// references the volume by name from a containers/create body.
// bindSource() returns "" for named-volume references (no leading
// slash) so the host-bind allowlist is skipped. Closure: deny
// volumes/create with Driver=local + non-empty DriverOpts.
func TestSecurity_VolumeCreate_LocalDriverBindMount_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	req := mustNewRequest(t, http.MethodPost,
		"http://podman.sock/v1.41/volumes/create",
		map[string]any{
			"Name":   "evil",
			"Driver": "local",
			"DriverOpts": map[string]string{
				"type":   "none",
				"device": "/etc/hosts",
				"o":      "bind",
			},
		})
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("volumes/create local-driver+DriverOpts should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// Default-driver volumes/create (no DriverOpts) is the legitimate
// case and must still pass — proves the volumes/create policy is not
// a blanket deny.
func TestSecurity_VolumeCreate_DefaultDriver_Allowed(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	req := mustNewRequest(t, http.MethodPost,
		"http://podman.sock/v1.41/volumes/create",
		map[string]any{
			"Name":   "normal-volume",
			"Driver": "local",
		})
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("volumes/create default driver no opts should be allowed; got %d", resp.StatusCode)
	}
}

func TestSecurity_VolumeCreate_MalformedJSON_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	req, _ := http.NewRequest(http.MethodPost,
		"http://podman.sock/v1.41/volumes/create",
		bytes.NewReader([]byte(`{garbage`)))
	req.Header.Set("Content-Type", "application/json")
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed volumes/create body should be 400; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// The same exploit via inline Mounts of Type=volume with
// VolumeOptions.DriverConfig in the containers/create body. Closure:
// deny any Type=volume Mount with VolumeOptions.DriverConfig set.
func TestSecurity_MountVolumeWithDriverConfig_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"Mounts": []map[string]any{
			{
				"Type":   "volume",
				"Source": "anon",
				"Target": "/x",
				"VolumeOptions": map[string]any{
					"DriverConfig": map[string]any{
						"Name": "local",
						"Options": map[string]string{
							"type":   "none",
							"device": "/etc/hosts",
							"o":      "bind",
						},
					},
				},
			},
		},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Mounts Type=volume with VolumeOptions.DriverConfig should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// A plain Type=volume Mount with no VolumeOptions is the legitimate
// case (anonymous or named volume managed by podman's default driver
// settings). Forward unmodified.
func TestSecurity_MountVolumeWithoutDriverConfig_Allowed(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"Mounts": []map[string]any{
			{
				"Type":   "volume",
				"Source": "namedvol",
				"Target": "/x",
			},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Mounts Type=volume without DriverConfig should be allowed; got %d", resp.StatusCode)
	}
}

// DeviceCgroupRules is a parallel cgroup-rule mechanism to Devices;
// non-empty must deny.
func TestSecurity_DeviceCgroupRulesNonEmpty_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"DeviceCgroupRules": []string{"a *:* rwm"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("DeviceCgroupRules non-empty should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// DeviceRequests (GPU / nvidia-container-runtime) non-empty must deny.
func TestSecurity_DeviceRequestsNonEmpty_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"DeviceRequests": []map[string]any{
			{
				"Driver":       "nvidia",
				"Count":        -1,
				"Capabilities": [][]string{{"gpu"}},
			},
		},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("DeviceRequests non-empty should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// VolumesFrom non-empty must deny — the source container's mounts
// cannot be audited transitively.
func TestSecurity_VolumesFromNonEmpty_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"VolumesFrom": []string{"other-container:ro"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("VolumesFrom non-empty should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// MaskedPaths present (even as an empty array) overrides runc's safe
// default; must deny.
func TestSecurity_MaskedPathsPresent_Denied(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"empty-array-overrides-default", []string{}},
		{"explicit-allowlist-with-keys", []string{"/safe1", "/safe2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fu := newFakeUpstream(t)
			h := startProxy(t, fu)
			sock := h.sock
			resp := postCreate(t, sock, map[string]any{
				"MaskedPaths": tc.value,
			})
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("MaskedPaths present (%s) should be denied; got %d", tc.name, resp.StatusCode)
			}
			assertNoForward(t, fu)
		})
	}
}

func TestSecurity_ReadonlyPathsPresent_Denied(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"empty-array-overrides-default", []string{}},
		{"explicit-allowlist-with-keys", []string{"/safe1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fu := newFakeUpstream(t)
			h := startProxy(t, fu)
			sock := h.sock
			resp := postCreate(t, sock, map[string]any{
				"ReadonlyPaths": tc.value,
			})
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("ReadonlyPaths present (%s) should be denied; got %d", tc.name, resp.StatusCode)
			}
			assertNoForward(t, fu)
		})
	}
}

// CgroupnsMode=host — sibling of the other five host-namespace modes.
func TestSecurity_CgroupnsModeHost_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"CgroupnsMode": "host",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CgroupnsMode=host should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// Sysctls: some sysctls are not namespaced (writable from a container
// affects the host). Default-deny entirely; legitimate workflows can
// request specific entries through the field-admission process.
func TestSecurity_SysctlsNonEmpty_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"Sysctls": map[string]string{
			"net.ipv4.ip_forward": "1",
		},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Sysctls non-empty should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// ──────────────────────── field-value allowlists ───────────────────────────────
//
// The field-value layer: enumerable values inside admitted fields
// must match a literal allowlist. The tests below cover the
// Mount.Type allowlist, the NetworkMode family, and LogConfig.Type.

// Type="glob" with Source set to a host path that filepath.Glob
// will resolve to the literal path. Deny.
func TestSecurity_MountTypeGlob_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"Mounts": []map[string]any{
			{
				"Type":   "glob",
				"Source": "/etc/hosts",
				"Target": "/key",
			},
		},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Mount Type=glob should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// Mount Types outside the {bind, volume, tmpfs} allowlist must all
// deny. Includes case variants (case-SENSITIVE matching), whitespace-
// padded values, and unicode-lookalike tricks.
func TestSecurity_MountType_AllowlistEnforced(t *testing.T) {
	cases := []string{
		"glob",       // the CRITICAL PoC
		"image",      // image-as-volume; not audited
		"npipe",      // windows named pipe
		"artifact",   // podman artifact mount
		"ramfs",      // ramfs
		"devpts",     // pty fs
		"",           // empty Type
		"BIND",       // case variant
		"Bind",       // case variant
		"VOLUME",     // case variant
		" bind",      // leading whitespace
		"bind ",      // trailing whitespace
		"bind\t",     // tab
		"bind\n",     // newline
		"bind\u200b", // zero-width space
	}
	for _, tp := range cases {
		tp := tp
		t.Run(strconv.Quote(tp), func(t *testing.T) {
			fu := newFakeUpstream(t)
			h := startProxy(t, fu)
			sock := h.sock
			resp := postCreate(t, sock, map[string]any{
				"Mounts": []map[string]any{
					{
						"Type":   tp,
						"Source": h.allowedDir,
						"Target": "/x",
					},
				},
			})
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("Mount Type=%q should be denied; got %d", tp, resp.StatusCode)
			}
			assertNoForward(t, fu)
		})
	}
}

// ──────── NetworkMode-family value allowlist ───────
//
// NetworkMode + 5 siblings (PidMode, IpcMode, UTSMode, UsernsMode,
// CgroupnsMode) allow only an explicit set of literals (plus a
// user-defined-name regex for NetworkMode). Anything else denies.

func TestSecurity_NetworkMode_AllowedLiterals(t *testing.T) {
	cases := []string{"", "bridge", "none", "default", "slirp4netns", "pasta"}
	for _, mode := range cases {
		mode := mode
		t.Run(strconv.Quote(mode), func(t *testing.T) {
			fu := newFakeUpstream(t)
			h := startProxy(t, fu)
			resp := postCreate(t, h.sock, map[string]any{
				"NetworkMode": mode,
			})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("NetworkMode=%q should be allowed; got %d", mode, resp.StatusCode)
			}
		})
	}
}

func TestSecurity_NetworkMode_UserDefinedName_Allowed(t *testing.T) {
	cases := []string{"my-net", "user_net1", "a.b.c", "X", "net.with.dots"}
	for _, name := range cases {
		name := name
		t.Run(name, func(t *testing.T) {
			fu := newFakeUpstream(t)
			h := startProxy(t, fu)
			resp := postCreate(t, h.sock, map[string]any{
				"NetworkMode": name,
			})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("NetworkMode=%q (user-defined name) should be allowed; got %d", name, resp.StatusCode)
			}
		})
	}
}

func TestSecurity_NetworkMode_DangerousValues_Denied(t *testing.T) {
	cases := []string{
		"host",              // literal host
		"HOST",              // case (denyIfUnsafeModeValue lowercases)
		"container:abc123",  // container reference
		"ns:/proc/1/ns/net", // namespace path
		"/proc/1/ns/net",    // path injection
		"my net",            // whitespace
		"-leading-dash",     // regex requires leading alphanumeric
		".leading-dot",      // regex requires leading alphanumeric
		"name with space",   // whitespace
		"name\twith\ttab",   // tab
		"a:b",               // generic colon
		"network/path",      // slash
	}
	for _, mode := range cases {
		mode := mode
		t.Run(strconv.Quote(mode), func(t *testing.T) {
			fu := newFakeUpstream(t)
			h := startProxy(t, fu)
			resp := postCreate(t, h.sock, map[string]any{
				"NetworkMode": mode,
			})
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("NetworkMode=%q should be denied; got %d", mode, resp.StatusCode)
			}
			assertNoForward(t, fu)
		})
	}
}

// Simple namespace modes (PidMode/IpcMode/UTSMode/UsernsMode/
// CgroupnsMode) admit only {"", "private"} and deny everything else,
// including "container:<id>".
func TestSecurity_SimpleNamespaceModes_AllowedLiterals(t *testing.T) {
	fields := []string{"PidMode", "IpcMode", "UTSMode", "UsernsMode", "CgroupnsMode"}
	literals := []string{"", "private"}
	for _, field := range fields {
		for _, lit := range literals {
			field, lit := field, lit
			t.Run(field+"="+strconv.Quote(lit), func(t *testing.T) {
				fu := newFakeUpstream(t)
				h := startProxy(t, fu)
				resp := postCreate(t, h.sock, map[string]any{
					field: lit,
				})
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("%s=%q should be allowed; got %d", field, lit, resp.StatusCode)
				}
			})
		}
	}
}

func TestSecurity_SimpleNamespaceModes_DangerousValues_Denied(t *testing.T) {
	fields := []string{"PidMode", "IpcMode", "UTSMode", "UsernsMode", "CgroupnsMode"}
	dangerous := []string{
		"host",              // already covered by cycle 4 but now via allowlist
		"container:abc",     // container reference
		"ns:/proc/1/ns/pid", // namespace path
		"shareable",         // outside the {"", "private"} allowlist
		"private ",          // whitespace
		"my-network",        // user-defined names NOT allowed for non-NetworkMode modes
	}
	for _, field := range fields {
		for _, value := range dangerous {
			field, value := field, value
			t.Run(field+"="+strconv.Quote(value), func(t *testing.T) {
				fu := newFakeUpstream(t)
				h := startProxy(t, fu)
				resp := postCreate(t, h.sock, map[string]any{
					field: value,
				})
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("%s=%q should be denied; got %d", field, value, resp.StatusCode)
				}
				assertNoForward(t, fu)
			})
		}
	}
}

// ─────────────── LogConfig.Type allowlist ─────────────────────
func TestSecurity_LogConfigType_LocalDrivers_Allowed(t *testing.T) {
	cases := []string{"", "json-file", "none", "journald", "k8s-file", "passthrough", "passthrough-tty"}
	for _, typ := range cases {
		typ := typ
		t.Run(strconv.Quote(typ), func(t *testing.T) {
			fu := newFakeUpstream(t)
			h := startProxy(t, fu)
			resp := postCreate(t, h.sock, map[string]any{
				"LogConfig": map[string]any{"Type": typ},
			})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("LogConfig.Type=%q should be allowed; got %d", typ, resp.StatusCode)
			}
		})
	}
}

func TestSecurity_LogConfigType_NetworkDrivers_Denied(t *testing.T) {
	cases := []string{"syslog", "splunk", "fluentd", "gelf", "awslogs", "etwlogs", "logentries"}
	for _, typ := range cases {
		typ := typ
		t.Run(strconv.Quote(typ), func(t *testing.T) {
			fu := newFakeUpstream(t)
			h := startProxy(t, fu)
			resp := postCreate(t, h.sock, map[string]any{
				"LogConfig": map[string]any{"Type": typ},
			})
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("LogConfig.Type=%q (network driver) should be denied; got %d", typ, resp.StatusCode)
			}
			assertNoForward(t, fu)
		})
	}
}

func TestSecurity_LogConfigType_UnknownDriver_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	resp := postCreate(t, h.sock, map[string]any{
		"LogConfig": map[string]any{"Type": "bogus-future-driver"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("LogConfig.Type=bogus-future-driver should be denied; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// LogConfig.Config (driver-specific options like max-size) is
// FORWARDED; verify the typed parse admits a Config payload
// alongside a known-safe Type.
func TestSecurity_LogConfigWithLocalDriverAndOptions_Allowed(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	resp := postCreate(t, h.sock, map[string]any{
		"LogConfig": map[string]any{
			"Type":   "json-file",
			"Config": map[string]string{"max-size": "10m"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("LogConfig with json-file + Config should be allowed; got %d", resp.StatusCode)
	}
}

// Positive control: Mount.Type=tmpfs is admitted (no host-file
// access path). Proves the allowlist is real and not a blanket deny.
func TestSecurity_MountTypeTmpfs_Allowed(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	resp := postCreate(t, sock, map[string]any{
		"Mounts": []map[string]any{
			{
				"Type":   "tmpfs",
				"Target": "/in-memory",
			},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Mount Type=tmpfs should be allowed; got %d", resp.StatusCode)
	}
}

// ──────── schema-inversion: unknown-field rejection per endpoint ────────
//
// Every body-bearing endpoint parses with DisallowUnknownFields. Any
// HostConfig (or top-level / exec / update / volume / network) field
// that is not in the explicit allowlist struct is rejected as an
// unknown-field error. This closes the "future docker-API field
// silently bypasses the proxy" class of escape.

func TestSecurity_SchemaInversion_UnknownHostConfigField_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	// A made-up HostConfig field. If the proxy ever silently
	// admitted unknown fields, this would be forwarded — the test
	// proves the schema inversion fires.
	resp := postCreate(t, sock, map[string]any{
		"BogusFutureSandboxEscape": "value",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown HostConfig field should be rejected; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
	env := readEnvelope(t, resp)
	if !strings.Contains(env.Message, "unknown") &&
		!strings.Contains(env.Message, "BogusFutureSandboxEscape") {
		t.Errorf("envelope should name the unknown field, got %q", env.Message)
	}
}

func TestSecurity_SchemaInversion_UnknownTopLevelField_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	// Same idea, but at the top level of containers/create.
	body, _ := json.Marshal(map[string]any{
		"Image":                    "alpine",
		"BogusTopLevelEscapeField": "x",
	})
	req, _ := http.NewRequest(http.MethodPost,
		"http://podman.sock/v1.41/containers/create",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown top-level field should be rejected; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

func TestSecurity_SchemaInversion_UnknownMountField_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	// Unknown field inside a HostConfig.Mounts entry. Tests the
	// second nested allowlist (hostConfigMount).
	resp := postCreate(t, sock, map[string]any{
		"Mounts": []map[string]any{
			{
				"Type":                  "bind",
				"Source":                h.allowedDir,
				"Target":                "/x",
				"BogusNestedMountField": true,
			},
		},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown Mount field should be rejected; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

func TestSecurity_SchemaInversion_UnknownUpdateField_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithCaps(t, fu)
	sock := h.sock

	req := mustNewRequest(t, http.MethodPost,
		"http://podman.sock/v1.41/containers/abc/update",
		map[string]any{"BogusUpdateField": 1})
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown update field should be rejected; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

func TestSecurity_SchemaInversion_UnknownExecField_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	req := mustNewRequest(t, http.MethodPost,
		"http://podman.sock/v1.41/containers/abc/exec",
		map[string]any{"BogusExecField": true})
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown exec field should be rejected; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

func TestSecurity_SchemaInversion_UnknownVolumeCreateField_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	req := mustNewRequest(t, http.MethodPost,
		"http://podman.sock/v1.41/volumes/create",
		map[string]any{"Name": "v", "BogusVolField": 1})
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown volumes/create field should be rejected; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

func TestSecurity_SchemaInversion_UnknownNetworkCreateField_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	req := mustNewRequest(t, http.MethodPost,
		"http://podman.sock/v1.41/networks/create",
		map[string]any{"Name": "n", "BogusNetField": 1})
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown networks/create field should be rejected; got %d", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// Positive control: networks/create with only admitted fields is
// forwarded. Proves the schema inversion is not a blanket deny.
func TestSecurity_NetworkCreate_AdmittedFields_Allowed(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxy(t, fu)
	sock := h.sock

	req := mustNewRequest(t, http.MethodPost,
		"http://podman.sock/v1.41/networks/create",
		map[string]any{"Name": "netname", "Driver": "bridge"})
	resp := doRequest(t, sock, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("networks/create with admitted fields should be allowed; got %d", resp.StatusCode)
	}
}

func TestSecurity_NegativeControl_RootAllowlistPasses(t *testing.T) {
	fu := newFakeUpstream(t)
	dir := shortSocketDir(t)
	cfg := Config{
		ListenerPath: filepath.Join(dir, "proxy.sock"),
		UpstreamPath: fu.sockPath,
		// The only difference from the rejection test above: "/"
		// is in the allowlist.
		AllowedBindSources: []string{"/"},
		AllowedCaps:        []string{},
		// Resource caps intentionally LEFT DISABLED so the negative
		// control isolates the bind-source policy: if caps were
		// active, the body would also need to satisfy them and a
		// failure could mislead about which policy fired.
		AuditWriter: &bytes.Buffer{},
	}
	startProxyWithConfig(t, cfg, nil, cfg.ListenerPath)

	// EXACTLY the same body as TestSecurity_HostBindOutsideAllowlist_Denied —
	// /etc/passwd on the host. With AllowedBindSources=["/"] this MUST
	// be allowed.
	body, _ := json.Marshal(map[string]any{
		"Image": "alpine",
		"HostConfig": map[string]any{
			"Binds": []string{"/etc/passwd:/host_passwd:ro"},
		},
	})
	req, err := http.NewRequest(http.MethodPost,
		"http://podman.sock/v1.41/containers/create",
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp := doRequest(t, cfg.ListenerPath, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("NEGATIVE CONTROL FAILED: the same body that is rejected with a normal allowlist must pass with allowlist=['/']; got status %d — review the entire security suite", resp.StatusCode)
	}
	if fu.captured() != 1 {
		t.Fatalf("NEGATIVE CONTROL FAILED: upstream call count = %d, want 1; the request did not reach the upstream", fu.captured())
	}
}

// splitNonEmptyLines is a defensive split that drops the trailing
// empty entry that strings.Split produces when the buffer ends with a
// newline. Used by audit-log assertions that count "real" lines.
func splitNonEmptyLines(s string) []string {
	out := []string{}
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}
