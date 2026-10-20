package cmd

// reset_instance_dirs_2960_test.go — issue #2960: `prism reset` must remove
// the instance-ID-keyed work dir and podman-proxy audit dir for every
// session it snapshots, not leave them behind. Issue #3150 adds the
// per-session TMPDIR to the same removal.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
)

// TestRunReset_RemovesInstanceDirsForEverySnapshottedSession verifies AC:
// "After `prism reset` of a session, neither the work dir nor the audit
// directory of that session remains." It also covers the case where the
// session has no pi resume pointer (so it is excluded from the
// resume-pointer transcript path) but still has an instance_id and must
// still have its directories removed.
func TestRunReset_RemovesInstanceDirsForEverySnapshottedSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	const session = "repo@2960-reset"
	const instanceID = "2960-reset-instance"
	if err := d.UpsertStatus(session, "repo", "/worktree/"+session, "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(session, instanceID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	d.Close()

	workDir, err := container.SessionWorkDirPath(instanceID)
	if err != nil {
		t.Fatalf("SessionWorkDirPath: %v", err)
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(workDir): %v", err)
	}
	auditDir, err := container.PodmanProxyAuditDirPath(instanceID)
	if err != nil {
		t.Fatalf("PodmanProxyAuditDirPath: %v", err)
	}
	if err := os.MkdirAll(auditDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(auditDir): %v", err)
	}
	t.Cleanup(container.SetSessionTmpRootForTest(t.TempDir()))
	tmpDir, err := container.SessionTmpDirPath(instanceID)
	if err != nil {
		t.Fatalf("SessionTmpDirPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "go-build1"), 0o700); err != nil {
		t.Fatalf("MkdirAll(tmpDir): %v", err)
	}

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	_, instanceIDs, err := resetSnapshotSessions()
	if err != nil {
		t.Fatalf("resetSnapshotSessions: %v", err)
	}
	if err := resetMarkDBEnded(); err != nil {
		t.Fatalf("resetMarkDBEnded: %v", err)
	}
	found := false
	for _, id := range instanceIDs {
		if id == instanceID {
			found = true
		}
	}
	if !found {
		t.Fatalf("resetSnapshotSessions instanceIDs = %v, want to contain %q", instanceIDs, instanceID)
	}
	for _, id := range instanceIDs {
		removeSessionInstanceDirs(id)
	}

	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Errorf("work dir %s still exists after reset (err=%v)", workDir, err)
	}
	if _, err := os.Stat(auditDir); !os.IsNotExist(err) {
		t.Errorf("audit dir %s still exists after reset (err=%v)", auditDir, err)
	}
	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Errorf("per-session TMPDIR %s still exists after reset (err=%v)", tmpDir, err)
	}
}

// TestRunReset_NoInstanceID_NoError covers the edge case: a session that
// never enabled containers has no instance_id, so it is simply absent from
// the returned slice — no error, nothing to remove.
func TestRunReset_NoInstanceID_NoError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.UpsertStatus("repo@2960-noiid", "repo", "/worktree/repo@2960-noiid", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	_, instanceIDs, err := resetSnapshotSessions()
	if err != nil {
		t.Fatalf("resetSnapshotSessions: %v", err)
	}
	if err := resetMarkDBEnded(); err != nil {
		t.Fatalf("resetMarkDBEnded: %v", err)
	}
	for _, id := range instanceIDs {
		if id == "" {
			t.Errorf("instanceIDs contains an empty string: %v", instanceIDs)
		}
	}
	for _, id := range instanceIDs {
		removeSessionInstanceDirs(id) // must not panic even if called
	}
}
