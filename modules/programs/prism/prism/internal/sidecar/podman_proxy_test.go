package sidecar

// Tests for the per-session podman-proxy wiring.
//
// These tests use sidecartest.NewIsolated per AGENTS.md
// "Test-suite isolation" so they never touch a real podman socket on
// the host. The PodmanUpstreamPath override is the seam that lets us point
// the proxy at:
//
//   - a non-existent path, exercising the friendly 503 envelope from the
//     proxy package without a real podman dependency, OR
//   - an httptest-style upstream stub (not exercised here yet -- Step 3 only
//     needs to prove that the sidecar STARTS the proxy and forwards the
//     dial-failure case correctly; deeper request-shape tests live in the
//     podmanproxy package).
//
// Session names use the "prism-test@" prefix per the test isolation
// convention.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/harness"
	prismsession "github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// newPodmanProxyTestSidecar builds a Sidecar wired to start the podman proxy
// when the agent_status.containers_enabled column is true. The caller must
// flip the column before invoking Run -- toggling it after Run starts will
// not retroactively start the proxy (the read happens once at startup).
//
// The supplied upstream path may be empty (no override; production
// discovery applies on the host) or a path to a stub / non-existent file
// (test seam).
//
// Returns the constructed Sidecar plus the resolved proxy listener path so
// tests can probe / assert on the listener directly.
func newPodmanProxyTestSidecar(t *testing.T, bus *sidecartest.Bus, session, upstream string) (*Sidecar, string) {
	t.Helper()
	listenerPath, err := prismsession.SidecarPodmanProxyPath(session)
	if err != nil {
		t.Fatalf("SidecarPodmanProxyPath: %v", err)
	}
	if len(listenerPath) > maxSunPath {
		t.Fatalf("podman proxy socket path too long (%d > %d): %s", len(listenerPath), maxSunPath, listenerPath)
	}
	// Use the legacy HTTP-port path (empty HarnessName) with a fake SSE
	// harness so Run() does not require a live podman socket OR a live PI
	// extension. We override SubscribeFn to block on ctx.Done instead of
	// returning an immediately-closed channel — otherwise Run() exits at
	// the SSE loop before the test gets a chance to probe the proxy.
	h := newSSEHarness()
	h.SubscribeFn = func(ctx context.Context) (<-chan harness.HarnessEvent, error) {
		ch := make(chan harness.HarnessEvent)
		go func() {
			<-ctx.Done()
			close(ch)
		}()
		return ch, nil
	}
	cfg := Config{
		SessionName:             session,
		Repo:                    "prism-test",
		Worktree:                t.TempDir(),
		DB:                      bus.DB,
		Clock:                   newTestClock(),
		AgentRole:               "worker",
		InstanceID:              "test-instance-" + t.Name(),
		HarnessURL:              "http://127.0.0.1:1", // unreachable; not used with overridden SubscribeFn
		Harness:                 h,
		PodmanProxyListenerPath: listenerPath,
		PodmanUpstreamPath:      upstream,
	}
	return New(cfg), listenerPath
}

// runSidecarBackground starts the sidecar's Run loop in a goroutine and
// returns a channel that closes once Run exits. The channel is closed
// (not just sent to) so multiple receivers can race on it: tests typically
// wait on it explicitly, and the t.Cleanup wait is a backstop.
func runSidecarBackground(t *testing.T, sc *Sidecar, ctx context.Context) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := sc.Run(ctx); err != nil {
			t.Logf("Run returned: %v", err)
		}
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Log("Run() did not exit within 3s of context cancellation")
		}
	})
	return done
}

