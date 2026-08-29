package dashboard_test

import (
	"os/exec"
	"testing"

	"github.com/prismatic-koi/prism/internal/dashboard"
)

// TestFetchGitHubStats_GitLabRemoteSkipsWithoutFalseZero verifies the GitLab
// dashboard degrade path. On a gitlab.com origin remote, FetchGitHubStats
// must not shell out to gh (which would fail or misbehave against a repo it
// cannot resolve), and must not silently reset the prior open-PR count to a
// false 0. It
// signals "skip, keep the prior value" via the existing Err:true contract
// (see GithubStatsMsg), which both dashboard.Model variants already honour
// by leaving GhOpenPRs untouched.
func TestFetchGitHubStats_GitLabRemoteSkipsWithoutFalseZero(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init")
	runGit("remote", "add", "origin", "git@gitlab.com:owner/repo.git")
	t.Chdir(dir)

	msg := dashboard.FetchGitHubStats()
	stats, ok := msg.(dashboard.GithubStatsMsg)
	if !ok {
		t.Fatalf("FetchGitHubStats() returned %T, want dashboard.GithubStatsMsg", msg)
	}
	if !stats.Err {
		t.Error("FetchGitHubStats() on a gitlab.com remote: Err = false, want true (must signal skip-preserve, not a real 0 count)")
	}
	if stats.OpenPRs != 0 {
		t.Errorf("FetchGitHubStats() on a gitlab.com remote: OpenPRs = %d, want 0 (unused when Err is true, but pin the zero value anyway)", stats.OpenPRs)
	}
}
