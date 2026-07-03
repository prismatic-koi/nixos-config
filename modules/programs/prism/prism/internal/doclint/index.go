package doclint

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// sourceIndex is a lookup structure over the prism source tree. Built once
// per Scan run.
type sourceIndex struct {
	prismRoot string
	repoRoot  string // optional; "" when only the prism subtree is available

	// idents is the set of every identifier-shaped word that appears in
	// any indexed source file. Used to satisfy Go-ident, SQL-ident,
	// env-var, and audit-token resolution.
	idents map[string]bool

	// strings is the concatenation of every quoted string literal that
	// appears in any indexed .go file. Used to look up audit-log reason
	// tokens (e.g. `host_bind:`) that live only as printf-style format
	// strings.
	strings string

	// basenames maps every bare filename encountered while walking to a
	// list of absolute paths where that filename lives. Used to satisfy
	// bare-filename references in docs (e.g. `` `policy.go` `` without a
	// directory prefix).
	basenames map[string][]string
}

var (
	// identRe extracts every identifier-shaped word from Go source. It is
	// deliberately loose — we don't care whether the word is a keyword, a
	// type, a var, or the middle of a comment. Presence anywhere is enough
	// to resolve a doc reference against it.
	identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

	// stringLitRe extracts double-quoted string literals from Go source.
	// It is a lexer-style heuristic and does not attempt to fully parse
	// escapes — good enough for prefix/substring lookup of audit reasons.
	stringLitRe = regexp.MustCompile(`"[^"\\\n]*(?:\\.[^"\\\n]*)*"`)
)

// indexableExtensions is the set of file extensions we extract identifiers
// from. .go files also contribute their string literals to the audit-log
// reason lookup; other file types only contribute identifier presence.
var indexableExtensions = map[string]bool{
	".go":  true,
	".nix": true,
	".ts":  true,
	".tsx": true,
	".js":  true,
	".jsx": true,
}

// buildSourceIndex walks the prism source tree (and, when repoRoot is set,
// the prism-adjacent files under <repoRoot>/modules/programs/prism/) and
// indexes every identifier and .go string literal it finds.
//
// The dual-context split lives here: in the nix sandbox where only the
// prism subtree is present, repoRoot is "" and only the Go source is
// indexed — that is enough for the docs shipped inside the subtree
// (podman-proxy.md, sandbox-exec-testing.md, stdout-capture-testing.md).
// In a full checkout the Nix and pi-TS source under
// modules/programs/prism/ is also indexed, so the repo-root AGENTS.md's
// references to identifiers like `agentMaxOpenFilesSoft` (in prism-tui.nix)
// or `BLOCKED_BASH_PATTERNS` (in pi/extensions/prism.ts) resolve cleanly.
func buildSourceIndex(prismRoot string) (*sourceIndex, error) {
	idx := &sourceIndex{
		prismRoot: prismRoot,
		idents:    map[string]bool{},
		basenames: map[string][]string{},
	}

	var sb strings.Builder

	walk := func(root string, indexGoStringLiterals bool) error {
		return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				base := d.Name()
				// Skip build/cache directories that never contain source
				// worth indexing.
				if base == ".git" || base == "vendor" || base == "node_modules" || base == "dist" || base == "target" || base == ".direnv" {
					return filepath.SkipDir
				}
				return nil
			}
			ext := extOf(path)
			// Record every file's basename for bare-filename lookups. Cheap
			// and covers .md, .sql, and other doc-referenced file types.
			if base := filepath.Base(path); base != "" {
				idx.basenames[base] = append(idx.basenames[base], path)
			}
			if !indexableExtensions[ext] {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range identRe.FindAll(b, -1) {
				idx.idents[string(m)] = true
			}
			if indexGoStringLiterals && ext == ".go" {
				for _, m := range stringLitRe.FindAll(b, -1) {
					sb.Write(m)
					sb.WriteByte('\n')
				}
			}
			return nil
		})
	}

	if err := walk(prismRoot, true); err != nil {
		return nil, err
	}
	return idx, finalizeStrings(idx, &sb)
}

// finalizeStrings copies the accumulated string-literal buffer into the
// index and is broken out so buildSourceIndexWithRepoRoot below can invoke
// buildSourceIndex + extend without duplicating logic.
func finalizeStrings(idx *sourceIndex, sb *strings.Builder) error {
	idx.strings = sb.String()
	return nil
}

