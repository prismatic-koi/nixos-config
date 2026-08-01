package doclint

// Sentence-length check for STE Rule 6.3 (descriptive) — 25 words per
// sentence. Rule 5.1's stricter 20-word procedural limit is NOT applied:
// the lint cannot classify a passage as procedural or descriptive, so
// the more permissive descriptive limit applies uniformly. Documented
// in docs/doclint.md.
//
// This check runs on a DIFFERENT view of the doc than the other STE
// checks. The stripped-prose view used by runSteChecks replaces every
// backticked span with spaces, which destroys the "each backticked
// span counts as one word" semantics from Rule 8.6. So this check
// consumes the raw content and does its own line classification,
// sentence segmentation, and Rule 8.4-8.7 tokenisation.
//
// Design bias — precision over recall. Per docs/doclint.md's governing
// principle ("a lint that false-positives on unrelated PRs will get
// deleted") the tokeniser prefers to UNDERCOUNT words rather than
// overcount them. Every rule that admits ambiguity (list items, tables,
// blockquotes, embedded code, quoted text) is resolved in the direction
// that PREVENTS a finding. The false-positive rate is asserted at the
// integration-test layer against the four in-scope docs.

import (
	"bytes"
	"regexp"
	"strings"
	"unicode"
)

// The uniform per-sentence word limit. See package comment.
const sentenceWordLimit = 25

// Regexes used by the sentence-length pass. All are used against the
// stripped-of-code, stripped-of-comments view.
var (
	// listItemPrefixRe: a line whose first non-blank character starts a
	// markdown list item (unordered `-`, `*`, `+` or ordered `1.`, `12)`).
	// Nested lists (indented list markers) also match.
	listItemPrefixRe = regexp.MustCompile(`^\s*(?:[-*+]\s+|\d+[.)]\s+)`)

	// tableRowRe: a line whose first non-blank character is `|`. Covers
	// header rows, separator rows (`| --- |`), and body rows.
	tableRowRe = regexp.MustCompile(`^\s*\|`)

	// headingRe: an ATX heading line.
	headingRe = regexp.MustCompile(`^\s{0,3}#{1,6}\s`)

	// blockquoteRe: a blockquote line.
	blockquoteRe = regexp.MustCompile(`^\s*>`)

	// indentedCodeRe: a line indented by four or more spaces (or a tab).
	// In CommonMark this is an indented code block ONLY when it does not
	// sit inside a list, but a bare four-space indent under a plain
	// paragraph is nearly always code in these docs, so we skip it.
	indentedCodeRe = regexp.MustCompile(`^(?:    |\t)`)

	// setextUnderlineRe: `====` or `----` line following a heading. We
	// treat these as heading-adjacent and skip.
	setextUnderlineRe = regexp.MustCompile(`^\s*(=+|-+)\s*$`)

	// htmlBlockRe: a line that starts with an HTML tag, comment, or
	// directive residue. These get skipped as block-level markup.
	htmlBlockRe = regexp.MustCompile(`^\s*<`)

	// numberWithUnitRe: a number optionally followed by a unit, treated
	// as one word per Rule 8.6. Matches `5s`, `5 s`, `10ms`, `1.5h`,
	// `10 MB`, `100%`. Greedy on the numeric side; conservative on the
	// unit (letters only, up to five). Deliberately does NOT match a
	// number followed by a general word (e.g. "5 apples") because that
	// is not a unit.
	numberWithUnitRe = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:\s?[A-Za-z%]{1,5})?\b`)

	// quotedTextRe: text inside straight double quotes on a single
	// line, treated as one word per Rule 8.6.
	quotedTextRe = regexp.MustCompile(`"[^"\n]+"`)
)

// runSentenceLengthCheck scans a document for sentences that exceed
// sentenceWordLimit. Returns steFindings ready for promotion.
//
// If `enabled` is non-nil and enabled["ste-6.3-sentence-length"] is
// false, the check is a no-op — used by the revert-and-watch-fail
// test that proves the check is not vacuous.
func runSentenceLengthCheck(content []byte, enabled map[string]bool) []steFinding {
	const rule = "ste-6.3-sentence-length"
	if enabled != nil && !enabled[rule] {
		return nil
	}

	// Strip fenced code blocks and HTML comments the same way runSteChecks
	// does — those are always out of scope for prose measurement.
	buf := stripFencedBlocks(content)
	buf = blankHTMLComments(buf)

	// Group contiguous prose lines into paragraphs. A prose line is one
	// that is NOT a heading, list item, table row, blockquote, indented
	// code, HTML block, or setext underline.
	paragraphs := extractProseParagraphs(buf)

	var out []steFinding
	for _, p := range paragraphs {
		// Segment each paragraph into sentences and word-count each.
		for _, s := range segmentSentences(p.text) {
			if s.text == "" {
				continue
			}
			words := countWordsSTE(s.text)
			if words <= sentenceWordLimit {
				continue
			}
			// Trim the reported text so it fits on a diagnostic line.
			trim := strings.TrimSpace(s.text)
			if len(trim) > 80 {
				trim = trim[:77] + "..."
			}
			out = append(out, steFinding{
				line:  p.startLine + s.lineOffset,
				text:  trim,
				rule:  rule,
				note:  "Rule 6.3 limits a descriptive sentence to 25 words; write two sentences.",
				start: p.startByte + s.offset,
			})
		}
	}
	return out
}

