# sidebar-spike design notes

Working notes for the in-session iteration loop. This file captures
the design decisions converged on during smoke-testing with Ben, and
is the **draft of the design-doc addendum** that lands in
`modules/programs/prism/prism/docs/multiplexer-proposal.md` on the
"ship it" verdict.

While the spike is iterating, this file is structured but informal —
hypotheses, what was tried, what landed. On convergence it is
rewritten into a tight prescriptive subsection for the design doc.

## v1 — initial proposal

The starting strawman, recorded for diff against later revisions.

### Hierarchy

Single nested tree, two levels of nesting:

```
prism · N sessions
─────────────────────────────────
 ▾ nixos-config
 ├─ ●  @main                idle
 ├─ ◑  @2141-mux-spike       reviewing
 │  ├─ ●  ~review-1-review-code   active
 │  ├─ ●  ~review-1-review-goal   active
 │  └─ ●  ~review-1-review-qa     active
 ├─ ●  @degender-global-...  active
 └─ ◐  @battery-monitor-...  waiting
 ▾ home-ops
 ├─ ○  @main                 idle
 └─ ▲  @plex-image-bump      escalated
 ▸ pi-extensions
                                          ↑↓ ←→ ⏎ ⇥ q
```

Repo headers use `▾` (expanded) and `▸` (collapsed) as the
disclosure indicator. Tree branch lines are `├─` / `└─` / `│`.

### Width

Fixed 32 columns. Resize is deferred to the real render layer in PR #3.

### State → glyph + colour

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

`prism · <count> sessions` at the top, bold white, separated from the
tree by a single horizontal-line divider.

### Footer

Keymap hint `↑↓ nav  ←→ collapse  ⏎ select  ⇥ pane  q quit` at the
bottom, dimmed. Truncates to glyphs-only when the sidebar width
forces it.

### Branch-line rendering

`├─` for non-last children, `└─` for the last child of a parent.
Subsession rows pad their leading column with `│  ` to keep the
trunk visible, except when the parent session is the last child of
its repo — in that case the trunk is dropped and the alignment is
preserved with spaces (matches herdr's quieter look on trailing
clusters).

### Right pane (placeholder)

Static text describing the selected session: name, current state,
active pane, and a hint that the real pane host lands in PR #3.

## Open questions / iteration targets

These are the questions Ben's smoke-test will answer. They are
listed here as iteration targets rather than committed answers.

1. **Do filled circles vs half-filled circles read at a glance?** The
   v1 mapping uses `●` / `○` / `◐` / `◑` to span the state vocabulary
   without leaving the circle family. Herdr's actual sidebar mixes a
   check, a braille spinner, and a filled circle for the same axis;
   that may read better.
2. **Does `▲` for escalated look out of place next to circle glyphs?**
   The triangle is an intentional break from the circle family because
   escalated is the one state that demands user attention. If it
   feels too noisy, switch to a red `●` and rely on colour alone.
3. **Animated spinner for `active`?** v1 uses a static `●` for active;
   herdr animates this with a braille spinner. Adding the spinner
   would require a per-tick re-render of just the spinner cells.
4. **Repo header colour when collapsed?** v1 keeps the same colour
   regardless. Dimming a collapsed repo header may make the
   expanded-vs-collapsed distinction read faster.
5. **Where does the review-cycle number go?** The hierarchy example in
   #2147 shows `~review-1-review-code` — the cycle number is embedded
   in the session name. Is that the right place, or should it sit
   next to the parent worker as a small badge?
6. **What does "selected" mean for a repo header?** v1 makes the
   highlight apply to the header itself; left/right collapse it. An
   alternative is to treat repo headers as non-selectable and snap
   focus to the first child — closer to how herdr's workspace list
   reads (you select workspaces, not the group label).
7. **Header content.** `prism · N sessions` is a placeholder. The
   real surface might include the active coordinator's repo, the
   merge-queue depth, or the count of escalated sessions.

## What survives "ship it"

On Ben's convergence signal, this file's content is rewritten into a
prescriptive subsection of `multiplexer-proposal.md` — likely
`§3.x UI reference: sidebar`. The rewrite covers:

- Hierarchy structure (with ASCII example matching the final design)
- Glyph + colour mapping per prism state
- Sidebar width policy
- Header / footer treatment
- Selection-highlight pattern
- Any other decisions locked in during iteration

The spike directory itself does not survive — a follow-up cleanup PR
deletes it after the design has been consumed by PR #3 of the
multiplexer programme.
