package sidecar

// host_api_auth_test.go — the host-API credential (#3379).
//
// The role gates of the host API read the start config of the sidecar. They
// are correct only when the caller is the session of that sidecar. Each
// sidecar writes a new secret next to its hostapi.sock and refuses a request
// that does not carry it. These tests run real sidecars on real sockets:
//
//   - A request with the secret of another session, or with no secret, gets
//     401 on each coordinator-only verb of a coordinator socket, and the
//     handler does not run (the prism stub does not run).
//   - A worker that puts a symlink to the coordinator socket at its own
//     hostapi.sock gets no coordinator verb. The CLI transport refuses the
//     symlink. A client that follows the symlink reaches the coordinator
//     sidecar, which refuses the secret of the worker.
//   - The paired negative serves the routes without the credential check (the
//     handler before #3379) behind the same symlink. The worker then runs
//     coordinator verbs, so the positive tests prove the check.
//   - The secret is new at each start, the file is 0600 and is removed at
//     shutdown, and the sidecar log does not hold it.
//   - A sidecar refuses to bind a hostapi.sock path that is a symlink.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/hostapiauth"
	"github.com/prismatic-koi/prism/internal/rundir"
	prismsession "github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

const (
	authRepo        = "prism-test-auth"
	authCoordinator = "prism-test-auth@main"
	authWorker      = "prism-test-auth@feature-x"
)

// authCoordinatorVerbs are the coordinator-only verbs of the AC of #3379,
// plus the other coordinator-only endpoints. /prompt to another worker is a
// cross-session prompt, which only a coordinator can send.
var authCoordinatorVerbs = []struct {
	method, path, body string
}{
	{http.MethodPost, "/spawn", `{"branch":"feature-y","prompt":"do the thing"}`},
	{http.MethodPost, "/investigate", `{"prompt":"look into this"}`},
	{http.MethodPost, "/merge", `{"pr":1}`},
	{http.MethodGet, "/merges", ""},
	{http.MethodPost, "/merges/cancel", `{"pr":1}`},
	{http.MethodGet, "/audit", ""},
	{http.MethodPost, "/prompt", `{"session":"prism-test-auth@other-worker","prompt":"hello"}`},
	{http.MethodPost, "/cleanup", `{"session":"prism-test-auth@feature-x","yes":true}`},
	{http.MethodPost, "/close", `{"session":"prism-test-auth@feature-x"}`},
	{http.MethodGet, "/logs?session=prism-test-auth@feature-x", ""},
	{http.MethodGet, "/db/query?sql=SELECT+1", ""},
	{http.MethodGet, "/db/tables", ""},
}

// markerStub returns a prism stub that appends to marker and exits 1. A
// handler that runs the stub leaves the marker file.
func markerStub(t *testing.T) (stub, marker string) {
	t.Helper()
	marker = filepath.Join(t.TempDir(), "stub-ran")
	return "#!/bin/sh\necho ran >> '" + marker + "'\nexit 1\n", marker
}

// authFixture runs a coordinator sidecar and a worker sidecar of one repo.
type authFixture struct {
	coord, worker *modeSidecar
	marker        string
	db            *db.DB
}

func newAuthFixture(t *testing.T, mode config.IsolationMode) authFixture {
	t.Helper()
	bus := sidecartest.NewIsolated(t, "")
	for _, row := range []struct{ name, role string }{
		{authCoordinator, "coordinator"},
		{authWorker, "worker"},
		{"prism-test-auth@other-worker", "worker"},
	} {
		if err := bus.DB.UpsertStatusSeedRootAgentName(row.name, authRepo, "/tmp/"+row.name, "active", nil, nil, row.role, "", ""); err != nil {
			t.Fatalf("seed %s: %v", row.name, err)
		}
	}
	stub, marker := markerStub(t)
	return authFixture{
		coord:  startModeSidecarWith(t, mode, authCoordinator, authRepo, "coordinator", bus.DB, stub, nil),
		worker: startModeSidecarWith(t, mode, authWorker, authRepo, "worker", bus.DB, stub, nil),
		marker: marker,
		db:     bus.DB,
	}
}

// readHostAPISecret returns the secret that the sidecar of sockPath wrote.
func readHostAPISecret(t *testing.T, sockPath string) string {
	t.Helper()
	secret, err := hostapiauth.ReadSecret(hostapiauth.SecretPath(sockPath))
	if err != nil {
		t.Fatalf("read the host-API secret of %s: %v", sockPath, err)
	}
	return secret
}

