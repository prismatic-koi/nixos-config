package doclint

import (
	"bufio"
	"bytes"
	"regexp"
	"strings"
)

// tokenOccurrence is a single backticked token seen in a doc file, with its
// stripped text and 1-based line number.
type tokenOccurrence struct {
	Raw  string // as it appeared inside the backticks
	Text string // stripped of trailing punctuation
	Line int
}

// codeFenceRe matches the start of a fenced code block (```lang or ~~~lang).
var codeFenceRe = regexp.MustCompile(`^\s*(` + "```" + `|~~~)`)

// ignoreDirectiveRe matches an HTML-comment annotation that lists tokens to
// exempt from the lint for this file:
//
//	<!-- doclint-ignore: token1, token2, token3 -->
//
// Whitespace inside the list is ignored. Multiple directives per file are
// allowed and their lists union.
//
// The regex is anchored to line start (`(?m)^\s*<!--`) so that a directive
// appearing INLINE inside a backticked prose example — e.g. a heading
// like “ `<!-- doclint-ignore: token1, token2 -->` “ — is not
// accidentally honoured. That anchor plus the stripFencedBlocks pass in
// extractIgnoreSet closes both the fenced-block leak and the inline-
// backticked leak.
var ignoreDirectiveRe = regexp.MustCompile(`(?ms)^\s*<!--\s*doclint-ignore:\s*(.*?)\s*-->`)

// skipFileDirectiveRe matches a whole-file opt-out annotation:
//
//	<!-- doclint-skip-file: this doc describes an external interface -->
//
// The reason text after the colon is required (so the exemption is
// self-documenting) but its content is not otherwise inspected.
//
// Same line-start anchoring and fenced-block guard as ignoreDirectiveRe
// apply, for the same reason.
//
// The capture group holds the directive body (everything after
// `doclint-skip-file:`), which parseSkipFileDirective splits into an
// optional class list and a reason. See docs/doclint.md for the syntax.
var skipFileDirectiveRe = regexp.MustCompile(`(?m)^\s*<!--\s*doclint-skip-file:\s*([^\n]+?)\s*-->`)

// backtickRe matches a single-backtick span on one line (double backticks are
// used to quote content that itself contains a backtick — we match those with
// a separate pass, but they are rare in these docs).
var backtickRe = regexp.MustCompile("`([^`\n]+)`")

// doubleBacktickRe matches a “…“ span (which may contain single backticks
// inside). Kept for completeness even though the current docs do not use it.
var doubleBacktickRe = regexp.MustCompile("``([^\n]+?)``")

// extractIgnoreSet parses all doclint-ignore directives from the document
// content and returns the union of listed tokens.
//
// Directives inside fenced code blocks are prose examples — they are
// stripped before regex matching so that a doc that documents the
// directive syntax does not accidentally contribute tokens to its own
// ignore set. The whole-file skip guard in parseSkipFileDirective
// applies the same strip for the same reason.
func extractIgnoreSet(content []byte) map[string]bool {
	prose := stripFencedBlocks(content)
	out := map[string]bool{}
	for _, m := range ignoreDirectiveRe.FindAllSubmatch(prose, -1) {
		list := string(m[1])
		for _, tok := range strings.Split(list, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			out[tok] = true
		}
	}
	return out
}

// skipFileScope records which lint categories a `doclint-skip-file`
// directive suppresses.
//
// An unparameterised directive (`<!-- doclint-skip-file: reason -->`)
// suppresses both categories: both fields are true. A
// scoped directive names one or more categories before a `|`
// separator (`<!-- doclint-skip-file: identifiers | reason -->` or
// `<!-- doclint-skip-file: identifiers, ste | reason -->`) and only
// those categories are suppressed.
//
// Recognised class names: `identifiers`, `ste`. Unknown names are
// silently ignored so a typo does not accidentally widen the skip.
// Callers must therefore treat an all-false scope as a no-op.
type skipFileScope struct {
	identifiers bool
	ste         bool
}

