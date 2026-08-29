package session

import "github.com/prismatic-koi/prism/internal/titlegen"

// title_fallback.go — prism-side fallback title for a newly spawned session.
//
// The dashboard `title` column reads `agent_status.title`. prism populates
// that column only from a harness-reported value (internal/sidecar/events.go
// handleSessionCreated / handleSessionUpdated read `properties.info.title`
// off pi's session.created / .updated SSE events). pi has no automatic
// title-generation step in any mode: a session name is set only by an
// explicit `set_session_name` RPC call or the interactive `/name` command,
// both user-initiated. Interactive mode's `updateTerminalTitle` writes only
// the host terminal's title via an ANSI escape sequence. It never touches the
// SSE `info.title` field prism reads. A headless `prism spawn` worker never
// calls `set_session_name`, so it never emits a title-bearing event.
//
// A handful of titled rows are opencode artifacts — titles written by a
// previous harness, on long-lived session rows that pi later reused. They are
// NOT sessions a human renamed interactively. Treat them as clearable
// artifacts, not as precious rows. Two facts hide the origin and must not
// mislead the next reader: the `harness` column tracks a row's latest
// incarnation, not the harness that wrote the title, and the titles describe
// work that finished days before the earliest retained pi event on the same
// row. This distinction is why migrateV39ToV40 clears them and why
// `title_source` records provenance rather than leaving it to be re-derived.
//
// Fixing pi is out of scope for this repo (pi's source is an external package
// — see modules/programs/prism/pi.nix). The fallback seeds a sensible title
// at spawn time from the spawn prompt, which prism already has in hand. It is
// the second-best title: a model-generated summary supersedes it for
// coordinator and worker sessions (internal/titlegen), and the fallback
// stands when the model call fails, is slow, or is not configured.
//
// The seed call (SpawnSession, internal/session/spawn.go) derives a fallback
// only when agent_status currently has no title at all — nil on a fresh row,
// or still nil on a row left behind by a prior incarnation of the same
// session name (the respawn-after-cleanup path,
// internal/db/respawn_after_cleanup_test.go). A real harness-reported title
// or a human rename from a prior life is left untouched:
// UpsertStatusSeedRootAgentName is an upsert, and its ON CONFLICT branch
// applies the same title = COALESCE(excluded.title, title) as the INSERT
// branch, so passing nil whenever a title already exists preserves it. A
// later harness-reported title still overwrites the fallback normally: the
// sidecar's harness-title path always COALESCEs a nil title to keep the
// existing value and overwrites only with a non-nil title (helpers.strPtr
// normalises an empty harness-reported title to nil, so an empty string from
// the harness can never clobber the fallback or a real title).
//
// deriveFallbackTitle also strips control bytes (ESC and friends) from the
// prompt text before it can reach agent_status.title, since the column is
// rendered verbatim in the tmux dashboard and a spawn prompt can originate
// from untrusted external text (an issue body, a PR description, ...) —
// see titlegen.Sanitise.

// fallbackTitleMaxRunes is the fallback title's rune budget. It is
// titlegen.MaxTitleRunes, not a second copy: a fallback title and a
// generated title land in the same column and must not be allowed to drift
// to different widths.
const fallbackTitleMaxRunes = titlegen.MaxTitleRunes

// deriveFallbackTitle returns a short, human-meaningful title derived from a
// spawn prompt: the first non-blank line, whitespace-collapsed, stripped of
// control bytes, and truncated to fallbackTitleMaxRunes runes with a
// trailing ellipsis. Returns "" when the prompt has no non-blank line (e.g.
// an empty prompt), in which case the caller should leave the seeded title
// unset (nil) rather than write an empty string.
//
// The derivation itself lives in titlegen.Sanitise, which the generated-title
// path applies to model output as well. One definition, so the two title
// sources cannot disagree about what a title may contain — which is a
// security property, not only a cosmetic one: the control-byte strip is what
// keeps an ANSI escape sequence out of every viewer's terminal.
func deriveFallbackTitle(prompt string) string {
	return titlegen.Sanitise(prompt)
}
