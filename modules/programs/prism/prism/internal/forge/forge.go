// Package forge classifies a git remote as GitHub or GitLab.
//
// This is deliberately a single small helper, not a multi-forge interface.
// GitHub remains prism's primary, fully-supported forge. GitLab (gitlab.com
// only) gets a minimal support/error/degrade split:
//   - prism review / prism pr: supported.
//   - prism merge: errors with a glab pointer.
//   - branch-protection probe, merge-queue watcher, dashboard gh-graphql
//     poll: degrade cleanly — detect and skip.
package forge

import (
	"strings"

	"github.com/prismatic-koi/prism/internal/git"
)

// Forge identifies which git hosting service a remote belongs to.
type Forge int

const (
	// GitHub is the default forge: github.com remotes, and any remote this
	// package does not recognise, both classify as GitHub — unknown remotes
	// fall back to the primary forge.
	GitHub Forge = iota
	// GitLab identifies a gitlab.com remote.
	GitLab
)

// FromRemoteURL classifies a git remote URL into a Forge. Recognises
// `git@gitlab.com:...` and `https://gitlab.com/...` (and any other URL
// containing the gitlab.com host) as GitLab; everything else, including
// github.com and unrecognised hosts, defaults to GitHub.
func FromRemoteURL(remoteURL string) Forge {
	if strings.Contains(remoteURL, "gitlab.com") {
		return GitLab
	}
	return GitHub
}

// IsGitLab is a convenience predicate over FromRemoteURL.
func IsGitLab(remoteURL string) bool {
	return FromRemoteURL(remoteURL) == GitLab
}

// OriginRemoteURL returns the `origin` remote URL for the git repo at dir
// (the current directory when dir is ""). Returns "" on any error (not a
// git repo, no origin remote, git not on PATH) — callers should treat that
// as "cannot determine, assume GitHub", matching FromRemoteURL's default.
func OriginRemoteURL(dir string) string {
	repo, err := git.OpenDir(dir)
	if err != nil {
		return ""
	}
	url, err := repo.Output("remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return url
}

// DetectFromDir combines OriginRemoteURL and FromRemoteURL for the common
// case of classifying the repo at dir (or the current directory when dir is
// "").
func DetectFromDir(dir string) Forge {
	return FromRemoteURL(OriginRemoteURL(dir))
}

// IsGitLabDir reports whether the repo at dir (or the current directory
// when dir is "") has a gitlab.com origin remote.
func IsGitLabDir(dir string) bool {
	return DetectFromDir(dir) == GitLab
}
