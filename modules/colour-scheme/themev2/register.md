# themev2 personalisation map

This is the authoritative **personalisation map** for the `themev2` schema
(migration increment #1). It records, per slot, whether a colour is **native**
to the scheme (`upstream`) or **invented** for it (`derived` / `adjusted`), so
that the invented ones can be found again and tuned.

**This is not a fidelity audit.** Divergence from an upstream palette is not a
defect to minimise. An `adjusted` value — a hand-picked literal that does not
match any source colour — is a legitimate, first-class personalisation choice.
A `derived` value is a colour the scheme did not provide, computed so the slot
is filled. The map exists to make those choices visible and revisitable, not
to flag them as problems. Record every one honestly; tune them later.

The palette data lives in `palette.nix`; the schema option lives in
`../schema.nix`; the visual form of this map is `nix run .#theme-preview`.

## Palette shape

- **Neutrals** — a semantic band (dark → light), no baseX codes:
  `background_darkest`, `background_dark`, `background`, `surface`, `overlay`,
  `muted`, `foreground_dim`, `foreground`. The default text and background
  colours are first-class slots here.
- **Bright band** — kept for ANSI correctness (kitty `color9`–`color14` map to
  `bright_red`/`yellow`/`green`/`cyan`/`blue`/`magenta`), plus `bright_orange`
  and `bright_brown`.
- **Tailwind-inspired hue palette** — a flat list of Tailwind's 17 chromatic
  hue names (red, orange, amber, yellow, lime, green, emerald, teal, cyan,
  sky, blue, indigo, violet, purple, fuchsia, pink, rose) with **two
  documented adaptations**:
  - `brown` is **added** — Tailwind omits it, but base24/ANSI and terminals
    need it.
  - `maroon` is **not a slot** — it is reachable by luminance (`darken red`).

  `magenta` maps onto Tailwind `fuchsia`.
- **Tinted backgrounds** and a **role core** (`primary`, `secondary`, `error`,
  `warning`, `success`, `info`, `selection`, `cursor`, `border`), both
  computed. Roles do not duplicate neutrals — anything needing the default
  text/background colour references `neutrals.foreground` /
  `neutrals.background`.

## Provenance categories

| Category | Form | In this map |
|---|---|---|
| upstream | literal hex present in the scheme's authoritative source (native) | no |
| derived | value produced by a Nix expression — a `colourLib` call, or an alias to another slot | yes |
| adjusted | hand-picked literal hex that does **not** match the source (a personalisation) | yes, plus an inline comment in `palette.nix` |

Provenance tracks the **hex**, not the name. A slot tagged `upstream` may carry
a Tailwind name that differs from the source palette's own name for that hex
(e.g. Catppuccin's `mauve` fills the `violet` slot). `colourLib` shifts
luminance only, not hue, so a `derived` hue is the nearest native colour
lightened or darkened. Role aliases are recorded as `derived` with method
`alias -> <slot>`.

## Authoritative sources

- **edge** — `sainnhe/edge`, dark background, `style = default`
  (`autoload/edge.vim`). No native orange, no bright band.
- **everforest** — `sainnhe/everforest`, dark background, `background = medium`
  (`autoload/everforest.vim`). Muted palette — many Tailwind hues are derived.
- **catppuccin-latte** — `catppuccin/palette`, `latte` flavour
  (`palette.json`).

### Upstream name → Tailwind slot mapping (catppuccin-latte)

| Latte source name | Tailwind slot | Hex |
|---|---|---|
| red | `red` | `#d20f39` |
| peach | `orange` | `#fe640b` |
| yellow | `yellow` | `#df8e1d` |
| green | `green` | `#40a02b` |
| teal | `teal` | `#179299` |
| sapphire | `cyan` | `#209fb5` |
| sky | `sky` | `#04a5e5` |
| blue | `blue` | `#1e66f5` |
| lavender | `indigo` | `#7287fd` |
| mauve | `violet` | `#8839ef` |
| pink | `fuchsia` | `#ea76cb` |
| flamingo | `pink` | `#dd7878` |
| maroon | `rose` | `#e64553` |

## edge (dark)

### adjusted (personalisation)

| Slot | Value | Note |
|---|---|---|
| `orange` (hue) | `#e59676` | edge has no native orange; this literal is carried over from the v1 theme. A deliberate personalisation. |

### derived

| Slot | Group | Value | Method |
|---|---|---|---|
| `bright_red` | brights | `#ee878d` | `lighten red 15` |
| `bright_orange` | brights | `#e8a58a` | `lighten orange 15` |
| `bright_yellow` | brights | `#e2c388` | `lighten yellow 15` |
| `bright_green` | brights | `#aed193` | `lighten green 15` |
| `bright_cyan` | brights | `#82cacf` | `lighten cyan 15` |
| `bright_blue` | brights | `#77aed5` | `lighten blue 15` |
| `bright_magenta` | brights | `#dda7ee` | `lighten fuchsia 15` |
| `bright_brown` | brights | `#b78571` | `lighten brown 15` |
| `amber` | hues | `#ccaa6a` | `darken yellow 8` |
| `lime` | hues | `#aed193` | `lighten green 15` |
| `emerald` | hues | `#53a8ad` | `darken teal 10` |
| `cyan` | hues | `#6dc1c7` | `lighten teal 10` |
| `blue` | hues | `#5fa0ce` | `darken sky 12` |
| `indigo` | hues | `#5088af` | `darken blue 15` |
| `purple` | hues | `#d591eb` | `lighten violet 6` |
| `fuchsia` | hues | `#d898ec` | `lighten violet 12` |
| `pink` | hues | `#da9fed` | `lighten violet 18` |
| `rose` | hues | `#ed7a81` | `lighten red 6` |
| `brown` | hues | `#ab7058` | `darken orange 25` (adaptation) |
| `bg_red` | backgrounds | `#592b2d` | `darken red 62` |
| `bg_green` | backgrounds | `#3c4c30` | `darken green 62` |
| `bg_blue` | backgrounds | `#243c4e` | `darken blue 62` |
| `bg_yellow` | backgrounds | `#54462c` | `darken yellow 62` |
| `bg_visual` | backgrounds | `#523959` | `darken fuchsia 62` |
| `primary` | roles | `#a0c980` | `alias -> green` |
| `secondary` | roles | `#5fa0ce` | `alias -> blue` |
| `error` | roles | `#ec7279` | `alias -> red` |
| `warning` | roles | `#e59676` | `alias -> orange` (aliases the adjusted orange) |
| `success` | roles | `#a0c980` | `alias -> green` |
| `info` | roles | `#5fa0ce` | `alias -> blue` |
| `selection` | roles | `#3b3e48` | `alias -> overlay` |
| `cursor` | roles | `#c5cdd9` | `alias -> foreground` |
| `border` | roles | `#535c6a` | `alias -> muted` |

## everforest (dark)

No adjusted slots. The palette is muted, so many Tailwind hues are derived.

### derived

| Slot | Group | Value | Method |
|---|---|---|---|
| `background_darkest` | neutrals | `#1b1f23` | `darken bg0 40` |
| `bright_red` | brights | `#e99193` | `lighten red 15` |
| `bright_orange` | brights | `#e9a789` | `lighten orange 15` |
| `bright_yellow` | brights | `#e0c692` | `lighten yellow 15` |
| `bright_green` | brights | `#b4c993` | `lighten green 15` |
| `bright_cyan` | brights | `#9eccc6` | `lighten cyan 15` |
| `bright_blue` | brights | `#92c5be` | `lighten blue 15` |
| `bright_magenta` | brights | `#dca8c0` | `lighten fuchsia 15` |
| `bright_brown` | brights | `#b88770` | `lighten brown 15` |
| `amber` | hues | `#c9ac74` | `darken yellow 8` |
| `lime` | hues | `#b4c993` | `lighten green 15` |
| `teal` | hues | `#73a880` | `darken emerald 12` |
| `cyan` | hues | `#8ec3bc` | `lighten blue 12` |
| `sky` | hues | `#98c8c2` | `lighten blue 20` |
| `indigo` | hues | `#689992` | `darken blue 18` |
| `violet` | hues | `#c089a3` | `darken fuchsia 10` |
| `purple` | hues | `#d79db8` | `lighten fuchsia 4` |
| `pink` | hues | `#daa5be` | `lighten fuchsia 12` |
| `rose` | hues | `#e78587` | `lighten red 6` |
| `brown` | hues | `#ac7257` | `darken orange 25` (adaptation) |
| `bg_red` | backgrounds | `#572f30` | `darken red 62` |
| `bg_green` | backgrounds | `#3f4830` | `darken green 62` |
| `bg_blue` | backgrounds | `#304744` | `darken blue 62` |
| `bg_yellow` | backgrounds | `#534730` | `darken yellow 62` |
| `bg_visual` | backgrounds | `#513a45` | `darken fuchsia 62` |
| `primary` | roles | `#a7c080` | `alias -> green` |
| `secondary` | roles | `#7fbbb3` | `alias -> blue` |
| `error` | roles | `#e67e80` | `alias -> red` |
| `warning` | roles | `#e69875` | `alias -> orange` |
| `success` | roles | `#a7c080` | `alias -> green` |
| `info` | roles | `#7fbbb3` | `alias -> blue` |
| `selection` | roles | `#475258` | `alias -> overlay` |
| `cursor` | roles | `#d3c6aa` | `alias -> foreground` |
| `border` | roles | `#7a8478` | `alias -> muted` |

## catppuccin-latte (light)

No adjusted slots, and the neutrals are all native. Thirteen hue slots map
straight to upstream latte colours (see the mapping table above); the rest are
derived.

### derived

| Slot | Group | Value | Method |
|---|---|---|---|
| `bright_red` | brights | `#b80d32` | `darken red 12` |
| `bright_orange` | brights | `#df5809` | `darken orange 12` |
| `bright_yellow` | brights | `#c47c19` | `darken yellow 12` |
| `bright_green` | brights | `#388c25` | `darken green 12` |
| `bright_cyan` | brights | `#1c8b9f` | `darken cyan 12` |
| `bright_blue` | brights | `#1a59d7` | `darken blue 12` |
| `bright_magenta` | brights | `#cd67b2` | `darken fuchsia 12` |
| `bright_brown` | brights | `#9b3d06` | `darken brown 12` |
| `amber` | hues | `#cd821a` | `darken yellow 8` |
| `lime` | hues | `#5cae4a` | `lighten green 15` |
| `emerald` | hues | `#329fa5` | `lighten teal 12` |
| `purple` | hues | `#9650f0` | `lighten violet 12` |
| `brown` | hues | `#b14607` | `darken orange 30` (adaptation) |
| `bg_red` | backgrounds | `#f6d3db` | `lighten red 82` |
| `bg_green` | backgrounds | `#dcedd8` | `lighten green 82` |
| `bg_blue` | backgrounds | `#d6e3fd` | `lighten blue 82` |
| `bg_yellow` | backgrounds | `#f9ead6` | `lighten yellow 82` |
| `bg_visual` | backgrounds | `#fbe6f5` | `lighten fuchsia 82` |
| `primary` | roles | `#40a02b` | `alias -> green` |
| `secondary` | roles | `#1e66f5` | `alias -> blue` |
| `error` | roles | `#d20f39` | `alias -> red` |
| `warning` | roles | `#fe640b` | `alias -> orange` |
| `success` | roles | `#40a02b` | `alias -> green` |
| `info` | roles | `#1e66f5` | `alias -> blue` |
| `selection` | roles | `#ccd0da` | `alias -> overlay` |
| `cursor` | roles | `#4c4f69` | `alias -> foreground` |
| `border` | roles | `#9ca0b0` | `alias -> muted` |

Note: on the light scheme, brights are `darken`ed and tinted backgrounds are
`lighten`ed toward white — the light-vs-dark conditional mirrors the pattern
in `modules/programs/prism/pi.nix`.
