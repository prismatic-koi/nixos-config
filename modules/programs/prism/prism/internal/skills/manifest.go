package skills

// ComputeManifest returns a stable hash-like identifier for the skills
// directory at skillsDir, suitable for storing in spawn_inputs.skills_manifest_hash.
//
// # Return shapes
//
//   - "nix:<basename>"   — when skillsDir is a symlink whose target is under
//     /nix/store/ (the normal NixOS/home-manager deployment). A single
//     os.Readlink call is enough; no file I/O is required, and two system
//     builds with identical skill content produce the same nix-store path
//     (content-addressed).
//
//   - "sha256:<hex>"    — when the directory is a plain (non-nix) tree.
//     The hash is computed by walking the tree in lexical order, hashing
//     each file's content, and then SHA-256'ing the concatenated
//     "<relative-path>\0<file-sha256-hex>\0" pairs. mtimes are excluded,
//     so the hash is deterministic across renames / rebuilds that do not
//     change content.
//
//   - ""                — when skillsDir does not exist. The caller should
//     write NULL to spawn_inputs.skills_manifest_hash rather than an empty
//     string, so that NULL unambiguously signals "not captured".
//
// # Opencode skill-loading lifecycle
//
// Skills are loaded once at session initialisation by opencode's Skill.state
// Ref (Effect-TS q.make call in the Skill service layer). The scan runs
// Z2() which walks the skills directories synchronously before the session
// event loop starts. After that point the skills set is frozen for the
// lifetime of the session — there is no watcher or per-invocation re-scan.
//
// Consequence: the spawn-time hash captured here is a complete manifest of
// "what skills the agent could have loaded during this session". A
// skills_manifest_hash_end column on sessions is NOT needed; the spawn-time
// value is authoritative for the full session.
//
// (This conclusion was reached by reading the opencode 1.14.18 JS bundle.
// If a future opencode version adds dynamic re-scanning, add
// skills_manifest_hash_end TEXT on the sessions table to capture session-end
// state, and file a TODO here.)

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ComputeManifest computes the skills manifest hash for skillsDir.
// See package-level doc for the three return shapes.
func ComputeManifest(skillsDir string) (string, error) {
	// Check whether the directory exists at all (also handles the case where
	// XDG_CONFIG_HOME points somewhere unusual).
	if _, err := os.Lstat(skillsDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("skills manifest: stat %q: %w", skillsDir, err)
	}

	// Fast path: nix-managed symlink → single os.Readlink, no file I/O.
	if target, err := os.Readlink(skillsDir); err == nil {
		if strings.HasPrefix(target, "/nix/store/") {
			return "nix:" + filepath.Base(target), nil
		}
	}

	// Slow path: plain directory — walk lexically, hash content.
	hash, err := contentHash(skillsDir)
	if err != nil {
		return "", fmt.Errorf("skills manifest: content hash %q: %w", skillsDir, err)
	}
	return "sha256:" + hash, nil
}

// ComputeAgentPromptHash returns a stable hash-like identifier for the agent
// role file at rolePath, suitable for storing in spawn_inputs.agent_prompt_hash.
//
// # Return shapes
//
//   - "nix:<basename>"   — when rolePath is a symlink whose target is under
//     /nix/store/ (or when the path resolves through symlinks in its parent
//     components to a nix-store path). Two builds with identical content
//     produce the same store path.
//
//   - "sha256:<hex>"    — SHA-256 of the file's contents, when the file is a
//     plain (non-nix) file.
//
//   - ""                — when rolePath does not exist. The caller should write
//     NULL to spawn_inputs.agent_prompt_hash.
func ComputeAgentPromptHash(rolePath string) (string, error) {
	if _, err := os.Lstat(rolePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("agent prompt hash: stat %q: %w", rolePath, err)
	}

	// Try symlink resolution on the file itself.
	if target, err := os.Readlink(rolePath); err == nil {
		if strings.HasPrefix(target, "/nix/store/") {
			return "nix:" + filepath.Base(target), nil
		}
	}

	// Try resolving the full path via EvalSymlinks (catches cases where parent
	// directories are symlinked into the nix store).
	if resolved, err := filepath.EvalSymlinks(rolePath); err == nil {
		if strings.HasPrefix(resolved, "/nix/store/") {
			// Use the first two path components after /nix/store/ as the
			// derivation name (e.g. "/nix/store/abc123-foo/bar.md" → "abc123-foo").
			rest := strings.TrimPrefix(resolved, "/nix/store/")
			parts := strings.SplitN(rest, "/", 2)
			return "nix:" + parts[0], nil
		}
	}

	// Fallback: hash the file content.
	data, err := os.ReadFile(rolePath)
	if err != nil {
		return "", fmt.Errorf("agent prompt hash: read %q: %w", rolePath, err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// contentHash walks dir in lexical order and returns the hex-encoded SHA-256
// of the concatenated "<relpath>\0<file-sha256-hex>\0" sequence.
// Directories are not directly hashed (only the files within them matter).
func contentHash(dir string) (string, error) {
	type entry struct {
		relPath string
		hash    string
	}
	var entries []entry

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		// Skip symlinks that point to directories; symlinks to files are
		// followed by reading the file content below.
		if d.Type()&fs.ModeSymlink != 0 {
			info, err := os.Stat(path)
			if err != nil {
				return nil // broken symlink — skip
			}
			if info.IsDir() {
				return nil
			}
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %q: %w", path, err)
		}
		sum := sha256.Sum256(data)
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("rel %q: %w", path, err)
		}
		entries = append(entries, entry{relPath: filepath.ToSlash(rel), hash: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return "", err
	}

	// Ensure deterministic order (WalkDir is already lexical on most platforms,
	// but sort explicitly for correctness guarantees).
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].relPath < entries[j].relPath
	})

	h := sha256.New()
	for _, e := range entries {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00", e.relPath, e.hash)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
