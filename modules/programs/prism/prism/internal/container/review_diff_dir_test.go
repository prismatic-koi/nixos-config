package container

// review_diff_dir_test.go — issue #3377. A review agent gets a read-only
// grant on the review diff dir of its parent, in bwrap and in sandbox-exec,
// and no other session gets one. The host writes into the sandbox-exec work
// dir and into the own worktree git dir do not follow a symlink that the
// session put there.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/rundir"
)

// reviewDiffDirForTest makes $XDG_STATE_HOME/prism/review-diff/<name> under
// a new state dir and returns it.
func reviewDiffDirForTest(t *testing.T, name string) string {
	t.Helper()
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "prism", "review-diff", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBwrapReviewDiffDirBind_3377(t *testing.T) {
	dir := reviewDiffDirForTest(t, "abc")
	link := filepath.Join(filepath.Dir(dir), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		dir  string
		want []string
	}{
		{"empty", "", nil},
		{"missing", filepath.Join(filepath.Dir(dir), "missing"), nil},
		{"symlink", link, nil},
		{"dir", dir, []string{"--ro-bind", dir, dir}},
	}
	for _, tc := range cases {
		got := bwrapReviewDiffDirBind(tc.dir)
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("%s: bwrapReviewDiffDirBind(%q) = %q, want %q", tc.name, tc.dir, got, tc.want)
		}
	}
}

// TestBwrapBuildArgs_ReviewDiffDirReadOnly_3377: BuildArgs binds the review
// diff dir read-only at its host path, binds nothing else under review-diff,
// and a session with no ReviewDiffDir gets no review-diff bind.
func TestBwrapBuildArgs_ReviewDiffDirReadOnly_3377(t *testing.T) {
	dir := reviewDiffDirForTest(t, "abc")
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:      "repo@main~review-1-review-code",
		Worktree:         t.TempDir(),
		WorktreeReadOnly: true,
		ReviewDiffDir:    dir,
	})
	defer cleanup()
	args := (&bwrapIsolator{name: m.name}).BuildArgs(m)
	if !hasROBind(args, dir) {
		t.Errorf("no --ro-bind %s %s in args: %v", dir, dir, redactedArgs(args))
	}
	reviewDiffBase := filepath.Dir(dir)
	for _, flag := range []string{"--bind", "--bind-try", "--ro-bind", "--ro-bind-try"} {
		for _, tr := range findTriples(args, flag) {
			if strings.HasPrefix(tr[0], reviewDiffBase) && !(flag == "--ro-bind" && tr[0] == dir) {
				t.Errorf("unexpected %s %s %s: only the own diff dir, read-only", flag, tr[0], tr[1])
			}
		}
	}

	m2, _, cleanup2 := bwrapFixture(t, Config{SessionName: "repo@main", Worktree: t.TempDir()})
	defer cleanup2()
	for _, a := range (&bwrapIsolator{name: m2.name}).BuildArgs(m2) {
		if strings.Contains(a, "review-diff") {
			t.Errorf("a session with no ReviewDiffDir has a review-diff arg %q", a)
		}
	}
}

