package session

import "github.com/prismatic-koi/prism/internal/titlegen"

// title_fallback.go — prism-side fallback title for a newly spawned session
// (issue #2641).
//
// Diagnosis (recorded here so the fix's rationale travels with the code):
// the dashboard `title` column reads `agent_status.title`, which prism only
// ever populated from a harness-reported value (see
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
// never emits a title-bearing event. The cause is in pi (no auto-titling in
// any mode), not in prism's event handling. That conclusion still stands.
//
// CORRECTION (#2683): this file previously said the handful of titled rows
// were "sessions a human renamed interactively". That was wrong. They are
// opencode artifacts — titles written by the PREVIOUS harness, sitting on
// long-lived session rows that pi later reused.
//
// The measured evidence, from the live DB on navi:
//
//	harness    rows   titled
//	opencode      1        1  (100%)
//	pi         3748        3  (0.08%)
//
// The `harness` column tracks a row's LATEST incarnation, not the harness
// that wrote the title, which is what hid the origin. Dating settles it:
// `home-ops@main` carried "Renovate PR #2887 app-template v5 upgrade
// review". That PR was created 2026-05-04 and merged 2026-05-07, while the
// earliest retained pi event is 2026-05-09 — the title described work that
// finished two days before pi appears anywhere in the database, and it was
// still on the dashboard months later. opencode auto-generated titles; pi
// does not.
//
// Why the correction matters rather than being a footnote: "a human renamed
// these" implies the rows are precious and must never be touched, which is
// exactly the reasoning that let three months-stale titles sit on the
// dashboard. Naming them as opencode artifacts is what licensed #2683 to
// clear them (migrateV39ToV40) and to add `title_source` so provenance is
// recorded rather than re-derived by the next reader.
//
// Fixing pi is out of scope for this repo (pi's source is an external
// package — see modules/programs/prism/pi.nix). The accepted fallback is to
// seed a sensible title at spawn time from the spawn prompt, which prism
// already has in hand. Since #2683 that fallback is the SECOND-best title: a
// model-generated summary supersedes it for coordinator and worker sessions
// (internal/titlegen), and the fallback is what stands when the model call
// fails, is slow, or is not configured.
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
