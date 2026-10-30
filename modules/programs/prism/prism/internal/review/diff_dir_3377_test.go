package review_test

// diff_dir_3377_test.go — issue #3377. The diff file of a large diff goes to
// the diff dir of the parent session in the state dir, never into the
// worktree. The host write refuses a symlinked dir and does not follow a
// symlinked file. Parent cleanup removes the dir.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/review"
)

const (
	diffParentA = "prism-test@3377-a"
	diffParentB = "prism-test@3377-b"
)

// largeDiff is above the inline limits that the tests pass (10 bytes, 1
// line).
const largeDiff = "diff --git a/x b/x\n+line one\n+line two\n"

func diffDirOf(t *testing.T, parent string) string {
	t.Helper()
	dir, err := review.DiffDirForParent(parent)
	if err != nil {
		t.Fatalf("DiffDirForParent(%q): %v", parent, err)
	}
	return dir
}

func TestDiffDirForParent_InStateDirPerSession_3377(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	a, b := diffDirOf(t, diffParentA), diffDirOf(t, diffParentB)
	if a == b {
		t.Fatalf("two parents share the diff dir %s", a)
	}
	base := filepath.Join(state, "prism", "review-diff")
	for _, d := range []string{a, b} {
		if filepath.Dir(d) != base {
			t.Errorf("diff dir %s is not directly in %s", d, base)
		}
	}
	if got, err := review.DiffStateDir(); err != nil || got != base {
		t.Errorf("DiffStateDir() = %q, %v; want %q", got, err, base)
	}
}

// TestDiffStateDir_RelativeStateHomeRefused_3377: a relative XDG_STATE_HOME
// resolves against the cwd of `prism review`, the worktree, so it is an
// error, and a large diff is then inlined.
func TestDiffStateDir_RelativeStateHomeRefused_3377(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/state")
	if dir, err := review.DiffStateDir(); err == nil {
		t.Errorf("DiffStateDir() = %q with a relative XDG_STATE_HOME, want an error", dir)
	}
	if dir, err := review.DiffDirForParent(diffParentA); err == nil {
		t.Errorf("DiffDirForParent() = %q with a relative XDG_STATE_HOME, want an error", dir)
	}
}

func TestPlaceDiff_SmallDiffInlinedNoFile_3377(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "review-diff", "abc")
	got := review.PlaceDiffForTest(stateDir, "7", 1, "+x\n", 1024, 100)
	if got.Diff != "+x\n" || got.DiffFilePath != "" {
		t.Errorf("small diff: Diff=%q DiffFilePath=%q, want the diff inline and no file", got.Diff, got.DiffFilePath)
	}
	if _, err := os.Lstat(stateDir); !os.IsNotExist(err) {
		t.Errorf("a small diff made the diff dir %s (err=%v)", stateDir, err)
	}
}

func TestPlaceDiff_LargeDiffWritesFile_3377(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "review-diff", "abc")
	got := review.PlaceDiffForTest(stateDir, "7", 2, largeDiff, 10, 1)
	if got.Diff != "" {
		t.Errorf("large diff is inline: %q", got.Diff)
	}
	if want := filepath.Join(stateDir, "pr-7-round-2.diff"); got.DiffFilePath != want {
		t.Fatalf("DiffFilePath = %q, want %q", got.DiffFilePath, want)
	}
	data, err := os.ReadFile(got.DiffFilePath)
	if err != nil || string(data) != largeDiff {
		t.Errorf("diff file holds %q (err=%v), want the diff", data, err)
	}
	info, err := os.Lstat(got.DiffFilePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Errorf("diff file: %v %v, want a regular file with mode 0600", info, err)
	}
}