// TestGenerateProfile_ReviewDiffDirReadOnly_3377: the profile of a review
// agent grants a read of the diff dir of its parent (section 5l), no rule
// grants a write of it or of review-diff/, and the profile of a session with
// no ReviewDiffDir names no review-diff path.
func TestGenerateProfile_ReviewDiffDirReadOnly_3377(t *testing.T) {
	m, _ := newTmpDirProfile(t, "instance-a")
	base := filepath.Join(auditLocationTestHome, ".local", "state", "prism", "review-diff")
	dir := filepath.Join(base, "abc")
	m.cfg.ReviewDiffDir = dir
	profile := generateProfile(m)

	want := "(allow file-read* file-test-existence file-read-metadata\n  (subpath " + quoteSBPL(dir) + "))\n"
	if !strings.Contains(profile, want) {
		t.Errorf("profile missing the section-5l read of the diff dir:\n%s\nprofile:\n%s", want, profile)
	}
	if n := strings.Count(profile, quoteSBPL(dir)); n != 1 {
		t.Errorf("the diff dir is named %d times in the profile, want 1 (the read-only grant)", n)
	}
	if strings.Contains(profile, quoteSBPL(base)+")") {
		t.Errorf("the profile names the whole review-diff dir %s, want the own diff dir only:\n%s", base, profile)
	}
	writes := writeClauses(profile)
	if !strings.Contains(writes, "(subpath "+quoteSBPL(m.cfg.Worktree)+")") {
		t.Fatalf("writeClauses found no write grant of the worktree, so the check below proves nothing:\n%s", writes)
	}
	for p := dir; p != "/"; p = filepath.Dir(p) {
		if strings.Contains(writes, "(subpath "+quoteSBPL(p)+")") {
			t.Errorf("a write grant covers %s, the diff dir or an ancestor of it", p)
		}
	}

	m.cfg.ReviewDiffDir = ""
	if p := generateProfile(m); strings.Contains(p, "review-diff") {
		t.Errorf("a profile with no ReviewDiffDir names review-diff:\n%s", p)
	}
}

// writeClauses returns the text of each allow clause in profile that has a
// file-write operation in its first line.
func writeClauses(profile string) string {
	var sb strings.Builder
	for _, clause := range strings.Split(profile, "(allow ")[1:] {
		end := strings.Index(clause, "\n")
		if end < 0 || !strings.Contains(clause[:end], "file-write") {
			continue
		}
		body := clause
		if i := strings.Index(body, "\n\n"); i >= 0 {
			body = body[:i]
		}
		sb.WriteString(body)
	}
	return sb.String()
}

func TestGenerateProfile_SessionWorkDirNodeDeny_3377(t *testing.T) {
	m, profile := newTmpDirProfile(t, "instance-a")
	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		t.Fatal(err)
	}
	want := "(deny file-write-unlink file-write-create file-write-mode file-write-flags file-write-acl\n" +
		"  (literal " + quoteSBPL(sessionDir) + "))\n"
	if !strings.Contains(profile, want) {
		t.Errorf("profile missing the section-21n work dir node deny:\n%s\nprofile:\n%s", want, profile)
	}
	if !strings.Contains(profile, "  (subpath "+quoteSBPL(sessionDir)+")\n") {
		t.Errorf("profile missing the section-6 grant of the work dir, so the files in it are not writable:\n%s", profile)
	}
}

// TestGenerateProfile_SessionWorkDirNodeDenyFollowsEveryAllow pins the order:
// SBPL resolves a conflict in favour of the later rule, so the section-21n
// deny must follow the section-6 grant and every other allow.
func TestGenerateProfile_SessionWorkDirNodeDenyFollowsEveryAllow(t *testing.T) {
	m, profile := newTmpDirProfile(t, "instance-a")
	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		t.Fatal(err)
	}
	denyIdx := strings.Index(profile, dirNodeDeny(symlinkForms(sessionDir)))
	lastAllow := strings.LastIndex(profile, "(allow ")
	if denyIdx < 0 {
		t.Fatalf("profile missing the section-21n work dir node deny:\n%s", profile)
	}
	if denyIdx < lastAllow {
		t.Errorf("section-21n work dir node deny (idx %d) precedes an allow (idx %d); SBPL would outrank the deny", denyIdx, lastAllow)
	}
}

