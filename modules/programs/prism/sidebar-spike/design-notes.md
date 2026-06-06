# sidebar-spike design notes

Working notes for the in-session iteration loop. This file captures
the design decisions converged on during smoke-testing with Ben, and
is the **draft of the design-doc addendum** that lands in
`modules/programs/prism/prism/docs/multiplexer-proposal.md` on the
"ship it" verdict.

While the spike is iterating, this file is structured but informal —
hypotheses, what was tried, what landed. On convergence it is
rewritten into a tight prescriptive subsection for the design doc.

## Revision log

- **v1 (initial)** — strawman: 32-col sidebar, `● ○ ◐ ◑ ▲` glyph
  family, repo→session→review three-level nested tree, no responsive
  layout, no slot truncation.
- **v2 (this)** — fixes four issues from v1 smoke-test: review-label
  noise, slot overflow / wrap-bleed, header-disappears render bug,
  and adds a narrow-mode responsive layout.

## v2 — converged so far

### Hierarchy

Single nested tree, two levels of nesting, **review subsessions
collapsed by default**:

```
 prism · 8 sessions
 ──────────────────────────────
 ▾ nixos-config
 ├─ ○  @main                  idle
 ├─ ◑  @2141-mux-spike (5 rev) reviewing
 ├─ ●  @degender-global-i…    active
 ├─ ◐  @battery-monitor-ref…  waiting
 └─ ●  @stale-finished-ses…   finished
 ▾ home-ops
 ├─ ○  @main                  idle
 └─ ▲  @plex-image-bump       escalated
 ▸ pi-extensions
 ↑↓ nav  ←→ collapse  ⏎ ⇥ q
```

Repo headers use `▾` (expanded) and `▸` (collapsed). Tree branch lines
are `├─` / `└─` / `│`. Session rows whose review group is collapsed
carry a trailing dim `(N rev)` badge; right-arrow on the session row
expands the reviews.

