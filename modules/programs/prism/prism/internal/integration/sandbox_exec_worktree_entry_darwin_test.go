//go:build darwin

package integration_test

// sandbox_exec_worktree_entry_darwin_test.go — the worktree path guard of
// the sandbox-exec profile (section 21b of generateProfile).
//
// The profile grants (subpath BareRoot) read-write, and the worktree is a
// child of BareRoot. Without the guard, a sandboxed process can rename the
// worktree, or BareRoot itself, away and put a symlink at its path. `prism
// container run --mount` then gives the worktree path to podman on the
// host, and podman mounts the symlink target.
//
// The positive test runs both swaps under the production profile and
// expects them to fail, while a write inside the worktree still works. The
// negative tests remove the guard and expect each swap to succeed, which
// proves that the guard is what stops it.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// newWorktreeEntryManager builds a manager whose worktree is a direct child
// of BareRoot, the production layout. BareRoot sits two levels under HOME
// so that the BareRoot-ancestor block of the profile fires.
func newWorktreeEntryManager(t *testing.T) (*container.Manager, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	wrap, err := os.MkdirTemp(home, ".prism-3061-worktree-entry-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wrap) })
	bareRoot := filepath.Join(wrap, "repo")
	worktree := filepath.Join(bareRoot, "feature")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("MkdirAll(worktree): %v", err)
	}
	return container.New(container.Config{
		SessionName:  "integ-worktree-entry",
		InstanceID:   "integ-sbx-" + strings.ReplaceAll(t.Name(), "/", "-"),
		Worktree:     worktree,
		BareRoot:     bareRoot,
		GitUserName:  "test-user",
		GitUserEmail: "test@example.com",
	}), worktree
}

// requireNixCoreutilsDir returns the /nix/store bin directory of mv. The
// Apple-signed /bin tools abort under the test profile shape.
func requireNixCoreutilsDir(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("mv")
	if err != nil {
		t.Skipf("mv not found in PATH: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Skipf("EvalSymlinks(%q): %v", p, err)
	}
	if !strings.HasPrefix(resolved, "/nix/store/") {
		t.Skipf("mv resolves to %q, not a /nix/store path", resolved)
	}
	return filepath.Dir(resolved)
}

// runWorktreeScript runs script under the profile with WT set to the
// worktree, BARE to its parent (BareRoot), and DEST to a directory that the
// profile grants for write (under TMPDIR, section 3b).
func runWorktreeScript(t *testing.T, profilePath, worktree, script string) ([]byte, error) {
	t.Helper()
	nixBash := requireNixBash(t)
	cmd := exec.Command(sandboxExecPath, "-f", profilePath, nixBash, "-c", script)
	cmd.Env = append(os.Environ(),
		"WT="+worktree,
		"BARE="+filepath.Dir(worktree),
		"DEST="+t.TempDir(),
		"PATH="+requireNixCoreutilsDir(t))
	return cmd.CombinedOutput()
}

// worktreeDenyClause matches the section-21b deny with all its literals.
var worktreeDenyClause = regexp.MustCompile(`(?s)\(deny file-write-unlink file-write-create\n.*?\)\)\n`)

func withoutWorktreeDeny(p string) string { return worktreeDenyClause.ReplaceAllString(p, "") }

// TestSandboxExecWorktreeEntry_CannotBeReplaced: under the production
// profile a write inside the worktree works, and neither the worktree nor
// BareRoot can be renamed or removed.
func TestSandboxExecWorktreeEntry_CannotBeReplaced(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	m, worktree := newWorktreeEntryManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	const script = `touch "$WT/probe" || exit 10
mv "$WT" "$WT.moved" && exit 11
mv "$BARE" "$DEST/saved" && exit 14
rm -f "$WT/probe" || exit 13
rmdir "$WT" && exit 12
exit 0`
	out, err := runWorktreeScript(t, profilePath, worktree, script)
	if err != nil {
		t.Fatalf("a worktree or BareRoot swap was not blocked (exit 11/12/14), or a write inside the worktree failed (exit 10/13).\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}
	info, statErr := os.Lstat(worktree)
	if statErr != nil || !info.IsDir() {
		t.Errorf("worktree is no longer a directory after the blocked swap: %v", statErr)
	}
}

// TestSandboxExecWorktreeEntry_ReplaceableWithoutDeny is the paired
// negative test: with the deny removed, the same sandbox replaces the
// worktree with a symlink.
func TestSandboxExecWorktreeEntry_ReplaceableWithoutDeny(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	m, worktree := newWorktreeEntryManager(t)
	profilePath := withMutatedProfile(t, m, withoutWorktreeDeny)

	out, err := runWorktreeScript(t, profilePath, worktree, `mv "$WT" "$WT.moved" && ln -s / "$WT"`)
	if err != nil {
		t.Fatalf("without the deny the swap still failed, so the positive test proves nothing.\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}
	if info, statErr := os.Lstat(worktree); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("without the deny the worktree path is not a symlink after the swap: %v", statErr)
	}
}

// TestSandboxExecWorktreeEntry_BareRootReplaceableWithoutDeny is the paired
// negative test for the ancestor case: with the deny removed, the sandbox
// moves BareRoot away and puts a symlink at its path.
func TestSandboxExecWorktreeEntry_BareRootReplaceableWithoutDeny(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	m, worktree := newWorktreeEntryManager(t)
	profilePath := withMutatedProfile(t, m, withoutWorktreeDeny)

	out, err := runWorktreeScript(t, profilePath, worktree, `mv "$BARE" "$DEST/saved" && ln -s / "$BARE"`)
	if err != nil {
		t.Fatalf("without the deny the BareRoot swap still failed, so the positive test proves nothing about BareRoot.\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}
	if info, statErr := os.Lstat(filepath.Dir(worktree)); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("without the deny the BareRoot path is not a symlink after the swap: %v", statErr)
	}
}