// TestPlaceDiff_SymlinkedDirRefused_3377: a diff dir that is a symlink is
// refused. No file goes to the symlink target, and the diff is inlined.
func TestPlaceDiff_SymlinkedDirRefused_3377(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "review-diff", "abc")
	if err := os.MkdirAll(filepath.Dir(stateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, stateDir); err != nil {
		t.Fatal(err)
	}

	got := review.PlaceDiffForTest(stateDir, "7", 1, largeDiff, 10, 1)

	if got.DiffFilePath != "" || got.Diff != largeDiff {
		t.Errorf("symlinked dir: DiffFilePath=%q Diff=%q, want no file and the diff inline", got.DiffFilePath, got.Diff)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Errorf("the host wrote %d entries into the symlink target %s", len(entries), target)
	}
}

// TestPlaceDiff_SymlinkedFileNotFollowed_3377: a symlink at the diff file
// path, for example to a startup file of the shell, is replaced. The target
// does not change.
func TestPlaceDiff_SymlinkedFileNotFollowed_3377(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "zshenv")
	const before = "# user startup file\n"
	if err := os.WriteFile(target, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "review-diff", "abc")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := review.DiffFilePathForTest(stateDir, "7", 3)
	if err := os.Symlink(target, planted); err != nil {
		t.Fatal(err)
	}

	got := review.PlaceDiffForTest(stateDir, "7", 3, largeDiff, 10, 1)

	if data, err := os.ReadFile(target); err != nil || string(data) != before {
		t.Errorf("the symlink target changed to %q (err=%v), want %q", data, err, before)
	}
	if got.DiffFilePath != planted {
		t.Fatalf("DiffFilePath = %q, want %q", got.DiffFilePath, planted)
	}
	info, err := os.Lstat(planted)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("diff path is not a regular file after the write: %v %v", info, err)
	}
	if data, _ := os.ReadFile(planted); string(data) != largeDiff {
		t.Errorf("diff file holds %q, want the diff", data)
	}
}

// TestPlaceDiff_SamePRAndRoundTwoSessions_3377: two sessions that review the
// same PR number in the same round write to different files.
func TestPlaceDiff_SamePRAndRoundTwoSessions_3377(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	a := review.PlaceDiffForTest(diffDirOf(t, diffParentA), "42", 1, largeDiff+"+a\n", 10, 1)
	b := review.PlaceDiffForTest(diffDirOf(t, diffParentB), "42", 1, largeDiff+"+b\n", 10, 1)
	if a.DiffFilePath == "" || a.DiffFilePath == b.DiffFilePath {
		t.Fatalf("diff files: %q and %q, want two different files", a.DiffFilePath, b.DiffFilePath)
	}
	for path, want := range map[string]string{a.DiffFilePath: largeDiff + "+a\n", b.DiffFilePath: largeDiff + "+b\n"} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Errorf("%s holds %q (err=%v), want %q", path, data, err, want)
		}
	}
}

// TestPlaceDiff_NoWritableDirInlines_3377: an empty diff dir, a relative
// one, and one that cannot be made all give the inline diff and no file in
// the cwd.
func TestPlaceDiff_NoWritableDirInlines_3377(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	readOnly := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })

	dirs := []string{"", ".prism-review"}
	if os.Geteuid() != 0 {
		dirs = append(dirs, filepath.Join(readOnly, "review-diff", "abc"))
	}
	for _, stateDir := range dirs {
		got := review.PlaceDiffForTest(stateDir, "7", 1, largeDiff, 10, 1)
		if got.DiffFilePath != "" || got.Diff != largeDiff {
			t.Errorf("stateDir %q: DiffFilePath=%q Diff=%q, want no file and the diff inline", stateDir, got.DiffFilePath, got.Diff)
		}
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Errorf("the host wrote %d entries into the cwd %s", len(entries), cwd)
	}
}

