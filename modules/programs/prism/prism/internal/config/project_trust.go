// Package config — project_trust.go
//
// Loads the pi project-trust allowlist from
// ~/.config/prism/project-trust.json (issue #3216).
//
// prism starts every pi with --approve or --no-approve. pi then ignores
// trust.json, the project_trust extension event, and defaultProjectTrust.
// prism passes --approve only for a session of a repo in this list (see
// internal/projecttrust).
//
// The prism NixOS module renders the file from
// nx.programs.prism.projectTrust.trustedRepos:
//
//	{
//	  "trusted_repos": ["home-ops", "obsidian"]
//	}
//
// Deployment through Nix is a security property. Neither
// internal/container/mounts.go (bwrap) nor the sandbox-exec profile gives a
// sandbox access to this file, so no sandbox can read or write it.
//
// A missing file is not an error: it yields an empty list, and every pi
// starts with --no-approve.

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProjectTrustFileName is the basename of the rendered file under
// ~/.config/prism/.
const ProjectTrustFileName = "project-trust.json"

// ProjectTrustFile is the on-disk structure of project-trust.json.
type ProjectTrustFile struct {
	// TrustedRepos holds repo directory names, not session names and not
	// paths.
	TrustedRepos []string `json:"trusted_repos"`
}

// ProjectTrustPath returns the path to project-trust.json. Returns "" when
// no home directory is discoverable.
func ProjectTrustPath() string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "prism", ProjectTrustFileName)
}

// LoadProjectTrustedRepos reads project-trust.json and returns the repo
// names that it declares.
//
// A missing file returns (nil, nil). An unreadable or malformed file returns
// an error, and the caller must fail closed: it treats the list as empty.
// Entries are trimmed, and empty entries are dropped, so a stray "" cannot
// match a repo that failed to resolve.
func LoadProjectTrustedRepos() ([]string, error) {
	path := ProjectTrustPath()
	if path == "" {
		return nil, fmt.Errorf("project trust: cannot determine %s path (no home directory)", ProjectTrustFileName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("project trust: read %s: %w", path, err)
	}
	var f ProjectTrustFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("project trust: parse %s: %w", path, err)
	}
	var repos []string
	for _, repo := range f.TrustedRepos {
		if repo = strings.TrimSpace(repo); repo != "" {
			repos = append(repos, repo)
		}
	}
	return repos, nil
}

// ExpandHomePath expands a leading "~/" in p to the home directory of the
// user. Other paths are returned unchanged.
func ExpandHomePath(p string) string {
	return expandHomePath(p)
}