// proseParagraph is a contiguous run of markdown prose lines, with the
// original 1-based line number of the paragraph's first line.
type proseParagraph struct {
	text      string
	startLine int // 1-based line number of the first line of the paragraph
	startByte int // byte offset in the stripped buffer where the paragraph begins
}

// extractProseParagraphs returns the prose paragraph blocks in a
// pre-stripped buffer. Non-prose lines (headings, list items, table rows,
// blockquotes, indented code, HTML block markup, setext underlines,
// blank lines) act as paragraph terminators and never contribute text.
func extractProseParagraphs(buf []byte) []proseParagraph {
	var out []proseParagraph
	lines := bytes.Split(buf, []byte{'\n'})

	// Track byte offsets of each line so paragraphs can report an
	// absolute offset back to the caller.
	lineOffsets := make([]int, len(lines))
	off := 0
	for i, line := range lines {
		lineOffsets[i] = off
		off += len(line) + 1 // +1 for the '\n'
	}

	var cur []string
	var curStartLine int
	var curStartByte int
	flush := func() {
		if len(cur) == 0 {
			return
		}
		out = append(out, proseParagraph{
			text:      strings.Join(cur, " "),
			startLine: curStartLine,
			startByte: curStartByte,
		})
		cur = nil
	}

	// Track whether we are inside a list item's continuation. A list item's
	// wrapped/continuation lines are indented by two-or-more spaces and
	// belong to the item, not to a fresh prose paragraph. Without this,
	// wrapped list items look like new prose blocks and false-positive
	// against the sentence-length limit.
	inListItem := false
	for i, lineBytes := range lines {
		line := string(lineBytes)
		lineNo := i + 1

		if strings.TrimSpace(line) == "" {
			flush()
			// A blank line ends the list item's continuation window as well.
			inListItem = false
			continue
		}
		if listItemPrefixRe.MatchString(line) {
			flush()
			inListItem = true
			continue
		}
		if inListItem && isListItemContinuation(line) {
			continue
		}
		if headingRe.MatchString(line) ||
			setextUnderlineRe.MatchString(line) ||
			tableRowRe.MatchString(line) ||
			blockquoteRe.MatchString(line) ||
			indentedCodeRe.MatchString(line) ||
			htmlBlockRe.MatchString(line) {
			flush()
			inListItem = false
			continue
		}
		// A left-aligned prose line ends the list-item continuation window.
		inListItem = false
		if len(cur) == 0 {
			curStartLine = lineNo
			curStartByte = lineOffsets[i]
		}
		cur = append(cur, line)
	}
	flush()
	return out
}

// isListItemContinuation reports whether a line reads as the continuation
// of a preceding list item: any indent of two spaces or more.
func isListItemContinuation(line string) bool {
	if len(line) < 2 {
		return false
	}
	return (line[0] == ' ' && line[1] == ' ') || line[0] == '\t'
}

// sentence is a segmented sentence within a paragraph, with its offset
// (byte + line) relative to the paragraph.
type sentence struct {
	text       string
	offset     int
	lineOffset int
}

// segmentSentences splits a joined paragraph into sentences per the
// STE-relevant terminators:
//
//   - `.`, `!`, `?` followed by whitespace + capital letter (or end of
//     paragraph) — the standard NLP heuristic.
//   - Trailing `:` at the end of the paragraph (Rule 8.4: a vertical-list
//     lead-in colon ends a sentence for word-count purposes).
//
// The heuristic under-detects: a sentence terminator not followed by a
// capital letter (e.g. `.md`, `1.5`, `U.S. president`) does not split.
// That is the precision-preferring choice.
func segmentSentences(paragraph string) []sentence {
	var out []sentence
	if paragraph == "" {
		return nil
	}

	// Collect candidate split points.
	// A split point splits the sentence AFTER the punctuation character.
	splits := []int{} // byte indices IMMEDIATELY AFTER the terminator
	runes := []rune(paragraph)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		// Absorb any run of the same class (e.g. `!!`, `?!`).
		j := i
		for j+1 < len(runes) && (runes[j+1] == '.' || runes[j+1] == '!' || runes[j+1] == '?') {
			j++
		}
		// After the terminator run, skip inline markdown emphasis markers
		// (`*`, `_`, `**`, `__`) and closing brackets/parens that commonly
		// abut a sentence terminator (e.g. `for v1.**`, `see (x).`). The
		// character AFTER those, if any, is the split-candidate.
		k := j + 1
		for k < len(runes) && (runes[k] == '*' || runes[k] == '_' || runes[k] == ')' || runes[k] == ']') {
			k++
		}
		// Peek at the next non-space rune from k.
		for k < len(runes) && unicode.IsSpace(runes[k]) {
			k++
		}
		splitAt := j + 1
		if k >= len(runes) {
			// Terminator at end of paragraph — always splits.
			splits = append(splits, byteOffsetOfRune(runes, splitAt))
			i = j
			continue
		}
		// Split when the next non-space rune is a capital letter or a
		// heading-ish emphasis start (`*`, `_`, `(`, `[`, backtick).
		if unicode.IsUpper(runes[k]) || runes[k] == '*' || runes[k] == '_' || runes[k] == '(' || runes[k] == '[' || runes[k] == '`' {
			splits = append(splits, byteOffsetOfRune(runes, splitAt))
			i = j
			continue
		}
		i = j
	}
	// Rule 8.4: trailing `:` on the last non-space char of the paragraph.
	trimmed := strings.TrimRightFunc(paragraph, unicode.IsSpace)
	if strings.HasSuffix(trimmed, ":") {
		splits = append(splits, len(trimmed))
	}

	last := 0
	for _, s := range splits {
		if s <= last {
			continue
		}
		if s > len(paragraph) {
			s = len(paragraph)
		}
		frag := paragraph[last:s]
		lineOff := strings.Count(paragraph[:last], "\n")
		out = append(out, sentence{
			text:       frag,
			offset:     last,
			lineOffset: lineOff,
		})
		last = s
	}
	// Trailing text after the last split (unterminated final sentence).
	if last < len(paragraph) {
		frag := paragraph[last:]
		if strings.TrimSpace(frag) != "" {
			lineOff := strings.Count(paragraph[:last], "\n")
			out = append(out, sentence{
				text:       frag,
				offset:     last,
				lineOffset: lineOff,
			})
		}
	}
	return out
}

