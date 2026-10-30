package cmd

// Tests that the host-side opens of the run-dir logs in cmd refuse a
// symlink (#3373). The sandbox of a session can write its run dir, so it can
// put a symlink at agent-run.log, for example to a startup file of the shell
// or to a credential.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	prismsession "github.com/prismatic-koi/prism/internal/session"
)

// plantedAgentRunLog puts a symlink at the agent-run log of sess, under a new
// XDG_STATE_HOME, to a new file that holds content. It returns the log path
// and the target path.
func plantedAgentRunLog(t *testing.T, sess, content string) (logPath, target string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logPath, err := prismsession.AgentRunLogPath(sess)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	target = filepath.Join(t.TempDir(), "zshenv")
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, logPath); err != nil {
		t.Fatal(err)
	}
	return logPath, target
}

// TestOpenAgentRunLog_RefusesSymlink: agent-run tees the stderr of the
// sandbox into the file that openAgentRunLog opens. A symlink at the path
// gets no file, and the target does not change.
func TestOpenAgentRunLog_RefusesSymlink(t *testing.T) {
	const sess = "myrepo@planted"
	_, target := plantedAgentRunLog(t, sess, "keep")

	if f := openAgentRunLog(sess); f != nil {
		_, _ = f.WriteString("planted\n")
		_ = f.Close()
		t.Error("openAgentRunLog opened a symlinked agent-run log")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Errorf("the symlink target holds %q, want %q", b, "keep")
	}
}

// TestOpenAgentRunLog_RefusesSymlinkedRunDir: a run dir that is a symlink to
// the run dir of another session gets no file.
func TestOpenAgentRunLog_RefusesSymlinkedRunDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const sess = "myrepo@swapped"
	logPath, err := prismsession.AgentRunLogPath(sess)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Dir(logPath)
	other := filepath.Join(filepath.Dir(runDir), "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, runDir); err != nil {
		t.Fatal(err)
	}
	if f := openAgentRunLog(sess); f != nil {
		_ = f.Close()
		t.Error("openAgentRunLog opened a log in a symlinked run dir")
	}
	if entries, _ := os.ReadDir(other); len(entries) != 0 {
		t.Errorf("openAgentRunLog made %d entries in the run dir of the other session", len(entries))
	}
}

// TestLogsReaders_RefuseSymlink: each host-side read of `prism logs` refuses
// a symlinked log and does not print the target.
func TestLogsReaders_RefuseSymlink(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	const secret = "ghp_secret_token_value"
	logPath, _ := plantedAgentRunLog(t, "myrepo@planted", secret+"\n")

	// A finished session makes runLogsFollow print the log and return, so a
	// read through the symlink fails the test instead of a wait.
	dbPath := filepath.Join(t.TempDir(), "prism.db")
	SetTestDBPath(dbPath)
	t.Cleanup(func() { SetTestDBPath("") })
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	_ = d.UpsertStatus("myrepo@planted", "myrepo", "/wt", "finished", nil, nil)
	d.Close()

	readers := map[string]func() error{
		"runLogsFull": func() error { return runLogsFull(logPath) },
		"runLogsTail": func() error { return runLogsTail(logPath, 5) },
		"runLogsFollow": func() error {
			return runLogsFollow("myrepo@planted", logPath)
		},
		"readLogForDelivery": func() error {
			b, err := readLogForDelivery(logPath, false, 0)
			if strings.Contains(string(b), secret) {
				t.Errorf("readLogForDelivery returned the target content")
			}
			return err
		},
	}
	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			var err error
			out := captureStdoutFn(t, func() { err = read() })
			if !errors.Is(err, syscall.ELOOP) {
				t.Errorf("%s: %v, want an error that wraps ELOOP", name, err)
			}
			if strings.Contains(out, secret) {
				t.Errorf("%s printed the symlink target: %q", name, out)
			}
		})
	}
}

// TestLogsReaders_ReadPlainLog: the same readers still read a plain log.
func TestLogsReaders_ReadPlainLog(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	logFile := writeTempLogFile(t, "line\n")
	if out := captureStdoutFn(t, func() {
		if err := runLogsFull(logFile); err != nil {
			t.Errorf("runLogsFull: %v", err)
		}
	}); out != "line\n" {
		t.Errorf("runLogsFull printed %q, want %q", out, "line\n")
	}
	if b, err := readLogForDelivery(logFile, false, 0); err != nil || string(b) != "line\n" {
		t.Errorf("readLogForDelivery: %q, %v", b, err)
	}
}
