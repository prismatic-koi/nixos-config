package iris_test

// cleanup_logfile_test.go — tests that iris.CleanupSession removes the
// per-session log file at <LogDir>/<session>.log (issue #1675).
//
// Other cleanup-step coverage lives in internal/iris/parity/cleanup_test.go;
// this file is narrowly scoped to the log-file removal step so it can be
// asserted without standing up a full git+pi layout.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

// TestCleanup_RemovesPerSessionLogFile asserts that CleanupSession removes
// the per-session log file when LogDir is configured.
func TestCleanup_RemovesPerSessionLogFile(t *testing.T) {
	iso := iristest.NewIsolated(t)

	sessionName := iristest.SessionName("logfile-cleanup")
	instanceID := "iris-test-logfile-cleanup-001"
	role := "worker"

	// Seed a session row so CleanupSession resolves it.
	if err := iso.DB.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Worktree:    "", // worktree-less is fine: cleanup tolerates it.
		Harness:     "pi",
		AgentRole:   &role,
		StartedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	// Create a per-session log file at the canonical location.
	logPath := iso.Paths.SessionLogPath(sessionName)
	if err := os.MkdirAll(iso.Paths.LogDir, 0o700); err != nil {
		t.Fatalf("MkdirAll LogDir: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("some log content\n"), 0o600); err != nil {
		t.Fatalf("WriteFile log: %v", err)
	}

	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:    iso.DB,
		RunDir:      iso.Paths.RunDir,
		LogDir:      iso.Paths.LogDir,
		ArchiveRoot: iso.Paths.ArchiveRoot,
	}, sessionName)
	if err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}

	if !res.LogFileRemoved {
		t.Errorf("LogFileRemoved=false, want true (errors=%v)", res.Errors)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("log file %q still exists after cleanup (err=%v)", logPath, err)
	}
}

// TestCleanup_MissingLogFileIsNotAnError asserts that cleaning up a session
// with no log file (e.g. one that crashed before writing) still succeeds and
// reports LogFileRemoved=true (treating absent-on-cleanup as the success
// path).
func TestCleanup_MissingLogFileIsNotAnError(t *testing.T) {
	iso := iristest.NewIsolated(t)

	sessionName := iristest.SessionName("logfile-cleanup-missing")
	instanceID := "iris-test-logfile-cleanup-002"
	role := "worker"

	if err := iso.DB.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Worktree:    "",
		Harness:     "pi",
		AgentRole:   &role,
		StartedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	logPath := iso.Paths.SessionLogPath(sessionName)
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("precondition: log file %q must not exist", logPath)
	}

	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:    iso.DB,
		RunDir:      iso.Paths.RunDir,
		LogDir:      iso.Paths.LogDir,
		ArchiveRoot: iso.Paths.ArchiveRoot,
	}, sessionName)
	if err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}
	if !res.LogFileRemoved {
		t.Errorf("LogFileRemoved=false on missing log; want true (errors=%v)", res.Errors)
	}
	// Sanity: errors slice should not mention the log file at all.
	for _, e := range res.Errors {
		if filepath.Dir(logPath) != "" && e != nil {
			// We don't want a "no such file or directory" error here.
			if errMentionsPath(e.Error(), logPath) {
				t.Errorf("unexpected error mentioning log path: %v", e)
			}
		}
	}
}

// errMentionsPath is a cheap substring check used to confirm we did not
// surface a not-exist error in the cleanup result for the missing log.
func errMentionsPath(msg, path string) bool {
	return len(msg) >= len(path) && contains(msg, path)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
