//go:build darwin

package integration_test

// sandbox_exec_run_dir_darwin_test.go — the per-session run dir of the
// sandbox-exec profile (the section-6 run-dir grant and the section-21m
// deny of generateProfile, #3373).
//
// The run dir, $XDG_STATE_HOME/prism/run/<hash>/, holds hostapi.sock,
// pipe.secret, and agent-run.log of the session. The session reads and
// writes the files in it. It cannot remove, rename, or replace the run dir
// node, and it cannot change the mode, the flags, or the ACL of the node.
// Without the deny, the session can put a symlink to the run dir of another
// session at the path. The sidecar of the session then refuses to start
// (internal/rundir), so it does not listen at the hostapi.sock of the other
// session.
//
// The tests use the connect probe of
// sandbox_exec_container_socket_darwin_test.go and the probe extras
// (augmentProfileForProbe). They put XDG_STATE_HOME, and so the run dirs,
// under the real home (stateHomeOutsideTmp): the profile grants a read of
// the user $TMPDIR, and a run dir there makes a read test vacuous. Each
// positive test has a paired negative that mutates the profile and gets the
// opposite result.

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/rundir"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar"
)

const (
	runDirOwnSession   = "prism-test@3373-own"
	runDirOtherSession = "prism-test@3373-other"
)

// runDirNodeDenyOps are the operations of the section-21m deny, in the order
// that the generator writes them.
var runDirNodeDenyOps = sessionTmpNodeDenyOps

// runDirNodeDenyBlock is the section-21m clause for runDir, with the
// operations ops (default: runDirNodeDenyOps). withMutatedProbeProfile fails
// when the text is not in the profile, so a copy that drifts from the
// generator fails the negative tests.
func runDirNodeDenyBlock(runDir string, ops ...string) string {
	if len(ops) == 0 {
		ops = runDirNodeDenyOps
	}
	return "(deny " + strings.Join(ops, " ") + "\n" +
		"  (literal " + sbplQuoteForTest(runDir) + "))\n"
}

// runDirFixture is the own session, with its profile manager and run dir,
// and the run dir of another session with a live hostapi.sock.
type runDirFixture struct {
	m           *container.Manager
	runDir      string
	otherRunDir string
	otherSock   string
}

// newRunDirFixture sets XDG_STATE_HOME under the real home and makes the
// fixture. The run dir of the own session is empty.
func newRunDirFixture(t *testing.T) runDirFixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))
	m := newPipeSessionManager(t, runDirOwnSession)
	own, err := session.SidecarHostAPIPath(runDirOwnSession)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	otherSock, err := session.SidecarHostAPIPath(runDirOtherSession)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(otherSock), 0o700); err != nil {
		t.Fatalf("make the run dir of the other session: %v", err)
	}
	startUnixListener(t, otherSock)
	return runDirFixture{m: m, runDir: filepath.Dir(own), otherRunDir: filepath.Dir(otherSock), otherSock: otherSock}
}

// requireEmptyRunDir fails the test when the run dir holds a file. A remove
// of a dir that is not empty fails for that reason alone, so a remove check
// on such a dir proves nothing.
func requireEmptyRunDir(t *testing.T, runDir string) {
	t.Helper()
	entries, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatalf("read the run dir %s: %v", runDir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("the run dir %s holds %d entries before the remove check, want none", runDir, len(entries))
	}
}

// TestSandboxExecRunDir_FilesUsableUnderProductionProfile: under the
// production profile, the session creates, reads, renames, removes, and
// changes the mode of a file in its run dir, and connects to the
// hostapi.sock of its sidecar there.
func TestSandboxExecRunDir_FilesUsableUnderProductionProfile(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	fx := newRunDirFixture(t)
	prepared, _ := preparePositiveProfile(t, fx.m)
	profilePath := writeProbePositiveProfile(t, prepared)

	f := filepath.Join(fx.runDir, "agent-run.log")
	expectOp(t, profilePath, probe, true, "the session cannot create a file in its run dir", "--create", f)
	expectOp(t, profilePath, probe, true, "the session cannot read a file in its run dir", "--read", f)
	expectOp(t, profilePath, probe, true, "the session cannot chmod a file in its run dir", "--chmod", f, "600")
	expectOp(t, profilePath, probe, true, "the session cannot rename a file in its run dir", "--rename", f, f+".2")
	expectOp(t, profilePath, probe, true, "the session cannot remove a file in its run dir", "--remove", f+".2")

	sock := filepath.Join(fx.runDir, "hostapi.sock")
	startUnixListener(t, sock)
	expectConnected(t, profilePath, probe, sock, "the session cannot connect to the hostapi.sock in its run dir")
}

