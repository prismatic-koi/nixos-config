// Package iris — sandbox_file_tools.go
//
// D-4: per-tool sandbox for the six file tools (read, edit, write, grep, find, ls).
//
// This file contains the OS-independent helpers:
//   - validateToolPath — Go-side path validation (primary enforcement)
//   - isUnder, resolvePartial — helpers for the validator
//   - sbplQuote — SBPL path quoting (used by Darwin profile generator)
//   - fileToolEnvArgs — minimal env vars for file tool subprocesses
//   - resolveBinaryPath — resolve a tool name to its /nix/store/... path
//
// OS-specific sandbox implementations live in:
//   - file_sandbox_linux.go  — bwrap
//   - file_sandbox_darwin.go — sandbox-exec

package iris

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// validateToolPath validates that a tool's path argument is safe to execute
// against.  It:
//
//  1. Converts relative paths to absolute using worktree as the base.
//  2. Resolves symlinks (filepath.EvalSymlinks) to detect symlink traversal.
//  3. Collapses ".." segments on the result of (2) via filepath.Clean.
//  4. Rejects the path if it is not rooted under worktree OR under tmpDir.
//
// tmpDir is the per-session /tmp backing directory on the host
// (~/.local/state/iris/run/<instance_id>/tmp/).  Paths inside it are allowed
// because the sandbox maps it to /tmp inside the subprocess.
//
// On success the resolved absolute path is returned.
// On failure a nil string and a descriptive error are returned.
func validateToolPath(worktree, tmpDir, rawPath string) (string, error) {
	// Make absolute relative to the worktree.
	abs := rawPath
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(worktree, rawPath)
	}
	abs = filepath.Clean(abs)

	// Resolve symlinks to detect traversal via symlinks.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// If the path doesn't exist yet (e.g. write to a new file), EvalSymlinks
		// will fail.  Walk up the tree to find the deepest existing ancestor,
		// then re-join the suffix.
		resolved, err = resolvePartial(abs)
		if err != nil {
			return "", fmt.Errorf("path %q: cannot resolve: %w", rawPath, err)
		}
	}
	resolved = filepath.Clean(resolved)

	// Check whether the resolved path is under worktree or tmpDir.
	if isUnder(resolved, worktree) || (tmpDir != "" && isUnder(resolved, tmpDir)) {
		return resolved, nil
	}

	return "", fmt.Errorf("path %q is outside the session worktree and not under /tmp", rawPath)
}

// isUnder reports whether child is equal to base or is a descendant of it.
func isUnder(child, base string) bool {
	if child == base {
		return true
	}
	return strings.HasPrefix(child, base+string(filepath.Separator))
}

// resolvePartial resolves symlinks as deep as possible for a path that may
// not fully exist yet (e.g. a file that is about to be written).  It walks
// upward until it finds an existing prefix, resolves that prefix through
// EvalSymlinks, then re-attaches the non-existing suffix.
func resolvePartial(abs string) (string, error) {
	// Split the path into parts and find the deepest existing ancestor.
	dir := abs
	var suffix []string
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached root without finding anything — fall back to Clean.
			return abs, nil
		}
		suffix = append([]string{filepath.Base(dir)}, suffix...)
		dir = parent
	}

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return abs, nil
	}
	return filepath.Join(append([]string{resolved}, suffix...)...), nil
}

// sbplQuote wraps a path in SBPL double-quote syntax, escaping backslashes
// and double-quotes.  This is a copy of the unexported quoteSBPL in
// internal/container/sandbox_exec.go — reproduced here so the iris package
// does not need to import the container package just for quoting.
func sbplQuote(path string) string {
	escaped := strings.ReplaceAll(path, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	return "\"" + escaped + "\""
}

// fileToolEnvArgs returns the --setenv pairs for the standard set of
// environment variables that file tools need.  This is a minimal set:
// PATH, HOME, USER — enough for binary resolution and basic operations.
// Credentials are explicitly excluded.
func fileToolEnvArgs() []string {
	var args []string

	pathVal := os.Getenv("PATH")
	if pathVal == "" {
		pathVal = "/run/current-system/sw/bin:/nix/var/nix/profiles/default/bin:/usr/bin:/bin"
	}
	args = append(args, "--setenv", "PATH", pathVal)

	for _, key := range []string{"HOME", "USER", "LOGNAME", "LANG", "LC_ALL"} {
		if val := os.Getenv(key); val != "" {
			args = append(args, "--setenv", key, val)
		}
	}

	return args
}

// resolveBinaryPath resolves a binary name (e.g. "cat", "grep", "sh") to its
// absolute path on the host.  It intentionally does NOT follow symlinks to
// the final target: on NixOS, tools like 'cat' are symlinks to 'coreutils'
// which uses argv[0] to determine which tool to run.  If we resolved 'cat'
// all the way to 'coreutils', the binary would not know it should behave as
// 'cat'.  Instead we return the first-level absolute path from LookPath
// (e.g. /run/current-system/sw/bin/cat), which is accessible inside the
// sandbox because we mount /run/current-system RO.
func resolveBinaryPath(name string) (string, error) {
	// If it's already absolute, use it directly.
	if filepath.IsAbs(name) {
		return name, nil
	}

	// Look up in PATH — returns the absolute path without following symlinks.
	found, err := exec.LookPath(name)
	if err != nil {
		// Fallback: try common well-known locations for basic tools.
		for _, prefix := range []string{"/bin", "/usr/bin", "/run/current-system/sw/bin"} {
			candidate := filepath.Join(prefix, name)
			if _, statErr := os.Stat(candidate); statErr == nil {
				return candidate, nil
			}
		}
		return "", fmt.Errorf("binary %q not found in PATH: %w", name, err)
	}
	return found, nil
}
