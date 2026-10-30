package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runDirWriter is a host-side write of a file in the run dir of a session.
type runDirWriter struct {
	name string
	path func(sessionName string) (string, error)
	// write writes text to the file and reports whether it returned no error.
	write func(sessionName, text string) bool
}

var runDirWriters = []runDirWriter{
	{
		name: "openStartupLog",
		path: AgentStartupLogPath,
		write: func(sessionName, text string) bool {
			l := openStartupLog(sessionName)
			if l == nil {
				return false
			}
			l.log("%s", text)
			l.close()
			return true
		},
	},
	{
		name: "WriteInitialPrompt",
		path: InitialPromptPath,
		write: func(sessionName, text string) bool {
			_, err := WriteInitialPrompt(sessionName, text)
			return err == nil
		},
	},
	{
		name: "logHostNoModelWarning",
		path: AgentRunLogPath,
		write: func(sessionName, text string) bool {
			logHostNoModelWarning(Opts{SessionName: sessionName, Agent: text})
			return true
		},
	},
}

// plantTarget writes a file outside the run dir, as a file of the user, and
// returns its path.
func plantTarget(t *testing.T) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "zshenv")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	return target
}

func assertUnchanged(t *testing.T, name, target string) {
	t.Helper()
	if b, err := os.ReadFile(target); err != nil || string(b) != "keep" {
		t.Errorf("%s changed the symlink target: it holds %q (%v), want %q", name, b, err, "keep")
	}
}

// TestRunDirWriters_WriteOwnFile: each writer writes its file in the run dir
// as before.
func TestRunDirWriters_WriteOwnFile(t *testing.T) {
	for _, w := range runDirWriters {
		t.Run(w.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const sess = "repo@run-dir-own"
			if !w.write(sess, "own-text") {
				t.Fatalf("%s failed on a plain run dir", w.name)
			}
			path, _ := w.path(sess)
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
				t.Fatalf("%s made %v (%v), want a regular file with mode 0600", w.name, info, err)
			}
			if b, _ := os.ReadFile(path); !strings.Contains(string(b), "own-text") {
				t.Errorf("%s wrote %q, want it to hold own-text", w.name, b)
			}
		})
	}
}

// TestRunDirWriters_DoNotFollowSymlinkedFile: the sandbox can replace a file
// in its run dir with a symlink, for example to a startup file of the shell.
// No writer writes or truncates the target.
func TestRunDirWriters_DoNotFollowSymlinkedFile(t *testing.T) {
	for _, w := range runDirWriters {
		t.Run(w.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const sess = "repo@run-dir-link"
			path, _ := w.path(sess)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			target := plantTarget(t)
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			w.write(sess, "planted-text")
			assertUnchanged(t, w.name, target)
		})
	}
}

// TestRunDirWriters_RefuseSymlinkedRunDir: no writer writes in a run dir that
// is a symlink to the run dir of another session.
func TestRunDirWriters_RefuseSymlinkedRunDir(t *testing.T) {
	for _, w := range runDirWriters {
		t.Run(w.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const sess = "repo@run-dir-swap"
			path, _ := w.path(sess)
			runDir := filepath.Dir(path)
			other := filepath.Join(filepath.Dir(runDir), "other")
			if err := os.MkdirAll(other, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, runDir); err != nil {
				t.Fatal(err)
			}
			if w.write(sess, "swapped-text") && w.name != "logHostNoModelWarning" {
				t.Errorf("%s returned success in a symlinked run dir", w.name)
			}
			if entries, _ := os.ReadDir(other); len(entries) != 0 {
				t.Errorf("%s wrote %d entries in the run dir of the other session", w.name, len(entries))
			}
		})
	}
}
