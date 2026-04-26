package sidecar

// Tests for the host-API socket bind path (#1050).
//
// Two concerns are covered here:
//
//   - AC-3: a session whose name would historically have produced a
//     >104-byte sun_path (e.g. a long worker session composed with
//     ~review-N-review-<role>) must now bind successfully — i.e. the path
//     scheme does not return EINVAL for the worst-case name.
//   - AC-4: if net.Listen("unix", ...) fails for any reason, Run() must
//     surface the error rather than continuing with a nil listener.

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	opencode "github.com/prismatic-koi/prism/internal/harness/opencode"
	prismsession "github.com/prismatic-koi/prism/internal/session"
)

// TestHostAPIBind_WorstCaseSessionName_DoesNotEINVAL is the AC-3 regression
// test. It exercises the actual net.Listen("unix", ...) call against a path
// derived from the worst-case session name from the issue body (an 80-char
// branch + ~review-99-review-context suffix) and asserts that the bind
// succeeds — i.e. the new short-hash directory scheme produces a bindable
// path even for pathological inputs. The path-length budget itself is
// asserted independently in
// internal/session.TestSidecarHostAPIPath_LengthInvariant_*.
//
// The bind is performed under a freshly-created temp directory rooted at /tmp
// (kept deliberately short so the per-test path itself stays inside the
// kernel limit on every CI runner). This means the test exercises the
// SessionDirName-derived suffix shape, which is the part this fix changes —
// the long input session name no longer pollutes the on-disk path.
func TestHostAPIBind_WorstCaseSessionName_DoesNotEINVAL(t *testing.T) {
	// Mirror the worst-case shape called out by AC-1 / AC-3.
	worstCase := "nixos-config@" + strings.Repeat("x", 80) + "~review-99-review-context"

	// Build the path under a short, controlled XDG_STATE_HOME so the test
	// does not depend on the runner's TMPDIR length. The point of this
	// regression test is the SUFFIX of the path (which used to embed the
	// full session name); the prefix is owned by the deployment.
	stateHome, err := shortStateHome(t)
	if err != nil {
		t.Fatalf("shortStateHome: %v", err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)

	sockPath, err := prismsession.SidecarHostAPIPath(worstCase)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}

	if mkErr := os.MkdirAll(filepath.Dir(sockPath), 0o700); mkErr != nil {
		t.Fatalf("mkdir socket dir: %v", mkErr)
	}

	ln, listenErr := net.Listen("unix", sockPath)
	if listenErr != nil {
		// If we got EINVAL, that is the exact bug #1050 fixed; surface it
		// loudly so the regression is unmistakable.
		if errors.Is(listenErr, syscall.EINVAL) {
			t.Fatalf("net.Listen returned EINVAL — sun_path overflow regression (#1050): %v (path=%q, len=%d)",
				listenErr, sockPath, len(sockPath))
		}
		t.Fatalf("net.Listen unexpected error: %v (path=%q, len=%d)", listenErr, sockPath, len(sockPath))
	}
	defer ln.Close()
}

// shortStateHome creates a short-prefix directory under /tmp (NOT under
// t.TempDir(), which Go derives from TMPDIR and may itself be long enough to
// blow the budget on some runners). The test cleans up via t.Cleanup.
func shortStateHome(t *testing.T) (string, error) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "p1050-")
	if err != nil {
		return "", err
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir, nil
}

// TestSidecarRun_BindFailureReturnsError is the AC-4 regression test. It
// injects a bind failure by pointing HostAPISockPath at a path that cannot be
// created (a directory whose parent is a regular file) and asserts that Run()
// returns within a bounded time with a non-nil error mentioning "bind failed".
//
// Before the #1050 fix this would log-and-continue with hostAPIListener nil,
// leaving the agent partially functional and effectively undetectable.
func TestSidecarRun_BindFailureReturnsError(t *testing.T) {
	// Construct an HostAPISockPath whose parent directory cannot be created
	// because some component along the path is a regular file — that makes
	// both MkdirAll and the subsequent net.Listen fail without depending on
	// platform-specific permission behaviour.
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("blocker"), 0o644); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	// Path: <tmp>/not-a-dir/sub/hostapi.sock — MkdirAll on the parent will
	// fail (ENOTDIR) and net.Listen will then fail with the missing dir.
	sockPath := filepath.Join(blocker, "sub", "hostapi.sock")

	clk := newTestClock()
	d := openTestDB(t)

	cfg := Config{
		SessionName:     "test-repo@bind-fail",
		Repo:            "test-repo",
		Worktree:        "/tmp/test-worktree",
		OpencodeURL:     "http://127.0.0.1:1", // unreachable, but Run() should exit before SSE setup matters
		HostAPISockPath: sockPath,
		DB:              d,
		Clock:           clk,
		Harness:         opencode.New("http://127.0.0.1:1", nil, "", ""),
	}
	s := New(cfg)

	// Run with a bounded timeout. AC-4 says "within a bounded time".
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Run returned nil error despite injected bind failure")
		}
		if !strings.Contains(err.Error(), "bind failed") &&
			!strings.Contains(err.Error(), "host-API socket") {
			t.Fatalf("Run returned wrong error (want bind-related): %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Run did not exit within bounded time after bind failure")
	}

	// Defensive: ensure no listener was registered on the sidecar.
	s.mu.Lock()
	ln := s.hostAPIListener
	s.mu.Unlock()
	if ln != nil {
		t.Error("hostAPIListener was set despite bind failure")
	}
}
