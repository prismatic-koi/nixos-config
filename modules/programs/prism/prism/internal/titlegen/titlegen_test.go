package titlegen

import (
	"strings"
	"testing"
)

// TestSanitise covers the shared title derivation. The cases mirror
// internal/session's TestDeriveFallbackTitle exactly, because
// deriveFallbackTitle now delegates here and the two must not be allowed to
// diverge.
func TestSanitise(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   \n\t\n  ", ""},
		{"single line", "Fix the login bug", "Fix the login bug"},
		{
			"first non-blank line, later lines ignored",
			"\n\n  Fix GitHub issue #2641: spawned worker sessions have no title\n\nMore detail below.\n",
			"Fix GitHub issue #2641: spawned worker sessions have no title",
		},
		{"internal whitespace collapsed", "Fix   the    login\tbug   ", "Fix the login bug"},
		{
			"truncates long first line with ellipsis",
			"This is a very long prompt title that goes on and on and on and on and on and on and on and on forever",
			"This is a very long prompt title that goes on and on and on and on and on and o…",
		},
		{
			// An ESC surviving into the title would carry an ANSI/OSC escape
			// sequence into every viewer's terminal. The ESC is dropped; its
			// payload bytes stay behind as inert printable text.
			"ANSI escape injection",
			"Fix login \x1b[31mbug\x1b[0m now",
			"Fix login [31mbug[0m now",
		},
		{"other control bytes dropped", "deploy\x07\x00\x7f service", "deploy service"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Sanitise(tc.in)
			if got != tc.want {
				t.Errorf("Sanitise(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len([]rune(got)) > MaxTitleRunes {
				t.Errorf("Sanitise(%q) = %q, exceeds MaxTitleRunes %d", tc.in, got, MaxTitleRunes)
			}
		})
	}
}

// TestSanitise_StripsEveryControlByte is the exhaustive form of the security
// AC: no byte below 0x20, and not 0x7F, may reach agent_status.title, since
// that column is rendered verbatim into the tmux dashboard and the source
// text can come from an issue body or a PR description.
func TestSanitise_StripsEveryControlByte(t *testing.T) {
	for b := 0; b < 0x20; b++ {
		in := "safe" + string(rune(b)) + "text"
		got := Sanitise(in)
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("Sanitise(%q) = %q, which still carries control byte %#x", in, got, r)
			}
		}
	}
	got := Sanitise("safe\x7ftext")
	if strings.ContainsRune(got, 0x7f) {
		t.Errorf("Sanitise kept DEL: %q", got)
	}
}

// TestSanitise_OSCSequenceIsDefanged is the concrete attack this protects
// against: an OSC 0 sequence that rewrites the viewer's terminal title. With
// the ESC and BEL dropped, what is left is inert text.
func TestSanitise_OSCSequenceIsDefanged(t *testing.T) {
	got := Sanitise("\x1b]0;pwned\x07real title")
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x07) {
		t.Fatalf("Sanitise left an escape or bell byte in %q", got)
	}
	if !strings.Contains(got, "real title") {
		t.Errorf("Sanitise(%q) dropped the legitimate text: %q", "OSC payload", got)
	}
}

// TestEligible pins the scope decision from the issue: coordinators and
// workers are titled; review agents are not, and are the reason the check
// exists. The allowlist shape means an unknown role is excluded rather than
// silently opted in.
func TestEligible(t *testing.T) {
	eligible := []string{"coordinator", "worker"}
	for _, role := range eligible {
		if !Eligible(role) {
			t.Errorf("Eligible(%q) = false, want true", role)
		}
	}

	notEligible := []string{
		// Every review-agent role. These are the 3,290-row majority the
		// issue excludes: a session named `...~review-1-review-qa` already
		// says what it is.
		"review-goal", "review-code", "review-security", "review-qa", "review-context",
		// Other non-titled roles.
		"investigate", "ac", "retro",
		// An unrecorded role must fail closed, not be guessed at.
		"",
		// A role invented tomorrow must be excluded until someone opts it in.
		"review-performance", "some-future-role",
		// Case and spacing are not normalised — the column holds exact values.
		"Worker", "COORDINATOR", " worker",
	}
	for _, role := range notEligible {
		if Eligible(role) {
			t.Errorf("Eligible(%q) = true, want false", role)
		}
	}
}
