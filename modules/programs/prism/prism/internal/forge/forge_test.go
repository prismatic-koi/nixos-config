package forge

import "testing"

func TestFromRemoteURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want Forge
	}{
		{"ssh gitlab", "git@gitlab.com:owner/repo.git", GitLab},
		{"https gitlab", "https://gitlab.com/owner/repo.git", GitLab},
		{"ssh github", "git@github.com:owner/repo.git", GitHub},
		{"https github", "https://github.com/owner/repo.git", GitHub},
		{"unrecognised host defaults to github", "https://example.com/owner/repo.git", GitHub},
		{"empty defaults to github", "", GitHub},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FromRemoteURL(c.url); got != c.want {
				t.Errorf("FromRemoteURL(%q) = %v, want %v", c.url, got, c.want)
			}
		})
	}
}

func TestIsGitLab(t *testing.T) {
	if !IsGitLab("git@gitlab.com:owner/repo.git") {
		t.Error("expected gitlab.com remote to be detected as GitLab")
	}
	if IsGitLab("git@github.com:owner/repo.git") {
		t.Error("expected github.com remote to be detected as GitHub, not GitLab")
	}
}

func TestOriginRemoteURLNotAGitRepo(t *testing.T) {
	// A directory with no .git and no origin remote must return "" rather
	// than error out — callers rely on this to default to GitHub.
	if got := OriginRemoteURL(t.TempDir()); got != "" {
		t.Errorf("OriginRemoteURL(non-git dir) = %q, want empty string", got)
	}
}
