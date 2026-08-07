package session

// title_fallback.go — prism-side fallback title for a newly spawned session
// (issue #2641).
//
// Diagnosis (recorded here so the fix's rationale travels with the code):
// the dashboard `title` column reads `agent_status.title`, which prism only
// ever populates from a harness-reported value (see
// internal/sidecar/events.go's handleSessionCreated / handleSessionUpdated,
// which read `properties.info.title` off pi's session.created / .updated SSE
// events). Inspecting the pi package itself (pi-monorepo dist/core
// /session-manager.js and dist/modes/{interactive,rpc}) shows pi has no
// automatic title-generation step anywhere: a session's name is set only by
// an explicit `set_session_name` RPC call or the interactive `/name`-style
// command — both user-initiated. Interactive-mode's `updateTerminalTitle`
// only writes the *host terminal's* title via an ANSI escape sequence; it
// never touches the SSE `info.title` field prism reads. A headless
// `prism spawn` worker never has anything call `set_session_name`, so it
// never emits a title-bearing event — hence 4 titled rows out of 3,807 (the
// four are sessions a human renamed interactively). The cause is in pi (no
// auto-titling in any mode), not in prism's event handling.
//
// Fixing that is out of scope for this repo (pi's source is an external
// package — see modules/programs/prism/pi.nix). The accepted fallback is to
// seed a sensible title at spawn time from the spawn prompt, which prism
// already has in hand.
//
// The seed call (SpawnSession, internal/session/spawn.go) only derives a
// fallback when agent_status currently has no title at all — nil on a fresh
// row, or still nil on a row left behind by a prior incarnation of the same
// session name (the respawn-after-cleanup path, internal/db/
// respawn_after_cleanup_test.go, #2094). A real harness-reported title or a
// human rename from that prior life is left untouched: UpsertStatusSeedRootAgentName
// is an upsert, and its ON CONFLICT branch applies the same
// title = COALESCE(excluded.title, title) as the INSERT branch, so passing
// nil there (as SpawnSession now does whenever a title already exists)
// preserves it. A later harness-reported title (if pi ever gains
// auto-titling, or a human renames the session) still overwrites the
// fallback normally: the sidecar's harness-title path always COALESCEs a
// nil title to "keep the existing value" and only overwrites with a non-nil
// title (helpers.strPtr already normalises an empty harness-reported title
// to nil, so an empty string from the harness can never clobber the
// fallback or a real title — see #2641 AC on the NULL-vs-empty-string
// distinction).
//
// deriveFallbackTitle also strips control bytes (ESC and friends) from the
// prompt text before it can reach agent_status.title, since the column is
// rendered verbatim in the tmux dashboard and a spawn prompt can originate
// from untrusted external text (an issue body, a PR description, ...) —
// see collapseWhitespace below.
const fallbackTitleMaxRunes = 80

// deriveFallbackTitle returns a short, human-meaningful title derived from a
// spawn prompt: the first non-blank line, whitespace-collapsed and truncated
// to fallbackTitleMaxRunes runes with a trailing ellipsis. Returns "" when
// the prompt has no non-blank line (e.g. an empty prompt), in which case the
// caller should leave the seeded title unset (nil) rather than write an
// empty string.
func deriveFallbackTitle(prompt string) string {
	line := firstNonBlankLine(prompt)
	if line == "" {
		return ""
	}
	runes := []rune(line)
	if len(runes) > fallbackTitleMaxRunes {
		return string(runes[:fallbackTitleMaxRunes-1]) + "…"
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
// The drop step matters for security, not just cosmetics: agent_status.title
// is rendered verbatim into the tmux dashboard (internal/dashboard/sessions.go's
// RenderSessionRow), and a spawn prompt's first line can originate from
// untrusted external text (an issue body, a PR description, ...). Without
// this, an ESC byte (0x1B) surviving into the title could carry an ANSI/OSC
// escape sequence into every viewer's terminal — display spoofing at
// minimum, and terminal-emulator-dependent worse (#2641 review). Dropping
// the control byte (rather than treating it as a word boundary, like
// whitespace) means an escape sequence's payload bytes are left as inert
// printable text with no ESC prefix to interpret them.
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