// callFollowingSymlinks sends one request to sockPath with secret in the
// credential header (none when secret is ""). Its dial follows a symlink at
// sockPath, as a client that skips hostapiauth.DialUnix does.
func callFollowingSymlinks(t *testing.T, sockPath, secret, method, path, body string) (int, string) {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://prism-hostapi"+path, reqBody)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if secret != "" {
		req.Header.Set(hostapiauth.Header, secret)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s via %s: %v", method, path, sockPath, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func assertStubDidNotRun(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("a refused request ran the prism stub: its handler ran")
	}
}

// TestHostAPIAuth_CoordinatorSocketRefusesOtherCredential is the root-cause
// AC of #3379, in each isolation mode. A request to the socket of a
// coordinator with the secret of a worker, or with no secret, gets 401 on
// each coordinator-only verb, and no handler runs. The positive end sends
// the secret of the coordinator to the same socket and gets past the check.
func TestHostAPIAuth_CoordinatorSocketRefusesOtherCredential(t *testing.T) {
	for _, mode := range config.ValidIsolationModes {
		t.Run(string(mode), func(t *testing.T) {
			fx := newAuthFixture(t, mode)
			workerSecret := readHostAPISecret(t, fx.worker.hostAPISock)

			for _, tc := range authCoordinatorVerbs {
				for _, cred := range []struct{ name, secret string }{
					{"the worker secret", workerSecret},
					{"no secret", ""},
				} {
					code, body := callFollowingSymlinks(t, fx.coord.hostAPISock, cred.secret, tc.method, tc.path, tc.body)
					if code != http.StatusUnauthorized {
						t.Errorf("%s %s to the coordinator socket with %s: status %d, body %q, want 401",
							tc.method, tc.path, cred.name, code, truncateForLog(body, 200))
					}
				}
			}
			assertStubDidNotRun(t, fx.marker)

			code, body := callHostAPISocket(t, fx.coord.hostAPISock, http.MethodGet, "/merges", "")
			if code != http.StatusOK {
				t.Errorf("GET /merges with the coordinator secret: status %d, body %q, want 200", code, truncateForLog(body, 200))
			}
			code, body = callHostAPISocket(t, fx.coord.hostAPISock, http.MethodPost, "/spawn", authCoordinatorVerbs[0].body)
			if code == http.StatusUnauthorized || code == http.StatusForbidden {
				t.Errorf("POST /spawn with the coordinator secret: status %d, body %q, want the handler to run", code, truncateForLog(body, 200))
			}
			if _, err := os.Stat(fx.marker); err != nil {
				t.Errorf("POST /spawn with the coordinator secret did not run the prism stub, so the refusals above prove nothing: %v", err)
			}
		})
	}
}

// plantSocketSymlink replaces the hostapi.sock of the worker with a symlink
// to the hostapi.sock of the coordinator, as the sandbox of the worker can
// do in its run dir.
func plantSocketSymlink(t *testing.T, fx authFixture) {
	t.Helper()
	if err := os.Remove(fx.worker.hostAPISock); err != nil {
		t.Fatalf("remove the worker socket: %v", err)
	}
	if err := os.Symlink(fx.coord.hostAPISock, fx.worker.hostAPISock); err != nil {
		t.Fatalf("plant the symlink: %v", err)
	}
}

// TestHostAPIAuth_SymlinkToCoordinatorSocketGetsNoCoordinatorVerb is the
// scenario of #3379. The worker replaces its hostapi.sock with a symlink to
// the socket of the coordinator. The CLI transport refuses the symlink and
// names the path. A client that follows the symlink reaches the coordinator
// sidecar with the secret of the worker and gets 401 on each coordinator
// verb. The positive end sends the secret of the coordinator through the
// same symlink and gets 200, so the path reaches the coordinator and the
// credential is what refuses the worker.
func TestHostAPIAuth_SymlinkToCoordinatorSocketGetsNoCoordinatorVerb(t *testing.T) {
	fx := newAuthFixture(t, config.IsolationBwrap)
	workerSecret := readHostAPISecret(t, fx.worker.hostAPISock)
	plantSocketSymlink(t, fx)

	client := &http.Client{Transport: hostapiauth.UnixTransport(fx.worker.hostAPISock, nil), Timeout: 5 * time.Second}
	resp, err := client.Get("http://prism-hostapi/merges")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("the CLI transport sent a request through the symlinked socket (status %d)", resp.StatusCode)
	}
	if !errors.Is(err, rundir.ErrSymlink) || !strings.Contains(err.Error(), fx.worker.hostAPISock) {
		t.Errorf("the CLI transport error %q does not wrap rundir.ErrSymlink and name %s", err, fx.worker.hostAPISock)
	}

	for _, tc := range authCoordinatorVerbs {
		code, body := callFollowingSymlinks(t, fx.worker.hostAPISock, workerSecret, tc.method, tc.path, tc.body)
		if code != http.StatusUnauthorized {
			t.Errorf("%s %s through the symlink with the worker secret: status %d, body %q, want 401",
				tc.method, tc.path, code, truncateForLog(body, 200))
		}
	}
	assertStubDidNotRun(t, fx.marker)

	coordSecret := readHostAPISecret(t, fx.coord.hostAPISock)
	if code, body := callFollowingSymlinks(t, fx.worker.hostAPISock, coordSecret, http.MethodGet, "/merges", ""); code != http.StatusOK {
		t.Errorf("GET /merges through the symlink with the coordinator secret: status %d, body %q, want 200: the symlink does not reach the coordinator, so the refusals above prove nothing",
			code, truncateForLog(body, 200))
	}
}

