# UI Design Language
*Hyprland + Quickshell desktop — living design document*

---

## Philosophy: Dormant → Alive

Everything is hidden or minimal at rest. When invoked — a submap, a launcher, a dialog — it arrives with purpose, then disappears cleanly. The interface does not demand attention. It responds to intention.

This is expressed most clearly in the waybar: two strips, hidden until `Super` is held, then revealed. This pattern extends to all UI elements. Ambient surfaces are quiet. Invoked surfaces are expressive.

The enso brushstroke wallpaper (which adapts per machine/colourscheme) is the visual north star. The UI should feel **drawn, not rendered** — intentional but imperfect, organic but precise. Think ink on wood.

---

## Colour System

### Scheme-Agnostic Roles

The UI is designed against semantic roles, not hardcoded hex values, so it works across Everforest, Onedark, Catppuccin, and future schemes.

| Role | Everforest value | Purpose |
|---|---|---|
| `background` | `bg0` `#2d353b` | Base surface for all elements |
| `surface` | `bg1` `#343f44` | Slightly raised floating elements |
| `surface-high` | `bg2` `#3d484d` | Selected items, hover states |
| `scrim` | `bg_dim` `#232a2e` | Deep background, overlays |
| `text` | `foreground` `#d3c6aa` | Primary readable text |
| `subtext` | `grey2` `#9da9a0` | Hints, inactive labels, timestamps |
| `muted` | `grey1` `#859289` | Disabled states, bracket shortcut hints |
| `primary` | `green` `#a7c080` | Main interactive accent |
| `secondary` | `aqua/blue` `#83c092` / `#7fbbb3` | Secondary accent |
| `warn` | `orange` `#e69875` | Warnings, moderate-destructive actions |
| `danger` | `red` `#e67e80` | Destructive actions |
| `info` | `yellow` `#dbbc7f` | Informational states |
| `special` | `purple` `#d699b6` | Special or elevated states |

### The Rainbow

The rainbow is defined by the seven terminal accent colours in order:

```
red → orange → yellow → green → aqua → blue → purple
45deg gradient
```

It is a **scheme-constant** — it does not change between colourschemes, only its individual colours shift slightly with the palette. It is deployed at full saturation; no muting is needed. Bold and bright is intentional.

**Semantic meaning of the rainbow:** it means *this thing has your attention right now*. It is the active/focus indicator. It appears wherever keyboard focus or active state needs to be communicated. It does not appear on ambient or inactive surfaces.

---

## Transparency

Transparency is used sparingly and with clear intent. It connects surfaces to the environment (the wallpaper, the world behind) rather than layering for visual effect.

| Surface | Opacity | Rationale |
|---|---|---|
| Terminal (kitty) | 0.80 | Embedded in environment, connected to wallpaper |
| Waybar strips | 0.85–0.90 | Ambient, slightly embedded |
| Notifications | 1.00 | Transience expressed through animation, not opacity |
| Dialogs / launcher / submap indicator | 1.00 | Fully present and invoked; rainbow border does the work |

No blur. No glassmorphism. Flat opaque surfaces for invoked elements.

---

## Typography

Two fonts, multiple levels of hierarchy:

**Noto Sans** — for mode labels, headings, and anything that *names a context*. Submap titles (EXIT, RESIZE), workspace labels, dialog titles. Clean and neutral — present without competing with the content it frames.

**JetBrains Mono** — for everything functional. Action labels, data values, temperatures, percentages, timestamps, and keyboard shortcut hints `[l]` `[s]`. Terminal-precise, unambiguous.

Hierarchy is expressed through **weight and colour**, not font switching between action labels and data. Mode labels stand apart in Noto Sans bold; everything beneath them lives in JetBrains Mono, differentiated by weight (DemiBold for actions, regular for hints) and colour role (semantic colours for actions, `muted` for hints).

Shortcut hints follow a consistent pattern throughout: `[key]` in `muted` colour, always monospace, always lowercase.

> **Future consideration:** Noto Serif may be introduced for spacious contexts — large dialog headings, launcher result names, or workspace labels where the extra warmth and organic quality can breathe. It is not currently deployed.

---

## Surface Treatment

All floating surfaces share these properties:

- **Background:** `background` or `surface` at 100% opacity
- **Border:** rainbow gradient, consistent weight, slightly rounded
- **Corner radius:** consistent pill-like rounding — established by the exit menu, applied everywhere
- **No drop shadows**
- **No gradients on backgrounds**
- **No decorative elements** — the rainbow border is the only decoration

The rainbow border is the signature. Everything else steps back.

---

## Dimming (Per-Invocation Scrim)

Some invoked surfaces warrant dimming the world behind them. This is a **per-element decision**, not a global setting, based on whether the user needs to see the screen to act.

**Dim when:** the invocation is high-stakes or pauses normal interaction. The user's attention should be fully captured — the desktop behind is irrelevant. Examples: exit menu, destructive confirmations.

**Do not dim when:** the user needs spatial context to act. They are looking at windows, workspaces, or layout while using the submap. Examples: resize mode, workspace switching.

