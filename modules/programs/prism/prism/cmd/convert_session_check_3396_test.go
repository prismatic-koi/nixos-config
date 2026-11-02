package cmd

// Tests for #3396: prism convert refuses when it cannot tell if a session
// is in the clone, refuses a plain clone in a bare+worktree layout, and
// finds a tmux session in the clone in a locale that is not UTF-8.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// withConvertCheckTmux redirects tmux.TmuxBin to a spy that logs each call
// (tmuxCalls). Its list-sessions runs listing, a shell command. new-session
// fails, so a start that gets past the checks of session.Create starts
// nothing. Each other command exits 0. It returns the log file.
func withConvertCheckTmux(t *testing.T, listing string) string {
	t.Helper()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "tmux-args")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> " + logFile + "; done\n" +
		"printf '\\n' >> " + logFile + "\n" +
		"if [ \"$1\" = -u ]; then shift; fi\n" +
		"case \"$1\" in\n" +
		"  list-sessions) " + listing + " ;;\n" +
		"  has-session) exit 1 ;;\n" +
		"  new-session) echo 'stub tmux: new-session refused' >&2; exit 1 ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write spy tmux: %v", err)
	}
	prev := tmux.TmuxBin
	tmux.TmuxBin = filepath.Join(dir, "tmux")
	t.Cleanup(func() { tmux.TmuxBin = prev })
	return logFile
}

// noTmuxServerListing is a listing of withConvertCheckTmux: no server runs.
const noTmuxServerListing = "echo 'no server running on /stub/default' >&2; exit 1"

// withRecordedConvertStops replaces the sidecar stop of the conversion with
// a fake that records the session, and returns the sessions in order.
func withRecordedConvertStops(t *testing.T) *[]string {
	t.Helper()
	var stops []string
	prev := switchProjectStopSidecar
	switchProjectStopSidecar = func(name string) error {
		stops = append(stops, name)
		return nil
	}
	t.Cleanup(func() { switchProjectStopSidecar = prev })
	return &stops
}

// assertCloneUnchanged fails t unless dir is still a plain clone with no
// .bare, and its conversion lock is free.
func assertCloneUnchanged(t *testing.T, dir string) {
	t.Helper()
	if info, err := os.Lstat(filepath.Join(dir, ".git")); err != nil || !info.IsDir() {
		t.Errorf("%s/.git: %v, want the dir of a plain clone", dir, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".bare")); !os.IsNotExist(err) {
		t.Errorf("%s/.bare: %v, want none", dir, err)
	}
	lock, err := session.LockForConvert(dir, 0)
	if err != nil {
		t.Errorf("the refused conversion kept the lock of %s: %v", dir, err)
		return
	}
	lock.Release()
}

// AC (#3396): when prism cannot open the DB, read the rows, or ask tmux,
// or when a line of the tmux output does not parse, prism convert refuses.
// The error names the dir and the cause. The conversion stops no session
// and changes nothing.
func TestConvertPlainClone_RefusesWhenTheSessionCheckFails_3396(t *testing.T) {
	cases := []struct {
		name    string
		listing string
		noDB    bool
		cause   string
	}{
		{
			name:    "tmux gives no answer",
			listing: "echo 'error connecting to /stub/default (Permission denied)' >&2; exit 1",
			cause:   "Permission denied",
		},
		{
			name:    "a line of the tmux output does not parse",
			listing: "printf 'cv3396other_/elsewhere\\n'",
			cause:   "cv3396other_/elsewhere",
		},
		{
			name:    "the DB does not open",
			listing: noTmuxServerListing,
			noDB:    true,
			cause:   "open the DB",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, name := newPlainCloneDir(t, "cv3396check")
			seedHookRows(t, name)
			if c.noDB {
				notADir := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(notADir, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				SetTestDBPath(filepath.Join(notADir, "prism.db"))
			}
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			tmuxLog := withConvertCheckTmux(t, c.listing)
			installFakeRunner(t)
			withChildCleanup(t)
			stops := withRecordedConvertStops(t)
			calls := withConvertToBare(t, func(string, func(string)) (string, error) { return "", nil })
			realDir, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}

			var res convertResult
			captureStderrDuringFn(t, func() { res, err = convertPlainClone(dir, func(string) {}) })

			if err == nil || !strings.Contains(err.Error(), "convert "+realDir) || !strings.Contains(err.Error(), "cannot tell") || !strings.Contains(err.Error(), c.cause) {
				t.Fatalf("convertPlainClone = %v, want a refusal that names the dir and the cause %q", err, c.cause)
			}
			if *calls != 0 || len(*stops) != 0 || tmuxKilled(t, tmuxLog, name) || res.ran {
				t.Errorf("conversions = %d, stops = %v, tmux kill = %v, ran = %v; want none", *calls, *stops, tmuxKilled(t, tmuxLog, name), res.ran)
			}
			assertCloneUnchanged(t, dir)
		})
	}
}

