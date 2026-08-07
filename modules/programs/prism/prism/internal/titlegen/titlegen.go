// Package titlegen produces the dashboard title and the issue/ticket
// reference for a prism session (issue #2683).
//
// Two values, two mechanisms
// --------------------------
//
// The package deliberately splits the work in half, because the two halves
// have different failure costs:
//
//   - The TITLE is a summary. A model produces it (Generator). A wrong or
//     missing title costs a little display quality and nothing else, so the
//     path is best-effort: on any failure the caller falls back to Sanitise
//     over the source text, which is the same deterministic derivation
//     internal/session's deriveFallbackTitle uses (#2641).
//
//   - The ISSUE REFERENCE is an identifier. A regex produces it
//     (ExtractIssueRef). NO model output is used for this field, ever. A
//     model can return a plausible-but-wrong number, and a wrong issue link
//     is worse than an empty column: it silently misattributes work to
//     another piece of work. When the source text carries no reference, the
//     field stays empty and the caller writes NULL.
//
// Scope
// -----
//
// Eligible reports which sessions are titled at all. Coordinators and
// workers are; review agents are not. A session named
// `nixos-config@x~review-1-review-qa` already says what it is, and there are
// thousands of such rows, so titling them is pure cost and noise.
//
// Security
// --------
//
// Both values are rendered VERBATIM into the tmux dashboard
// (internal/dashboard/sessions.go's RenderSessionRow) and into
// `prism sessions list`. The source text is untrusted: a spawn prompt
// routinely quotes an issue body or a PR description, and a coordinator's
// first user message is whatever was pasted into the terminal. Sanitise
// therefore drops control bytes — an ESC (0x1B) surviving into the title
// would carry an ANSI/OSC escape sequence into every viewer's terminal.
// Model output gets exactly the same treatment: it is untrusted text too,
// and the remote end is not a trust boundary.
package titlegen

// MaxTitleRunes bounds a title. It matches internal/session's
// fallbackTitleMaxRunes so a generated title and a fallback title occupy the
// same column width in every renderer.
const MaxTitleRunes = 80

// eligibleRoots is the set of root_agent_name values whose sessions get a
// generated title.
//
// It is an ALLOWLIST, not a review-agent denylist. A denylist fails open:
// a role added later (a new review dimension, a new analysis agent) would
// silently start making model calls and writing titles nobody asked for. An
// allowlist fails closed, which is the correct direction for a path that
// spends quota.
var eligibleRoots = map[string]bool{
	"coordinator": true,
	"worker":      true,
}

// Eligible reports whether a session with this root_agent_name should be
// titled by the generator.
//
// An empty rootAgentName is NOT eligible. A row with no recorded role is
// most often a bare `prism switch` session or a legacy row, and guessing
// would put the model call on exactly the sessions we know least about.
func Eligible(rootAgentName string) bool {
	return eligibleRoots[rootAgentName]
}

// Sanitise reduces arbitrary source text to a single-line, control-byte-free
// title of at most MaxTitleRunes runes.
//
// The steps, in order:
//
//  1. take the first line that is non-blank once whitespace is collapsed;
//  2. collapse every run of spaces/tabs/carriage returns to one space;
//  3. DROP every other control byte (0x00–0x1F and 0x7F) outright;
//  4. truncate to MaxTitleRunes runes, with a trailing ellipsis when it had
//     to cut.
//
// Returns "" when the text has no non-blank line. The caller must treat ""
// as "write no title" (NULL), never as an empty-string title — see the
// agent_status.title column comment in internal/db/db.go for why the two
// must stay distinguishable.
//
// Step 3 drops the control byte rather than treating it as a word boundary.
// That matters: dropping the ESC leaves an escape sequence's payload bytes
// behind as inert printable text with no ESC prefix to give them meaning,
// whereas turning ESC into a space would still leave `[31m` looking like a
// truncated sequence to a careless downstream re-assembler.
func Sanitise(s string) string {
	line := firstNonBlankLine(s)
	if line == "" {
		return ""
	}
	runes := []rune(line)
	if len(runes) > MaxTitleRunes {
		return string(runes[:MaxTitleRunes-1]) + "…"
	}
	return line
}

// firstNonBlankLine returns the first line of s (after collapsing internal
// whitespace) that is not empty once trimmed, or "" if every line is blank.
func firstNonBlankLine(s string) string {
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			line := collapseWhitespace(s[start:i])
			if line != "" {
				return line
			}
			start = i + 1
		}
	}
	return ""
}

// collapseWhitespace trims s, collapses any run of whitespace (spaces, tabs,
// carriage returns) into a single space, and drops every other control byte
// entirely (anything below 0x20 not already handled, plus 0x7F/DEL).
//
// See the Sanitise doc comment for why the drop is a drop and not a
// substitution.
func collapseWhitespace(s string) string {
	var b []byte
	inSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			if !inSpace && len(b) > 0 {
				b = append(b, ' ')
			}
			inSpace = true
		case c < 0x20 || c == 0x7f:
			// Drop other control bytes (ESC and friends) outright — do not
			// let them act as a whitespace boundary either.
		default:
			inSpace = false
			b = append(b, c)
		}
	}
	// Trim a trailing space left by collapsing (e.g. "foo   " -> "foo ").
	for len(b) > 0 && b[len(b)-1] == ' ' {
		b = b[:len(b)-1]
	}
	return string(b)
}