// TestSandboxExecRunDir_FilesDeniedWithoutGrant is the paired negative. It
// points the run-dir rules of the profile at a path that does not exist, and
// the same create and connect then fail.
func TestSandboxExecRunDir_FilesDeniedWithoutGrant(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	fx := newRunDirFixture(t)
	mutatedPath := withMutatedProbeProfile(t, fx.m, func(p string) string {
		return strings.ReplaceAll(p, sbplQuoteForTest(fx.runDir), sbplQuoteForTest(fx.runDir+".prism-3373-disabled"))
	})

	expectOp(t, mutatedPath, probe, false, "the create still works without the run-dir grant, so the positive test proves nothing",
		"--create", filepath.Join(fx.runDir, "agent-run.log"))
	sock := filepath.Join(fx.runDir, "hostapi.sock")
	startUnixListener(t, sock)
	expectDenied(t, mutatedPath, probe, sock, "the connect still works without the run-dir grant, so the positive test proves nothing")
}

// TestSandboxExecRunDir_NodeCannotBeReplaced: under the production profile,
// the session cannot remove or rename the node of its run dir (section
// 21m). When the node is absent, the session cannot put a symlink to the run
// dir of another session, or a new dir, at its path.
func TestSandboxExecRunDir_NodeCannotBeReplaced(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	fx := newRunDirFixture(t)
	prepared, _ := preparePositiveProfile(t, fx.m)
	profilePath := writeProbePositiveProfile(t, prepared)

	requireEmptyRunDir(t, fx.runDir)
	expectOp(t, profilePath, probe, false, "the session removed its run dir node", "--remove", fx.runDir)
	moved := filepath.Join(sessionWorkDirOf(t, fx.m), "moved-run-dir")
	expectOp(t, profilePath, probe, false, "the session renamed its run dir node", "--rename", fx.runDir, moved)
	if info, err := os.Lstat(fx.runDir); err != nil || !info.IsDir() {
		t.Fatalf("the run dir %s is not a dir after the denied operations: %v %v", fx.runDir, info, err)
	}

	// The host removes the run dir, as when it is absent. The session must
	// not make the path again.
	if err := os.Remove(fx.runDir); err != nil {
		t.Fatalf("remove the run dir: %v", err)
	}
	expectOp(t, profilePath, probe, false, "the session put a symlink to the run dir of another session at its run dir path", "--symlink", fx.otherRunDir, fx.runDir)
	expectOp(t, profilePath, probe, false, "the session made a dir at its run dir path", "--mkdir", fx.runDir)
	if _, err := os.Lstat(fx.runDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the run dir path %s exists after the denied operations: %v", fx.runDir, err)
	}
}

