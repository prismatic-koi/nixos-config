// Package config — repo_skills.go
//
// Loads the repo skills from ~/.config/prism/repo-skills.json (issue #3463).
//
// prism gives pi one --skill <path> for each skill of the repo of a session
// (internal/reposkills). pi loads a --skill path also with --no-approve,
// because that path is not a project resource.
//
// The prism NixOS module renders the file from nx.programs.prism.repoSkills.
// Each key is a repo directory name. Each value is a list of nix store paths:
//
//	{
//	  "nixos-config": ["/nix/store/<hash>-site-theming"]
//	}
//
// Deployment through Nix is a security property. Neither
// internal/container/mounts.go (bwrap) nor the sandbox-exec profile gives a
// sandbox access to this file, so no sandbox can read or write it.

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RepoSkillsFileName is the basename of the rendered file under
// ~/.config/prism/.
const RepoSkillsFileName = "repo-skills.json"

// RepoSkillsPath returns the path to repo-skills.json. Returns "" when no
// home directory is discoverable.
func RepoSkillsPath() string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "prism", RepoSkillsFileName)
}

// LoadRepoSkills reads repo-skills.json and returns the skill paths of each
// repo, keyed by repo directory name.
//
// A missing, unreadable, or malformed file returns an error. The NixOS module
// always renders the file, so a missing file is a fault. Repo names and paths
// are trimmed, and empty entries are dropped.
func LoadRepoSkills() (map[string][]string, error) {
	path := RepoSkillsPath()
	if path == "" {
		return nil, fmt.Errorf("repo skills: cannot determine %s path (no home directory)", RepoSkillsFileName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("repo skills: read %s: %w", path, err)
	}
	var raw map[string][]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("repo skills: parse %s: %w", path, err)
	}
	out := make(map[string][]string, len(raw))
	for repo, paths := range raw {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		for _, p := range paths {
			if p = strings.TrimSpace(p); p != "" {
				out[repo] = append(out[repo], p)
			}
		}
	}
	return out, nil
}