// waitForPath polls until path exists or the timeout elapses. Returns true if
// the path showed up in time.
func waitForPath(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// waitForPathGone polls until path does NOT exist or the timeout elapses.
// Returns true if the path was gone within timeout.
func waitForPathGone(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// proxyClientFor returns an http.Client whose Transport dials the supplied
// Unix socket. Identical in shape to the helper used inside the podmanproxy
// package tests but kept local so we do not export the helper from there.
func proxyClientFor(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 3 * time.Second,
	}
}

// setContainersEnabled flips the agent_status.containers_enabled column for
// session via a direct DB update. NewIsolated has already seeded the row.
func setContainersEnabled(t *testing.T, bus *sidecartest.Bus, session string, enabled bool) {
	t.Helper()
	v := 0
	if enabled {
		v = 1
	}
	// The DB type exposes QueryRow (used by tests for surgical updates);
	// RETURNING + Scan is the idiomatic shape in this package -- see
	// review_recovery_test.go and notify_test.go for the same pattern.
	if err := bus.DB.QueryRow(
		"UPDATE agent_status SET containers_enabled = ? WHERE session_name = ? RETURNING session_name",
		v, session,
	).Scan(new(string)); err != nil {
		t.Fatalf("set containers_enabled=%d for %q: %v", v, session, err)
	}
}

// ── "no proxy listener when containers_enabled=0" ─────────────────────

// TestPodmanProxy_DefaultOff_NoListener verifies that when
// containers_enabled is false (the default for every existing session), the
// sidecar does NOT create a podman.sock listener file in the per-session run
// directory.
func TestPodmanProxy_DefaultOff_NoListener(t *testing.T) {
	session := "prism-test@" + t.Name()
	bus := sidecartest.NewIsolated(t, session)

	sc, listenerPath := newPodmanProxyTestSidecar(t, bus, session, "")
	// Explicitly leave containers_enabled false (the default seeded by
	// NewIsolated -> UpsertStatusWithAgent).
	setContainersEnabled(t, bus, session, false)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := runSidecarBackground(t, sc, ctx)

	// Give the proxy goroutine plenty of time to NOT start. We deliberately
	// poll for the absence of the socket rather than spin a single check, so
	// scheduler latency does not give us a false negative.
	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(listenerPath); err == nil {
			t.Fatalf("podman.sock unexpectedly exists at %s while containers_enabled=0", listenerPath)
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	<-done
}

// ── AC: "containers_enabled=1 -> proxy listens, audit log exists, 503 on probe" ──

// TestPodmanProxy_ContainersEnabled_ProxyListensAndReturns503 covers the
// happy-but-no-upstream path: containers_enabled is true, the proxy starts,
// the listener socket exists, the audit log exists, and a probe request
// returns the friendly 503 envelope from the podmanproxy package. This is
// the production failure mode on Darwin without `podman machine start` --
// the AC explicitly calls it out.
func TestPodmanProxy_ContainersEnabled_ProxyListensAndReturns503(t *testing.T) {
	session := "prism-test@" + t.Name()
	bus := sidecartest.NewIsolated(t, session)

	// Point the proxy at a path that does not exist; the proxy's
	// upstreamErrorHandler maps the resulting ENOENT/ECONNREFUSED to the
	// friendly 503 envelope. We construct the path under the test's XDG
	// tempdir so we never leak outside the isolation.
	upstream := filepath.Join(bus.XDGStateHome, "fake-podman.sock")

	sc, listenerPath := newPodmanProxyTestSidecar(t, bus, session, upstream)
	setContainersEnabled(t, bus, session, true)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := runSidecarBackground(t, sc, ctx)

	// The listener must appear within a couple of seconds. The proxy goroutine
	// is launched via goNotify from inside Run() after the FK guard, so the
	// socket binds asynchronously.
	if !waitForPath(listenerPath, 3*time.Second) {
		t.Fatalf("podman.sock listener did not appear at %s", listenerPath)
	}

	// The audit file path is deterministic: <sessionDir>/podman-proxy.log.
	sessionDir, err := container.SessionWorkDirPath(sc.cfg.InstanceID)
	if err != nil {
		t.Fatalf("SessionWorkDirPath: %v", err)
	}
	auditPath := filepath.Join(sessionDir, "podman-proxy.log")

	// Fire a probe request at the proxy's ListenerPath. Expect 503 + the
	// friendly envelope shape locked by the parent issue's AC.
	client := proxyClientFor(listenerPath)
	resp, err := client.Get("http://podman.sock/v1.41/_ping")
	if err != nil {
		t.Fatalf("GET /_ping via proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "podman socket unavailable") {
		t.Errorf("envelope: got %q, want substring 'podman socket unavailable'", body)
	}

	// The audit log must exist and contain at least one line after the probe.
	// Audit writes are append-only and serialised under the proxy's auditMu,
	// so a single Stat-after-probe is race-free.
	st, err := os.Stat(auditPath)
	if err != nil {
		t.Fatalf("stat audit log %s: %v", auditPath, err)
	}
	if st.Size() == 0 {
		t.Errorf("audit log %s is empty after probe; want at least one line", auditPath)
	}
	// Spot-check the audit line shape: it must parse as JSON with the
	// expected fields. Use the first line only; trailing bytes (if any) may
	// belong to an in-flight request.
	auditBytes, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	firstLine := strings.SplitN(strings.TrimSpace(string(auditBytes)), "\n", 2)[0]
	var rec map[string]any
	if err := json.Unmarshal([]byte(firstLine), &rec); err != nil {
		t.Fatalf("audit line is not valid JSON: %v\nline=%q", err, firstLine)
	}
	for _, field := range []string{"timestamp", "method", "endpoint", "decision", "reason"} {
		if _, ok := rec[field]; !ok {
			t.Errorf("audit line missing field %q: %v", field, rec)
		}
	}

	cancel()
	<-done
}

// ── "ctx cancellation drains the proxy and unlinks the socket" ─────────

// TestPodmanProxy_CtxCancellationUnlinksSocket verifies that cancelling the
// sidecar's run context exits the proxy goroutine within 5 seconds and
// unlinks the listener socket file as a side effect of the
// proxy's Serve loop's deferred cleanup.
func TestPodmanProxy_CtxCancellationUnlinksSocket(t *testing.T) {
	session := "prism-test@" + t.Name()
	bus := sidecartest.NewIsolated(t, session)

	upstream := filepath.Join(bus.XDGStateHome, "fake-podman.sock")
	sc, listenerPath := newPodmanProxyTestSidecar(t, bus, session, upstream)
	setContainersEnabled(t, bus, session, true)

	ctx, cancel := context.WithCancel(context.Background())
	done := runSidecarBackground(t, sc, ctx)
	if !waitForPath(listenerPath, 3*time.Second) {
		t.Fatalf("podman.sock listener did not appear at %s", listenerPath)
	}

	cancel()

	// The proxy's Serve loop removes the socket file on ctx cancellation.
	if !waitForPathGone(listenerPath, 5*time.Second) {
		t.Errorf("podman.sock still exists 5s after ctx cancellation: %s", listenerPath)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("Run() did not exit within 5s of ctx cancellation")
	}
}

// ── "real upstream socket path not in sidecar log" ─────────────────────

// TestPodmanProxy_UpstreamDiscovery_OverridePrecedence verifies the
// override-precedence rule: when Config.PodmanUpstreamPath is non-empty,
// platform-specific discovery is short-circuited and the override value is
// returned verbatim. This is the seam that lets tests run without a real
// podman socket.
func TestPodmanProxy_UpstreamDiscovery_OverridePrecedence(t *testing.T) {
	sc := New(Config{
		SessionName:        "prism-test@override",
		Harness:            newSSEHarness(),
		PodmanUpstreamPath: "/tmp/explicit-override.sock",
	})
	if got := sc.resolvePodmanUpstreamPath(); got != "/tmp/explicit-override.sock" {
		t.Errorf("resolvePodmanUpstreamPath with override = %q, want %q",
			got, "/tmp/explicit-override.sock")
	}
}

// TestPodmanProxy_UpstreamDiscovery_Linux_XDGRuntimeDir asserts the NixOS
// happy path: when $XDG_RUNTIME_DIR is set, the upstream
// socket is $XDG_RUNTIME_DIR/podman/podman.sock. The shape of the path is
// stable across distros that use the systemd user-podman socket.
func TestPodmanProxy_UpstreamDiscovery_Linux_XDGRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/12345")
	sc := New(Config{
		SessionName: "prism-test@xdgrt",
		Harness:     newSSEHarness(),
	})
	got := sc.resolvePodmanUpstreamLinux()
	want := "/run/user/12345/podman/podman.sock"
	if got != want {
		t.Errorf("resolvePodmanUpstreamLinux with XDG_RUNTIME_DIR set = %q, want %q", got, want)
	}
}

// TestPodmanProxy_UpstreamDiscovery_Linux_Fallback asserts the documented
// fallback when $XDG_RUNTIME_DIR is unset: /run/user/<uid>/podman/podman.sock.
// The numeric uid is whatever os.Getuid() returns in this process; we just
// check the prefix and suffix shape, not the literal value, so the test is
// reproducible across CI / dev hosts.
func TestPodmanProxy_UpstreamDiscovery_Linux_Fallback(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	sc := New(Config{
		SessionName: "prism-test@fallback",
		Harness:     newSSEHarness(),
	})
	got := sc.resolvePodmanUpstreamLinux()
	if !strings.HasPrefix(got, "/run/user/") || !strings.HasSuffix(got, "/podman/podman.sock") {
		t.Errorf("resolvePodmanUpstreamLinux fallback = %q, want /run/user/<uid>/podman/podman.sock shape", got)
	}
}

// TestPodmanProxy_UpstreamDiscovery_Darwin_MissingPodmanReturnsPlaceholder
// asserts the Darwin failure mode: when `podman` is not
// on PATH (or `podman machine inspect` exits non-zero) the discovery returns
// a stable placeholder string so the proxy's friendly 503 envelope fires
// instead of the sidecar refusing to start. We engineer the "podman not
// found" condition by shrinking PATH to an empty tempdir.
//
// The test is GOOS-agnostic: it exercises the function directly, regardless
// of whether the developer's host has podman installed.
func TestPodmanProxy_UpstreamDiscovery_Darwin_MissingPodmanReturnsPlaceholder(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	sc := New(Config{
		SessionName: "prism-test@darwin-missing",
		Harness:     newSSEHarness(),
	})
	got := sc.resolvePodmanUpstreamDarwin()
	// We do NOT assert the literal placeholder string here; the
	// guarantee that matters is "discovery never returns a
	// path that points at the real host podman socket when discovery
	// fails". An absolute, non-existent path satisfies that contract,
	// and the proxy package's friendly 503 then fires.
	if _, err := os.Stat(got); err == nil {
		t.Errorf("resolvePodmanUpstreamDarwin returned an EXISTING path %q on a host with no podman; the friendly 503 path would not fire", got)
	}
	if got == "" || got[0] != '/' {
		t.Errorf("resolvePodmanUpstreamDarwin returned non-absolute path %q", got)
	}
}

// TestPodmanProxy_ContainerNamePrefix_WiredFromSession verifies the
// container-name-prefix wiring: when the sidecar starts the proxy, the
// proxy's Config.ContainerNamePrefix is set to
// "prism-<sessionName>-" so the cleanup sweep can locate every
// container belonging to this session. We probe the live behaviour
// by POSTing a containers/create request with an explicit Name that
// does NOT start with the session prefix — the proxy must reject
// with 403 and audit reason name_prefix_mismatch.
//
// The test uses a real (test) upstream socket so the proxy's policy
// path runs end-to-end rather than short-circuiting on dial failure.
// We do not need the upstream to respond meaningfully; we just need
// the policy to fire BEFORE the upstream is dialled.
func TestPodmanProxy_ContainerNamePrefix_WiredFromSession(t *testing.T) {
	session := "prism-test@" + t.Name()
	bus := sidecartest.NewIsolated(t, session)

	// A non-existent upstream is fine: the policy rejection fires
	// before any dial attempt, so the friendly 503 path does not run.
	upstream := filepath.Join(bus.XDGStateHome, "unused-upstream.sock")

	sc, listenerPath := newPodmanProxyTestSidecar(t, bus, session, upstream)
	setContainersEnabled(t, bus, session, true)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := runSidecarBackground(t, sc, ctx)
	if !waitForPath(listenerPath, 3*time.Second) {
		t.Fatalf("podman.sock listener did not appear at %s", listenerPath)
	}

	client := proxyClientFor(listenerPath)
	body := strings.NewReader(`{"Image":"alpine","Name":"not-our-prefix"}`)
	resp, err := client.Post("http://podman.sock/v1.41/containers/create",
		"application/json", body)
	if err != nil {
		t.Fatalf("POST containers/create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 403 (body=%q) — the sidecar did not wire ContainerNamePrefix",
			resp.StatusCode, got)
	}
	respBody, _ := io.ReadAll(resp.Body)
	var env struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (raw=%q)", err, respBody)
	}
	wantPrefix := "prism-" + session + "-"
	if !strings.Contains(env.Message, wantPrefix) {
		t.Errorf("envelope message does not name the wired prefix %q; got %q",
			wantPrefix, env.Message)
	}

	cancel()
	<-done
}

// TestPodmanProxy_UpstreamPathNotInSidecarLog verifies the security AC:
// even when the proxy is started and audited, the rendered sidecar log
// must NOT contain the real upstream socket path verbatim. The audit log
// may reference proxied endpoints (e.g. /v1.41/_ping), but the upstream
// path itself is a host secret -- leaking it via the sidecar log would
// undo part of the isolation story.
//
// We pick an upstream path with a distinctive sentinel substring so a
// naive substring search is precise.
func TestPodmanProxy_UpstreamPathNotInSidecarLog(t *testing.T) {
	session := "prism-test@" + t.Name()
	bus := sidecartest.NewIsolated(t, session)

	sentinel := "PODMAN_UPSTREAM_SENTINEL_DO_NOT_LOG"
	upstream := filepath.Join(bus.XDGStateHome, sentinel+".sock")

	sc, listenerPath := newPodmanProxyTestSidecar(t, bus, session, upstream)
	// captureLog rewrites sc.cfg.Logger to a buffer-backed log.Logger and
	// returns a snapshot accessor that drains notifyWG before reading; this
	// is the canonical race-safe log capture in this package. Call it BEFORE
	// Run so the proxy goroutine writes through the
	// captured logger from the start.
	getLogs := captureLog(sc)
	setContainersEnabled(t, bus, session, true)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := runSidecarBackground(t, sc, ctx)
	if !waitForPath(listenerPath, 3*time.Second) {
		t.Fatalf("podman.sock listener did not appear at %s", listenerPath)
	}

	// Fire a probe so the audit + proxy paths are exercised in full.
	client := proxyClientFor(listenerPath)
	resp, err := client.Get("http://podman.sock/v1.41/_ping")
	if err != nil {
		t.Fatalf("GET /_ping: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	cancel()
	<-done

	got := getLogs()
	if strings.Contains(got, sentinel) {
		t.Errorf("sidecar log contains the upstream sentinel %q -- the real upstream socket path leaked into the log surface\nlog:\n%s", sentinel, got)
	}
}
