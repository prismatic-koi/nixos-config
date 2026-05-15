package iris

// supervisor_logfile_test.go — tests for the per-session log file writer
// (issue #1675).
//
// Covers:
//   - openSessionLogFile creates the parent directory and opens with append.
//   - openSessionLogFile is a no-op when logDir is empty.
//   - newSessionLogger writes through to the per-session file when provided
//     and tees to stderr (we verify the file half here; stderr is exercised
//     transitively by every test that calls log.Printf).
//   - Re-opening for the same session appends rather than truncates.
//   - sanitiseSessionFileName replaces path separators.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenSessionLogFile_CreatesDirAndFile(t *testing.T) {
	tmp := t.TempDir()
	logDir := filepath.Join(tmp, "logs")
	// Parent doesn't exist yet — openSessionLogFile must MkdirAll it.

	f, err := openSessionLogFile(logDir, "iris-test@branch")
	if err != nil {
		t.Fatalf("openSessionLogFile: %v", err)
	}
	if f == nil {
		t.Fatal("expected non-nil file, got nil")
	}
	defer f.Close()

	want := filepath.Join(logDir, "iris-test@branch.log")
	if f.Name() != want {
		t.Fatalf("file path mismatch:\n got:  %q\n want: %q", f.Name(), want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("stat %q: %v", want, err)
	}
}

func TestOpenSessionLogFile_EmptyDirIsNoop(t *testing.T) {
	f, err := openSessionLogFile("", "any@branch")
	if err != nil {
		t.Fatalf("openSessionLogFile: %v", err)
	}
	if f != nil {
		t.Fatalf("expected nil file when logDir is empty, got %v", f.Name())
	}
}

func TestOpenSessionLogFile_AppendsOnReopen(t *testing.T) {
	tmp := t.TempDir()

	f1, err := openSessionLogFile(tmp, "x@y")
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if _, err := f1.WriteString("first\n"); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	f1.Close()

	f2, err := openSessionLogFile(tmp, "x@y")
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	if _, err := f2.WriteString("second\n"); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	f2.Close()

	got, err := os.ReadFile(filepath.Join(tmp, "x@y.log"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "first\nsecond\n"
	if string(got) != want {
		t.Fatalf("file content mismatch:\n got:  %q\n want: %q", got, want)
	}
}

func TestNewSessionLogger_WritesToFile(t *testing.T) {
	tmp := t.TempDir()
	f, err := openSessionLogFile(tmp, "x@y")
	if err != nil {
		t.Fatalf("openSessionLogFile: %v", err)
	}
	defer f.Close()

	logger := newSessionLogger(f, "x@y")
	logger.Print("hello world")

	// Flush by syncing the file.
	_ = f.Sync()

	got, err := os.ReadFile(filepath.Join(tmp, "x@y.log"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(got), "hello world") {
		t.Fatalf("expected 'hello world' in log file, got %q", string(got))
	}
	// Prefix should self-identify the session for cross-log grep.
	if !strings.Contains(string(got), "[iris:x@y]") {
		t.Fatalf("expected [iris:x@y] prefix in log file, got %q", string(got))
	}
}

func TestSanitiseSessionFileName_ReplacesSeparators(t *testing.T) {
	cases := map[string]string{
		"plain@branch":      "plain@branch",
		"a/b":               "a_b",
		"with\\backslash":   "with_backslash",
		"nul\x00byte":       "nul_byte",
		"normal-name_only.": "normal-name_only.",
	}
	for in, want := range cases {
		got := sanitiseSessionFileName(in)
		if got != want {
			t.Errorf("sanitiseSessionFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathsSessionLogPath_UsesSanitiser(t *testing.T) {
	p := Paths{LogDir: "/log"}
	if got := p.SessionLogPath("a@b"); got != "/log/a@b.log" {
		t.Errorf("plain: got %q want /log/a@b.log", got)
	}
	if got := p.SessionLogPath("evil/../path"); got != "/log/evil_.._path.log" {
		t.Errorf("traversal: got %q", got)
	}
}