// AC (#3396): prism convert refuses a plain clone in a bare+worktree
// layout, by its path and through a symlink outside the layout. The error
// names the dir and the layout. The control shows that a start in such a
// clone takes no start lock: session.Create does not refuse it while the
// conversion lock is held.
func TestConvertPlainClone_RefusesCloneInABareLayout_3396(t *testing.T) {
	outer := filepath.Join(t.TempDir(), "outer")
	nested := filepath.Join(outer, "main", "nested")
	for _, d := range []string{filepath.Join(outer, ".bare"), filepath.Join(nested, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(nested, link); err != nil {
		t.Fatal(err)
	}
	name := mustNameFor(t, nested, "")
	seedHookRows(t, name)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	tmuxLog := withConvertCheckTmux(t, noTmuxServerListing)
	installFakeRunner(t)
	withChildCleanup(t)
	stops := withRecordedConvertStops(t)
	calls := withConvertToBare(t, func(string, func(string)) (string, error) { return "", nil })
	realNested, err := filepath.EvalSymlinks(nested)
	if err != nil {
		t.Fatal(err)
	}
	realOuter, err := filepath.EvalSymlinks(outer)
	if err != nil {
		t.Fatal(err)
	}

	lock, err := session.LockForConvert(nested, 0)
	if err != nil {
		t.Fatalf("LockForConvert: %v", err)
	}
	opts := session.Opts{Layout: session.LayoutFull, IsolationMode: "host", HarnessName: "pi", PIExtensionDir: "/test/prism-pi-extension", SessionName: name}
	err = session.Create(name, nested, opts)
	lock.Release()
	if errors.Is(err, session.ErrConvertInProgress) || err == nil || !strings.Contains(err.Error(), "new-session") {
		t.Fatalf("control: session.Create with the conversion lock held = %v, want a start that reaches tmux new-session", err)
	}

	for _, p := range []string{nested, link} {
		_, err := convertPlainClone(p, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "convert "+realNested) || !strings.Contains(err.Error(), "bare+worktree layout of "+realOuter) {
			t.Errorf("convertPlainClone(%s) = %v, want a refusal that names the dir and the layout", p, err)
		}
	}
	if *calls != 0 || len(*stops) != 0 || tmuxKilled(t, tmuxLog, name) {
		t.Errorf("conversions = %d, stops = %v, tmux kill = %v; want none", *calls, *stops, tmuxKilled(t, tmuxLog, name))
	}
	assertCloneUnchanged(t, nested)
}

// setCLocale sets LC_ALL=C, with no TMUX, LANG, or LC_CTYPE, for the rest
// of t. A tmux client with TMUX set, or in a UTF-8 locale, writes UTF-8
// output.
func setCLocale(t *testing.T) {
	t.Helper()
	for _, k := range []string{"TMUX", "LANG", "LC_CTYPE"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LC_ALL", "C")
}

// AC (#3396): with LC_ALL=C, prism convert finds a tmux session in the
// clone that has another name than the root session, and stops it. The
// control shows that tmux with no -u writes no tab in this locale.
func TestConvertPlainClone_FindsTmuxSessionInCLocale_3396(t *testing.T) {
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	dir, name := newPlainCloneDir(t, "cv3396locale")
	const alias = "cv3396alias"
	seedHookRows(t, name)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	pidPath, err := session.SidecarPIDPath(alias)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.run("new-session", "-ds", alias, "-c", dir, "sleep 600"); err != nil {
		t.Fatalf("start the session in the clone: %v", err)
	}
	setCLocale(t)
	out, err := exec.Command(s.bin, "-L", s.socket, "-f", "/dev/null", "list-sessions", "-F", "#{session_name}\t#{session_path}").Output()
	if err != nil || strings.Contains(string(out), "\t") || !strings.Contains(string(out), alias+"_") {
		t.Fatalf("control: tmux with no -u and LC_ALL=C wrote %q, %v; want no tab", out, err)
	}

	installFakeRunner(t)
	withChildCleanup(t)
	stops := withRecordedConvertStops(t)
	prevProcs := stopSessionSandboxProcs
	stopSessionSandboxProcs = func(string) error { return nil }
	t.Cleanup(func() { stopSessionSandboxProcs = prevProcs })
	calls := withConvertToBare(t, func(d string, _ func(string)) (string, error) { return filepath.Join(d, "main"), nil })

	var convErr error
	captureStderrDuringFn(t, func() { _, convErr = convertPlainClone(dir, func(string) {}) })
	if convErr != nil {
		t.Fatalf("convertPlainClone: %v", convErr)
	}
	if !slices.Equal(*stops, []string{name, alias}) || *calls != 1 {
		t.Errorf("stops = %v, conversions = %d; want [%s %s] and one conversion", *stops, *calls, name, alias)
	}
	if s.hasSession(alias) {
		t.Errorf("the tmux session %s still runs in the converted clone", alias)
	}
}