// TestPrepareSessionWorkDir_RefusesSymlinkedWorkDir_3377: a work dir that is
// a symlink fails the prepare, and no file goes to the symlink target.
func TestPrepareSessionWorkDir_RefusesSymlinkedWorkDir_3377(t *testing.T) {
	newFakeHome(t)
	m := newSandboxExecManagerWithInstance(Config{SessionName: "repo@feat", InstanceID: "work-dir-symlink-3377"})
	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(sessionDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, sessionDir); err != nil {
		t.Fatal(err)
	}

	if _, err := m.PrepareSessionWorkDir(); !errors.Is(err, rundir.ErrSymlinkDir) {
		t.Errorf("PrepareSessionWorkDir with a symlinked work dir: %v, want an error that wraps rundir.ErrSymlinkDir", err)
	}
	if err := (&sandboxExecIsolator{name: m.name}).WriteGitconfig(m); !errors.Is(err, rundir.ErrSymlinkDir) {
		t.Errorf("WriteGitconfig with a symlinked work dir: %v, want an error that wraps rundir.ErrSymlinkDir", err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Errorf("the host wrote %d entries into the symlink target %s", len(entries), target)
	}
}

// TestPrepareSessionWorkDir_SymlinkedFilesNotFollowed_3377: a symlink at
// ssh-config, gitconfig, or allowed_signers is replaced by the generated
// file. The target does not change.
func TestPrepareSessionWorkDir_SymlinkedFilesNotFollowed_3377(t *testing.T) {
	newFakeHome(t)
	m := newSandboxExecManagerWithInstance(Config{SessionName: "repo@feat", InstanceID: "work-dir-file-symlink-3377"})
	sessionDir, err := m.PrepareSessionWorkDir()
	if err != nil {
		t.Fatalf("PrepareSessionWorkDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sessionDir) })

	const before = "# user file\n"
	paths := []string{
		SessionWorkDirSshConfigPath(sessionDir),
		SessionWorkDirGitconfigPath(sessionDir),
		SessionWorkDirAllowedSignersPath(sessionDir),
	}
	targets := map[string]string{}
	for _, p := range paths {
		target := filepath.Join(t.TempDir(), filepath.Base(p))
		if err := os.WriteFile(target, []byte(before), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(p); err != nil {
			t.Fatalf("remove %s: %v", p, err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		targets[p] = target
	}

	if _, err := m.PrepareSessionWorkDir(); err != nil {
		t.Fatalf("second PrepareSessionWorkDir: %v", err)
	}
	for p, target := range targets {
		if data, _ := os.ReadFile(target); string(data) != before {
			t.Errorf("the symlink target of %s changed to %q", filepath.Base(p), data)
		}
		if info, err := os.Lstat(p); err != nil || !info.Mode().IsRegular() {
			t.Errorf("%s is not a regular file after the prepare: %v %v", p, info, err)
		}
	}
}

// TestEnsureDirBelow_RefusesSymlinkedComponent_3377: a symlink at a
// component below the root is refused, and no dir is made in its target.
func TestEnsureDirBelow_RefusesSymlinkedComponent_3377(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "Library")); err != nil {
		t.Fatal(err)
	}
	err := ensureDirBelow(root, filepath.Join(root, "Library", "Caches", "Google"))
	if !errors.Is(err, rundir.ErrSymlinkDir) {
		t.Errorf("ensureDirBelow through a symlinked component: %v, want an error that wraps rundir.ErrSymlinkDir", err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Errorf("the host made %d entries in the symlink target %s", len(entries), target)
	}

	if err := ensureDirBelow(root, filepath.Join(root, "x", "y")); err != nil {
		t.Fatalf("ensureDirBelow of a new dir: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(root, "x", "y")); err != nil || !info.IsDir() {
		t.Errorf("ensureDirBelow did not make the dir: %v %v", info, err)
	}
	if err := ensureDirBelow(root, filepath.Dir(root)); err == nil {
		t.Error("ensureDirBelow accepted a dir that is not below the root")
	}
}

// TestEnsureFileExists_DanglingSymlinkNotFollowed_3377: a dangling symlink
// at the path is left, and its target is not made.
func TestEnsureFileExists_DanglingSymlinkNotFollowed_3377(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "made-by-host")
	path := filepath.Join(dir, "config.worktree")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := ensureFileExists(path, nil); err != nil {
		t.Fatalf("ensureFileExists: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("ensureFileExists made the symlink target %s (err=%v)", target, err)
	}

	fresh := filepath.Join(dir, "config")
	if err := ensureFileExists(fresh, []byte("x")); err != nil {
		t.Fatalf("ensureFileExists of a new file: %v", err)
	}
	if data, err := os.ReadFile(fresh); err != nil || string(data) != "x" {
		t.Errorf("new file holds %q (err=%v), want %q", data, err, "x")
	}
}