// TestHostAPIAuth_SymlinkRedirectActsAsCoordinatorWithoutCheck is the paired
// negative: the coordinator routes without requireHostAPICredential (the
// handler before #3379) behind the same symlink. The worker then gets 200 on
// GET /merges and runs the prism stub with POST /spawn, as the coordinator.
func TestHostAPIAuth_SymlinkRedirectActsAsCoordinatorWithoutCheck(t *testing.T) {
	fx := newAuthFixture(t, config.IsolationBwrap)
	workerSecret := readHostAPISecret(t, fx.worker.hostAPISock)

	dir := stateguardtest.ShortTempDir(t, "prefix")
	unchecked := filepath.Join(dir, "hostapi.sock")
	ln, err := net.Listen("unix", unchecked)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: fx.coord.sc.hostAPIRoutes()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	if err := os.Remove(fx.worker.hostAPISock); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(unchecked, fx.worker.hostAPISock); err != nil {
		t.Fatal(err)
	}

	if code, body := callFollowingSymlinks(t, fx.worker.hostAPISock, workerSecret, http.MethodGet, "/merges", ""); code != http.StatusOK {
		t.Errorf("GET /merges through the symlink without the check: status %d, body %q, want 200", code, truncateForLog(body, 200))
	}
	callFollowingSymlinks(t, fx.worker.hostAPISock, workerSecret, http.MethodPost, "/spawn", authCoordinatorVerbs[0].body)
	if _, err := os.Stat(fx.marker); err != nil {
		t.Errorf("POST /spawn through the symlink without the check did not run the prism stub, so the positive test proves nothing: %v", err)
	}
}