// writeFakeGH puts a gh stub in a new dir at the front of PATH. It answers
// `gh pr view` with PR metadata and `gh pr diff` with diff.
func writeFakeGH(t *testing.T, diff string) {
	t.Helper()
	dir := t.TempDir()
	diffFile := filepath.Join(dir, "pr.diff")
	if err := os.WriteFile(diffFile, []byte(diff), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"\"pr view\") printf '%s\\n' '{\"number\":7,\"title\":\"t\",\"body\":\"\",\"headRefName\":\"h\",\"baseRefName\":\"main\"}' ;;\n" +
		"\"pr diff\") cat '" + diffFile + "' ;;\n" +
		"*) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestFetchPRContext_LargeDiffInStateDirNotWorktree_3377 runs the GitHub path
// of FetchPRContextWithOpts with a gh stub. A diff above the inline limit
// goes to the diff dir of the parent, and the worktree gets no file. A diff
// under the limit is inline, and no file is written.
func TestFetchPRContext_LargeDiffInStateDirNotWorktree_3377(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	worktree := t.TempDir()
	// runGH opens the repo of the cwd. Outside a repo, the result does not
	// depend on the checkout that runs the test.
	t.Chdir(worktree)
	stateDir := diffDirOf(t, diffParentA)

	large := largeDiff + strings.Repeat("+more\n", 20)
	writeFakeGH(t, large)
	got := review.FetchPRContextWithOpts(review.FetchPRContextOpts{
		PRNumber:       "7",
		Round:          1,
		InlineMaxLines: 5,
		Worktree:       worktree,
		StateDir:       stateDir,
	})
	if got.FetchFailed {
		t.Fatal("FetchFailed with the gh stub")
	}
	if want := filepath.Join(stateDir, "pr-7-round-1.diff"); got.DiffFilePath != want || got.Diff != "" {
		t.Fatalf("large diff: DiffFilePath=%q Diff=%q, want the file %s", got.DiffFilePath, got.Diff, want)
	}
	if data, err := os.ReadFile(got.DiffFilePath); err != nil || string(data) != large {
		t.Errorf("diff file holds %q (err=%v), want the PR diff", data, err)
	}
	if entries, _ := os.ReadDir(worktree); len(entries) != 0 {
		t.Errorf("the worktree holds %d entries after the review fetch, want none", len(entries))
	}

	small := "+one\n"
	writeFakeGH(t, small)
	got = review.FetchPRContextWithOpts(review.FetchPRContextOpts{
		PRNumber: "7",
		Round:    2,
		Worktree: worktree,
		StateDir: stateDir,
	})
	if got.Diff != small || got.DiffFilePath != "" {
		t.Errorf("small diff: Diff=%q DiffFilePath=%q, want the diff inline and no file", got.Diff, got.DiffFilePath)
	}
	if _, err := os.Lstat(filepath.Join(stateDir, "pr-7-round-2.diff")); !os.IsNotExist(err) {
		t.Errorf("a small diff wrote a diff file (err=%v)", err)
	}
}

// TestParentCleanup_RemovesDiffDir_3377: parent cleanup, which `prism
// cleanup` and `prism close --yes` run, removes the diff dir of the parent
// and keeps the diff dir of another parent.
func TestParentCleanup_RemovesDiffDir_3377(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := openTestDB(t)
	t.Cleanup(review.ReapSideEffectsWithStopForTest(nil, func(string) bool { return true }, nil))
	seedRound(t, d, diffParentA, 1, []string{"finished"}, true)
	seedRound(t, d, diffParentB, 1, []string{"finished"}, true)
	own := review.PlaceDiffForTest(diffDirOf(t, diffParentA), seedRoundPR, 1, largeDiff, 10, 1).DiffFilePath
	kept := review.PlaceDiffForTest(diffDirOf(t, diffParentB), seedRoundPR, 1, largeDiff, 10, 1).DiffFilePath
	if own == "" || kept == "" {
		t.Fatal("no diff file was written")
	}

	review.CleanupReviewSessionsForParent(d, diffParentA, "")

	if _, err := os.Lstat(diffDirOf(t, diffParentA)); !os.IsNotExist(err) {
		t.Errorf("the diff dir of the parent still exists after cleanup (err=%v)", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("the diff file of another parent was removed: %v", err)
	}
}
