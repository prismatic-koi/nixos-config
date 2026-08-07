package titlegen

import (
	"strings"
	"testing"
)

// TestExtractIssueRef covers the deterministic extraction contract (#2683):
// the two accepted forms, GitHub-before-Jira precedence, first-match-wins
// within a form, and — the edge case that matters most — "" for text that
// names no issue at all.
func TestExtractIssueRef(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		// ── GitHub form ──────────────────────────────────────────────────
		{"github plain", "Please implement GitHub issue #2683 in this repo", "#2683"},
		{"github single digit", "fixes #7", "#7"},
		{"github in parens", "the regression (#2202) is documented", "#2202"},
		{"github no space before hash", "see issue#123 for detail", "#123"},
		{"github closes line", "Closes #2683", "#2683"},
		{"github first match wins", "Fixes #10, supersedes #9", "#10"},

		// ── Jira form ────────────────────────────────────────────────────
		{"jira plain", "Work on PLAT-123 today", "PLAT-123"},
		{"jira two-letter project", "CH-9 needs approval", "CH-9"},
		{"jira digits in key", "ticket A1B2-45 is open", "A1B2-45"},
		{"jira after punctuation", "(PROJ-1)", "PROJ-1"},
		{"jira first match wins", "PROJ-1 blocks PROJ-2", "PROJ-1"},

		// ── Precedence ───────────────────────────────────────────────────
		{
			"github wins over jira even when jira appears first",
			"Mirror of PLAT-500 — implement #2683",
			"#2683",
		},

		// ── No reference: must be "", never a guess ──────────────────────
		{"empty text", "", ""},
		{"prose with no reference", "Refactor the login flow and add tests", ""},
		{"bare number is not a reference", "bump the timeout to 30 seconds", ""},
		{"hash with no digits", "the # character alone", ""},
		{"lowercase jira-shaped token is not a key", "see plat-123", ""},
		{"single letter project is not a key", "A-1 is not a Jira key", ""},
		{"hash inside a longer token is rejected", "colour #1a2b3c", ""},
		{"jira key inside a longer token is rejected", "xPLAT-123", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractIssueRef(tc.text)
			if got != tc.want {
				t.Errorf("ExtractIssueRef(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// TestExtractIssueRef_AlwaysVerbatimSubstring pins the invariant that makes
// this function trustworthy: whatever it returns was actually present in the
// source text. A value that is not a substring would be an invented
// reference, which is the exact failure the model ban exists to prevent.
func TestExtractIssueRef_AlwaysVerbatimSubstring(t *testing.T) {
	texts := []string{
		"implement #2683 now",
		"PLAT-9 and #4",
		"nothing here",
		"trailing PROJ-77",
		"",
	}
	for _, text := range texts {
		got := ExtractIssueRef(text)
		if got == "" {
			continue
		}
		if !strings.Contains(text, got) {
			t.Errorf("ExtractIssueRef(%q) = %q, which is not a substring of the source text", text, got)
		}
	}
}

// TestExtractIssueRef_NeverCarriesControlBytes covers the security AC from
// the issue_ref side. The value is rendered verbatim in the tmux dashboard,
// and the source text is untrusted (an issue body, a PR description). The
// patterns admit only '#', '-', ASCII letters and digits, so a control byte
// cannot survive into the result even when one sits inside the match window.
func TestExtractIssueRef_NeverCarriesControlBytes(t *testing.T) {
	cases := []string{
		"issue #26\x1b[31m83 and more",
		"\x1b]0;evil\x07 PLAT-123",
		"#2683\x00",
		"\x07\x7f#42",
		"PLAT\x1b-123",
	}
	for _, text := range cases {
		got := ExtractIssueRef(text)
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Errorf("ExtractIssueRef(%q) = %q, which carries control byte %#x", text, got, r)
			}
		}
	}
}

// TestExtractIssueRef_UntrustedInputCannotForgeAReference is the adversarial
// bookend: text crafted to look like it carries a reference, but which does
// not match either form, must still yield "". A wrong reference silently
// misattributes work, so "no answer" has to be the reliable default.
func TestExtractIssueRef_UntrustedInputCannotForgeAReference(t *testing.T) {
	hostile := []string{
		"issue number: two thousand six hundred and eighty three",
		"gh-2683",
		"# 2683",
		"issue №2683",
		"JIRA ticket: plat 123",
	}
	for _, text := range hostile {
		if got := ExtractIssueRef(text); got != "" {
			t.Errorf("ExtractIssueRef(%q) = %q, want \"\" — no reference is present in the text", text, got)
		}
	}
}
