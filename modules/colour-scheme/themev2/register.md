# themev2 divergence register

This is the authoritative provenance record for the `themev2` base26 schema
(migration increment #1). It lists every slot whose provenance is **derived**
or **adjusted**, across the three sample schemes. The palette data lives in
`palette.nix`; the schema option lives in `../schema.nix`; the visual form of
this register is `nix run .#theme-preview`.

## Provenance model

Every slot carries one of three provenance categories:

| Category | Form | In this register |
|---|---|---|
| upstream | literal hex equal to the scheme's authoritative source | no |
| derived | value produced by a Nix expression — a `colourLib` call, or an alias to another slot | yes |
| adjusted | literal hex that does **not** match the source | yes, plus an inline comment in `palette.nix` |

The value form is a secondary signal: an `adjusted` literal looks exactly like
an `upstream` one. This register is the primary record.

Note on aliases: a role-core slot that points at a base slot is recorded as
`derived` with method `alias -> <slot>`, because its value is produced by a
Nix reference rather than being a literal from the source.

## Authoritative sources

- **edge** — `sainnhe/edge`, dark background, `style = default`
  (`autoload/edge.vim`). Upstream has **no orange slot** and no bright band.
- **everforest** — `sainnhe/everforest`, dark background, `background = medium`
  (`autoload/everforest.vim`). v1 matched upstream exactly.
- **catppuccin-latte** — `catppuccin/palette`, `latte` flavour
  (`palette.json`).

Historical context: `onedark` (not a sample scheme here) carries a known
undocumented orange fudge in v1. `edge` carries the same class of divergence —
its v1 orange `#e59676` has no upstream source. Both are recorded honestly as
`adjusted` where they appear in a themev2 scheme.

## edge (dark)

### adjusted

| Slot | Value | Source note |
|---|---|---|
| `orange` | `#e59676` | edge upstream has no orange slot; value carried over from the v1 theme. |

### derived

| Slot | Value | Method |
|---|---|---|
| `base07` | `#d0d7e0` | `lighten fg 20` |
| `brown` | `#ab7058` | `darken orange 25` |
| `bright_red` | `#ee878d` | `lighten red 15` |
| `bright_orange` | `#e8a58a` | `lighten orange 15` |
| `bright_yellow` | `#e2c388` | `lighten yellow 15` |
| `bright_green` | `#aed193` | `lighten green 15` |
| `bright_cyan` | `#75c5ca` | `lighten cyan 15` |
| `bright_blue` | `#82c0ee` | `lighten blue 15` |
| `bright_magenta` | `#d99bed` | `lighten magenta 15` |
| `bright_brown` | `#b78571` | `lighten brown 15` |
| `bg_red` | `#592b2d` | `darken red 62` |
| `bg_green` | `#3c4c30` | `darken green 62` |
| `bg_blue` | `#294559` | `darken blue 62` |
| `bg_yellow` | `#54462c` | `darken yellow 62` |
| `bg_visual` | `#503458` | `darken magenta 62` |
| `error` | `#ec7279` | `alias -> red` |
| `warning` | `#e59676` | `alias -> orange` (aliases the adjusted orange) |
| `success` | `#a0c980` | `alias -> green` |
| `info` | `#6cb6eb` | `alias -> blue` |
| `selection` | `#3b3e48` | `alias -> base02` |
| `cursor` | `#c5cdd9` | `alias -> base05` |
| `border` | `#535c6a` | `alias -> base03` |

## everforest (dark)

No adjusted slots — v1 matched upstream exactly.

### derived

| Slot | Value | Method |
|---|---|---|
| `base06` | `#d8ccb4` | `lighten fg 12` |
| `base07` | `#dfd5c1` | `lighten fg 28` |
| `base11` | `#1b1f23` | `darken bg0 40` |
| `brown` | `#ac7257` | `darken orange 25` |
| `bright_red` | `#e99193` | `lighten red 15` |
| `bright_orange` | `#e9a789` | `lighten orange 15` |
| `bright_yellow` | `#e0c692` | `lighten yellow 15` |
| `bright_green` | `#b4c993` | `lighten green 15` |
| `bright_cyan` | `#95c9a2` | `lighten cyan 15` |
| `bright_blue` | `#92c5be` | `lighten blue 15` |
| `bright_magenta` | `#dca8c0` | `lighten magenta 15` |
| `bright_brown` | `#b88770` | `lighten brown 15` |
| `bg_red` | `#572f30` | `darken red 62` |
| `bg_green` | `#3f4830` | `darken green 62` |
| `bg_blue` | `#304744` | `darken blue 62` |
| `bg_yellow` | `#534730` | `darken yellow 62` |
| `bg_visual` | `#513a45` | `darken magenta 62` |
| `error` | `#e67e80` | `alias -> red` |
| `warning` | `#e69875` | `alias -> orange` |
| `success` | `#a7c080` | `alias -> green` |
| `info` | `#7fbbb3` | `alias -> blue` |
| `selection` | `#475258` | `alias -> base02` |
| `cursor` | `#d3c6aa` | `alias -> base05` |
| `border` | `#7a8478` | `alias -> base03` |

## catppuccin-latte (light)

No adjusted slots — the monotone band and accents all map to upstream latte
colours.

### derived

| Slot | Value | Method |
|---|---|---|
| `brown` | `#b14607` | `darken orange 30` |
| `bright_red` | `#b80d32` | `darken red 12` |
| `bright_orange` | `#df5809` | `darken orange 12` |
| `bright_yellow` | `#c47c19` | `darken yellow 12` |
| `bright_green` | `#388c25` | `darken green 12` |
| `bright_cyan` | `#148086` | `darken cyan 12` |
| `bright_blue` | `#1a59d7` | `darken blue 12` |
| `bright_magenta` | `#7732d2` | `darken magenta 12` |
| `bright_brown` | `#9b3d06` | `darken brown 12` |
| `bg_red` | `#f6d3db` | `lighten red 82` |
| `bg_green` | `#dcedd8` | `lighten green 82` |
| `bg_blue` | `#d6e3fd` | `lighten blue 82` |
| `bg_yellow` | `#f9ead6` | `lighten yellow 82` |
| `bg_visual` | `#e9dbfc` | `lighten magenta 82` |
| `error` | `#d20f39` | `alias -> red` |
| `warning` | `#fe640b` | `alias -> orange` |
| `success` | `#40a02b` | `alias -> green` |
| `info` | `#1e66f5` | `alias -> blue` |
| `selection` | `#ccd0da` | `alias -> base02` |
| `cursor` | `#4c4f69` | `alias -> base05` |
| `border` | `#9ca0b0` | `alias -> base03` |

Note: on the light scheme, brights are `darken`ed (a more saturated variant
reads correctly on a light background) and tinted backgrounds are `lighten`ed
toward white — the light-vs-dark conditional mirrors the pattern in
`modules/programs/prism/pi.nix`.
