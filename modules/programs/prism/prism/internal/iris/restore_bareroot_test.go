package iris

// restore_bareroot_test.go — D-7 regression test: the restore path must
// populate SessionRecord.BareRoot so the credential broker can resolve the
// role-scoped GITHUB_TOKEN for bash subprocesses run by a restored session.
//
// Without this plumbing, restored sessions would silently downgrade to the
// host GITHUB_TOKEN fallback every time the daemon restarts — a credential
// downgrade with no visible symptom until tools start failing on private
// repos.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestNewRestoreSupervisor_PopulatesBareRoot constructs a restore supervisor
// directly and asserts that the resulting SessionRecord has BareRoot set
// from the SupervisorConfig.
func TestNewRestoreSupervisor_PopulatesBareRoot(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer database.Close()

	const wantBareRoot = "/path/to/fake/bare-root"

	cfg := SupervisorConfig{
		SessionName:      "test@restore-bareroot",
		Worktree:         tmp,
		Role:             "worker",
		BareRoot:         wantBareRoot,
		PIBinaryPath:     "/bin/true",
		RestartThreshold: 1,
		RunDir:           tmp,
		Database:         database,
	}

	sess := db.IrisSessionRow{
		InstanceID:  "test-instance-id",
		SessionName: "test@restore-bareroot",
		Worktree:    tmp,
		Role:        "worker",
		StartedAt:   time.Now(),
	}

	// Insert the sessions row so the supervisor's later DB writes do not
	// violate FK constraints — newRestoreSupervisor itself does not write,
	// but it calls IrisUpdateSessionState which requires the row to exist.
	if err := database.InsertSession(db.Session{
		InstanceID:  sess.InstanceID,
		SessionName: sess.SessionName,
		Worktree:    sess.Worktree,
		Harness:     "pi",
		StartedAt:   sess.StartedAt,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	sup, err := newRestoreSupervisor(cfg, sess)
	if err != nil {
		t.Fatalf("newRestoreSupervisor: %v", err)
	}
	defer sup.harness.Close()

	got := sup.SessionRecord().BareRoot
	if got != wantBareRoot {
		t.Errorf("SessionRecord.BareRoot = %q; want %q (restore path must propagate BareRoot from SupervisorConfig)",
			got, wantBareRoot)
	}
}
