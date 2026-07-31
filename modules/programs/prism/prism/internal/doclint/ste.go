package doclint

// STE (ASD-STE100) mechanical prose checks.
//
// This layer runs alongside the identifier-resolution scan (scanDoc) but
// operates on a stripped view of the doc's prose. It reports five classes
// of finding, each identified by a rule tag of the form `ste-<section>-<name>`:
//
//   ste-8.1-semicolon   — Rule 8.1: literal `;` outside code.
//   ste-4.2-contraction — Rule 4.2: `'ll`, `'re`, `'ve`, `'d`, `n't`.
//                         Possessive `'s` is NOT a contraction and never fires.
//   ste-gr6-latin       — GR-6:     `e.g.`, `i.e.`, `etc.`.
//   ste-3.2-modal       — Rule 3.2: `should`, `would`, `may`, `might`, `could`.
//   ste-3.4-perfect     — Rule 3.4: `has been`, `have been`, `had been`.
//
// Deliberately NOT implemented (issue #2490):
//
//   - Sentence length (deferred to #2496)
//   - Slop words, `-ing` clauses (deferred to #2496)
//   - Passive voice, part-of-speech rulings, synonym rotation (permanently
//     out of scope; need a grammar parser and belong to human review)
//
// High precision beats high recall. A lint that false-positives on
// unrelated PRs gets deleted.

import (
	"bufio"
	"bytes"
	"regexp"
	"sort"
	"strings"
)

// steInScopeBasenames lists the docs (by basename, expected to live at
// <prismRoot>/docs/) that participate in STE scanning. Extending this set
// beyond the four measured docs is a deliberate scope decision — see the
// issue #2490 body and docs/doclint.md.
var steInScopeBasenames = map[string]bool{
	"doclint.md":                true,
	"podman-proxy.md":           true,
	"sandbox-exec-testing.md":   true,
	"stdout-capture-testing.md": true,
}

// isSteInScope returns true when the doc path is one of the four in-scope
// docs at <prismRoot>/docs/. Nested subdirectories (docs/invariants/,
// docs/diagnoses/) never participate in STE scanning.
func isSteInScope(path, prismRoot string) bool {
	// Match against a canonical <prismRoot>/docs/<basename> layout.
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	if !steInScopeBasenames[base] {
		return false
	}
	// Guard against a same-basename file appearing under docs/invariants/
	// or docs/diagnoses/ in a future refactor: require the parent dir to
	// be exactly "docs".
	parent := ""
	if i := strings.LastIndex(path, "/"); i >= 0 {
		rest := path[:i]
		if j := strings.LastIndex(rest, "/"); j >= 0 {
			parent = rest[j+1:]
		} else {
			parent = rest
		}
	}
	return parent == "docs"
}

// steCheck names a single STE rule and its detection regex.
type steCheck struct {
	rule string
	re   *regexp.Regexp
	note string
}

// steChecks is the ordered list of enabled STE checks. The order matches
// the presentation order in docs/doclint.md.
//
// Each regex is designed for high precision on the stripped prose view
// produced by steStripToProse:
//
//   - Fenced code blocks removed (via stripFencedBlocks).
//   - Inline backticked spans blanked to spaces (preserves offsets).
//   - HTML comments blanked (multi-line safe; preserves line numbers).
//
// After stripping, backticked example tokens like “ `should` “,
// “ `e.g.` “, “ `has been` “ never contribute findings. Docs that
// document these rules by name can therefore mention them safely.
var steChecks = []steCheck{
	{
		rule: "ste-8.1-semicolon",
		// Bare `;` outside code. The stripped view is prose only, so we
		// simply search for the character.
		re:   regexp.MustCompile(`;`),
		note: "Rule 8.1 forbids semicolons; write two sentences.",
	},
	{
		rule: "ste-4.2-contraction",
		// Match `'ll`, `'re`, `'ve`, `'d`, and `n't`. The word-character
		// prefix + `\b` at the end anchors the match. Possessive `'s` is
		// deliberately absent from the alternation — an AC.
		re:   regexp.MustCompile(`(?i)\b\w+(?:n't|'ll|'re|'ve|'d)\b`),
		note: "Rule 4.2 forbids contractions; write the full form.",
	},
	{
		rule: "ste-gr6-latin",
		// The trailing period is required so English words that happen to
		// start with `e`, `i`, or `etc` do not false-positive. We accept
		// only the three canonical spellings named in the issue.
		re:   regexp.MustCompile(`(?i)\b(?:e\.g\.|i\.e\.|etc\.)`),
		note: "GR-6 forbids Latin abbreviations; use 'for example', 'that is', 'and more'.",
	},
	{
		rule: "ste-3.2-modal",
		re:   regexp.MustCompile(`(?i)\b(?:should|would|may|might|could)\b`),
		note: "Rule 3.2 forbids these modals; use 'must' for a requirement, 'can' for capability, delete or restate a recommendation, 'If X, then Y' for a hypothetical.",
	},
	{
		rule: "ste-3.4-perfect",
		re:   regexp.MustCompile(`(?i)\b(?:has|have|had)\s+been\b`),
		note: "Rule 3.4 forbids perfect tenses; use the simple past or present.",
	},
}

