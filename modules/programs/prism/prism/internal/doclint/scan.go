package doclint

import (
	"os"
	"path/filepath"
)

// scanDoc runs the token extract + classify + resolve pipeline on a single
// markdown file and returns the findings for that file.
func scanDoc(path string, idx *sourceIndex) ([]Finding, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		// The dual-context constraint: if a doc file disappeared between
		// discoverDocs and scanDoc, treat it as absent (no findings).
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Whole-file opt-out for docs that describe an interface living outside
	// this repository (e.g. pi-rpc-interface.md, pi-wire-protocol.md, whose
	// identifiers live in the external pi coding-agent package).
	if hasSkipFileDirective(content) {
		return nil, nil
	}

	ignore := extractIgnoreSet(content)
	tokens := extractTokens(content)

	// Locate repoRoot by walking up from the doc file's directory until we
	// see an AGENTS.md sibling; used by filePathExists so that a repo-root
	// AGENTS.md can cite `modules/...` paths that also exist under
	// prismRoot with the prefix stripped. Passing "" is safe (means no
	// repo-root disk lookup fallback).
	repoRoot := findRepoRootForDoc(path, idx.prismRoot)

	var findings []Finding
	seen := map[string]bool{} // per-file dedup key: line+token+rule
	for _, tok := range tokens {
		if tok.Text == "" {
			continue
		}
		if ignore[tok.Text] || ignore[tok.Raw] {
			continue
		}
		class := classify(tok.Text)
		if class == classSkip {
			continue
		}
		ok, rule, note := resolve(tok.Text, class, idx, repoRoot)
		if ok {
			continue
		}
		key := ruleKey(tok.Line, tok.Text, rule)
		if seen[key] {
			continue
		}
		seen[key] = true
		findings = append(findings, Finding{
			File:  path,
			Line:  tok.Line,
			Token: tok.Text,
			Rule:  rule,
			Note:  note,
		})
	}
	return findings, nil
}

func ruleKey(line int, token, rule string) string {
	return token + "|" + rule + "|" + itoa(line)
}

// itoa avoids the strconv import for a tiny helper.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		n = -n
		neg = true
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// findRepoRootForDoc returns the repo root containing the doc file, if it
// can be determined by looking at the doc's directory ancestors. Returns
// "" if no ancestor has an AGENTS.md that is not inside the prism subtree
// (i.e. we could not resolve the repo root).
func findRepoRootForDoc(docPath, prismRoot string) string {
	// If the doc lives under prismRoot/docs, the repo root is four levels
	// above prismRoot (modules/programs/prism/prism/ -> repo).
	if abs, err := filepath.Abs(docPath); err == nil {
		if abs != docPath {
			docPath = abs
		}
	}
	if inside(prismRoot, docPath) {
		candidate := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(prismRoot))))
		if _, err := os.Stat(filepath.Join(candidate, "AGENTS.md")); err == nil {
			return candidate
		}
		return ""
	}
	// Doc lives at the repo root or elsewhere — the doc's own directory
	// is the repo root candidate.
	return filepath.Dir(docPath)
}

func inside(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return len(rel) > 0 && rel[0] != '.' && !filepath.IsAbs(rel)
}
