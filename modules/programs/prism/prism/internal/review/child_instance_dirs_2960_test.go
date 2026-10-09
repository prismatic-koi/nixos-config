package review_test

// child_instance_dirs_2960_test.go — issue #2960: a review-agent child
// session's own instance-ID-keyed directory trees (work dir and
// podman-proxy audit dir) must be removed when the parent's cleanup
// cascades to it, not left behind.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
	"github.com/prismatic-koi/prism/internal/review"
)

// mkInstanceDirs creates a non-empty work dir and audit dir for instanceID
// under a fresh XDG_STATE_HOME rooted at t.TempDir(), and returns both paths.
func mkInstanceDirs(t *testing.T, instanceID string) (workDir, auditDir string) {
	t.Helper()
	workDir, err := container.SessionWorkDirPath(instanceID)
	if err != nil {
		t.Fatalf("SessionWorkDirPath: %v", err)
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(workDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "gitconfig"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}

	auditDir, err = container.PodmanProxyAuditDirPath(instanceID)
	if err != nil {
		t.Fatalf("PodmanProxyAuditDirPath: %v", err)
	}
	if err := os.MkdirAll(auditDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(auditDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(auditDir, "audit.log"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write audit.log: %v", err)
	}
	return workDir, auditDir
}

func mustNotExist(t *testing.T, path, label string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s %s still exists after cleanup (err=%v)", label, path, err)
	}
}

// TestCleanupAgentSession_RemovesChildInstanceDirs verifies that
// cleanupAgentSession — the per-child helper CleanupReviewSessionsForParent
// calls for every review-agent child — removes that child's own work dir
// and podman-proxy audit dir, keyed by ITS OWN instance_id.
func TestCleanupAgentSession_RemovesChildInstanceDirs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	d := openTestDB(t)
	const session = "prism-test@2960-child~review-1-review-code"
	const instanceID = "2960-child-instance"

	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/x", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(session, instanceID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}

	workDir, auditDir := mkInstanceDirs(t, instanceID)

	review.CleanupAgentSessionForTest(d, session, db.ReapCauseParentCleanup)

	mustNotExist(t, workDir, "work dir")
	mustNotExist(t, auditDir, "audit dir")
}

// TestCleanupAgentSession_NoInstanceID_NoError verifies the edge case where
// a child session never enabled containers and has no instance_id recorded
// — cleanupAgentSession must not error and must not attempt a removal with
// an empty instance ID.
func TestCleanupAgentSession_NoInstanceID_NoError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	d := openTestDB(t)
	const session = "prism-test@2960-noiid~review-1-review-code"

	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/x", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Must not panic or error with no instance_id set.
	review.CleanupAgentSessionForTest(d, session, db.ReapCauseParentCleanup)

	st, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil || st.EndedAt == nil {
		t.Fatalf("row not ended after cleanup: %+v", st)
	}
}

// TestCleanupAgentSession_SweepsChildPrismContainers verifies that a child
// that used `prism container` has its labelled containers removed and its
// prism-container audit dir removed.
func TestCleanupAgentSession_SweepsChildPrismContainers(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	d := openTestDB(t)
	const session = "prism-test@3061-child~review-1-review-qa"
	const instanceID = "30610000-0000-4000-8000-000000000001"
	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/x", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(session, instanceID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	auditDir, err := container.PrismContainerAuditDirPath(instanceID)
	if err != nil {
		t.Fatalf("PrismContainerAuditDirPath: %v", err)
	}
	if err := os.MkdirAll(auditDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	fake := &prismcontainertest.Fake{}
	fake.Add(prismcontainertest.Container{ID: "child", State: "running", Labels: map[string]string{prismcontainer.LabelInstanceID: instanceID}})
	fake.Add(prismcontainertest.Container{ID: "other", State: "running", Labels: map[string]string{prismcontainer.LabelInstanceID: "30610000-0000-4000-8000-000000000002"}})
	t.Cleanup(review.SetChildPrismContainerRunnerForTest(fake))

	review.CleanupAgentSessionForTest(d, session, db.ReapCauseParentCleanup)

	if left := fake.Containers(); len(left) != 1 || left[0].ID != "other" {
		t.Errorf("containers left = %+v, want only other", left)
	}
	mustNotExist(t, auditDir, "prism-container audit dir")
}
