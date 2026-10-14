// Package stateguardtest keeps a test suite away from the real prism state of
// the host.
//
// In a prism session with `host` isolation, the test process inherits the
// real XDG_STATE_HOME, XDG_CONFIG_HOME, and TMPDIR of the user. Without
// isolation, code under test then reads and writes the real
// ~/.local/state/prism and ~/.config/prism (issue #3146).
package stateguardtest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ShortTempDir returns a new directory with a short path and removes it at
// cleanup. A Unix socket path must fit in sun_path (104 bytes on Darwin).
// On macOS, os.TempDir() is a long /var/folders/... path in a host shell, so
// the directory goes under /tmp first. If /tmp is not writable, it goes under
// os.TempDir().
func ShortTempDir(t testing.TB, pattern string) string {
	t.Helper()
	dir, err := mkdirShort(pattern)
	if err != nil {
		t.Fatalf("stateguardtest: create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func mkdirShort(pattern string) (string, error) {
	dir, err := os.MkdirTemp("/tmp", pattern)
	if err != nil {
		dir, err = os.MkdirTemp("", pattern)
	}
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(dir)
}

// SuiteEnv names each variable that IsolateSuite sets.
var SuiteEnv = []string{"XDG_STATE_HOME", "XDG_CONFIG_HOME", "SHELL"}

// IsolateSuite is for TestMain, before m.Run. It points XDG_STATE_HOME and
// XDG_CONFIG_HOME at a new directory that the suite owns, and sets SHELL to
// /bin/sh. It returns the directory and a function that restores the
// environment and removes the directory.
//
// Every prism state and config path reads XDG_STATE_HOME or XDG_CONFIG_HOME
// first, so no code under test reaches ~/.local/state/prism or
// ~/.config/prism. A test that needs another value sets it with t.Setenv.
//
// A tmux server takes its default-shell from SHELL. With /bin/sh, a pane
// command does not read the zsh startup files of the user, which can set
// XDG_STATE_HOME back to the real path.
//
// HOME is not changed: the nix build sandbox sets HOME to an unwritable
// path, and that catches a test that writes under HOME.
func IsolateSuite() (root string, restore func()) {
	root, err := mkdirShort("prism-suite-" + strings.TrimSuffix(filepath.Base(os.Args[0]), ".test") + "-")
	if err != nil {
		panic(fmt.Sprintf("stateguardtest: create suite dir: %v", err))
	}
	type saved struct {
		val string
		had bool
	}
	origs := map[string]saved{}
	for _, k := range SuiteEnv {
		v, ok := os.LookupEnv(k)
		origs[k] = saved{v, ok}
	}
	for _, sub := range []string{"state", "config"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o700); err != nil {
			panic(fmt.Sprintf("stateguardtest: create suite dir: %v", err))
		}
	}
	os.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	os.Setenv("SHELL", "/bin/sh")
	return root, func() {
		for k, s := range origs {
			if s.had {
				os.Setenv(k, s.val)
			} else {
				os.Unsetenv(k)
			}
		}
		_ = os.RemoveAll(root)
	}
}

// RequireIsolated fails t when an XDG variable that IsolateSuite sets does
// not point inside root. A package calls it from a test, to detect a TestMain
// that lost its IsolateSuite call.
func RequireIsolated(t testing.TB, root string) {
	t.Helper()
	if root == "" {
		t.Fatal("stateguardtest: TestMain did not call IsolateSuite")
	}
	for _, k := range []string{"XDG_STATE_HOME", "XDG_CONFIG_HOME"} {
		v := os.Getenv(k)
		rel, err := filepath.Rel(root, v)
		if v == "" || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("%s=%q is outside the suite dir %q: the suite can reach the real prism state", k, v, root)
		}
	}
}
