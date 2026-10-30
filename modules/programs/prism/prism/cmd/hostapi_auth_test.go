package cmd

// hostapi_auth_test.go — the CLI host-API client and the host-API
// credential (#3379), with a real sidecar.
//
// The other proxy tests of this package serve HostAPIHandlerForTest, which
// has no credential check. These tests run Sidecar.Run, which serves the
// routes behind the check, so a client that does not send the secret of the
// socket fails here.

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/hostapiauth"
	"github.com/prismatic-koi/prism/internal/rundir"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

const (
	haRepo  = "prism-test-hostauth"
	haCoord = "prism-test-hostauth@main"
)

// startAuthCoordinator runs a coordinator sidecar whose host-API socket is
// in a short run dir, and returns the socket path. The pipe of the sidecar
// waits for an agent that never comes, so only the host API serves. The
// sidecar gets its instance ID at start: Run sets a missing ID after it
// binds the socket, and /merges refuses a request before that.
func startAuthCoordinator(t *testing.T) string {
	t.Helper()
	bus := sidecartest.NewIsolated(t, "")
	if err := bus.DB.UpsertStatusSeedRootAgentName(haCoord, haRepo, "/tmp/"+haCoord, "active", nil, nil, "coordinator", "", ""); err != nil {
		t.Fatalf("seed the coordinator: %v", err)
	}
	runDir := filepath.Join(stateguardtest.ShortTempDir(t, "hac"), "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(runDir, "hostapi.sock")
	pipe := filepath.Join(runDir, "pipe.sock")
	sc := sidecar.New(sidecar.Config{
		SessionName:           haCoord,
		InstanceID:            "prism-test-3379-" + session.SessionDirName(t.Name()),
		Repo:                  haRepo,
		Worktree:              t.TempDir(),
		DB:                    bus.DB,
		Clock:                 sidecar.RealClock(),
		AgentRole:             "coordinator",
		HarnessName:           "pi",
		Harness:               pih.New("", "coordinator", ""),
		HostAPISockPath:       sock,
		HarnessPipeSockPath:   pipe,
		HarnessPipeSecretPath: filepath.Join(runDir, "pipe.secret"),
		StartupConnectTimeout: time.Minute,
		PipeReconnectTimeout:  time.Minute,
		Logger:                log.New(io.Discard, "", 0),
	})
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
			return sock
		}
		if time.Now().After(deadline) {
			t.Fatalf("the host-API socket %s did not listen", sock)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestHostAPIClient_SendsTheCredentialOfItsSocket: the CLI client sends the
// secret in hostapi.secret next to the socket of PRISM_HOST_API, so a
// coordinator verb works through the socket of the session.
func TestHostAPIClient_SendsTheCredentialOfItsSocket(t *testing.T) {
	sock := startAuthCoordinator(t)
	var rows []map[string]any
	if err := proxyGetFromHostAPI("unix://"+sock, "/merges", nil, &rows); err != nil {
		t.Fatalf("GET /merges through the coordinator socket: %v", err)
	}
}

// TestHostAPIClient_RefusedWithoutTheCredential is the paired negative: with
// the secret file of the run dir replaced by another value, the same call
// gets the 401 of the credential check.
func TestHostAPIClient_RefusedWithoutTheCredential(t *testing.T) {
	sock := startAuthCoordinator(t)
	if err := rundir.WriteFile(hostapiauth.SecretPath(sock), []byte("not-the-secret")); err != nil {
		t.Fatal(err)
	}
	err := proxyGetFromHostAPI("unix://"+sock, "/merges", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("GET /merges with a wrong secret file: %v, want the unauthorized refusal", err)
	}
}

// TestHostAPIClient_RefusesSymlinkedSocket is the #3379 scenario on the
// client: PRISM_HOST_API names the hostapi.sock of a worker, which the
// worker replaced with a symlink to the coordinator socket. The client does
// not dial it, and its error names the path.
func TestHostAPIClient_RefusesSymlinkedSocket(t *testing.T) {
	coordSock := startAuthCoordinator(t)
	workerDir := filepath.Join(stateguardtest.ShortTempDir(t, "haw"), "run")
	if err := os.MkdirAll(workerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	workerSock := filepath.Join(workerDir, "hostapi.sock")
	if _, err := hostapiauth.WriteNewSecret(hostapiauth.SecretPath(workerSock)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(coordSock, workerSock); err != nil {
		t.Fatal(err)
	}

	err := proxyGetFromHostAPI("unix://"+workerSock, "/merges", nil, nil)
	if !errors.Is(err, rundir.ErrSymlink) || !strings.Contains(err.Error(), workerSock) {
		t.Errorf("GET /merges through a symlinked socket: %v, want an error that wraps rundir.ErrSymlink and names %s", err, workerSock)
	}
}