// byteOffsetOfRune returns the byte offset in the source string for the
// rune at index i in the []rune slice.
func byteOffsetOfRune(runes []rune, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(runes) {
		return len(string(runes))
	}
	return len(string(runes[:i]))
}

// countWordsSTE counts the words in a sentence using the STE tokenisation
// rules from Section 8:
//
//   - Rule 8.5: text inside parentheses counts as ONE word.
//   - Rule 8.6: a backticked span, a number with a unit, quoted text,
//     an abbreviation, or an alphanumeric identifier counts as ONE word.
//   - Rule 8.7: a hyphenated word counts as ONE word.
//
// Implementation strategy: replace each "counts as one word" pattern
// with a single placeholder token before splitting on whitespace. The
// order matters — backticked and parenthesised text are consumed first
// so their internal characters cannot be re-tokenised.
func countWordsSTE(s string) int {
	// Rule 8.6: backticked span → 1 token. Two passes so double-backticks
	// don't fall into the single-backtick regex.
	s = doubleBacktickRe.ReplaceAllString(s, " § ")
	s = backtickRe.ReplaceAllString(s, " § ")
	// Rule 8.5: text inside parentheses → 1 token. Non-nested only.
	s = parenSpanRe.ReplaceAllString(s, " § ")
	// Rule 8.6: quoted text → 1 token.
	s = quotedTextRe.ReplaceAllString(s, " § ")
	// Rule 8.6: number with unit → 1 token. Runs before generic
	// splitting so `10 MB` is not counted as two words.
	s = numberWithUnitRe.ReplaceAllString(s, " § ")

	// Rule 8.7: a hyphenated word counts as one word — a hyphen inside
	// [\w]-[\w] is joined. Also treat en/em dashes surrounded by
	// whitespace as sentence connectors, not word joiners: they act as
	// whitespace for tokenisation.
	s = strings.ReplaceAll(s, "\u2014", " ") // em dash
	s = strings.ReplaceAll(s, "\u2013", " ") // en dash
	// Collapse in-word hyphens ("sandbox-exec" → "sandboxexec").
	s = inWordHyphenRe.ReplaceAllString(s, "$1$2")

	// Split on whitespace, count non-empty tokens whose stripped form is
	// alphanumeric-ish. Bare punctuation (`,`, `--`, `→`) is not a word.
	fields := strings.Fields(s)
	count := 0
	for _, f := range fields {
		if isWordToken(f) {
			count++
		}
	}
	return count
}

// parenSpanRe matches a non-nested parenthesised span on a single joined
// paragraph. Deliberately does not recurse; nested parens in these docs
// are rare and would rather be undercount than over-split.
var parenSpanRe = regexp.MustCompile(`\([^()\n]*\)`)

// inWordHyphenRe matches a hyphen between two word characters, so a
// hyphenated word can be joined into one token.
var inWordHyphenRe = regexp.MustCompile(`(\w)-(\w)`)

// isWordToken reports whether s (already whitespace-trimmed to a field)
// counts as a word for the sentence-length limit. Bare punctuation and
// zero-content tokens do not.
func isWordToken(s string) bool {
	// Placeholder tokens for Rule 8.5/8.6/8.7 substitutions.
	if s == "§" {
		return true
	}
	// Strip leading/trailing punctuation the writer left dangling.
	trimmed := strings.TrimFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '§'
	})
	if trimmed == "" {
		return false
	}
	// At least one letter or digit or the placeholder.
	for _, r := range trimmed {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '§' {
			return true
		}
	}
	return false
}