// parseSkipFileDirective returns the scope of the first
// `doclint-skip-file` directive in content, plus a boolean indicating
// whether any directive was present. When no directive is present the
// returned scope is the zero value.
//
// As with extractIgnoreSet, fenced code blocks are stripped first —
// a documentation example of the directive syntax must not silently
// opt the doc that documents it out of the lint.
func parseSkipFileDirective(content []byte) (skipFileScope, bool) {
	prose := stripFencedBlocks(content)
	m := skipFileDirectiveRe.FindSubmatch(prose)
	if m == nil {
		return skipFileScope{}, false
	}
	body := string(m[1])
	pipe := strings.Index(body, "|")
	if pipe < 0 {
		// Unparameterised: global scope. The whole body is the reason.
		return skipFileScope{identifiers: true, ste: true}, true
	}
	var scope skipFileScope
	for _, c := range strings.Split(body[:pipe], ",") {
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "identifiers":
			scope.identifiers = true
		case "ste":
			scope.ste = true
		}
	}
	return scope, true
}

// hasSkipFileDirective reports whether the document opts out of
// every doclint category. For per-category scope, use
// parseSkipFileDirective and inspect its fields.
func hasSkipFileDirective(content []byte) bool {
	scope, ok := parseSkipFileDirective(content)
	return ok && scope.identifiers && scope.ste
}

// stripFencedBlocks returns content with every ```-fenced or ~~~-fenced
// code block removed (each fenced line is replaced with a blank line so
// line numbers of the surviving prose are preserved for any future
// callers that care). Directive-detection regexes run against this
// stripped view.
func stripFencedBlocks(content []byte) []byte {
	var out []byte
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	inFence := false
	for sc.Scan() {
		line := sc.Bytes()
		if codeFenceRe.Match(line) {
			inFence = !inFence
			out = append(out, '\n')
			continue
		}
		if inFence {
			out = append(out, '\n')
			continue
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out
}

// extractTokens returns all backticked tokens outside fenced code blocks.
func extractTokens(content []byte) []tokenOccurrence {
	var out []tokenOccurrence

	sc := bufio.NewScanner(bytes.NewReader(content))
	// Some docs (pi-wire-protocol.md at ~1200 lines with long lines) exceed
	// the default 64 KiB scanner buffer with pathological wrap. Give the
	// scanner a large max so we don't silently drop lines.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	inFence := false
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if codeFenceRe.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}

		// Strip HTML comments (which may contain backticks in doc examples,
		// including our own doclint-ignore directive with example tokens).
		stripped := stripHTMLComments(line)

		// Double-backtick spans first (rare but supported), then single.
		remaining := stripped
		for _, m := range doubleBacktickRe.FindAllStringSubmatchIndex(remaining, -1) {
			raw := remaining[m[2]:m[3]]
			out = append(out, tokenOccurrence{Raw: raw, Text: stripTrailingPunct(raw), Line: lineNo})
		}
		// Blank out matched double-backtick spans so single-backtick pass
		// does not re-match their contents.
		remaining = doubleBacktickRe.ReplaceAllString(remaining, " ")

		for _, m := range backtickRe.FindAllStringSubmatch(remaining, -1) {
			raw := m[1]
			out = append(out, tokenOccurrence{Raw: raw, Text: stripTrailingPunct(raw), Line: lineNo})
		}
	}
	return out
}

// stripHTMLComments removes any inline HTML comment (<!-- ... -->) from a
// single line. Multi-line HTML comments are handled by the ignoreDirectiveRe
// pass on the whole file; this only affects the single-line token scan.
func stripHTMLComments(line string) string {
	for {
		i := strings.Index(line, "<!--")
		if i < 0 {
			return line
		}
		j := strings.Index(line[i:], "-->")
		if j < 0 {
			// Unterminated on this line; drop the rest.
			return line[:i]
		}
		line = line[:i] + line[i+j+3:]
	}
}

// stripTrailingPunct trims trailing punctuation that a writer commonly leaves
// inside the backticks by accident, e.g. `Foo.Bar,` -> `Foo.Bar`. We do not
// strip leading punctuation because a leading `-` or `$` is meaningful
// (CLI flag, env var reference).
func stripTrailingPunct(s string) string {
	return strings.TrimRight(s, ".,;:!?)]}")
}
