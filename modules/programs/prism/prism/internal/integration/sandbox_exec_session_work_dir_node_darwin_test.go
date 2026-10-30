//go:build darwin

package integration_test

// sandbox_exec_session_work_dir_node_darwin_test.go — the section-21n deny of
// generateProfile on the session work dir node (#3377).
//
// The work dir, $XDG_STATE_HOME/prism/sessions/<instance ID>/, holds the
// ssh-config, gitconfig, and allowed_signers that Prepare writes on the host.
// The session writes files in it (section 6), but it cannot rename, remove,
// or replace the node, and it cannot change the mode of the node. Without
// the deny, the session can put a symlink at the path. Prepare on a restore
// then refuses the symlinked work dir (internal/rundir), and it writes no
// file in the symlink target. Each positive test has a paired negative that
// mutates the profile and gets the opposite result.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/rundir"
	"github.com/prismatic-koi/prism/internal/session"
)

const workDirNodeSession = "prism-test@3377-work-dir"

// workDirNodeFixture returns the profile Manager, its work dir, and a path in
// its run dir to rename the work dir to. The run dir is in the same state
// home, which is under the real home (stateHomeOutsideTmp).
func workDirNodeFixture(t *testing.T) (*container.Manager, string, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))
	m := newPipeSessionManager(t, workDirNodeSession)
	runDir, err := session.SidecarHostAPIPath(workDirNodeSession)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	return m, sessionWorkDirOf(t, m), filepath.Join(filepath.Dir(runDir), "moved-work-dir")
}

// TestSandboxExecSessionWorkDirNode_CannotBeReplaced: under the production
// profile, the session cannot rename its work dir node. When the node is
// absent, the session cannot put a symlink or a new dir at its path. The
// session still writes a file in its work dir.
func TestSandboxExecSessionWorkDirNode_CannotBeReplaced(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	m, workDir, moved := workDirNodeFixture(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeProbePositiveProfile(t, prepared)

	expectOp(t, profilePath, probe, true, "the session cannot write a file in its work dir", "--create", filepath.Join(workDir, "kube-cache-probe"))
	expectOp(t, profilePath, probe, false, "the session renamed its work dir node", "--rename", workDir, moved)
	if info, err := os.Lstat(workDir); err != nil || !info.IsDir() {
		t.Fatalf("the work dir %s is not a dir after the denied rename: %v %v", workDir, info, err)
	}

	// The host removes the work dir, as when it is absent. The session must
	// not make the path again.
	if err := os.RemoveAll(workDir); err != nil {
		t.Fatalf("remove the work dir: %v", err)
	}
	target := t.TempDir()
	expectOp(t, profilePath, probe, false, "the session put a symlink at its work dir path", "--symlink", target, workDir)
	expectOp(t, profilePath, probe, false, "the session made a dir at its work dir path", "--mkdir", workDir)
	if _, err := os.Lstat(workDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the work dir path %s exists after the denied operations: %v", workDir, err)
	}
}

// TestSandboxExecSessionWorkDirNode_SwapWorksWithoutDeny is the paired
// negative: with the section-21n deny removed, the session moves its work dir
// away and puts a symlink at the path. Prepare, as on a restore, then
// refuses the symlinked work dir and writes no file in the target.
func TestSandboxExecSessionWorkDirNode_SwapWorksWithoutDeny(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	m, workDir, moved := workDirNodeFixture(t)
	mutatedPath := withMutatedProbeProfile(t, m, func(p string) string {
		return strings.Replace(p, runDirNodeDenyBlock(workDir), "", 1)
	})

	target := t.TempDir()
	expectOp(t, mutatedPath, probe, true, "the work dir rename still fails without section 21n, so the positive test proves nothing", "--rename", workDir, moved)
	expectOp(t, mutatedPath, probe, true, "the symlink at the work dir path still fails without section 21n, so the positive test proves nothing", "--symlink", target, workDir)
	if got, err := os.Readlink(workDir); err != nil || got != target {
		t.Fatalf("the work dir path is not a symlink to %s after the swap: %q %v", target, got, err)
	}

	if _, err := m.PrepareSessionWorkDir(); !errors.Is(err, rundir.ErrSymlinkDir) {
		t.Errorf("Prepare of the swapped work dir returned %v, want an error that wraps rundir.ErrSymlinkDir", err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Errorf("Prepare wrote %d entries into the symlink target %s", len(entries), target)
	}
}

// TestSandboxExecSessionWorkDirNode_ModeLocked: under the production profile,
// the session cannot chmod its work dir node, and the mode does not change.
func TestSandboxExecSessionWorkDirNode_ModeLocked(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	m, workDir, _ := workDirNodeFixture(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeProbePositiveProfile(t, prepared)

	before, _ := nodeModeAndFlags(t, workDir)
	expectOp(t, profilePath, probe, false, "the session changed the mode of its work dir node", "--chmod", workDir, "777")
	if after, _ := nodeModeAndFlags(t, workDir); after != before {
		t.Errorf("the mode of the work dir node changed: %v -> %v", before, after)
	}
}

// TestSandboxExecSessionWorkDirNode_ModeWritableWithoutDeny is the paired
// negative: with file-write-mode taken out of the section-21n deny, the chmod
// of the work dir node works.
func TestSandboxExecSessionWorkDirNode_ModeWritableWithoutDeny(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	m, workDir, _ := workDirNodeFixture(t)
	var rest []string
	for _, o := range runDirNodeDenyOps {
		if o != "file-write-mode" {
			rest = append(rest, o)
		}
	}
	mutatedPath := withMutatedProbeProfile(t, m, func(p string) string {
		return strings.Replace(p, runDirNodeDenyBlock(workDir), runDirNodeDenyBlock(workDir, rest...), 1)
	})
	expectOp(t, mutatedPath, probe, true, "the chmod of the work dir node still fails without file-write-mode in section 21n, so the positive test proves nothing", "--chmod", workDir, "777")
	if mode, _ := nodeModeAndFlags(t, workDir); mode != 0o777 {
		t.Errorf("the work dir node has mode %v after the chmod, want 0777", mode)
	}
}
