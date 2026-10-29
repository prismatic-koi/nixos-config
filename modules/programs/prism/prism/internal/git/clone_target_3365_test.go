package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cloneTargetFixture gives a project location and an origin with one commit
// on main.
func cloneTargetFixture(t *testing.T) (code, origin string) {
	t.Helper()
	isolateHostGitConfig(t)
	code = filepath.Join(t.TempDir(), "code")
	if err := os.MkdirAll(code, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, []string{code}, nil)
	return code, newOrigin(t)
}

// failAtTracking returns a progress func that deletes refs/heads/main in the
// .bare of target when the clone starts the upstream step. The step then
// fails, after the bare clone.
func failAtTracking(t *testing.T, target string, extra func()) func(string) {
	t.Helper()
	return func(msg string) {
		if !strings.HasPrefix(msg, "setting up tracking") {
			return
		}
		bare := filepath.Join(target, ".bare")
		if out, err := exec.Command("git", "--git-dir", bare, "update-ref", "-d", "refs/heads/main").CombinedOutput(); err != nil {
			t.Errorf("update-ref -d: %v\n%s", err, out)
		}
		if extra != nil {
			extra()
		}
	}
}

func assertGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("the failed clone left %s: %v", p, err)
		}
	}
}

// TestCloneWorktree_RefusesNonEmptyTarget: a target that exists and is not
// empty is refused before the clone starts, and nothing in it changes
// (issue #3365).
func TestCloneWorktree_RefusesNonEmptyTarget(t *testing.T) {
	code, origin := cloneTargetFixture(t)
	target := filepath.Join(code, "repo")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(target, "keep")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var steps []string
	err := CloneWorktree(origin, target, func(msg string) { steps = append(steps, msg) })
	if !errors.Is(err, ErrCloneTargetNotEmpty) {
		t.Fatalf("CloneWorktree(%s) = %v, want ErrCloneTargetNotEmpty", target, err)
	}
	if !strings.Contains(err.Error(), target) {
		t.Errorf("error %q does not name the target %s", err, target)
	}
	if len(steps) != 0 {
		t.Errorf("CloneWorktree started the clone: %q", steps)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "x" {
		t.Errorf("the refused clone changed %s: %q, %v", keep, data, err)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the refused clone changed %s: %d entries, want 1", target, len(entries))
	}
}

// TestCloneWorktree_FailureKeepsFilesItDidNotMake: a failure after the bare
// clone removes the paths that the clone made, and keeps a file that another
// process put in the target during the clone (issue #3365).
func TestCloneWorktree_FailureKeepsFilesItDidNotMake(t *testing.T) {
	code, origin := cloneTargetFixture(t)
	target := filepath.Join(code, "repo")
	user := filepath.Join(target, "user-file")

	err := CloneWorktree(origin, target, failAtTracking(t, target, func() {
		if err := os.WriteFile(user, []byte("mine"), 0o600); err != nil {
			t.Errorf("write %s: %v", user, err)
		}
	}))
	if err == nil {
		t.Fatal("CloneWorktree succeeded; the injected failure did not work")
	}
	if data, err := os.ReadFile(user); err != nil || string(data) != "mine" {
		t.Errorf("the failed clone removed or changed %s: %q, %v", user, data, err)
	}
	assertGone(t, filepath.Join(target, ".bare"), filepath.Join(target, ".git"))
}

// TestCloneWorktree_FailureInEmptyTargetKeepsTarget: a failure after the bare
// clone into an empty dir that was there removes what the clone made and
// keeps the dir.
func TestCloneWorktree_FailureInEmptyTargetKeepsTarget(t *testing.T) {
	code, origin := cloneTargetFixture(t)
	target := filepath.Join(code, "repo")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := CloneWorktree(origin, target, failAtTracking(t, target, nil)); err == nil {
		t.Fatal("CloneWorktree succeeded; the injected failure did not work")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("the failed clone removed %s, a dir that was there: %v", target, err)
	}
	if len(entries) != 0 {
		t.Errorf("the failed clone left %d entries in %s", len(entries), target)
	}
}

// TestCloneWorktree_FailureInNewTargetRemovesTarget: a failure after the bare
// clone into a new target removes the target and the parents it made.
func TestCloneWorktree_FailureInNewTargetRemovesTarget(t *testing.T) {
	code, origin := cloneTargetFixture(t)
	target := filepath.Join(code, "repo")

	if err := CloneWorktree(origin, target, failAtTracking(t, target, nil)); err == nil {
		t.Fatal("CloneWorktree succeeded; the injected failure did not work")
	}
	assertGone(t, target)
	if _, err := os.Stat(code); err != nil {
		t.Errorf("the failed clone removed %s: %v", code, err)
	}
}

// TestCloneWorktree_EmptyExistingTarget: a clone into an empty dir that is
// there works as before.
func TestCloneWorktree_EmptyExistingTarget(t *testing.T) {
	code, origin := cloneTargetFixture(t)
	target := filepath.Join(code, "repo")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CloneWorktree(origin, target, func(string) {}); err != nil {
		t.Fatalf("CloneWorktree(%s): %v", target, err)
	}
	for _, p := range []string{".bare", ".git", "main"} {
		if _, err := os.Stat(filepath.Join(target, p)); err != nil {
			t.Errorf("clone did not make %s: %v", p, err)
		}
	}
}