// TestRequireHostAPICredential_HandlerDoesNotRun: the check answers 401 and
// does not call the next handler for a wrong or a missing secret, and for
// any request while the sidecar has no secret.
func TestRequireHostAPICredential_HandlerDoesNotRun(t *testing.T) {
	var logBuf bytes.Buffer
	s := New(Config{SessionName: authWorker, Harness: pih.New("", "", ""), Logger: log.New(&logBuf, "", 0)})
	ran := false
	h := s.requireHostAPICredential(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
	}))
	send := func(secret string) int {
		req, _ := http.NewRequest(http.MethodGet, "/merges", nil)
		if secret != "" {
			req.Header.Set(hostapiauth.Header, secret)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	for _, secret := range []string{"", "anything"} {
		if code := send(secret); code != http.StatusUnauthorized || ran {
			t.Errorf("no secret stored, header %q: status %d, handler ran %v; want 401 and no run", secret, code, ran)
		}
	}
	const secret = "0123456789abcdef"
	s.hostAPISecret.Store(func() *string { v := secret; return &v }())
	for _, wrong := range []string{"", "0123456789abcdeX", secret + "0"} {
		if code := send(wrong); code != http.StatusUnauthorized || ran {
			t.Errorf("header %q: status %d, handler ran %v; want 401 and no run", wrong, code, ran)
		}
	}
	if code := send(secret); code != http.StatusOK || !ran {
		t.Errorf("the right secret: status %d, handler ran %v; want 200 and a run", code, ran)
	}
	if strings.Contains(logBuf.String(), secret) {
		t.Error("the sidecar log holds the secret")
	}
}

// TestHostAPIAuth_NewSecretAtEachStart is the restart edge case: `prism
// restart`, `prism restore`, and the sidecar restart of `prism switch` each
// start a new sidecar for the session. The new sidecar writes a new secret,
// the secret of the old one gets 401, and a client that reads the file at
// each request keeps working. The file has mode 0600, the stop removes it,
// and no log line holds a secret.
func TestHostAPIAuth_NewSecretAtEachStart(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	if err := bus.DB.UpsertStatusSeedRootAgentName(authWorker, authRepo, "/tmp/"+authWorker, "active", nil, nil, "worker", "", ""); err != nil {
		t.Fatal(err)
	}
	var logBuf syncBuffer
	logger := log.New(&logBuf, "", 0)
	first := startModeSidecarWith(t, config.IsolationSandboxExec, authWorker, authRepo, "worker", bus.DB, failingStub, logger)
	secretPath := hostapiauth.SecretPath(first.hostAPISock)
	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatalf("the sidecar wrote no secret file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the secret file has mode %v, want 0600", info.Mode().Perm())
	}
	oldSecret := readHostAPISecret(t, first.hostAPISock)
	if code, _ := callHostAPISocket(t, first.hostAPISock, http.MethodGet, "/list-sessions", ""); code != http.StatusOK {
		t.Fatalf("GET /list-sessions on the first sidecar: status %d, want 200", code)
	}

	first.sc.Shutdown()
	if _, err := os.Stat(secretPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the secret file is still there after the stop: %v", err)
	}

	second := startModeSidecarWith(t, config.IsolationSandboxExec, authWorker, authRepo, "worker", bus.DB, failingStub, logger)
	newSecret := readHostAPISecret(t, second.hostAPISock)
	if newSecret == oldSecret {
		t.Fatal("the restarted sidecar wrote the same secret")
	}
	if code, _ := callFollowingSymlinks(t, second.hostAPISock, oldSecret, http.MethodGet, "/list-sessions", ""); code != http.StatusUnauthorized {
		t.Errorf("the secret of the stopped sidecar: status %d, want 401", code)
	}
	if code, body := callHostAPISocket(t, second.hostAPISock, http.MethodGet, "/list-sessions", ""); code != http.StatusOK {
		t.Errorf("GET /list-sessions on the restarted sidecar: status %d, body %q, want 200", code, truncateForLog(body, 200))
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "no valid credential") {
		t.Errorf("the sidecar did not log the refusal, so the log check below proves nothing; log:\n%s", logs)
	}
	for _, s := range []string{oldSecret, newSecret} {
		if strings.Contains(logs, s) {
			t.Error("the sidecar log holds a host-API secret")
		}
	}
}

// TestSidecarRun_RefusesSymlinkedHostAPISock: the sidecar of a session whose
// hostapi.sock is a symlink to the socket of another session does not start.
// Its error names the path, and the socket of the other session stays live
// and is not replaced.
func TestSidecarRun_RefusesSymlinkedHostAPISock(t *testing.T) {
	sidecartest.NewIsolated(t, "")
	ownSock, err := prismsession.SidecarHostAPIPath(authWorker)
	if err != nil {
		t.Fatal(err)
	}
	otherSock, err := prismsession.SidecarHostAPIPath(authCoordinator)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ownSock, otherSock} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ln, err := net.Listen("unix", otherSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	before, err := os.Lstat(otherSock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(otherSock, ownSock); err != nil {
		t.Fatal(err)
	}

	var logBuf syncBuffer
	sc := New(Config{
		SessionName:     authWorker,
		Repo:            authRepo,
		Worktree:        t.TempDir(),
		DB:              openTestDB(t),
		Clock:           newTestClock(),
		AgentRole:       "worker",
		HarnessName:     "pi",
		Harness:         pih.New("", "", ""),
		HostAPISockPath: ownSock,
		Logger:          log.New(&logBuf, "", 0),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- sc.Run(ctx) }()
	select {
	case err := <-runErr:
		if !errors.Is(err, rundir.ErrSymlink) || !strings.Contains(err.Error(), ownSock) {
			t.Errorf("Run returned %v, want an error that wraps rundir.ErrSymlink and names %s", err, ownSock)
		}
	case <-ctx.Done():
		sc.Shutdown()
		t.Fatal("the sidecar did not refuse the symlinked hostapi.sock")
	}
	if !strings.Contains(logBuf.String(), ownSock) {
		t.Errorf("the sidecar log does not name %s:\n%s", ownSock, logBuf.String())
	}
	if after, err := os.Lstat(otherSock); err != nil || !os.SameFile(before, after) {
		t.Errorf("the socket of the other session was replaced: %v", err)
	}
	if _, err := os.Stat(hostapiauth.SecretPath(otherSock)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused sidecar wrote a secret into the run dir of the other session: %v", err)
	}
	if target, err := os.Readlink(ownSock); err != nil || target != otherSock {
		t.Errorf("the symlink changed: %q %v", target, err)
	}
}