// steFinding is an intermediate representation before promotion to the
// package-level Finding type.
type steFinding struct {
	line  int
	text  string
	rule  string
	note  string
	start int // byte offset within the full doc (for stable ordering)
}

// runSteChecks scans a doc's prose (already stripped of code and HTML
// comments) and returns the finding list.
//
// The `enabled` map, if non-nil, allows callers to disable specific
// rule tags — used by the revert-and-watch-fail tests that prove each
// check is not a no-op (AC in #2490).
func runSteChecks(prose []byte, enabled map[string]bool) []steFinding {
	var out []steFinding
	// Precompute line offsets so we can convert byte offsets to 1-based
	// line numbers cheaply.
	lines := lineOffsets(prose)
	for _, c := range steChecks {
		if enabled != nil && !enabled[c.rule] {
			continue
		}
		for _, m := range c.re.FindAllIndex(prose, -1) {
			start := m[0]
			text := string(prose[start:m[1]])
			out = append(out, steFinding{
				line:  lineFromOffset(lines, start),
				text:  text,
				rule:  c.rule,
				note:  c.note,
				start: start,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].line != out[j].line {
			return out[i].line < out[j].line
		}
		if out[i].start != out[j].start {
			return out[i].start < out[j].start
		}
		return out[i].rule < out[j].rule
	})
	return out
}

// steStripToProse produces the prose-only view of a doc used by the STE
// checks: fenced code blocks blanked, inline backticked spans blanked,
// HTML comments blanked. Line numbers of the surviving prose are
// preserved so findings can report the true source line.
func steStripToProse(content []byte) []byte {
	// stripFencedBlocks preserves line numbers by writing a bare `\n`
	// for every removed line.
	prose := stripFencedBlocks(content)
	// Blank multi-line HTML comments in place, preserving line count.
	prose = blankHTMLComments(prose)
	// Blank single-line and inline backticked spans, preserving offsets.
	prose = blankBacktickedSpans(prose)
	return prose
}

// blankHTMLComments replaces every `<!-- ... -->` (possibly multi-line)
// with spaces, keeping newlines intact so line numbers do not shift.
// A missing terminator drops to the end of the buffer (matching
// stripHTMLComments's single-line policy).
func blankHTMLComments(buf []byte) []byte {
	out := make([]byte, len(buf))
	copy(out, buf)
	for {
		i := bytes.Index(out, []byte("<!--"))
		if i < 0 {
			return out
		}
		j := bytes.Index(out[i:], []byte("-->"))
		if j < 0 {
			// Blank to end of buffer, preserving newlines.
			for k := i; k < len(out); k++ {
				if out[k] != '\n' {
					out[k] = ' '
				}
			}
			return out
		}
		end := i + j + len("-->")
		for k := i; k < end; k++ {
			if out[k] != '\n' {
				out[k] = ' '
			}
		}
	}
}

// blankBacktickedSpans replaces every “ `...` “ and “ “...“ “ on a
// single line with spaces, preserving offsets and newlines. Multi-line
// backticked spans are rare in these docs and are not supported here.
func blankBacktickedSpans(buf []byte) []byte {
	out := make([]byte, 0, len(buf))
	sc := bufio.NewScanner(bytes.NewReader(buf))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	first := true
	for sc.Scan() {
		if !first {
			out = append(out, '\n')
		}
		first = false
		line := sc.Bytes()
		// Blank double-backtick spans first (must not re-enter the
		// single-backtick pass).
		blanked := make([]byte, len(line))
		copy(blanked, line)
		for _, m := range doubleBacktickRe.FindAllIndex(blanked, -1) {
			for k := m[0]; k < m[1]; k++ {
				blanked[k] = ' '
			}
		}
		for _, m := range backtickRe.FindAllIndex(blanked, -1) {
			for k := m[0]; k < m[1]; k++ {
				blanked[k] = ' '
			}
		}
		out = append(out, blanked...)
	}
	return out
}

// lineOffsets returns the byte offset of every `\n` in buf, allowing
// O(log n) line-from-offset lookups.
func lineOffsets(buf []byte) []int {
	// Sentinel offsets: -1 at index 0 means the first line starts at
	// byte 0. Each entry after that is the offset of a `\n`.
	offsets := []int{-1}
	for i, b := range buf {
		if b == '\n' {
			offsets = append(offsets, i)
		}
	}
	return offsets
}

// lineFromOffset returns the 1-based line number for a byte offset.
func lineFromOffset(offsets []int, off int) int {
	// Binary search for the largest offset <= off.
	lo, hi := 0, len(offsets)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if offsets[mid] <= off {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	// offsets[0] = -1 is the sentinel meaning "before line 1".
	return lo + 1
}
