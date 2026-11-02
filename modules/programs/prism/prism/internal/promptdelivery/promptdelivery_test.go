package promptdelivery_test

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	_ "github.com/prismatic-koi/prism/internal/harness/pi" // register pi harness for ShapeOf("pi") in tests
	"github.com/prismatic-koi/prism/internal/promptdelivery"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

// TestDeliverToSession_NoSocketPipeHarness_Refused: a session whose harness
// does not have the socket-pipe shape gets an error, and prism sends no HTTP
// request to its harness port. The server below listens on that port, so a
// request to it fails the test.
func TestDeliverToSession_NoSocketPipeHarness_Refused(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	sid := "session-123"

	empty, unknown := "", "opencode"
	for name, h := range map[string]*string{"empty harness": &empty, "nil harness": nil, "unknown harness": &unknown} {
		t.Run(name, func(t *testing.T) {
			status := &db.Status{
				SessionName:      "myrepo@feature",
				Harness:          h,
				HarnessPort:      &port,
				HarnessSessionID: &sid,
			}
			err := promptdelivery.DeliverToSession("myrepo@feature", status, "hello harness", "", "")
			if err == nil || !strings.Contains(err.Error(), "has no socket pipe") {
				t.Fatalf("DeliverToSession = %v, want an error that says the harness has no socket pipe", err)
			}
		})
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("HTTP requests to the harness port = %d, want 0", n)
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
	err := promptdelivery.DeliverToSession("myrepo@feature", status, "hello", "", "")
	if err == nil {
		t.Fatal("expected error for missing socket, got nil")
	}
	// Error should mention the socket or session.
	errStr := err.Error()
	if !strings.Contains(errStr, "socket") && !strings.Contains(errStr, "session") {
		t.Errorf("error %q should mention socket or session", errStr)
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
			// Use a short temp dir rather than t.TempDir() to keep the
			// resulting socket path inside sun_path (104 bytes on Darwin):
			// t.TempDir embeds the full subtest name.
			xdgTmp := stateguardtest.ShortTempDir(t, "prism-pd-")
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

			if deliverErr := promptdelivery.DeliverToSession(sessionName, status, "hello pi", "", tc.deliverAs); deliverErr != nil {
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
	// Use a short temp dir (not t.TempDir) to keep the resulting socket path
	// inside sun_path (104 bytes on Darwin) — t.TempDir embeds the test name.
	xdgTmp := stateguardtest.ShortTempDir(t, "prism-pd-tomb-")
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

	err = promptdelivery.DeliverToSession(sessionName, status, "hello", "", "")
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
	xdgTmp := stateguardtest.ShortTempDir(t, "prism-pd-guard-xdg-")
	t.Setenv("XDG_STATE_HOME", xdgTmp)
	t.Setenv(promptdelivery.EnvRestrictHostAPI, "1")

	// Stand up a real Unix-socket listener at an outside path. If the guard
	// fails to fire and a dial is attempted, this listener will record the
	// connection and the test will detect it.
	outside := stateguardtest.ShortTempDir(t, "prism-pd-guard-outside-")

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

	err := promptdelivery.DeliverToSession("myrepo@guard", status, "hello", "", "")
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
