package sidecar

// Tests for the refusal of a symlinked run dir (#3373). A sandbox can write
// its run dir. If it puts a symlink to the run dir of another session at the
// path, the next sidecar of the session (for example after `prism restore`)
// must not remove the hostapi.sock of the other session and listen at its
// path.

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/rundir"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

// lockedBuffer is a bytes.Buffer that is safe for concurrent writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// swappedRunDir makes the run dir of another session, with a file at
// sockName in it, and a run dir path for this session that is a symlink to
// it. It returns the path of this run dir and of the file of the other
// session.
func swappedRunDir(t *testing.T, sockName string) (ownRunDir, otherSock string) {
	t.Helper()
	root := stateguardtest.ShortTempDir(t, "p3373-")
	otherRunDir := filepath.Join(root, "other")
	if err := os.Mkdir(otherRunDir, 0o700); err != nil {
		t.Fatal(err)
	}
	otherSock = filepath.Join(otherRunDir, sockName)
	if err := os.WriteFile(otherSock, []byte("other session"), 0o600); err != nil {
		t.Fatal(err)
	}
	ownRunDir = filepath.Join(root, "own")
	if err := os.Symlink(otherRunDir, ownRunDir); err != nil {
		t.Fatal(err)
	}
	return ownRunDir, otherSock
}

// runRefused runs a sidecar with cfg and checks that Run refuses the
// symlinked run dir, logs the reason, and leaves the file of the other
// session as it was.
func runRefused(t *testing.T, cfg Config, otherSock string) {
	t.Helper()
	logBuf := &lockedBuffer{}
	cfg.Logger = log.New(logBuf, "", 0)
	cfg.DB = openTestDB(t)
	cfg.Clock = newTestClock()
	s := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()
	var err error
	select {
	case err = <-errCh:
	case <-ctx.Done():
		s.Shutdown()
		t.Fatal("Run did not return for a symlinked run dir")
	}

	if !errors.Is(err, rundir.ErrSymlinkDir) {
		t.Errorf("Run returned %v, want an error that wraps rundir.ErrSymlinkDir", err)
	}
	if logged := logBuf.String(); !strings.Contains(logged, "refusing to write in") || !strings.Contains(logged, rundir.ErrSymlinkDir.Error()) {
		t.Errorf("the sidecar log does not name the reason of the refusal:\n%s", logged)
	}
	if b, readErr := os.ReadFile(otherSock); readErr != nil || string(b) != "other session" {
		t.Errorf("the file of the other session at %s changed: %q (%v)", otherSock, b, readErr)
	}
	s.mu.Lock()
	ln, pipeLn := s.hostAPIListener, s.harnessPipeListener
	s.mu.Unlock()
	if ln != nil || pipeLn != nil {
		t.Error("the sidecar has a listener after it refused the run dir")
	}
}

// TestSidecarRun_RefusesSymlinkedRunDir_HostAPISock: the sidecar does not
// remove the hostapi.sock of the other session or listen at its path.
func TestSidecarRun_RefusesSymlinkedRunDir_HostAPISock(t *testing.T) {
	ownRunDir, otherSock := swappedRunDir(t, "hostapi.sock")
	runRefused(t, Config{
		SessionName:     "test-repo@swap",
		Repo:            "test-repo",
		Worktree:        t.TempDir(),
		HarnessURL:      "http://127.0.0.1:1",
		HostAPISockPath: filepath.Join(ownRunDir, "hostapi.sock"),
		Harness:         newSSEHarness(),
	}, otherSock)
}

// TestSidecarRun_RefusesSymlinkedRunDir_LiveOtherSidecar: when the sidecar
// of the other session listens, the refusal names the symlink, not a
// duplicate start of this session.
func TestSidecarRun_RefusesSymlinkedRunDir_LiveOtherSidecar(t *testing.T) {
	ownRunDir, otherSock := swappedRunDir(t, "probe")
	otherHostAPI := filepath.Join(filepath.Dir(otherSock), "hostapi.sock")
	ln, err := net.Listen("unix", otherHostAPI)
	if err != nil {
		t.Fatalf("listen %s: %v", otherHostAPI, err)
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

	runRefused(t, Config{
		SessionName:     "test-repo@swap-live",
		Repo:            "test-repo",
		Worktree:        t.TempDir(),
		HarnessURL:      "http://127.0.0.1:1",
		HostAPISockPath: filepath.Join(ownRunDir, "hostapi.sock"),
		Harness:         newSSEHarness(),
	}, otherSock)
	if c, err := net.Dial("unix", otherHostAPI); err != nil {
		t.Errorf("the listener of the other session is gone: %v", err)
	} else {
		_ = c.Close()
	}
}

// TestSidecarRun_RefusesSymlinkedRunDir_PipeSock: the sidecar does not
// remove the pipe.sock of the other session or listen at its path.
func TestSidecarRun_RefusesSymlinkedRunDir_PipeSock(t *testing.T) {
	ownRunDir, otherSock := swappedRunDir(t, "pipe.sock")
	pipeSock := filepath.Join(ownRunDir, "pipe.sock")
	runRefused(t, Config{
		SessionName:           "test-repo@swap-pipe",
		Repo:                  "test-repo",
		Worktree:              t.TempDir(),
		AgentRole:             "worker",
		HarnessName:           "pi",
		HarnessPipeSockPath:   pipeSock,
		HarnessPipeSecretPath: pipeSecretPathFor(pipeSock),
		StartupConnectTimeout: 200 * time.Millisecond,
		PipeReconnectTimeout:  50 * time.Millisecond,
		Harness:               pih.New("", "", ""),
	}, otherSock)
}
