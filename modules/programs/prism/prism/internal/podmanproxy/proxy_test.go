package podmanproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Tests for the non-security ACs in #2318:
//
//   - NewProxy returns nil + non-nil error on missing required Config.
//   - NewProxy returns *Proxy + nil error on a valid Config.
//   - Serve(ctx) blocks until ctx is cancelled, then returns nil after
//     the listener socket file is removed.
//   - classifyUpstreamErr maps each known syscall error to the right
//     short-form reason.
//   - normalisePath strips version and libpod prefixes as documented.
//   - isAllowedBindSource closes the substring trap and respects /.
//   - capIsAllowed normalises CAP_ prefixes and is case-insensitive.

// ─────────────────────── NewProxy / Config validation ──────────────────

func TestNewProxy_RejectsMissingListenerPath(t *testing.T) {
	_, err := NewProxy(Config{UpstreamPath: "/x"})
	if err == nil {
		t.Fatal("expected error for missing ListenerPath, got nil")
	}
}

func TestNewProxy_RejectsMissingUpstreamPath(t *testing.T) {
	_, err := NewProxy(Config{ListenerPath: "/x"})
	if err == nil {
		t.Fatal("expected error for missing UpstreamPath, got nil")
	}
}

func TestNewProxy_AcceptsValidConfig(t *testing.T) {
	p, err := NewProxy(Config{
		ListenerPath: "/tmp/proxy.sock",
		UpstreamPath: "/tmp/upstream.sock",
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	if p == nil {
		t.Fatal("NewProxy returned a nil *Proxy with no error")
	}
}

func TestNewProxy_RejectsNegativeBodyCap(t *testing.T) {
	_, err := NewProxy(Config{
		ListenerPath: "/x",
		UpstreamPath: "/y",
		MaxBodyBytes: -1,
	})
	if err == nil {
		t.Fatal("expected error for negative MaxBodyBytes")
	}
}

func TestNewProxy_DefaultsAreApplied(t *testing.T) {
	p, err := NewProxy(Config{
		ListenerPath: "/x",
		UpstreamPath: "/y",
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	if p.cfg.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Errorf("MaxBodyBytes default: got %d, want %d", p.cfg.MaxBodyBytes, DefaultMaxBodyBytes)
	}
	if p.cfg.DialTimeout != DefaultDialTimeout {
		t.Errorf("DialTimeout default: got %v, want %v", p.cfg.DialTimeout, DefaultDialTimeout)
	}
	if p.cfg.Clock == nil {
		t.Error("Clock should default to time.Now")
	}
}

// ───────────────────────────── Serve lifecycle ─────────────────────────

func TestServe_BlocksUntilContextCancelled(t *testing.T) {
	// shortSocketDir, not t.TempDir() — see proxy_security_test.go
	// for rationale (sockaddr_un.sun_path overflow under Nix sandbox).
	dir := shortSocketDir(t)
	listenPath := filepath.Join(dir, "proxy.sock")
	upstreamPath := filepath.Join(dir, "upstream.sock") // does not need to exist
	p, err := NewProxy(Config{
		ListenerPath: listenPath,
		UpstreamPath: upstreamPath,
		AuditWriter:  io.Discard,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- p.Serve(ctx) }()

	// Poll until the socket exists, then assert Serve has NOT returned.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(listenPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(listenPath); err != nil {
		t.Fatalf("listener socket %s never created: %v", listenPath, err)
	}

	select {
	case err := <-serveDone:
		t.Fatalf("Serve returned before context cancellation: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Good — Serve is still blocking.
	}

	cancel()

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned non-nil after cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return within 3s of cancel")
	}

	// AC: the listener socket file is removed.
	if _, err := os.Stat(listenPath); !os.IsNotExist(err) {
		t.Fatalf("listener socket %s still exists after Serve returned: stat err=%v", listenPath, err)
	}
}

func TestServe_BindFailureReturnsError(t *testing.T) {
	// Use a path that cannot exist (parent dir is a regular file).
	dir := shortSocketDir(t)
	regularFile := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(regularFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}
	listenPath := filepath.Join(regularFile, "proxy.sock") // bind under a non-dir
	p, err := NewProxy(Config{
		ListenerPath: listenPath,
		UpstreamPath: "/tmp/upstream.sock",
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = p.Serve(ctx)
	if err == nil {
		t.Fatal("expected bind error from Serve, got nil")
	}
}

func TestServe_RemovesStaleSocketBeforeBind(t *testing.T) {
	dir := shortSocketDir(t)
	listenPath := filepath.Join(dir, "proxy.sock")
	// Pre-create a stale socket file.
	if err := os.WriteFile(listenPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("pre-create: %v", err)
	}

	p, err := NewProxy(Config{
		ListenerPath: listenPath,
		UpstreamPath: "/tmp/upstream.sock",
		AuditWriter:  io.Discard,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- p.Serve(ctx) }()

	// Wait for the socket to be bound.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fi, err := os.Stat(listenPath)
		if err == nil && fi.Mode()&os.ModeSocket != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	fi, err := os.Stat(listenPath)
	if err != nil {
		t.Fatalf("post-bind stat: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("expected socket file, got mode %v", fi.Mode())
	}

	cancel()
	<-serveDone
}

// ───────────────────────── classifyUpstreamErr ─────────────────────────

func TestClassifyUpstreamErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"ECONNREFUSED", syscall.ECONNREFUSED, "connection refused"},
		{"ENOENT", syscall.ENOENT, "socket missing"},
		{"os.ErrNotExist", os.ErrNotExist, "socket missing"},
		{"DeadlineExceeded", context.DeadlineExceeded, "dial timeout"},
		{"EACCES", syscall.EACCES, "permission denied"},
		{"unknown", errors.New("something else"), "dial failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyUpstreamErr(tc.err)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ─────────────────────────── normalisePath ─────────────────────────────

func TestNormalisePath(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"/v1.41/containers/json", "containers/json"},
		{"/v5.0.0/libpod/containers/create", "containers/create"},
		{"/libpod/containers/json", "containers/json"},
		{"/containers/create", "containers/create"},
		{"/", ""},
		{"", ""},
		{"//etc/passwd", ""},
		{"//", ""},
		{"/v1/_ping", "_ping"},
		{"/version", "version"},
		{"/v1a/containers/json", "v1a/containers/json"}, // not a version
		{"no-leading-slash", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := normalisePath(tc.in)
			if got != tc.out {
				t.Errorf("normalisePath(%q) = %q, want %q", tc.in, got, tc.out)
			}
		})
	}
}

func TestIsVersionSegment(t *testing.T) {
	cases := map[string]bool{
		"v1":     true,
		"v1.41":  true,
		"v5.0.0": true,
		"v":      false,
		"v1a":    false,
		"":       false,
		"foo":    false,
	}
	for in, want := range cases {
		got := isVersionSegment(in)
		if got != want {
			t.Errorf("isVersionSegment(%q) = %v, want %v", in, got, want)
		}
	}
}

// ─────────────────────── isAllowedBindSource ───────────────────────────

// As of cycle 4, these tests use REAL paths because isAllowedBindSource
// now calls filepath.EvalSymlinks (which errors on paths that do not
// exist). The scaffold creates a /tmp/<rand>/ tree with sibling
// directories so each behavioural case maps to a real on-disk layout.
func TestIsAllowedBindSource(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "iabs")
	if err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	mkdir := func(p string) string {
		t.Helper()
		full := filepath.Join(base, p)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		return full
	}
	workspace := mkdir("workspace")
	sub := mkdir("workspace/sub")
	deeper := mkdir("workspace/sub/deeper")
	scratch := mkdir("scratch")
	wsOther := mkdir("workspace-other")
	ws2 := mkdir("workspace2")
	elseFile := filepath.Join(mkdir("elsewhere"), "file")
	if err := os.WriteFile(elseFile, []byte{}, 0o600); err != nil {
		t.Fatalf("touch %s: %v", elseFile, err)
	}

	p := &Proxy{cfg: Config{
		AllowedBindSources: []string{workspace, scratch},
	}}

	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"direct-match-workspace", workspace, true},
		{"child-of-workspace", sub, true},
		{"grandchild-of-workspace", deeper, true},
		{"direct-match-scratch", scratch, true},
		{"substring-trap-other", wsOther, false},
		{"substring-trap-2", ws2, false},
		{"file-outside-allowlist", elseFile, false},
		{"existing-host-path", "/etc/hosts", false},
		{"slash", "/", false},
		{"empty", "", false},
		{"relative-path", "relative/path", false},
		// Traversal escaping the allowlist: after lexical Clean the
		// source lands one level above base, which is not under the
		// workspace allowlist entry.
		{"traversal-out", workspace + "/../..", false},
		// Traversal that lands back inside the allowlist is allowed
		// (Clean collapses sub/.. to workspace itself).
		{"traversal-back-in", workspace + "/sub/..", true},
		// Non-existent paths fail EvalSymlinks and are denied.
		{"nonexistent", "/this/does/not/exist/anywhere/12345", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := p.isAllowedBindSource(tc.src)
			if got != tc.want {
				t.Errorf("isAllowedBindSource(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

func TestIsAllowedBindSource_RootEntry(t *testing.T) {
	p := &Proxy{cfg: Config{AllowedBindSources: []string{"/"}}}
	// Paths used here MUST exist on every Unix host the test suite
	// runs on (Linux Nix sandbox + Darwin dev). /etc/hosts, /tmp, /
	// are the safe trio.
	cases := []string{"/etc/hosts", "/tmp", "/"}
	for _, src := range cases {
		if !p.isAllowedBindSource(src) {
			t.Errorf("with root allowlist, %q should be allowed", src)
		}
	}
	// Empty source still not allowed (defensive).
	if p.isAllowedBindSource("") {
		t.Error("empty source should never be allowed, even with root allowlist")
	}
	// Non-existent paths fail EvalSymlinks and are denied even when
	// the allowlist is the universal "/". This is intentional: a
	// non-existent bind source is suspect.
	if p.isAllowedBindSource("/this/does/not/exist/anywhere/67890") {
		t.Error("non-existent path must be denied even with root allowlist (EvalSymlinks contract)")
	}
}

// ─────────────────────────── capIsAllowed ──────────────────────────────

func TestCapIsAllowed_NormalisesCAPPrefix(t *testing.T) {
	p := &Proxy{cfg: Config{AllowedCaps: []string{"NET_BIND_SERVICE"}}}

	cases := map[string]bool{
		"NET_BIND_SERVICE":     true,
		"CAP_NET_BIND_SERVICE": true,
		"net_bind_service":     true,
		"cap_net_bind_service": true,
		"SYS_ADMIN":            false,
		"CAP_SYS_ADMIN":        false,
		"":                     false,
	}
	for in, want := range cases {
		got := p.capIsAllowed(in)
		if got != want {
			t.Errorf("capIsAllowed(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCapIsAllowed_EmptyAllowlistDeniesAll(t *testing.T) {
	p := &Proxy{cfg: Config{AllowedCaps: nil}}
	if p.capIsAllowed("NET_BIND_SERVICE") {
		t.Error("with empty allowlist, no capability should be allowed")
	}
}

// ─────────────────────────── bindSource ────────────────────────────────

func TestBindSource(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/host/src:/in_container", "/host/src"},
		{"/host/src:/in_container:ro", "/host/src"},
		{"volume_name:/in_container", ""}, // named volume
		{"volume_name:/in:ro", ""},        // named volume w/ opts
		{"/", ""},                         // no colon = invalid bind syntax
		{"no-colon", ""},                  // invalid bind syntax
		{"", ""},                          // empty
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := bindSource(tc.in)
			if got != tc.want {
				t.Errorf("bindSource(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ───────────────────── handler dispatch sanity ─────────────────────────

// The classifyRequest table is tested implicitly by the security suite,
// but a small subset is worth pinning here so a refactor that breaks
// allowlist coverage shows up as a unit-test failure rather than a
// security-test failure (cheaper to debug).
func TestClassifyRequest_AllowsCoreEndpoints(t *testing.T) {
	cases := []struct {
		method, path string
		want         endpointKind
	}{
		{http.MethodGet, "_ping", endpointAllow},
		{http.MethodGet, "info", endpointAllow},
		{http.MethodGet, "version", endpointAllow},
		{http.MethodGet, "containers/json", endpointAllow},
		{http.MethodGet, "containers/abc/json", endpointAllow},
		{http.MethodPost, "containers/create", endpointPolicyCreate},
		{http.MethodPost, "containers/abc/start", endpointAllow},
		{http.MethodPost, "containers/abc/attach", endpointAllowStreaming},
		{http.MethodPost, "containers/abc/exec", endpointPolicyExec},
		{http.MethodPost, "containers/abc/update", endpointPolicyUpdate},
		{http.MethodPost, "exec/abc/start", endpointAllowStreaming},
		{http.MethodGet, "containers/abc/logs", endpointAllowStreaming},
		{http.MethodPut, "containers/abc/archive", endpointPolicyArchive},
		{http.MethodDelete, "containers/abc", endpointAllow},
		{http.MethodHead, "_ping", endpointAllow},
		// deny cases
		{http.MethodGet, "swarm", endpointDeny},
		{http.MethodPatch, "containers/abc", endpointDeny},
		{http.MethodPut, "containers/abc/start", endpointDeny},
	}
	for _, tc := range cases {
		t.Run(tc.method+"_"+tc.path, func(t *testing.T) {
			got := classifyRequest(tc.method, tc.path)
			if got != tc.want {
				t.Errorf("classifyRequest(%s, %s) = %v, want %v", tc.method, tc.path, got, tc.want)
			}
		})
	}
}
