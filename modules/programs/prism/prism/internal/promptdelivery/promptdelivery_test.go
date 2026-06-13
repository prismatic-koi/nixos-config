package promptdelivery_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	_ "github.com/prismatic-koi/prism/internal/harness/pi" // register pi harness for ShapeOf("pi") in tests
	"github.com/prismatic-koi/prism/internal/promptdelivery"
	"github.com/prismatic-koi/prism/internal/session"
)

// TestDeliverToSession_HTTPFallbackPath verifies that DeliverToSession routes
// sessions with an empty or unknown harness through the HTTP prompt_async
// endpoint (legacy HTTP fallback path).
func TestDeliverToSession_HTTPFallbackPath(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Extract the port from the test server URL.
	addr := srv.Listener.Addr().(*net.TCPAddr)
	port := addr.Port

	// Empty harness triggers HTTP fallback (no registered transport shape).
	emptyHarness := ""
	sid := "session-123"
	status := &db.Status{
		SessionName:      "myrepo@feature",
		Harness:          &emptyHarness,
		HarnessPort:      &port,
		HarnessSessionID: &sid,
	}

	err := promptdelivery.DeliverToSession("myrepo@feature", status, "hello harness", nil, "", "")
	if err != nil {
		t.Fatalf("DeliverToSession: %v", err)
	}

	// Verify the body was sent to prompt_async.
	if !strings.Contains(string(gotBody), "hello harness") {
		t.Errorf("expected body to contain 'hello harness', got %q", gotBody)
	}
}

// TestDeliverToSession_HTTPFallback_NoPort verifies that DeliverToSession
// returns an error when the HTTP fallback path is used but no harness port
// is set.
func TestDeliverToSession_HTTPFallback_NoPort(t *testing.T) {
	emptyHarness := ""
	status := &db.Status{
		SessionName: "myrepo@feature",
		Harness:     &emptyHarness,
		// HarnessPort is nil — cannot deliver via HTTP fallback.
	}

	err := promptdelivery.DeliverToSession("myrepo@feature", status, "hello", nil, "", "")
	if err == nil {
		t.Fatal("expected error for missing harness port, got nil")
	}
	if !strings.Contains(err.Error(), "harness port") {
		t.Errorf("error %q should mention harness port", err.Error())
	}
}

// TestDeliverToSession_PiPath_MissingSocket verifies that DeliverToSession
// returns a clear error when the host-API socket does not exist (session ended
// or socket cleaned up).
func TestDeliverToSession_PiPath_MissingSocket(t *testing.T) {
	piHarness := "pi"
	status := &db.Status{
		SessionName: "myrepo@feature",
		Harness:     &piHarness,
	}

	// The socket path doesn't exist — we expect a clear error, not a hang.
	err := promptdelivery.DeliverToSession("myrepo@feature", status, "hello", nil, "", "")
	if err == nil {
		t.Fatal("expected error for missing socket, got nil")
	}
	// Error should mention the socket or session.
	errStr := err.Error()
	if !strings.Contains(errStr, "socket") && !strings.Contains(errStr, "session") {
		t.Errorf("error %q should mention socket or session", errStr)
	}
}

// TestDeliverToSession_NilHarness verifies that DeliverToSession falls back to
// the HTTP path when status.Harness is nil (pre-migration rows).
func TestDeliverToSession_NilHarness(t *testing.T) {
	var gotRequest bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequest = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := srv.Listener.Addr().(*net.TCPAddr)
	port := addr.Port
	sid := "session-456"

	status := &db.Status{
		SessionName:      "myrepo@feature",
		Harness:          nil, // pre-migration: no harness recorded
		HarnessPort:      &port,
		HarnessSessionID: &sid,
	}

	err := promptdelivery.DeliverToSession("myrepo@feature", status, "hello nil harness", nil, "", "")
	if err != nil {
		t.Fatalf("DeliverToSession: %v", err)
	}
	if !gotRequest {
		t.Error("expected HTTP request to be made for nil harness, but none was sent")
	}
}

// TestDeliverToSession_CustomBodyBuilder verifies that a custom buildHTTPBody
// function is called for the HTTP fallback path and its output is used as the
// POST body.
func TestDeliverToSession_CustomBodyBuilder(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := srv.Listener.Addr().(*net.TCPAddr)
	port := addr.Port
	sid := "session-789"
	harness := "" // empty = HTTP fallback path

	status := &db.Status{
		SessionName:      "myrepo@feature",
		Harness:          &harness,
		HarnessPort:      &port,
		HarnessSessionID: &sid,
	}

	customBuilder := func(text string, _ *db.Status) map[string]any {
		return map[string]any{"custom_text": text, "extra": "field"}
	}

	err := promptdelivery.DeliverToSession("myrepo@feature", status, "custom body test", customBuilder, "", "")
	if err != nil {
		t.Fatalf("DeliverToSession: %v", err)
	}
	if gotBody["custom_text"] != "custom body test" {
		t.Errorf("custom_text = %v, want 'custom body test'", gotBody["custom_text"])
	}
	if gotBody["extra"] != "field" {
		t.Errorf("extra = %v, want 'field'", gotBody["extra"])
	}
}

