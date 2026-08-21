# themev2 divergence register

This is the authoritative provenance record for the `themev2` schema
(migration increment #1). It lists every slot whose provenance is **derived**
or **adjusted**, across the three sample schemes. The palette data lives in
`palette.nix`; the schema option lives in `../schema.nix`; the visual form of
this register is `nix run .#theme-preview`.

## Palette shape

- **Neutral spine** — the base24 monotone slots `base00`–`base07` plus extra
  backgrounds `base10`/`base11`.
- **Bright band** — kept for ANSI correctness (kitty `color9`–`color14` map to
  `bright_red`/`yellow`/`green`/`cyan`/`blue`/`magenta`), plus `bright_orange`
  and `bright_brown`.
- **Tailwind-inspired hue palette** — follows Tailwind's 17 chromatic hue
  names (red, orange, amber, yellow, lime, green, emerald, teal, cyan, sky,
  blue, indigo, violet, purple, fuchsia, pink, rose) with **two documented
  adaptations**:
  - `brown` is **added** — Tailwind omits it, but base24/ANSI and terminals
    need it.
  - `maroon` is **not a slot** — it is reachable by luminance (`darken red`).

  This hue band replaces the old base24 accent band; `magenta` maps onto
  Tailwind `fuchsia`.
- **Tinted backgrounds** and a **role core**, both computed.

## Provenance model

Every slot carries one of three provenance categories:

| Category | Form | In this register |
|---|---|---|
| upstream | literal hex present in the scheme's authoritative source | no |
| derived | value produced by a Nix expression — a `colourLib` call, or an alias to another slot | yes |
| adjusted | literal hex that does **not** match the source | yes, plus an inline comment in `palette.nix` |

Provenance tracks the **hex**, not the name. A slot tagged `upstream` may now
carry a Tailwind name that differs from the source palette's own name for that
hex (e.g. Catppuccin's `mauve` fills the `violet` slot). `colourLib` shifts
luminance only, not hue, so a `derived` hue is the nearest native colour
lightened or darkened. Role-core aliases are recorded as `derived` with method
`alias -> <slot>`, because their value is a Nix reference, not a source
literal.

## Authoritative sources

- **edge** — `sainnhe/edge`, dark background, `style = default`
  (`autoload/edge.vim`). Upstream has **no orange slot** and no bright band.
- **everforest** — `sainnhe/everforest`, dark background, `background = medium`
  (`autoload/everforest.vim`). Muted palette — many Tailwind hues are derived.
- **catppuccin-latte** — `catppuccin/palette`, `latte` flavour
  (`palette.json`).

Historical context: `onedark` (not a sample scheme here) carries a known
undocumented orange fudge in v1. `edge` carries the same class of divergence —
its v1 orange `#e59676` has no upstream source. It is recorded honestly as
`adjusted`.

### Upstream name → Tailwind slot mapping (catppuccin-latte)

Latte has enough colours to fill most hue slots directly. The source name and
the slot it fills:

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

### adjusted

| Slot | Value | Source note |
|---|---|---|
| `orange` (hue) | `#e59676` | edge upstream has no orange slot; value carried over from the v1 theme. |

### derived

| Slot | Group | Value | Method |
|---|---|---|---|
| `base07` | spine | `#d0d7e0` | `lighten fg 20` |
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
| `error` | roles | `#ec7279` | `alias -> red` |
| `warning` | roles | `#e59676` | `alias -> orange` (aliases the adjusted orange) |
| `success` | roles | `#a0c980` | `alias -> green` |
| `info` | roles | `#5fa0ce` | `alias -> blue` |
| `selection` | roles | `#3b3e48` | `alias -> base02` |
| `cursor` | roles | `#c5cdd9` | `alias -> base05` |
| `border` | roles | `#535c6a` | `alias -> base03` |

## everforest (dark)

No adjusted slots. The palette is muted, so many Tailwind hues are derived.

### derived

| Slot | Group | Value | Method |
|---|---|---|---|
| `base06` | spine | `#d8ccb4` | `lighten fg 12` |
| `base07` | spine | `#dfd5c1` | `lighten fg 28` |
| `base11` | spine | `#1b1f23` | `darken bg0 40` |
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
| `error` | roles | `#e67e80` | `alias -> red` |
| `warning` | roles | `#e69875` | `alias -> orange` |
| `success` | roles | `#a7c080` | `alias -> green` |
| `info` | roles | `#7fbbb3` | `alias -> blue` |
| `selection` | roles | `#475258` | `alias -> base02` |
| `cursor` | roles | `#d3c6aa` | `alias -> base05` |
| `border` | roles | `#7a8478` | `alias -> base03` |

## catppuccin-latte (light)

No adjusted slots. Thirteen hue slots map straight to upstream latte colours
(see the mapping table above); the rest are derived.

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
| `error` | roles | `#d20f39` | `alias -> red` |
| `warning` | roles | `#fe640b` | `alias -> orange` |
| `success` | roles | `#40a02b` | `alias -> green` |
| `info` | roles | `#1e66f5` | `alias -> blue` |
| `selection` | roles | `#ccd0da` | `alias -> base02` |
| `cursor` | roles | `#4c4f69` | `alias -> base05` |
| `border` | roles | `#9ca0b0` | `alias -> base03` |

Note: on the light scheme, brights are `darken`ed (a more saturated variant
reads correctly on a light background) and tinted backgrounds are `lighten`ed
toward white — the light-vs-dark conditional mirrors the pattern in
`modules/programs/prism/pi.nix`.
