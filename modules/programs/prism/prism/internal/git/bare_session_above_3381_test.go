package git

// bare_session_above_3381_test.go — CloneWorktree and ConvertToBare refuse
// while a session in the target root, or in a dir above it, runs (#3381).
// Such a session can write the .bare that they make, and the bare repo
// record accepts that .bare.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setSessionRuns makes runs the check of SetSessionRunsSource for the rest
// of t. A nil runs removes the check.
func setSessionRuns(t *testing.T, runs SessionRuns) {
	t.Helper()
	prev := sessionRunsSource.Load()
	if runs == nil {
		sessionRunsSource.Store(nil)
	} else {
		sessionRunsSource.Store(&runs)
	}
	t.Cleanup(func() { sessionRunsSource.Store(prev) })
}

func sessionRunsAnswer(running bool, err error) SessionRuns {
	return func(string) (bool, error) { return running, err }
}

// AC (#3381): a clone refuses while a session that started in the target or
// in a dir above it runs. It makes no .bare, and it removes only the dirs
// that it made. A session in another project does not refuse the clone.
func TestCloneWorktree_RefusesWhileSessionAtOrAboveRuns_3381(t *testing.T) {
	code, origin := cloneTargetFixture(t)
	useBareRecord(t)
	link := filepath.Join(t.TempDir(), "link-to-code")
	if err := os.Symlink(code, link); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(code, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		target  string
		start   SessionStart
		runs    SessionRuns
		wantErr string
	}{
		{"open record above", filepath.Join(code, "a1"), SessionStart{Name: "code", Worktree: code, Open: true}, sessionRunsAnswer(false, nil), "a session record with no end time names the session code"},
		{"open record in an empty target", empty, SessionStart{Name: "empty", Worktree: empty, Open: true}, sessionRunsAnswer(false, nil), "names the session empty"},
		{"the host says that it runs", filepath.Join(code, "a2"), SessionStart{Name: "code", Worktree: code}, sessionRunsAnswer(true, nil), "the session code runs"},
		{"the host cannot tell", filepath.Join(code, "a3"), SessionStart{Name: "code", Worktree: code}, sessionRunsAnswer(false, errors.New("no answer")), "cannot tell"},
		{"no check of the host", filepath.Join(code, "a4"), SessionStart{Name: "code", Worktree: code}, nil, "no check of the running sessions"},
		{"a symlink above", filepath.Join(code, "a5"), SessionStart{Name: "link", Worktree: link, Open: true}, sessionRunsAnswer(false, nil), "names the session link"},
		{"the filesystem root", filepath.Join(code, "a6"), SessionStart{Name: "slash", Worktree: "/", Open: true}, sessionRunsAnswer(false, nil), "names the session slash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSessionStarts(t, tc.start)
			setSessionRuns(t, tc.runs)
			_, existed := os.Stat(tc.target)
			err := CloneWorktree(origin, tc.target, func(string) {})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "clone into "+tc.target) {
				t.Fatalf("CloneWorktree(%s) = %v, want a refusal that has %q", tc.target, err, tc.wantErr)
			}
			if _, statErr := os.Lstat(filepath.Join(tc.target, ".bare")); !os.IsNotExist(statErr) {
				t.Errorf("the refused clone made %s/.bare: %v", tc.target, statErr)
			}
			if _, statErr := os.Stat(tc.target); (statErr == nil) != (existed == nil) {
				t.Errorf("the refused clone changed whether %s exists: before %v, after %v", tc.target, existed, statErr)
			}
		})
	}

	t.Run("no session runs at or above the target", func(t *testing.T) {
		setSessionStarts(t,
			SessionStart{Name: "code", Worktree: code},
			SessionStart{Name: "other@main", Worktree: filepath.Join(code, "other", "main"), Open: true},
			SessionStart{Name: "scratchpad", Worktree: code, Open: true},
		)
		setSessionRuns(t, sessionRunsAnswer(false, nil))
		target := filepath.Join(code, "ok")
		if err := CloneWorktree(origin, target, func(string) {}); err != nil {
			t.Fatalf("CloneWorktree(%s): %v", target, err)
		}
		if _, err := OpenBare(target); err != nil {
			t.Errorf("OpenBare(%s) after the clone: %v", target, err)
		}
	})
}

// AC (#3381): a conversion refuses while a session that started in the root
// or in a dir above it runs, and does not change the clone.
func TestConvertToBare_RefusesWhileSessionAtOrAboveRuns_3381(t *testing.T) {
	isolateHostGitConfig(t)
	useBareRecord(t)
	code := filepath.Join(t.TempDir(), "code")
	root := filepath.Join(code, "conv")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, []string{code}, nil)
	initRepoWithRemote(t, root, filepath.Join(t.TempDir(), "remote.git"), "main")

	for _, tc := range []struct {
		start   SessionStart
		runs    SessionRuns
		wantErr string
	}{
		{SessionStart{Name: "code", Worktree: code, Open: true}, sessionRunsAnswer(false, nil), "names the session code"},
		{SessionStart{Name: "conv", Worktree: root}, sessionRunsAnswer(true, nil), "the session conv runs"},
	} {
		setSessionStarts(t, tc.start)
		setSessionRuns(t, tc.runs)
		if _, err := ConvertToBare(root, func(string) {}); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("ConvertToBare(%s) with %+v = %v, want a refusal that has %q", root, tc.start, err, tc.wantErr)
		}
		if info, err := os.Lstat(filepath.Join(root, ".git")); err != nil || !info.IsDir() {
			t.Fatalf("the refused conversion changed %s/.git: %v", root, err)
		}
		if _, err := os.Lstat(filepath.Join(root, ".bare")); !os.IsNotExist(err) {
			t.Fatalf("the refused conversion made %s/.bare: %v", root, err)
		}
	}

	setSessionStarts(t, SessionStart{Name: "conv", Worktree: root})
	setSessionRuns(t, sessionRunsAnswer(false, nil))
	if _, err := ConvertToBare(root, func(string) {}); err != nil {
		t.Fatalf("ConvertToBare(%s) after the session stopped: %v", root, err)
	}
}