When expanded, reviews appear under their parent with the
`~review-N-<agent>` prefix shortened to `~N-<agent>` (see "Review
label rendering" below):

```
 ▾ nixos-config
 ├─ ○  @main                  idle
 ├─ ◑  @2141-mux-spike        reviewing
 │  ├─ ●  ~1-code             active
 │  ├─ ●  ~1-goal             active
 │  ├─ ●  ~1-qa               active
 │  ├─ ▲  ~1-security         escalated
 │  └─ ●  ~1-context          active
 └─ …
```

The two-level nesting limit (proposal §3 / issue #2148) is preserved;
review subsessions never have children of their own.

### Width

Fixed 32 columns in wide mode. Resize is deferred to the real render
layer in PR #3.

### State → glyph + colour (unchanged from v1)

| State | Glyph | Colour (Tailwind name) | Hex |
|---|---|---|---|
| active | `●` | green-400 | `#4ade80` |
| idle | `○` | grey-500 | `#71717a` |
| waiting | `◐` | yellow-400 | `#facc15` |
| reviewing | `◑` | blue-400 | `#60a5fa` |
| escalated | `▲` | red-400 | `#f87171` |
| finished | `●` (strikethrough) | grey-600 | `#52525b` |

### Selection highlight

Selected row: background `#3f3f46` (zinc-700), foreground `#fafafa`
(zinc-50), bold. The glyph keeps its own colour; only the name +
state-label adopt the selection treatment.

### Header

`prism · N sessions` at the top of the sidebar, pinned, never
scrolled. The count is the **total non-review sessions across all
repos** — review subsessions are deliberately excluded so the
number doesn't shift when the user expands/collapses a review group.
This is decision (a) from v2 issue #3; the alternative ("active
sessions only") is parked because the more stable read is a better
default.

### Footer

Keymap hint `↑↓ nav  ←→ collapse  ⏎ select  ⇥ pane  q quit` at the
bottom, dimmed and pinned. Truncates to glyphs-only when the sidebar
width forces it.

### Slot truncation policy (NEW in v2)

Every rendered row is hard-truncated to the sidebar's column width
using `charmbracelet/x/ansi.Truncate(s, width, "…")`. This is
ANSI-aware (handles the lipgloss-coloured strings correctly) and
prevents the v1 wrap-bleed where long names like
`@battery-monitor-refactor` overflowed past the sidebar's right edge
and broke the tree structure.

Applied consistently to:
- repo headers (`▾ nixos-config…` if needed)
- session names (`@battery-monitor-refac…`)
- review labels (`~1-security…`)
- the header and footer rows

The drop order when space is tight on a session row is:
1. State label (rightmost; rendered only when there's room)
2. Review-count badge (only present on collapsed-review session rows)
3. Truncate the name with `…`

### Header pinning fix (NEW in v2)

v1 bug: the `prism · 3 sessions` header disappeared when the first
repo was expanded. Root cause: the header was concatenated into the
sidebar's body string, which lipgloss then re-flowed when content
exceeded the sidebar's width, pushing the header off the top.

v2 fix: header / divider / scrollable body / footer are composed via
`lipgloss.JoinVertical(Left, header, divider, body, footer)`, each
component rendered at a fixed height. The body is auto-scrolled to
keep the cursor in view; the header and footer cannot be pushed off
because they sit outside the scrollable region.

### Default-collapsed reviews (NEW in v2)

Review subsessions are hidden by default. The parent session row
carries a dim ` (N rev)` badge that disappears when reviews are
expanded. Mirrors prism's existing convention:
- `prism sessions list` (without `--all`) hides review subsessions
- The dashboard hides them the same way

Right-arrow on a session row with reviews expands the review group;
left-arrow collapses it again. This is symmetric with the repo-header
collapse on left-arrow at the outer level.

### Responsive layout (NEW in v2)

Threshold: `terminal_width < 80` switches to **narrow mode**.
Rationale: 32-col sidebar + 48-col pane is too cramped for most
working content; below 80 a full-width pane is clearly better than
the split.

Narrow-mode layout, modelled on herdr's mobile pattern (clean-room
per AGPL constraint):

- **Top status bar** (1 row): `<repo>/<session> · <state>` left,
  `^B switch` right. Black background, subtle.
- **Pane** fills the rest of the terminal at full width.
- **No inline sidebar.**

Triggering `Ctrl-B` opens a **popover** — the sidebar tree rendered
on top of the pane, anchored to the top-left, ~38 columns wide
(`sidebar.Width + 6`, capped at `terminal_width - 4` so the pane
background peeks through on the right). The popover uses the same
header/body/footer render path as the wide-mode sidebar, just at a
different width.

Popover controls:
- `↑↓` / `←→` navigate exactly as in wide mode
- `Enter` selects and dismisses the popover
- `Esc` dismisses without selecting
- `Ctrl-B` toggles (so the same key opens and closes)

The popover keystroke binding (`Ctrl-B`) mirrors tmux's prefix
convention. Revisable — if it feels wrong in smoke-test, the
candidates are `Tab` (currently bound to pane cycle in the inner
ring), a literal `s` for "switch", or `?`.

### Right pane (placeholder)

Static text describing the selected session: name, current state,
active pane, and a hint that the real pane host lands in PR #3.

## Open questions / iteration targets

These are still open after v2. They are the questions Ben's
smoke-test of v2 (and later versions) will answer.

1. **Do `●` / `○` / `◐` / `◑` read at a glance?** Herdr's actual
   sidebar mixes a check (`✓`), a braille spinner, and a filled
   circle for the same axis; that may read better. v2 keeps the
   circle family from v1; consider the herdr-style mix.
2. **Does `▲` for escalated look out of place next to circle glyphs?**
   Intentional break because escalated demands attention. If too
   noisy, switch to red `●` and rely on colour alone.
3. **Animated spinner for `active`?** v2 uses a static `●`; herdr
   animates with a braille spinner. Would require per-tick re-render
   of just the spinner cells.
4. **Repo header colour when collapsed?** v2 keeps the same colour.
   Dimming a collapsed repo header may make the expanded-vs-collapsed
   distinction read faster.
5. **Where does the review-cycle number go?** v2 embeds it in the
   shortened name (`~1-code`). Alternative: a small badge next to the
   parent worker (`@2141-mux-spike [cycle 1]`).
6. **What does "selected" mean for a repo header?** v2 keeps the
   highlight on the header; left/right collapse it. Alternative: treat
   repo headers as non-selectable and snap focus to the first child.
7. **Header content.** `prism · N sessions` is a placeholder. The real
   surface might include the active coordinator's repo, the
   merge-queue depth, or the count of escalated sessions.
8. **Narrow-mode threshold (80).** Revisable. Could be 72 or 96
   depending on how cramped the right pane feels at the crossover.
9. **Popover keystroke binding (`Ctrl-B`).** Revisable. See above.
10. **Popover width (`sidebar.Width + 6`).** Revisable. Could match
    sidebar.Width exactly, or grow to 50% of terminal width.

## What survives "ship it"

On Ben's convergence signal, this file's content is rewritten into a
prescriptive subsection of `multiplexer-proposal.md` — likely
`§3.x UI reference: sidebar`. The rewrite covers:

- Hierarchy structure (with ASCII example matching the final design)
- Glyph + colour mapping per prism state
- Sidebar width policy (wide + narrow)
- Header / footer treatment
- Selection-highlight pattern
- Default-collapsed reviews + the badge form
- Slot truncation policy
- Responsive-layout threshold + popover binding
- Any other decisions locked in during iteration

The spike directory itself does not survive — a follow-up cleanup PR
deletes it after the design has been consumed by PR #3 of the
multiplexer programme.
