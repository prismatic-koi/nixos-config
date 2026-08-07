package git

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOriginRemoteURL returns the origin URL of a bare-layout repo.
func TestOriginRemoteURL(t *testing.T) {
	bareRoot := t.TempDir()
	origin := t.TempDir()
	tmpRepo := t.TempDir()
	initRepo(t, tmpRepo, "main")
	if out, err := exec.Command("git", "clone", "--bare", tmpRepo, origin).CombinedOutput(); err != nil {
		t.Fatalf("clone origin: %v\n%s", err, out)
	}
	barePath := filepath.Join(bareRoot, ".bare")
	if out, err := exec.Command("git", "clone", "--bare", origin, barePath).CombinedOutput(); err != nil {
		t.Fatalf("clone .bare: %v\n%s", err, out)
	}

	got, err := OriginRemoteURL(bareRoot)
	if err != nil {
		t.Fatalf("OriginRemoteURL: %v", err)
	}
	if got != origin {
		t.Errorf("OriginRemoteURL = %q, want %q", got, origin)
	}
}

// TestFetchGitLabMRBranch fetches a GitLab-style merge-request head ref into a
// local branch. The origin bare repo carries a
// refs/merge-requests/<iid>/head ref, exactly as gitlab.com exposes; the
// helper must fetch it into refs/heads/<source_branch>.
func TestFetchGitLabMRBranch(t *testing.T) {
	// Build a working repo with a second commit on a feature branch, then
	// bare-clone it as origin and stamp the MR head ref onto that commit.
	work := t.TempDir()
	initRepo(t, work, "main")
	runGitIn(t, work, "checkout", "-b", "feat/iam")
	runGitIn(t, work, "commit", "--allow-empty", "-m", "mr work")
	mrHead := runGitIn(t, work, "rev-parse", "HEAD")
	runGitIn(t, work, "checkout", "main")

	origin := t.TempDir()
	if out, err := exec.Command("git", "clone", "--bare", work, origin).CombinedOutput(); err != nil {
		t.Fatalf("clone origin: %v\n%s", err, out)
	}
	// Stamp the GitLab MR head ref in origin, then drop the ordinary branch
	// so the only way to reach the commit is via merge-requests/1/head — this
	// proves the helper fetches the MR ref, not a plain branch.
	if out, err := exec.Command("git", "--git-dir", origin, "update-ref",
		"refs/merge-requests/1/head", mrHead).CombinedOutput(); err != nil {
		t.Fatalf("stamp MR ref: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", origin, "branch", "-D", "feat/iam").CombinedOutput(); err != nil {
		t.Fatalf("delete feat/iam in origin: %v\n%s", err, out)
	}

	bareRoot := t.TempDir()
	barePath := filepath.Join(bareRoot, ".bare")
	if out, err := exec.Command("git", "clone", "--bare", origin, barePath).CombinedOutput(); err != nil {
		t.Fatalf("clone .bare: %v\n%s", err, out)
	}

	branch, err := FetchGitLabMRBranch(bareRoot, "1", func(iid string) (string, error) {
		if iid != "1" {
			t.Errorf("resolver iid = %q, want 1", iid)
		}
		return "feat/iam", nil
	})
	if err != nil {
		t.Fatalf("FetchGitLabMRBranch: %v", err)
	}
	if branch != "feat/iam" {
		t.Errorf("branch = %q, want feat/iam", branch)
	}

	// The local branch must exist in .bare and point at the MR head commit.
	got := runGitIn(t, bareRoot, "--git-dir", barePath, "rev-parse", "refs/heads/feat/iam")
	if got != mrHead {
		t.Errorf("refs/heads/feat/iam = %q, want %q", got, mrHead)
	}

	// Idempotent: a second call force-updates cleanly rather than failing.
	if _, err := FetchGitLabMRBranch(bareRoot, "1", func(string) (string, error) {
		return "feat/iam", nil
	}); err != nil {
		t.Fatalf("FetchGitLabMRBranch (second call): %v", err)
	}
}

// TestFetchGitLabMRBranch_ResolverError surfaces a source-branch resolution
// failure rather than fetching.
func TestFetchGitLabMRBranch_ResolverError(t *testing.T) {
	bareRoot := t.TempDir()
	initBareWithWorktree(t, bareRoot, "main")
	_, err := FetchGitLabMRBranch(bareRoot, "1", func(string) (string, error) {
		return "", errors.New("resolver boom")
	})
	if err == nil {
		t.Fatal("expected error when resolver fails, got nil")
	}
}

// TestFetchGitLabMRBranch_EmptySourceBranch refuses an empty source branch.
func TestFetchGitLabMRBranch_EmptySourceBranch(t *testing.T) {
	bareRoot := t.TempDir()
	initBareWithWorktree(t, bareRoot, "main")
	_, err := FetchGitLabMRBranch(bareRoot, "1", func(string) (string, error) {
		return "  ", nil
	})
	if err == nil {
		t.Fatal("expected error for empty source branch, got nil")
	}
}
