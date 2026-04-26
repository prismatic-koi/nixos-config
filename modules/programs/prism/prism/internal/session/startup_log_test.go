package session

// Tests for the per-agent startup log helper (#1051 Piece B). These verify
// the path resolution, the existence helper used by `prism logs`, and the
// best-effort tolerance to a nil receiver / unwritable directory.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentStartupLogPath_DefaultsToXDGState(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	got, err := AgentStartupLogPath("myrepo@feat")
	if err != nil {
		t.Fatalf("AgentStartupLogPath: %v", err)
	}
	// The per-session subdirectory uses the SessionDirName-derived 12-hex
	// SHA-256 prefix so the startup log is co-located with hostapi.sock and
	// agent-run.log (see #1066).
	want := filepath.Join(tmp, "prism", "run", SessionDirName("myrepo@feat"), "agent-startup.log")
	if got != want {
		t.Errorf("AgentStartupLogPath = %q, want %q", got, want)
	}
}

// TestAgentStartupLogPath_LivesUnderRunDir verifies that the startup log
// and the agent-run log both live somewhere under $XDG_STATE_HOME/prism/run/.
// They are not in the same leaf directory — agent-startup.log uses the raw
// session name (so a `prism logs <name> --startup` lookup is direct), while
// agent-run.log uses a SessionDirName-hashed prefix so the per-session
// directory stays under sun_path limits for the host-API socket (#1050).
// Aligning the two layouts is tracked separately; this test pins the
// current-and-intended shape so a future refactor must update both call
// sites coherently.
func TestAgentStartupLogPath_LivesUnderRunDir(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	startupPath, err := AgentStartupLogPath("myrepo@feat")
	if err != nil {
		t.Fatalf("AgentStartupLogPath: %v", err)
	}
	runPath, err := AgentRunLogPath("myrepo@feat")
	if err != nil {
		t.Fatalf("AgentRunLogPath: %v", err)
	}
	wantPrefix := filepath.Join(xdg, "prism", "run") + string(filepath.Separator)
	if !strings.HasPrefix(startupPath, wantPrefix) {
		t.Errorf("AgentStartupLogPath = %q, want prefix %q", startupPath, wantPrefix)
	}
	if !strings.HasPrefix(runPath, wantPrefix) {
		t.Errorf("AgentRunLogPath = %q, want prefix %q", runPath, wantPrefix)
	}
}

// TestAgentStartupLogExists_FalseWhenMissing verifies the existence helper
// returns false for sessions that have never been spawned.
func TestAgentStartupLogExists_FalseWhenMissing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if AgentStartupLogExists("myrepo@never-spawned") {
		t.Error("AgentStartupLogExists returned true for non-existent session")
	}
}

// TestOpenStartupLog_CreatesFileAndDirectory verifies the happy path: the
// directory is created if missing, the file is opened in append mode, and
// .log() writes a timestamped line.
func TestOpenStartupLog_CreatesFileAndDirectory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const sess = "myrepo@open-startup"

	logger := openStartupLog(sess)
	if logger == nil {
		t.Fatal("openStartupLog returned nil — expected a usable logger")
	}
	logger.log("hello %s", "world")
	logger.close()

	// The existence helper must now report true.
	if !AgentStartupLogExists(sess) {
		t.Error("AgentStartupLogExists returned false after openStartupLog + .log + .close")
	}

	// Read the file and verify the line was written.
	path, _ := AgentStartupLogPath(sess)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "hello world") {
		t.Errorf("startup log missing 'hello world': %q", string(data))
	}
	if !strings.Contains(string(data), "[startup]") {
		t.Errorf("startup log missing '[startup]' prefix: %q", string(data))
	}
}

// TestOpenStartupLog_NilReceiverIsNoop verifies that .log() and .close() on
// a nil *startupLogger do not panic. This matters for the SpawnSession path:
// when the log cannot be opened (unwritable XDG_STATE_HOME), openStartupLog
// returns nil and SpawnSession must continue to work without checking.
func TestStartupLogger_NilReceiverIsNoop(t *testing.T) {
	var l *startupLogger
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil receiver method panicked: %v", r)
		}
	}()
	l.log("anything")
	l.close()
}

// TestOpenStartupLog_AppendsAcrossOpens verifies that re-opening the same
// session's startup log preserves earlier content (O_APPEND). This matters
// for the rare case where SpawnSession is re-invoked with the same session
// name (e.g. ForceFresh path) — earlier breadcrumbs remain.
func TestOpenStartupLog_AppendsAcrossOpens(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const sess = "myrepo@append"

	l1 := openStartupLog(sess)
	if l1 == nil {
		t.Fatal("openStartupLog (1) returned nil")
	}
	l1.log("first")
	l1.close()

	l2 := openStartupLog(sess)
	if l2 == nil {
		t.Fatal("openStartupLog (2) returned nil")
	}
	l2.log("second")
	l2.close()

	path, _ := AgentStartupLogPath(sess)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "first") || !strings.Contains(string(data), "second") {
		t.Errorf("expected both 'first' and 'second' in log; got %q", string(data))
	}
}