| Element | Dim? | Rationale |
|---|---|---|
| Exit submap | Yes | High-stakes; desktop state is irrelevant |
| Resize submap | No | User needs to see the window being resized |
| Launcher | Yes | Full-attention search context |
| Notifications | No | Ambient; should not interrupt |

When applied, the scrim uses `scrim` (`bg_dim`) at **40–50% opacity**, covering the full screen behind the invoked surface. No blur — just a flat semi-transparent overlay that quiets the background without obscuring it entirely.

---

## The Rainbow as Active State

| Element | Rainbow usage |
|---|---|
| Hyprland window border | Focused window border (existing) |
| Workspace switcher | Active workspace segment filled with gradient |
| Launcher | Selected item gets rainbow left-border or underline |
| Submap indicator | Full border — submap *has* the keyboard |
| Dialogs | Full border — invoked surface |
| Notifications | No rainbow — single semantic colour instead |
| Waybar (ambient) | No rainbow — ambient, not invoked |

---

## Animation Language

**Spring physics, not linear easing.** Elements overshoot slightly and settle — they feel placed, not slid. Target: 150–200ms, quick but not abrupt.

**Single-axis entry per element type:**
- Dialogs — drop from top
- Notifications — slide from right
- Submap indicator — expand from centre (high-stakes invoked surface; must demand attention)
- Launcher — expand from centre

**The rainbow animates on invocation** — a brief sweep/shimmer on the border when an element appears, rather than a static gradient. The active state arriving is an event.

**Workspace switcher** — the rainbow gradient segment slides left/right between workspaces. This is the most expressive ambient animation in the system and the primary use of rainbow outside of borders.

---

## Element Specifications

### Dialogs (Exit menu, confirmations)
- Fully opaque `background`
- Rainbow border, consistent radius
- Semantic colour on action labels: `danger` for EXIT, `warn` for Shutdown, `info` for Reboot, `subtext` for Lock, `text` for Logout
- Shortcut hints: `[key]` in `muted`, JetBrains Mono
- Keyboard driven only

### Submap Indicator
- Centred pill, expands from centre on submap activation
- Rainbow border with brief shimmer sweep on invocation
- Three-level typography hierarchy:
  - **Mode label** (EXIT, RESIZE) — Noto Sans bold, `text` or `primary` colour (semantic per submap, e.g. `danger` for exit)
  - **Action labels** (Lock, Shutdown, ←↓↑→) — JetBrains Mono DemiBold, semantic colours where applicable (`warn` for Shutdown, `info` for Reboot, `subtext` for Lock, `text` for Logout)
  - **Shortcut hints** `[key]` — JetBrains Mono regular, `muted`
- **Dimming:** high-stakes submaps (e.g. exit) apply a `scrim` overlay at ~40–50% opacity behind the pill; utility submaps (e.g. resize) do not dim — see Dimming rule above
- Disappears on submap exit (scales down and fades)

### Waybar (top + bottom strips)
- Hidden at rest, revealed on `Super` hold
- Slight transparency (0.85–0.90), no border
- Top strip: workspace switcher (left), window title (centre), system tray (right)
- Bottom strip: system stats in mono — CPU, RAM, network, battery, clock
- Two typographic voices in action: Noto Sans for names/labels, Mono for values

### Workspace Switcher (Enso Arc)
- Five segments of a ~270° arc — open at the bottom, echoing the enso wallpaper
- Inactive segments: `muted` or `bg3`
- Active segment: filled with rainbow gradient
- Switching animates the gradient moving to the new segment
- Lives in top-left of waybar
- Implemented in QML (quickshell) for animation capability

### Notifications
- Top-right pills, fully opaque
- Single accent border colour based on urgency: `primary` (info), `warn` (warning), `danger` (critical)
- Auto-dismiss with a progress animation
- No rainbow — they are ambient, not invoked

### Launcher (future)
- Large centred panel, fully opaque `background`
- Rainbow border
- Noto Sans for result names, Mono for paths/metadata
- Selected item: rainbow left-border accent
- Keyboard nav only, fuzzy search

---

## What the Rainbow Is Not

The rainbow is not decoration. It is not applied broadly for visual interest. It has one meaning — **active focus** — and that meaning must remain consistent for it to communicate. When everything glows, nothing does.

Elements that are ambient, passive, or informational use single accent colours from the palette instead.

---

## Principles Summary

1. **Hidden until needed.** Everything rests. Nothing announces itself unprompted.
2. **Invoked surfaces arrive with intention.** Animation, opaque, rainbow border.
3. **The rainbow means focus.** One semantic, deployed consistently.
4. **Flat backgrounds, expressive borders.** No shadows, no blur on invoked elements.
5. **Two fonts, hierarchy through weight and colour.** Sans names contexts; mono handles everything functional.
6. **Organic but precise.** Spring animations, enso-inspired forms, imperfect geometry.
7. **Colour roles, not hex values.** The system works across any colourscheme.
8. **Transparency connects to environment.** Only terminals and ambient surfaces; never invoked ones.