// TestDeliverToSession_PiPath_DeliverAsForwarded verifies that DeliverToSession
// forwards the deliverAs parameter as the "deliver_as" JSON field in the
// /prompt POST body when the session uses the pi (TransportSocketPipe) harness.
// This ensures callers that pass "followUp" (e.g. notifyCoordinator) have their
// intent preserved end-to-end — not overridden with a hardcoded "nextTurn".
func TestDeliverToSession_PiPath_DeliverAsForwarded(t *testing.T) {
	tests := []struct {
		deliverAs string
		want      string
	}{
		{deliverAs: "followUp", want: "followUp"},
		{deliverAs: "steer", want: "steer"},
		{deliverAs: "nextTurn", want: "nextTurn"},
		{deliverAs: "", want: ""}, // empty → field omitted from body; sidecar defaults to nextTurn
	}

	for _, tc := range tests {
		t.Run("deliverAs="+tc.deliverAs, func(t *testing.T) {
			// Redirect XDG_STATE_HOME into a per-subtest temp dir so that
			// SidecarHostAPIPath never falls back to $HOME — which is
			// /homeless-shelter (unwritable) inside the Nix build sandbox.
			// Use os.MkdirTemp with a short prefix rather than t.TempDir() to
			// keep the resulting socket path under the 108-byte sun_path limit
			// (t.TempDir embeds the full subtest name which pushes it over).
			xdgTmp, mkTmpErr := os.MkdirTemp("", "prism-pd-*")
			if mkTmpErr != nil {
				t.Fatalf("create temp dir: %v", mkTmpErr)
			}
			t.Cleanup(func() { _ = os.RemoveAll(xdgTmp) })
			t.Setenv("XDG_STATE_HOME", xdgTmp)

			// Derive a session name that maps to a socket path we can control.
			sessionName := "myrepo@feature"
			sockPath, err := session.SidecarHostAPIPath(sessionName)
			if err != nil {
				t.Fatalf("resolve socket path: %v", err)
			}

			// Create the parent directory so the socket can be created there.
			dir := sockPath[:strings.LastIndex(sockPath, "/")]
			if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
				t.Fatalf("mkdir socket dir: %v", mkErr)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })

			// Start a Unix-socket HTTP server that captures the /prompt body.
			var gotBody map[string]string
			lns, listenErr := net.Listen("unix", sockPath)
			if listenErr != nil {
				t.Fatalf("listen on socket: %v", listenErr)
			}
			srv := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					b, _ := io.ReadAll(r.Body)
					_ = json.Unmarshal(b, &gotBody)
					w.WriteHeader(http.StatusOK)
				}),
			}
			go func() { _ = srv.Serve(lns) }()
			t.Cleanup(func() { _ = srv.Close() })

			piHarness := "pi"
			status := &db.Status{
				SessionName: sessionName,
				Harness:     &piHarness,
			}

			if deliverErr := promptdelivery.DeliverToSession(sessionName, status, "hello pi", nil, "", tc.deliverAs); deliverErr != nil {
				t.Fatalf("DeliverToSession: %v", deliverErr)
			}

			// Verify deliver_as in the captured body.
			if gotBody == nil {
				t.Fatal("server received no request body")
			}
			if tc.want == "" {
				// Empty deliverAs → field must be absent from the body.
				if _, ok := gotBody["deliver_as"]; ok {
					t.Errorf("deliver_as = %q in body, want absent (empty deliverAs omitted)",
						gotBody["deliver_as"])
				}
			} else {
				if got := gotBody["deliver_as"]; got != tc.want {
					t.Errorf("deliver_as = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// TestDeliverToSession_StaleTombstoneSocket verifies that when the host-API
// socket file exists on disk but no process is listening on it (the "stale
// tombstone" case left by a sidecar that exited abnormally without cleanup),
// DeliverToSession surfaces the differentiated diagnostic error rather than a
// generic dial failure. This is the operator's primary diagnostic surface for
// crashed-sidecar scenarios — a regression here would silently degrade the
// operator experience.
//
// The tombstone is constructed at the syscall level (bind + listen + close-fd
// without unlink) — net.Listener.Close() unlinks the file, which would not
// reproduce the abnormal-exit shape. The same technique is used by
// internal/sidecar/sidecar_hostapi_bind_test.go for the sidecar-side check.
func TestDeliverToSession_StaleTombstoneSocket(t *testing.T) {
	// Use a short prefix (not t.TempDir) to keep the resulting sun_path
	// under the 108-byte Linux limit — t.TempDir embeds the test name.
	xdgTmp, err := os.MkdirTemp("", "prism-pd-tomb-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(xdgTmp) })
	t.Setenv("XDG_STATE_HOME", xdgTmp)

	sessionName := "myrepo@tomb"
	sockPath, err := session.SidecarHostAPIPath(sessionName)
	if err != nil {
		t.Fatalf("resolve socket path: %v", err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(sockPath), 0o700); mkErr != nil {
		t.Fatalf("mkdir socket dir: %v", mkErr)
	}

	// Bind + listen + close-fd-without-unlink leaves a socket inode on disk
	// that produces ECONNREFUSED on connect (the tombstone shape).
	fd, sErr := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if sErr != nil {
		t.Fatalf("syscall.Socket: %v", sErr)
	}
	if bErr := syscall.Bind(fd, &syscall.SockaddrUnix{Name: sockPath}); bErr != nil {
		_ = syscall.Close(fd)
		t.Fatalf("syscall.Bind(%s): %v", sockPath, bErr)
	}
	if lErr := syscall.Listen(fd, 1); lErr != nil {
		_ = syscall.Close(fd)
		t.Fatalf("syscall.Listen: %v", lErr)
	}
	_ = syscall.Close(fd) // leave the inode on disk — the tombstone

	if _, statErr := os.Stat(sockPath); statErr != nil {
		t.Fatalf("expected tombstone socket on disk, got: %v", statErr)
	}

	// Sanity-check the platform actually returns ECONNREFUSED on this
	// shape. On Linux and Darwin it does; on exotic platforms a different
	// errno would prevent isStaleTombstoneSocket from triggering and the
	// test should skip rather than flake.
	if _, dialErr := net.DialTimeout("unix", sockPath, time.Second); dialErr == nil {
		t.Fatal("unexpected successful dial of tombstone socket")
	} else if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Skipf("platform returned %v (not ECONNREFUSED) for tombstone socket; skipping", dialErr)
	}

	piHarness := "pi"
	status := &db.Status{
		SessionName: sessionName,
		Harness:     &piHarness,
	}

	err = promptdelivery.DeliverToSession(sessionName, status, "hello", nil, "", "")
	if err == nil {
		t.Fatal("expected error for stale tombstone socket, got nil")
	}
	// Documented diagnostic from promptdelivery.go newUnixClient DialContext:
	//   "host-API socket at %s is a stale tombstone — sidecar has exited
	//    without cleanup (ECONNREFUSED on existing socket file): %w"
	if !strings.Contains(err.Error(), "stale tombstone") {
		t.Errorf("error %q should contain 'stale tombstone' diagnostic", err.Error())
	}
}

// TestDeliverToSession_RestrictHostapiGuard_RefusesOutOfXDG verifies that when
// PRISM_TEST_MODE_RESTRICT_HOSTAPI is set, DeliverToSession refuses to dial a
// host-API socket whose path is not under $XDG_STATE_HOME, and surfaces the
// documented guard error without attempting any dial.
//
// Because deliverViaSidecarSocket computes its sockPath from XDG_STATE_HOME
// and then compares it against the same XDG_STATE_HOME, the guard branch
// cannot be exercised end-to-end without an injection seam. The seam
// (SetSidecarHostAPIPathFn) is documented in export_test.go and exists
// purely to make this branch testable — keep its use limited to this test.
func TestDeliverToSession_RestrictHostapiGuard_RefusesOutOfXDG(t *testing.T) {
	xdgTmp, err := os.MkdirTemp("", "prism-pd-guard-xdg-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(xdgTmp) })
	t.Setenv("XDG_STATE_HOME", xdgTmp)
	t.Setenv(promptdelivery.EnvRestrictHostAPI, "1")

	// Stand up a real Unix-socket listener at an outside path. If the guard
	// fails to fire and a dial is attempted, this listener will record the
	// connection and the test will detect it.
	outside, err := os.MkdirTemp("", "prism-pd-guard-outside-*")
	if err != nil {
		t.Fatalf("create outside dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })

	outsideSock := filepath.Join(outside, "hostapi.sock")
	var dialed bool
	lns, listenErr := net.Listen("unix", outsideSock)
	if listenErr != nil {
		t.Fatalf("listen on outside socket: %v", listenErr)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			dialed = true
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() { _ = srv.Serve(lns) }()
	t.Cleanup(func() { _ = srv.Close() })

	// Inject a resolver that points at the outside socket — outside the
	// current XDG_STATE_HOME. This is exactly the shape the guard exists
	// to refuse.
	restore := promptdelivery.SetSidecarHostAPIPathFn(func(_ string) (string, error) {
		return outsideSock, nil
	})
	t.Cleanup(restore)

	piHarness := "pi"
	status := &db.Status{
		SessionName: "myrepo@guard",
		Harness:     &piHarness,
	}

	err = promptdelivery.DeliverToSession("myrepo@guard", status, "hello", nil, "", "")
	if err == nil {
		t.Fatal("expected guard error, got nil")
	}
	// Documented guard message from promptdelivery.go deliverViaSidecarSocket:
	//   "test-mode isolation guard: refusing to dial host-API socket at %s
	//    — path is outside XDG_STATE_HOME=%s (set
	//    PRISM_TEST_MODE_RESTRICT_HOSTAPI only in tests)"
	wantSubstrings := []string{
		"test-mode isolation guard",
		"refusing to dial",
		outsideSock,
		xdgTmp,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing expected substring %q", err.Error(), want)
		}
	}

	if dialed {
		t.Error("guard fired but a dial was still attempted (listener was contacted)")
	}
}

// TestDeliverToSession_HTTPNon2xx_SurfacesStatus verifies that when the HTTP
// fallback delivery path receives a non-2xx response, the returned error
// includes the status code and the URL — per the documented error format in
// promptdelivery.go (deliverViaHTTP: "http status %d from %s").
func TestDeliverToSession_HTTPNon2xx_SurfacesStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "4xx", status: http.StatusBadRequest},
		{name: "5xx", status: http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			port := srv.Listener.Addr().(*net.TCPAddr).Port
			emptyHarness := ""
			sid := "session-nonok"
			status := &db.Status{
				SessionName:      "myrepo@feature",
				Harness:          &emptyHarness,
				HarnessPort:      &port,
				HarnessSessionID: &sid,
			}

			err := promptdelivery.DeliverToSession("myrepo@feature", status, "hi", nil, "", "")
			if err == nil {
				t.Fatalf("expected non-2xx error for status %d, got nil", tc.status)
			}
			// Documented format: "http status %d from %s".
			wantStatus := fmt.Sprintf("http status %d", tc.status)
			if !strings.Contains(err.Error(), wantStatus) {
				t.Errorf("error %q should contain %q", err.Error(), wantStatus)
			}
			wantURL := fmt.Sprintf("http://localhost:%d/session/%s/prompt_async", port, sid)
			if !strings.Contains(err.Error(), wantURL) {
				t.Errorf("error %q should contain URL %q", err.Error(), wantURL)
			}
		})
	}
}

// TestHasPathPrefix is a table-driven test for the hasPathPrefix helper that
// gates the PRISM_TEST_MODE_RESTRICT_HOSTAPI guard. The helper must correctly
// distinguish a true sub-path from a prefix-of-prefix coincidence (e.g.
// "/a/bc" is NOT a child of "/a/b" even though the bytes match), and must
// handle trailing-slash normalisation and exact-match equality.
func TestHasPathPrefix(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		prefix string
		want   bool
	}{
		{name: "exact match", path: "/a/b", prefix: "/a/b", want: true},
		{name: "identical paths with file", path: "/a/b/c", prefix: "/a/b/c", want: true},
		{name: "prefix with trailing slash", path: "/a/b/c", prefix: "/a/b/", want: true},
		{name: "path with trailing slash, plain prefix", path: "/a/b/", prefix: "/a/b", want: true},
		{name: "true sub-path", path: "/a/b/c/d", prefix: "/a/b", want: true},
		{name: "prefix-of-prefix non-match", path: "/a/bc", prefix: "/a/b", want: false},
		{name: "prefix-of-prefix non-match deeper", path: "/foo/barbaz", prefix: "/foo/bar", want: false},
		{name: "completely different", path: "/x/y", prefix: "/a/b", want: false},
		// filepath.Clean("") == ".", filepath.Clean("/") == "/" — they
		// don't match, and "/" doesn't start with "." + "/", so an empty
		// prefix does not silently match absolute paths. This is the
		// security-relevant property: an empty XDG_STATE_HOME must not
		// silently authorise every socket path.
		{name: "empty prefix vs root path", path: "/", prefix: "", want: false},
		{name: "empty prefix vs file path", path: "/a", prefix: "", want: false},
		{name: "both empty", path: "", prefix: "", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := promptdelivery.HasPathPrefix(tc.path, tc.prefix)
			if got != tc.want {
				t.Errorf("HasPathPrefix(%q, %q) = %v, want %v", tc.path, tc.prefix, got, tc.want)
			}
		})
	}
}
