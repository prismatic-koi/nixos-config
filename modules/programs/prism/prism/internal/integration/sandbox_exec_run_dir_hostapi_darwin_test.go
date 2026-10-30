//go:build darwin

package integration_test

// sandbox_exec_run_dir_hostapi_darwin_test.go — the hostapi.sock of a
// sandbox-exec session, and the host-API credential (#3379).
//
// The session can write its run dir (section 6 of generateProfile). Without
// section 21n it can remove its hostapi.sock and put a symlink to the
// hostapi.sock of another session at the path. A Unix-socket connect
// follows a symlink. If the connect reaches the other sidecar, the other
// sidecar takes the request as a request of its own session, unless the
// request must carry the host-API secret of that session.
//
//   - Under the production profile the session cannot remove, rename, or
//     make the hostapi.sock path, so it cannot plant the symlink. It can
//     still connect to its own socket.
//   - The paired negative takes section 21n out. The plant then works. The
//     test sends a request through the symlink to the routes of a
//     coordinator without the credential check (the handler before #3379)
//     and records whether the request reached them.
//   - The same plant against a real coordinator sidecar, which has the
//     check: a request with the secret of the session, or with no secret,
//     never gets a coordinator answer.
//
// The tests use the connect probe of
// sandbox_exec_container_socket_darwin_test.go for the file operations, and
// an HTTP probe for the request. XDG_STATE_HOME is under the real home
// (stateHomeOutsideTmp), as in sandbox_exec_run_dir_darwin_test.go.

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/hostapiauth"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar"
)

// hostAPIProbeSrc is the source of the HTTP probe.
//
//	probe <sock> <secret-file> <method> <path>   read the secret from the file
//	probe <sock> - <method> <path> [<secret>]    use the literal secret, or none
//
// The probe sends one request to the Unix socket at <sock>, with the secret
// in the host-API credential header, and prints "STATUS <code> <body>". A
// connect that fails prints CONNECT-FAILED and exits 1. A secret file that
// it cannot read prints READ-DENIED and exits 2.
const hostAPIProbeSrc = `package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	sock, file, method, path := os.Args[1], os.Args[2], os.Args[3], os.Args[4]
	secret := ""
	if file != "-" {
		b, err := os.ReadFile(file)
		if err != nil {
			os.Stdout.WriteString("READ-DENIED " + err.Error() + "\n")
			os.Exit(2)
		}
		secret = strings.TrimSpace(string(b))
	} else if len(os.Args) > 5 {
		secret = os.Args[5]
	}
	client := &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", sock)
		}},
		Timeout: 5 * time.Second,
	}
	req, _ := http.NewRequest(method, "http://prism-hostapi"+path, nil)
	if secret != "" {
		req.Header.Set("` + hostapiauth.Header + `", secret)
	}
	resp, err := client.Do(req)
	if err != nil {
		os.Stdout.WriteString("CONNECT-FAILED " + err.Error() + "\n")
		os.Exit(1)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	os.Stdout.WriteString("STATUS " + strconv.Itoa(resp.StatusCode) + " " + strings.TrimSpace(string(b)) + "\n")
}
`

// buildHostAPIProbe compiles the HTTP probe into a t.TempDir(). See
// buildConnectProbe.
func buildHostAPIProbe(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "hostapiprobe.go")
	if err := os.WriteFile(src, []byte(hostAPIProbeSrc), 0o600); err != nil {
		t.Fatalf("write host-API probe source: %v", err)
	}
	bin := filepath.Join(dir, "hostapiprobe")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the host-API probe (go unavailable?): %v\n%s", err, out)
	}
	return bin
}

// hostAPISockDenyBlock is the section-21n clause for sock. A copy that
// drifts from the generator fails the negative tests (withMutatedProbeProfile).
func hostAPISockDenyBlock(sock string) string {
	return "(deny file-write*\n  (literal " + sbplQuoteForTest(sock) + "))\n"
}

// hostAPISockFixture is the own session, with its profile manager, its
// hostapi.sock path, and its host-API secret file, and the hostapi.sock path
// of a coordinator session.
type hostAPISockFixture struct {
	m           *container.Manager
	ownSock     string
	ownSecret   string
	coordSock   string
	coordSecret string
}

const hostAPICoordSession = "prism-test@3379-coord"

