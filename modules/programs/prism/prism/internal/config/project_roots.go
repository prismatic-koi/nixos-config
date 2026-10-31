package config

import (
	"os"
	"path/filepath"
	"strings"
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

// HoldsProjectRoots reports whether dir holds prism project roots of c: dir
// is a project location, a dir above a project location, or a dir above a
// project_specific root. An empty dir holds nothing. It returns the entry of
// config.json that dir holds,
// with a leading "~/" expanded, and location is true when the entry is a
// project location. Each dir in a project location is a project root, so a
// project location holds the roots that a later prism clone makes too.
//
// The compare is by path, and by file identity with the entry and with each
// dir above it. So a dir that is reached through a symlink, or with another
// letter case on a case-insensitive macOS volume, matches. An entry that
// does not exist matches by path only.
func (c Config) HoldsProjectRoots(dir string) (entry string, location, ok bool) {
	return HoldsProjectRoots(c.ProjectLocations, c.ProjectSpecific, dir)
}

// HoldsProjectRoots is Config.HoldsProjectRoots for the entries locations
// (project_locations) and specific (project_specific).
func HoldsProjectRoots(locations, specific []string, dir string) (entry string, location, ok bool) {
	if dir == "" {
		return "", false, false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false, false
	}
	dirForms := pathForms(abs)
	dirInfo, statErr := os.Stat(abs)
	if statErr != nil || !dirInfo.IsDir() {
		dirInfo = nil
	}
	holds := func(e string, strict bool) bool {
		for _, ef := range pathForms(e) {
			for _, df := range dirForms {
				if atOrAbove(df, ef, strict) {
					return true
				}
			}
			if dirInfo == nil {
				continue
			}
			p := ef
			if strict {
				p = filepath.Dir(ef)
			}
			for {
				if info, err := os.Stat(p); err == nil && os.SameFile(dirInfo, info) {
					return true
				}
				if filepath.Dir(p) == p {
					break
				}
				p = filepath.Dir(p)
			}
		}
		return false
	}
	for _, loc := range locations {
		if e := ExpandHomePath(loc); filepath.IsAbs(e) && holds(e, false) {
			return filepath.Clean(e), true, true
		}
	}
	for _, spec := range specific {
		if e := ExpandHomePath(spec); filepath.IsAbs(e) && holds(e, true) {
			return filepath.Clean(e), false, true
		}
	}
	return "", false, false
}

// pathForms returns p, cleaned, and p with its symlinks resolved when that
// differs.
func pathForms(p string) []string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
		return []string{p, r}
	}
	return []string{p}
}

// atOrAbove reports whether dir is path or a dir above path. With strict,
// dir must be above path. Both paths must be absolute and clean.
func atOrAbove(dir, path string, strict bool) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return rel != "." || !strict
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
