package config

import (
	"os"
	"path/filepath"
)

// IsProjectRoot reports whether root is a prism project root of c: a
// directory in one of the ProjectLocations, or one of the ProjectSpecific
// paths.
func (c Config) IsProjectRoot(root string) bool {
	return IsProjectRoot(c.ProjectLocations, c.ProjectSpecific, root)
}

// IsProjectRoot reports whether root is a prism project root: a directory in
// one of locations (project_locations in config.json), or one of specific
// (project_specific). A leading "~/" in an entry is expanded. An entry that
// is not absolute after the expansion matches nothing.
//
// The compare is by file identity (SameDir), so a root that is reached
// through a symlink, or with another letter case on a case-insensitive macOS
// volume, matches.
func IsProjectRoot(locations, specific []string, root string) bool {
	if !filepath.IsAbs(root) {
		return false
	}
	root = filepath.Clean(root)
	parent := filepath.Dir(root)
	for _, loc := range locations {
		if SameDir(parent, ExpandHomePath(loc)) {
			return true
		}
	}
	for _, spec := range specific {
		if SameDir(root, ExpandHomePath(spec)) {
			return true
		}
	}
	return false
}

// SameDir reports whether a and b are the same directory. It compares by
// file identity, not by string, so a configured "~/documents/obsidian"
// matches "~/Documents/obsidian" on a case-insensitive macOS volume. b must
// be absolute.
func SameDir(a, b string) bool {
	if !filepath.IsAbs(b) {
		return false
	}
	ai, err := os.Stat(a)
	if err != nil || !ai.IsDir() {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}