// newHostAPISockFixture sets XDG_STATE_HOME under the real home and makes
// the fixture. The own session has a live listener at its hostapi.sock and
// a secret file, as its sidecar makes them. Nothing listens at the socket
// of the coordinator yet.
func newHostAPISockFixture(t *testing.T) hostAPISockFixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))
	m := newPipeSessionManager(t, runDirOwnSession)
	own, err := session.SidecarHostAPIPath(runDirOwnSession)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	coord, err := session.SidecarHostAPIPath(hostAPICoordSession)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(coord), 0o700); err != nil {
		t.Fatalf("make the run dir of the coordinator: %v", err)
	}
	if _, err := hostapiauth.WriteNewSecret(hostapiauth.SecretPath(own)); err != nil {
		t.Fatalf("write the own secret: %v", err)
	}
	startUnixListener(t, own)
	return hostAPISockFixture{
		m:           m,
		ownSock:     own,
		ownSecret:   hostapiauth.SecretPath(own),
		coordSock:   coord,
		coordSecret: hostapiauth.SecretPath(coord),
	}
}

// coordinatorSidecar returns a coordinator sidecar of hostAPICoordSession.
func coordinatorSidecar(t *testing.T, sock string) *sidecar.Sidecar {
	t.Helper()
	runDir := filepath.Dir(sock)
	return sidecar.New(sidecar.Config{
		SessionName:           hostAPICoordSession,
		Repo:                  "prism-test",
		Worktree:              t.TempDir(),
		DB:                    openTestDB(t),
		Clock:                 sidecar.RealClock(),
		AgentRole:             "coordinator",
		HarnessName:           "pi",
		Harness:               pih.New("", "coordinator", ""),
		HostAPISockPath:       sock,
		HarnessPipeSockPath:   filepath.Join(runDir, "pipe.sock"),
		HarnessPipeSecretPath: filepath.Join(runDir, "pipe.secret"),
		StartupConnectTimeout: time.Minute,
		PipeReconnectTimeout:  time.Minute,
		Logger:                log.New(io.Discard, "", 0),
	})
}