// TestSandboxExecRunDir_SwapWorksWithoutDeny is the paired negative: with the
// section-21m deny removed, the session moves its run dir away and puts a
// symlink to the run dir of another session at the path. The sidecar of the
// session, as after a `prism restore`, then refuses to start, and the
// hostapi.sock of the other session stays live.
func TestSandboxExecRunDir_SwapWorksWithoutDeny(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	fx := newRunDirFixture(t)
	mutatedPath := withMutatedProbeProfile(t, fx.m, func(p string) string {
		return strings.Replace(p, runDirNodeDenyBlock(fx.runDir), "", 1)
	})

	otherBefore, err := os.Lstat(fx.otherSock)
	if err != nil {
		t.Fatalf("lstat the hostapi.sock of the other session: %v", err)
	}
	moved := filepath.Join(sessionWorkDirOf(t, fx.m), "moved-run-dir")
	expectOp(t, mutatedPath, probe, true, "the run dir rename still fails without section 21m, so the positive test proves nothing", "--rename", fx.runDir, moved)
	expectOp(t, mutatedPath, probe, true, "the symlink at the run dir path still fails without section 21m, so the positive test proves nothing", "--symlink", fx.otherRunDir, fx.runDir)
	if target, err := os.Readlink(fx.runDir); err != nil || target != fx.otherRunDir {
		t.Fatalf("the run dir path is not a symlink to the other run dir after the swap: %q %v", target, err)
	}

	hostAPI := filepath.Join(fx.runDir, "hostapi.sock")
	sc := sidecar.New(sidecar.Config{
		SessionName:     runDirOwnSession,
		Repo:            "prism-test",
		Worktree:        t.TempDir(),
		DB:              openTestDB(t),
		Clock:           sidecar.RealClock(),
		AgentRole:       "worker",
		HarnessName:     "pi",
		Harness:         pih.New("", "", ""),
		HostAPISockPath: hostAPI,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- sc.Run(ctx) }()
	select {
	case err := <-runErr:
		if !errors.Is(err, rundir.ErrSymlinkDir) {
			t.Errorf("the sidecar of the swapped session returned %v, want an error that wraps rundir.ErrSymlinkDir", err)
		}
	case <-ctx.Done():
		sc.Shutdown()
		t.Fatal("the sidecar of the swapped session did not refuse the symlinked run dir")
	}
	// The check runs outside the sandbox: section 17b denies the session a
	// connect in the run dir of another session, through the symlink too.
	if otherAfter, err := os.Lstat(fx.otherSock); err != nil || !os.SameFile(otherBefore, otherAfter) {
		t.Errorf("the hostapi.sock of the other session was replaced after the refused start: %v", err)
	}
	if c, err := net.Dial("unix", fx.otherSock); err != nil {
		t.Errorf("the hostapi.sock of the other session is not live after the refused start: %v", err)
	} else {
		_ = c.Close()
	}
}

// TestSandboxExecRunDir_NodeModeFlagsAndACLLocked: under the production
// profile, the session cannot chmod, chflags, or chmod +a the node of its
// run dir (section 21m). The mode, the flags, and the ACL of the node do not
// change.
func TestSandboxExecRunDir_NodeModeFlagsAndACLLocked(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	fx := newRunDirFixture(t)
	prepared, _ := preparePositiveProfile(t, fx.m)
	profilePath := writeProbePositiveProfile(t, prepared)
	clearFlagsAtCleanup(t, fx.runDir)

	before := readNodeState(t, fx.runDir)
	ops := []struct {
		name string
		run  func()
	}{
		{"chmod 777", func() {
			expectOp(t, profilePath, probe, false, "the session changed the mode of its run dir node", "--chmod", fx.runDir, "777")
		}},
		{"chflags uchg", func() {
			expectOp(t, profilePath, probe, false, "the session set the uchg flag on its run dir node", "--chflags", fx.runDir, strconv.Itoa(ufImmutable))
		}},
		{"chmod +a", func() {
			expectCmd(t, profilePath, false, "the session added an ACL entry to its run dir node", "/bin/chmod", "+a", aclEntry, fx.runDir)
		}},
	}
	for _, op := range ops {
		op.run()
		if got := readNodeState(t, fx.runDir); got != before {
			t.Errorf("%s changed the run dir node: %+v -> %+v", op.name, before, got)
		}
		// A uchg flag that one probe set makes the next chmod fail with
		// EPERM, so the next check passes whatever the profile holds.
		resetNode(t, fx.runDir, before)
	}
}

// TestSandboxExecRunDir_NodeModeFlagsAndACLWritableWithoutDeny is the paired
// negative: with one operation taken out of the section-21m deny, the
// matching change of the run dir node succeeds.
func TestSandboxExecRunDir_NodeModeFlagsAndACLWritableWithoutDeny(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	cases := []struct {
		op    string
		run   func(t *testing.T, profilePath, runDir string)
		check func(t *testing.T, runDir string)
	}{
		{"file-write-mode", func(t *testing.T, profilePath, runDir string) {
			expectOp(t, profilePath, probe, true, "the chmod of the run dir node still fails without file-write-mode in section 21m, so the positive test proves nothing", "--chmod", runDir, "777")
		}, func(t *testing.T, runDir string) {
			if mode, _ := nodeModeAndFlags(t, runDir); mode != 0o777 {
				t.Errorf("the run dir node has mode %v after the chmod, want 0777", mode)
			}
		}},
		{"file-write-flags", func(t *testing.T, profilePath, runDir string) {
			expectOp(t, profilePath, probe, true, "the chflags of the run dir node still fails without file-write-flags in section 21m, so the positive test proves nothing", "--chflags", runDir, strconv.Itoa(ufImmutable))
		}, func(t *testing.T, runDir string) {
			if _, flags := nodeModeAndFlags(t, runDir); flags&ufImmutable == 0 {
				t.Errorf("the run dir node has flags %#x after the chflags, want uchg", flags)
			}
		}},
		{"file-write-acl", func(t *testing.T, profilePath, runDir string) {
			expectCmd(t, profilePath, true, "chmod +a on the run dir node still fails without file-write-acl in section 21m, so the positive test proves nothing", "/bin/chmod", "+a", aclEntry, runDir)
		}, func(t *testing.T, runDir string) {
			if !readNodeState(t, runDir).acl {
				t.Errorf("the run dir node has no ACL after chmod +a")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			fx := newRunDirFixture(t)
			var rest []string
			for _, o := range runDirNodeDenyOps {
				if o != tc.op {
					rest = append(rest, o)
				}
			}
			mutatedPath := withMutatedProbeProfile(t, fx.m, func(p string) string {
				return strings.Replace(p, runDirNodeDenyBlock(fx.runDir), runDirNodeDenyBlock(fx.runDir, rest...), 1)
			})
			clearFlagsAtCleanup(t, fx.runDir)
			tc.run(t, mutatedPath, fx.runDir)
			tc.check(t, fx.runDir)
		})
	}
}