// buildSourceIndexWithRepoRoot builds a source index that additionally
// covers files outside the prism Go subtree — Nix modules across the
// repo (`.nix` under `<repoRoot>` recursively) and the pi TS extension
// source at `<repoRoot>/modules/programs/prism/pi/`. Callers pass
// repoRoot="" when it is not available (nix sandbox build).
//
// The scan skips well-known cache/build dirs (.git, node_modules, dist,
// target, .direnv, result*, .cache) and the prism Go subtree that has
// already been walked. It also records every visited file's basename so
// bare-filename references (e.g. `AGENTS.md`, `flake.nix`) resolve.
func buildSourceIndexWithRepoRoot(prismRoot, repoRoot string) (*sourceIndex, error) {
	idx, err := buildSourceIndex(prismRoot)
	if err != nil {
		return nil, err
	}
	if repoRoot == "" {
		return idx, nil
	}
	idx.repoRoot = repoRoot

	return idx, filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "vendor" || base == "node_modules" || base == "dist" || base == "target" || base == ".direnv" || base == ".cache" {
				return filepath.SkipDir
			}
			// nix build symlinks (`result`, `result-*`) point into /nix/store
			// and would explode the walk; skip.
			if strings.HasPrefix(base, "result") {
				return filepath.SkipDir
			}
			// Skip the prism Go subtree we already indexed.
			if path == prismRoot {
				return filepath.SkipDir
			}
			return nil
		}
		ext := extOf(path)
		if base := filepath.Base(path); base != "" {
			idx.basenames[base] = append(idx.basenames[base], path)
		}
		// In the repo-root walk (unlike the prismRoot walk) markdown files
		// also contribute identifiers: agent prompt files under
		// modules/programs/prism/agents/ are the source of truth for review
		// verdict names (`PASS_WITH_DISAGREEMENT` etc.) and other tokens
		// that prism docs legitimately cross-reference. Prism's own docs/
		// tree is walked as prismRoot, which SkipDir-guards this path, so
		// we don't accidentally use a doc's prose as evidence that a token
		// resolves.
		if ext != ".md" && !indexableExtensions[ext] {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range identRe.FindAll(b, -1) {
			idx.idents[string(m)] = true
		}
		return nil
	})
}

func extOf(path string) string {
	i := strings.LastIndexByte(path, '.')
	if i < 0 {
		return ""
	}
	return path[i:]
}

// hasIdent reports whether the given identifier appears anywhere in an
// indexed source file.
func (idx *sourceIndex) hasIdent(name string) bool {
	return idx.idents[name]
}

// hasBasename reports whether any indexed file's basename equals name.
// Used by the file-with-member resolver so that a bare `policy.go`
// reference (no directory) resolves against the well-known
// internal/podmanproxy/policy.go.
func (idx *sourceIndex) hasBasename(name string) bool {
	return len(idx.basenames[name]) > 0
}

// hasStringLiteralContaining reports whether any Go string literal in the
// indexed source contains the given substring. Used to resolve audit-log
// reason prefixes like `host_bind:` that appear in the source only as
// printf format strings.
func (idx *sourceIndex) hasStringLiteralContaining(sub string) bool {
	return strings.Contains(idx.strings, sub)
}

// filePathExists reports whether a doc-cited relative path resolves against
// the prism source root or (if provided) the repo root.
//
// For repo-root-relative paths that begin with `modules/programs/prism/prism/`,
// the prefix is stripped and the remainder is resolved under prismRoot — that
// makes the nix-sandbox build (where only the prism subtree exists) resolve
// paths written in the repo-root form correctly.
func (idx *sourceIndex) filePathExists(rel string, repoRoot string) bool {
	rel = strings.TrimPrefix(rel, "./")
	// Repo-root-relative: try to satisfy under prismRoot via prefix strip,
	// and under repoRoot if we have it.
	if strings.HasPrefix(rel, "modules/programs/prism/prism/") {
		stripped := strings.TrimPrefix(rel, "modules/programs/prism/prism/")
		if _, err := os.Stat(filepath.Join(idx.prismRoot, stripped)); err == nil {
			return true
		}
	}
	if _, err := os.Stat(filepath.Join(idx.prismRoot, rel)); err == nil {
		return true
	}
	if repoRoot != "" {
		if _, err := os.Stat(filepath.Join(repoRoot, rel)); err == nil {
			return true
		}
	}
	return false
}