// serveUnchecked serves the routes of a coordinator sidecar at sock without
// the credential check: the host-API handler before #3379.
func serveUnchecked(t *testing.T, sock string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	srv := &http.Server{Handler: coordinatorSidecar(t, sock).HostAPIHandlerForTest()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// runCoordinator runs a real coordinator sidecar at sock, with the credential
// check, and waits for the socket.
func runCoordinator(t *testing.T, sock string) {
	t.Helper()
	sc := coordinatorSidecar(t, sock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sc.Run(ctx)
	}()
	t.Cleanup(func() {
		sc.Shutdown()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the coordinator sidecar did not stop within 5s")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := net.DialTimeout("unix", sock, 100*time.Millisecond); err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the coordinator socket %s did not listen", sock)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// plantRedirect runs the session under profilePath: it removes its
// hostapi.sock and puts a symlink to the coordinator socket at the path.
// The profile must not hold section 21n.
func plantRedirect(t *testing.T, profilePath, connectProbe string, fx hostAPISockFixture) {
	t.Helper()
	expectOp(t, profilePath, connectProbe, true, "the remove of hostapi.sock still fails without section 21n, so the positive test proves nothing", "--remove", fx.ownSock)
	expectOp(t, profilePath, connectProbe, true, "the symlink at hostapi.sock still fails without section 21n, so the positive test proves nothing", "--symlink", fx.coordSock, fx.ownSock)
	if target, err := os.Readlink(fx.ownSock); err != nil || target != fx.coordSock {
		t.Fatalf("hostapi.sock is not a symlink to the coordinator socket after the plant: %q %v", target, err)
	}
}

// TestSandboxExecRunDir_HostAPISockCannotBeReplaced: under the production
// profile, the session cannot remove or rename its hostapi.sock (section
// 21n). When the socket is absent, the session cannot put a symlink to the
// socket of another session, or a file, at the path. The session can still
// connect to the socket of its sidecar.
func TestSandboxExecRunDir_HostAPISockCannotBeReplaced(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	fx := newHostAPISockFixture(t)
	startUnixListener(t, fx.coordSock)
	prepared, _ := preparePositiveProfile(t, fx.m)
	profilePath := writeProbePositiveProfile(t, prepared)

	before, err := os.Lstat(fx.ownSock)
	if err != nil {
		t.Fatal(err)
	}
	expectOp(t, profilePath, probe, false, "the session removed its hostapi.sock", "--remove", fx.ownSock)
	moved := filepath.Join(sessionWorkDirOf(t, fx.m), "moved-hostapi.sock")
	expectOp(t, profilePath, probe, false, "the session renamed its hostapi.sock", "--rename", fx.ownSock, moved)
	if after, err := os.Lstat(fx.ownSock); err != nil || !os.SameFile(before, after) {
		t.Fatalf("hostapi.sock changed after the denied operations: %v", err)
	}
	expectConnected(t, profilePath, probe, fx.ownSock, "the section-21n deny stops a connect to the own hostapi.sock")

	// The host removes the socket, as when the sidecar is down. The session
	// must not make the path.
	if err := os.Remove(fx.ownSock); err != nil {
		t.Fatal(err)
	}
	expectOp(t, profilePath, probe, false, "the session put a symlink to the socket of another session at its hostapi.sock path", "--symlink", fx.coordSock, fx.ownSock)
	expectOp(t, profilePath, probe, false, "the session made a file at its hostapi.sock path", "--create", fx.ownSock)
	if _, err := os.Lstat(fx.ownSock); !os.IsNotExist(err) {
		t.Errorf("the hostapi.sock path exists after the denied operations: %v", err)
	}
	expectOp(t, profilePath, probe, true, "the section-21n deny stops a write of another file in the run dir", "--create", filepath.Join(filepath.Dir(fx.ownSock), "agent-run.log"))
}

// TestSandboxExecRunDir_HostAPISockRedirectWithoutDeny is the paired
// negative of the test above, and the measurement of #3379. With section
// 21n taken out, the session plants the symlink. It then sends GET /merges,
// a coordinator-only verb, with its own secret through its hostapi.sock, to
// the routes of a coordinator without the credential check: the handler
// before #3379. The log line "#3379 pre-fix result" records whether the
// request reached them.
func TestSandboxExecRunDir_HostAPISockRedirectWithoutDeny(t *testing.T) {
	requireSandboxExec(t)
	connectProbe := buildConnectProbe(t)
	httpProbe := buildHostAPIProbe(t)
	fx := newHostAPISockFixture(t)
	serveUnchecked(t, fx.coordSock)
	mutatedPath := withMutatedProbeProfile(t, fx.m, func(p string) string {
		return strings.Replace(p, hostAPISockDenyBlock(fx.ownSock), "", 1)
	})

	plantRedirect(t, mutatedPath, connectProbe, fx)
	out, err := runProbe(t, mutatedPath, httpProbe, fx.ownSock, fx.ownSecret, http.MethodGet, "/merges")
	t.Logf("#3379 pre-fix result: GET /merges through the symlinked hostapi.sock to the coordinator routes without the credential check: exit=%v output=%q", err, strings.TrimSpace(out))
	if strings.Contains(out, "READ-DENIED") {
		t.Errorf("the session cannot read its own secret, so the request proves nothing: %s", out)
	}
}

// TestSandboxExecRunDir_HostAPISockRedirectRefusedByCredential: with section
// 21n taken out, the session plants the symlink, as above, but a real
// coordinator sidecar, with the credential check, owns the other socket. A
// request through the symlink with the secret of the session, or with no
// secret, never gets a coordinator answer. The log line "#3379 post-fix
// result" records each answer, and the answer to the secret of the
// coordinator, which shows whether the path reaches the coordinator.
func TestSandboxExecRunDir_HostAPISockRedirectRefusedByCredential(t *testing.T) {
	requireSandboxExec(t)
	connectProbe := buildConnectProbe(t)
	httpProbe := buildHostAPIProbe(t)
	fx := newHostAPISockFixture(t)
	runCoordinator(t, fx.coordSock)
	mutatedPath := withMutatedProbeProfile(t, fx.m, func(p string) string {
		return strings.Replace(p, hostAPISockDenyBlock(fx.ownSock), "", 1)
	})

	expectOp(t, mutatedPath, connectProbe, false, "the session read the host-API secret of the coordinator", "--read", fx.coordSecret)
	plantRedirect(t, mutatedPath, connectProbe, fx)

	for _, tc := range []struct{ name, secretFile string }{
		{"the secret of the session", fx.ownSecret},
		{"no secret", "-"},
	} {
		out, err := runProbe(t, mutatedPath, httpProbe, fx.ownSock, tc.secretFile, http.MethodGet, "/merges")
		t.Logf("#3379 post-fix result: GET /merges through the symlink with %s: exit=%v output=%q", tc.name, err, strings.TrimSpace(out))
		if strings.Contains(out, "STATUS 200") {
			t.Errorf("GET /merges through the symlink with %s got a coordinator answer: %s", tc.name, out)
		}
		if strings.Contains(out, "STATUS") && !strings.Contains(out, "STATUS 401") {
			t.Errorf("GET /merges through the symlink with %s: %s, want 401 or no connect", tc.name, out)
		}
	}

	raw, err := os.ReadFile(fx.coordSecret)
	if err != nil {
		t.Fatalf("read the secret of the coordinator outside the sandbox: %v", err)
	}
	out, err := runProbe(t, mutatedPath, httpProbe, fx.ownSock, "-", http.MethodGet, "/merges", strings.TrimSpace(string(raw)))
	t.Logf("#3379 post-fix result: GET /merges through the symlink with the secret of the coordinator: exit=%v output=%q", err, strings.TrimSpace(out))
}
