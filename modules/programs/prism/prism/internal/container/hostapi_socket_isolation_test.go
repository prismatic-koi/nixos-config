package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bwrapMountFlags are the bwrap options that take a host path as the source
// of a mount.
var bwrapMountFlags = []string{"--bind", "--ro-bind", "--dev-bind", "--bind-try", "--ro-bind-try", "--dev-bind-try"}

// isAncestorOrSelf reports whether dir is p or an ancestor dir of p.
func isAncestorOrSelf(dir, p string) bool {
	dir, p = filepath.Clean(dir), filepath.Clean(p)
	return dir == p || dir == "/" || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// TestBwrapBuildArgs_OtherSessionRunDirNotMounted is the Linux half of
// #3379. A session can put a symlink to the hostapi.sock of another session
// at its own hostapi.sock, and a connect follows the symlink. In bwrap the
// symlink then points at a path that does not exist in the sandbox: no mount
// source of the production argv is the run dir of another session or a dir
// above it. The sandbox gets its own run dir only.
func TestBwrapBuildArgs_OtherSessionRunDirNotMounted(t *testing.T) {
	m, fakeHome, cleanup := bwrapPIFixture(t)
	defer cleanup()

	// The production layout: the state dir and the project are under the
	// home dir, so a mount of the home dir or of the state dir fails here.
	state := filepath.Join(fakeHome, ".local", "state")
	t.Setenv("XDG_STATE_HOME", state)
	runRoot := filepath.Join(state, "prism", "run")
	ownRunDir := filepath.Join(runRoot, "0123456789ab")
	otherRunDir := filepath.Join(runRoot, "ba9876543210")
	bare := filepath.Join(fakeHome, "code", "repo")
	worktree := filepath.Join(bare, "feature")
	for _, d := range []string{ownRunDir, otherRunDir, filepath.Join(bare, ".bare"), worktree} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m.cfg.Worktree = worktree
	m.cfg.BareRoot = bare
	m.cfg.HostAPISockPath = filepath.Join(ownRunDir, "hostapi.sock")

	args := (&bwrapIsolator{name: m.name}).BuildArgs(m)
	if m.piBwrapErr != nil {
		t.Fatalf("BuildArgs pi mounts: %v", m.piBwrapErr)
	}
	if !hasBind(args, ownRunDir) {
		t.Fatalf("the own run dir %s is not bound, so the check below proves nothing: %v", ownRunDir, redactedArgs(args))
	}
	for _, flag := range bwrapMountFlags {
		for _, tri := range findTriples(args, flag) {
			src := tri[0]
			resolved := src
			if r, err := filepath.EvalSymlinks(src); err == nil {
				resolved = r
			}
			if isAncestorOrSelf(src, otherRunDir) || isAncestorOrSelf(resolved, otherRunDir) {
				t.Errorf("%s %s %s exposes the run dir of another session (%s) in the sandbox", flag, src, tri[1], otherRunDir)
			}
		}
	}
}

// TestIsAncestorOrSelf pins the helper, so the test above cannot pass by a
// helper that matches nothing.
func TestIsAncestorOrSelf(t *testing.T) {
	other := "/home/u/.local/state/prism/run/b"
	for _, dir := range []string{"/", "/home", "/home/u/.local/state", "/home/u/.local/state/prism/run", other, other + "/"} {
		if !isAncestorOrSelf(dir, other) {
			t.Errorf("isAncestorOrSelf(%q, %q) = false, want true", dir, other)
		}
	}
	for _, dir := range []string{"/home/u/.local/state/prism/run/a", "/home/u/.local/state/prism/usage", "/home/u/.local/state/prism/run/bb"} {
		if isAncestorOrSelf(dir, other) {
			t.Errorf("isAncestorOrSelf(%q, %q) = true, want false", dir, other)
		}
	}
}
