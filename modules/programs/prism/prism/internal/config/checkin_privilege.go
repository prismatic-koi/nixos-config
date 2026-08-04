// Package config — checkin_privilege.go
//
// Loads the tier-3 `/checkin` troubleshooting privilege list from
// ~/.config/prism/checkin-privileged-repos.json (issue #2587).
//
// `/checkin` has three permission tiers. Tier 1 is a worker, which may read
// only the review agents of its own session. Tier 2 is a coordinator, which
// may read its own repo plus cross-repo coordinators. Tier 3 is a coordinator
// of a repo named in this list, which may read any session in any repo. The
// tier-3 grant covers `/checkin` and no other endpoint, and every access it
// admits writes an audit event.
//
// The file is rendered declaratively by the prism NixOS module, in the same
// manner as profiles.json:
//
//	{
//	  "privileged_repos": ["nixos-config"]
//	}
//
// Deployment through Nix is a security property, not a convenience.
// internal/container/mounts.go binds only agents/ and profiles.json out of
// ~/.config/prism/, and both read-only, so this file is invisible and
// unwritable from inside every sandbox. A hand-edited runtime file would not
// carry those properties.
//
// Two host-side readers exist, one per route of the verb (issue #2619). The
// sidecar reads the file once at start, for the host-API route. The direct
// CLI route reads it per invocation in cmd/checkin_permission.go, because a
// `host`-mode caller has no sidecar of its own to read it. Both readers treat
// an unreadable file as an empty list.
//
// A missing file is not an error: it yields an empty list, which grants the
// privilege to nobody and reproduces the pre-#2587 behaviour exactly.

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CheckinPrivilegedReposFileName is the basename of the rendered file under
// ~/.config/prism/.
const CheckinPrivilegedReposFileName = "checkin-privileged-repos.json"

// CheckinPrivilegedReposFile is the on-disk structure of
// checkin-privileged-repos.json.
type CheckinPrivilegedReposFile struct {
	// PrivilegedRepos holds repo names (not session names). The privilege
	// attaches to the coordinator of each named repo. Keying on repo derives
	// the session precisely and keeps wildcards off a privilege list.
	PrivilegedRepos []string `json:"privileged_repos"`
}

// CheckinPrivilegedReposPath returns the path to
// checkin-privileged-repos.json. Returns "" when no home directory is
// discoverable.
func CheckinPrivilegedReposPath() string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "prism", CheckinPrivilegedReposFileName)
}

// LoadCheckinPrivilegedRepos reads and parses checkin-privileged-repos.json
// and returns the repo names it declares.
//
// A missing file returns (nil, nil): the privilege is granted to nobody, which
// is the same behaviour prism had before the file existed. An unreadable or
// malformed file returns an error, and the caller must fail closed — treat the
// list as empty rather than as "everyone".
//
// Entries are trimmed of surrounding whitespace, and empty entries are
// dropped, so a stray "" in the rendered list cannot match a session whose
// repo failed to resolve.
func LoadCheckinPrivilegedRepos() ([]string, error) {
	path := CheckinPrivilegedReposPath()
	if path == "" {
		return nil, fmt.Errorf("checkin privilege: cannot determine %s path (no home directory)", CheckinPrivilegedReposFileName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Absent file — no repo is privileged. Not an error.
			return nil, nil
		}
		return nil, fmt.Errorf("checkin privilege: read %s: %w", path, err)
	}
	var f CheckinPrivilegedReposFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("checkin privilege: parse %s: %w", path, err)
	}

	var repos []string
	for _, repo := range f.PrivilegedRepos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		repos = append(repos, repo)
	}
	return repos, nil
}
