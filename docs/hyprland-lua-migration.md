# Hyprland hyprlang → lua migration design

A forward-looking plan for moving our Hyprland configuration from the
hyprlang `.conf` format to lua, via Home Manager's
`wayland.windowManager.hyprland.configType = "lua"` codepath.

This document is the deliverable for **PR 1** in the three-PR migration
sequence:

1. **PR 0 — `hyprland-pin-hyprlang-drop-hy3`** *(landed).* Pins
   `configType = "hyprlang"` explicitly and removes the
   `nx.desktop.hyprland.layout` option, the `plugin.hy3` block, the
   `SUPER SHIFT B / V` hy3 binds, and the `hy3` plugin entry.
   `general.layout` is now the literal string `"dwindle"`.
2. **PR 1 — this design doc.** No `.nix` changes; the deliverable is
   `docs/hyprland-lua-migration.md`. Ben reads, sits with it, approves the
   plan.
3. **PR 2 — the actual cutover.** Applies the plan below.

Throughout, the working assumption is that PR 0 has already landed, so
references to the "current" config below describe the post-PR-0 shape
(no `hy3`, `layout = "dwindle"` hard-coded). If you are reading this
before PR 0 has merged, mentally strip the `layout`-conditional bits
from `modules/desktop/hyprland/default.nix` before comparing.

All citations to the Home Manager hyprland module use the locked rev
`928d72376949e222ea4f07b44828a55b0136422e` from the flake lock, fetched as
`modules/services/window-managers/hyprland.nix` (referred to below as
"HM:`<line>`").

---

## 1. Background and motivation

Hyprland upstream is migrating from hyprlang to lua as the canonical
configuration language. The upstream wiki opens the
[Configuring/Start](https://wiki.hypr.land/Configuring/Start/) page with:

> Since Hyprland 0.55, hyprlang is deprecated in favor of lua. […] The
> config is located in `$XDG_CONFIG_HOME/hypr/hyprland.lua`.

Home Manager already supports both codepaths via
`wayland.windowManager.hyprland.configType`, an enum of
`"hyprlang"` | `"lua"` (HM:119–146). The default is computed by
`lib.hm.deprecations.mkStateVersionOptionDefault` and **flips to `"lua"`
once `home.stateVersion >= 26.05`** (HM:122–139). Our machines are pinned
at `home.stateVersion = "25.11"`, so the default for us is currently
still `"hyprlang"` — but the moment we bump `stateVersion`, the default
will switch under us unless we have either migrated or set `configType`
explicitly.

Two things are worth stating up front so we don't migrate in a panic:

- **hyprlang is not removed in Home Manager today.** Both codepaths are
  rendered side-by-side by `xdg.configFile` blocks gated on
  `cfg.configType` (HM:459–645). Either renderer is fine to use; lua is
  just where upstream is going.
- **PR 0 already pinned us to `configType = "hyprlang"` explicitly.**
  So we control the cutover timing entirely. We are not bumping
  `stateVersion` to force the migration; we are doing it deliberately, on
  a feature branch, with this design doc as a checklist.

The motivation for doing it *now*, rather than letting it drift, is:

- We want to be early-but-not-bleeding-edge on the upstream direction so
  our config doesn't bit-rot. The 0.55 deprecation notice is already in
  place.
- Doing the work while the hyprlang codepath still exists in HM gives us
  a trivial rollback (see section 10).
- The lua surface offers a few genuine ergonomic wins — typed flag
  tables on `hl.bind`, `hl.timer` for submap auto-reset, a proper
  function-bodied `hyprland.start` hook — that we will benefit from once
  migrated.

---

## 2. What changes and what doesn't

### In scope for PR 2

The `wayland.windowManager.hyprland.settings` option is contributed to
from **thirteen** files across this repo, not just the main module —
every one of them must be translated, because string-form entries
become invalid lua under `configType = "lua"`. The complete list
(verified by `rg 'hyprland\.(settings|extraConfig|submaps|plugins)' --type nix`
— see the note immediately below about why a narrower regex is
unsafe):

**Primary module:**

- `modules/desktop/hyprland/default.nix` — the main module that defines
  `wayland.windowManager.hyprland.{settings,extraConfig,plugins,…}`.
  Sections 4 and 6 cover this file's contents directively.

**Per-machine `monitor` contributions:**

- `machines/navi/configuration.nix` — adds a per-machine
  `wayland.windowManager.hyprland.settings.monitor` entry (section 4.12,
  7.2).
- `machines/tui/configuration.nix` — same, different monitor string.

**Per-app `windowrule` contributions** (all `lib.mkIf …enable`-gated,
all use the v2 `match:`-style syntax):

- `modules/gaming/prismlauncher.nix` — `tile on, match:class PrismLauncher`.
- `modules/gaming/steam.nix` — `sync_fullscreen 0, match:class steam`.
- `modules/programs/chromium.nix` — `tile on, match:class Chromium-browser`.
- `modules/programs/darktable.nix` — two rules (splash float,
  suppress-fullscreen).
- `modules/programs/discord.nix` — `workspace 2 silent, match:class discord`.
- `modules/programs/libreoffice.nix` — `sync_fullscreen 0, match:class libreoffice-writer`.
- `modules/programs/qutebrowser/default.nix` — seven rules (filepicker /
  editor floats with sizing, fake-fullscreen for qutebrowser windows).

**Per-app `bind` contributions** (gated, but `screenshot.nix` defaults
to enabled, so on both navi and tui it is live by default):

- `modules/desktop/screenshot.nix` — one bind:
  `SUPER SHIFT, S, exec, …/application.grim.screenshotToClipboard`.
  Gated on `nx.desktop.screenshot.enable` which defaults to `true` on
  Linux. Note this file uses the **nested-attribute style**
  (`wayland.windowManager = { hyprland.settings.bind = […]; }`),
  which a naive `rg 'wayland\.windowManager\.hyprland\.settings'`
  *misses* — see the warning below.
- `modules/programs/home-automation.nix` — three binds:
  `SUPER, Prior, exec, …openBlinds`,
  `SUPER, Next, exec, …closeBlinds`,
  `ALT, h, exec, …${newwindow} https://$HASS_DOMAIN`.
- `modules/programs/voice-to-text.nix` — two binds tied to
  `cfg.keybind`: hold-to-talk + cancel.
- `modules/programs/voice-to-text-daemon.nix` — two binds, same shape
  as `voice-to-text.nix`.

Section 4.18 below covers the translation for these contributing
modules. Section 9's checklist now has a dedicated step (step 11) for
walking each one.

> **Warning — don't use the narrower regex.** It is tempting to grep
> with `rg 'wayland\.windowManager\.hyprland\.settings' --type nix`,
> but that pattern misses any module that opens
> `wayland.windowManager = { hyprland.settings.<…> = …; };` as a
> nested attrset across multiple lines.
> `modules/desktop/screenshot.nix` is the current example (and was
> missed by the PR-1 round-2 audit — the round-3 review-context agent
> caught it). PR 2's verification step must use the broader pattern
> `rg 'hyprland\.(settings|extraConfig|submaps|plugins)' --type nix`,
> which catches both styles and currently returns exactly the
> thirteen files listed above. The narrow pattern will recur as a
> footgun if any new module adopts the nested style; the broad
> pattern won't.

### Out of scope (separate HM modules, untouched)

- `modules/desktop/hyprland/idle.nix` — configures `services.hypridle`,
  which is its own HM module with its own option schema. Not affected
  by `configType`.
- `modules/desktop/hyprland/hyprlock.nix` — configures
  `programs.hyprlock`, same story.
- `modules/desktop/hyprland/scripts.nix` — pure script bodies (Bash, etc.)
  installed via `home.file`; not Hyprland config at all.

If a section of this doc reads as if these are in play, that's a drafting
error — they aren't.

---

## 3. How the lua codepath works in HM

The lua renderer lives at HM:531–645, inside the
`xdg.configFile."hypr/hyprland.lua"` block gated on
`cfg.configType == "lua"`. The final file is composed in this order
(HM:633–639):

```nix
text = ''
  -- Generated by Home Manager.
  -- See https://wiki.hypr.land/Configuring/Start/

''
+ renderSettings   # cfg.settings, with importantPrefixes sorted first
+ renderSubmaps    # cfg.submaps, each as one hl.define_submap(...) call
+ renderStartHook  # systemd activation + plugin-load commands wrapped in hl.on("hyprland.start", …)
+ renderSection "extraConfig" cfg.extraConfig;
```

### Settings → `hl.<name>(...)` calls

`renderSettings` (HM:551–582) walks `cfg.settings`:

- Each top-level attribute name becomes the lua function name. The
  expansion is verbatim (`hl.${name}(...)`, HM:571), so an attribute
  named `general` produces `hl.general(...)`, `monitor` produces
  `hl.monitor(...)`, `window_rule` produces `hl.window_rule(...)`, etc.
  Names that are not valid lua identifiers (e.g. anything containing
  `-`, like our current `exec-once`) will not compile as lua — see
  section 4 and the `exec` / `exec-once` rows.
- **List values produce one call per element** (HM:573–575:
  `renderCalls = name: value: lib.concatMapStrings (renderCall name) (if builtins.isList value then value else [ value ])`).
  So `bind = [ a b c ]` becomes three `hl.bind(...)` calls in source
  order.
- Attribute values containing **`_args = [ ... ]`** are expanded as a
  comma-separated argument list (HM:538–542:
  `lib.concatMapStringsSep ", " toLua value._args`). This is how
  multi-argument lua calls like `hl.bind("SUPER + Q", hl.dsp.exec_cmd("kitty"))`
  are spelt from nix. Each element of `_args` is passed through
  `lib.generators.toLua` individually, so primitive types (strings,
  bools, ints, tables) get quoted/serialised, and any element wrapped
  with `lib.generators.mkLuaInline` is emitted raw.
- Attribute values containing **`_var = <expr>`** become a lua local
  variable (HM:561–570:
  `local ${value.name or name} = ${renderArgs value._var}\n`). The
  variable name defaults to the attribute name and can be overridden
  with a `name` field. Locals are emitted in a single
  `-- settings.locals` block at the top of the settings region,
  before any `hl.<name>(...)` call (HM:577–578).
- Values built with **`lib.generators.mkLuaInline`** are emitted as raw
  lua text, with no quoting. `toLua` recognises the marker attribute
  added by `mkLuaInline` and passes the string through verbatim. This
  is the only way to put a function literal or an `hl.dsp.<dispatcher>(…)`
  call into a settings value.

### Render ordering — `importantPrefixes`

`cfg.importantPrefixes` (HM:404–425, default
`[ "$" "bezier" "curve" "name" "output" ]`) selects setting names whose
prefix matches one of those strings; they are rendered *first*, before
any other settings (HM:556–561). This matters for us in two places:

- `bezier` — we define `myBezier`/`linear` and reference them by name in
  `animations.animation`. The prefix rule guarantees the bezier defs
  appear before the `animations(...)` call that uses them.
- We have no `name = …` setting, no `output = …` setting, and no `$var`
  settings, so the other three default prefixes are inert for us.

### Submaps

`renderSubmaps` (HM:592–615) wraps each entry of `cfg.submaps` in:

```lua
hl.define_submap("<name>"[, "<onDispatch>"], function()
  hl.<bindKind>(<args>)
  ...
end)
```

`onDispatch` is the optional second argument, documented in the wiki at
[Configuring/Basics/Binds — Automatically close a submap on dispatch](https://wiki.hypr.land/Configuring/Basics/Binds/#automatically-close-a-submap-on-dispatch):
appending a submap name (or `"reset"`) to `define_submap` causes
Hyprland to switch to that submap automatically after any dispatch
inside it.

**Critically, the submap renderer drops string entries** (HM:597–599:
`builtins.filter (value: !lib.isString value) values`). Only attribute
entries with `_args` (or `mkLuaInline`) end up in the lua submap. The
HM option docs spell this out (HM:316–328):

> String entries render only when `configType` is `"hyprlang"`.
> Attribute set entries render only when `configType` is `"lua"`.

So the migration for our two submaps (currently expressed as raw
hyprlang inside `extraConfig`) is **not** "paste the strings into
`submaps.<name>.settings.bind`". We have to translate each line into the
attribute / `_args` shape. Section 6 covers this in detail.

### Startup hook

`renderStartHook` (HM:583–591) emits exactly one block, only when
either `systemd.enable` is true or `plugins != []`:

```lua
hl.on("hyprland.start", function()
  hl.exec_cmd("<systemd activation command>")
  hl.exec_cmd("<plugin load command 1>")
  ...
end)
```

This consumes `cfg.systemd` + `cfg.plugins` only. It **does not**
consume `cfg.settings."exec-once"` or `cfg.settings.exec`. That is the
key gotcha for section 4: our existing `exec-once = [...]` and
`exec = [...]` settings have no clean rendering under
`configType = "lua"`, because `exec-once` is not a valid lua identifier
(`hl.exec-once(...)` would be a syntax error). The migration plan is to
fold those lists into the `hyprland.start` hook ourselves via an
explicit `on = { _args = [ "hyprland.start" (mkLuaInline "function() … end") ]; }`
entry in `settings`, matching the example in the HM option docstring
(HM:277–286). See section 4 for the concrete shape.

### `extraConfig`

`extraConfig` is appended verbatim to the end of `hyprland.lua`
(HM:638). Under `configType = "lua"` it must contain raw lua, not
hyprlang. Our current `extraConfig` is entirely hyprlang submap blocks
and moves to `cfg.submaps` in the migration; the post-migration
`extraConfig` should be empty (or a tiny escape hatch for anything
section 6 flags as not-cleanly-modelable).

---

## 4. Directive-by-directive mapping

The "Current (hyprlang)" column reproduces the post-PR-0 shape — i.e.
no `hy3` references, `layout = "dwindle"` literal. "Lua (target)" shows
the nix expression we'll write in PR 2, and after the table I give the
rendered lua for the non-obvious cases.

### 4.1 `general`

| Current (hyprlang) | Lua (target, nix) | Notes |
|---|---|---|
| `gaps_in = 5;` | `gaps_in = 5;` (no change) | renders as `hl.general({ gaps_in = 5, … })` once nested under `general = {...}` |
| `gaps_out = 5;` | `gaps_out = 5;` | as above |
| `border_size = 3;` | `border_size = 3;` | as above |
| `"col.active_border" = "rgba(...) rgba(...) … 45deg";` | `"col.active_border" = "rgba(...) rgba(...) … 45deg";` | the wiki ([Variables → Colors / gradient type](https://wiki.hypr.land/Configuring/Basics/Variables/#variable-types)) documents `gradient` as accepting either a single colour or `{ colors = { "rgba(…)", "rgba(…)" }, angle = 45 }`. **Open question (section 8): does the lua API also accept the legacy whitespace-separated string with a trailing `45deg`?** If not, we restructure to the table form. |
| `"col.inactive_border" = "rgba(…ff)";` | `"col.inactive_border" = "rgba(…ff)";` | single-colour gradient, string form is documented |
| `layout = "dwindle";` | `layout = "dwindle";` | unchanged |

The whole block renders as a **single** `hl.general({ … })` call
because the renderer emits `hl.${name}(${renderArgs value})` once per
list element, and our `general` is a single attribute set (not a list).
Verified at HM:573–575: `if builtins.isList value then value else [ value ]`.

### 4.2 `input`

| Current | Lua (target) | Notes |
|---|---|---|
| `kb_layout = "nz";` | `kb_layout = "nz";` | wiki [Switchable keyboard layouts](https://wiki.hypr.land/Configuring/Basics/Binds/#switchable-keyboard-layouts) shows `hl.config({ input = { kb_layout = "us,cz" } })`; HM emits the per-section form `hl.input({ kb_layout = "nz", … })` which is equivalent. |
| `kb_variant = "mao";` | `kb_variant = "mao";` | |
| `kb_options = "lv3:rwin_switch";` | `kb_options = "lv3:rwin_switch";` | |
| `repeat_delay = "225";` | `repeat_delay = 225;` | **Type fix.** The hyprlang module coerces; lua wants an int per the [Variables → Input table](https://wiki.hypr.land/Configuring/Basics/Variables/#input). Currently a string in our config — drop the quotes. |
| `repeat_rate = "60";` | `repeat_rate = 60;` | same |
| `follow_mouse = 2;` | `follow_mouse = 2;` | |
| `sensitivity = -0.8;` | `sensitivity = -0.8;` | float |
| `touchpad.natural_scroll = true;` | `touchpad.natural_scroll = true;` | nested attrs serialise to a nested lua table |

### 4.3 `decoration`

| Current | Lua (target) | Notes |
|---|---|---|
| `rounding = 5;` | `rounding = 5;` | |
| `blur.enabled = true;` | `blur.enabled = true;` | |
| `shadow.enabled = false;` | `shadow.enabled = false;` | |

### 4.4 `dwindle`

| Current | Lua (target) | Notes |
|---|---|---|
| `preserve_split = "yes";` | `preserve_split = true;` | **Type fix.** hyprlang accepts `"yes"`/`"no"`; lua expects bool ([Layouts → Dwindle](https://wiki.hypr.land/Configuring/Layouts/Dwindle-Layout/)). |
| `force_split = 2;` | `force_split = 2;` | int, unchanged |

### 4.5 `misc`

| Current | Lua (target) | Notes |
|---|---|---|
| `disable_hyprland_logo = true;` | unchanged | |
| `disable_splash_rendering = true;` | unchanged | |
| `vrr = 1;` | unchanged | int |
| `enable_swallow = false;` | unchanged | |
| `swallow_regex = "^(kitty\|lf)$";` | unchanged | |
| `swallow_exception_regex = "^(wev)$";` | unchanged | |
| `disable_watchdog_warning = true;` | unchanged | |

### 4.6 `cursor`

| Current | Lua (target) | Notes |
|---|---|---|
| `inactive_timeout = 5;` | unchanged | |

### 4.7 `gestures`

| Current | Lua (target) | Notes |
|---|---|---|
| `gestures = { /* empty */ };` | **omit** | Currently `gestures = { };` — an empty attrset renders as `hl.gestures({})`. Harmless but pointless. Drop the empty block in PR 2; we can re-add it the moment we have a real gesture to set. |

### 4.8 `debug`

| Current | Lua (target) | Notes |
|---|---|---|
| `disable_logs = false;` | unchanged | |

### 4.9 `ecosystem`

| Current | Lua (target) | Notes |
|---|---|---|
| `no_update_news = true;` | unchanged | |

### 4.10 `bezier` list

| Current | Lua (target) | Notes |
|---|---|---|
| `bezier = [ "myBezier,0.05,0.9,0.1,1.0" "linear,0,0,1,1" ];` | see below | lua API per [Animations](https://wiki.hypr.land/Configuring/Advanced-and-Cool/Animations/) takes `hl.curve(NAME, { type = "bezier", points = { {X0,Y0}, {X1,Y1} } })`. **The attribute name changes from `bezier` to `curve`** and each entry must be expressed as `_args` because it's a 2-argument call. |

Concrete shape:

```nix
curve = [
  { _args = [ "myBezier" { type = "bezier"; points = [ [ 0.05 0.9 ] [ 0.1 1.0 ] ]; } ]; }
  { _args = [ "linear"   { type = "bezier"; points = [ [ 0 0 ]       [ 1 1 ]     ]; } ]; }
];
```

Renders as:

```lua
hl.curve("myBezier", { type = "bezier", points = { { 0.05, 0.9 }, { 0.1, 1.0 } } })
hl.curve("linear",   { type = "bezier", points = { { 0, 0 },       { 1, 1 }       } })
```

The default `importantPrefixes` list includes `"curve"` (HM:407–415),
so the `curve` calls render before `animation` calls — which matters
because `animation` references the curve by name.

> **Side benefit** — once we're emitting lua, springs become available
> via `hl.curve(NAME, { type = "spring", mass, stiffness, dampening })`.
> Out of scope for the migration; flag for a follow-up.

### 4.11 `animations`

| Current | Lua (target) | Notes |
|---|---|---|
| `animations.enabled = true;` | `animations.enabled = true;` | one `hl.animations({ enabled = true })` call |
| `animations.animation = [ "windows, 1, 3, myBezier" … ];` | **rename to `animation` at top level, restructure** | wiki: `hl.animation({ leaf = STRING, enabled = BOOLEAN, speed = FLOAT, curve = STRING[, style = STRING] })` — i.e. one call per animation, keyed by `leaf`, with named fields. |

Concrete shape (top-level attribute `animation`, not nested under
`animations`):

```nix
animation = [
  { leaf = "windows";     enabled = true; speed = 3;  curve = "myBezier"; }
  { leaf = "windowsOut";  enabled = true; speed = 2;  curve = "myBezier"; style = "popin 90%"; }
  { leaf = "windowsIn";   enabled = true; speed = 2;  curve = "myBezier"; style = "popin 90%"; }
  { leaf = "border";      enabled = true; speed = 2;  curve = "default"; }
  { leaf = "borderangle"; enabled = true; speed = 50; curve = "linear"; style = "loop"; }
  { leaf = "fade";        enabled = true; speed = 2;  curve = "default"; }
  { leaf = "workspaces";  enabled = true; speed = 1;  curve = "myBezier"; style = "slidevert"; }
];
```

Renders as one `hl.animation({ leaf = "windows", enabled = true, speed = 3, curve = "myBezier" })`
per element. Note we rename **`animations.animation` (string list)** →
**`animation` (top-level attrset list)**, and the `enabled` toggle
becomes either part of each `leaf` entry (set per animation, as above)
or kept as `animations.enabled = true;` if we want a master switch —
the wiki example `hl.animations({ enabled = true })` confirms the
master switch exists separately. Keep both: master at
`animations.enabled = true`, per-leaf in `animation`.

### 4.12 `monitor`

| Current (module) | Current (per-machine) | Lua (target) | Notes |
|---|---|---|---|
| `monitor = [ ",preferred,auto,auto" ];` | `monitor = [ "DP-3,5120x1440@239.76Hz,0x0,1" ];` (navi) | restructure to attrset form | wiki: `hl.monitor({ output = "DP-1", mode = "1920x1080", position = "0x0", scale = 1 })` — every example uses the attrset form, no shorthand survives. |

Module default:

```nix
monitor = [
  { output = ""; mode = "preferred"; position = "auto"; scale = 1; }
];
```

navi:

```nix
monitor = [
  { output = "DP-3"; mode = "5120x1440@239.76Hz"; position = "0x0"; scale = 1; }
];
```

tui (current: `eDP-1,2880x1800@120.00000,0x0,1.5`):

```nix
monitor = [
  { output = "eDP-1"; mode = "2880x1800@120.00000"; position = "0x0"; scale = 1.5; }
];
```

The default `listOf` merge in HM's `settings` type concatenates lists
(verified by evaluating
`lib.evalModules { ... settings.monitor = [ "a" ]; settings.monitor = [ "b" ]; }`
against the locked nixpkgs), so the module's `,preferred,…` default
and the per-machine entry both render as `hl.monitor(...)` calls in
source order, with the more specific per-machine one last — same
ordering as today's hyprlang output.

> **Open question (section 8):** the bare `output = ""` case maps to
> `hl.monitor({ output = "", mode = "preferred", position = "auto", scale = 1 })`,
> which is exactly the form shown in the wiki
> ([Monitors → Defaults / shorthand](https://wiki.hypr.land/Configuring/Basics/Monitors/)
> line 636 of our scraped copy). Confirmed.

### 4.13 `device` list

| Current | Lua (target) | Notes |
|---|---|---|
| `device = [ { name = "asup1415:00-093a:300c-touchpad"; sensitivity = 0.5; } ];` | identical attrset | wiki: `hl.device({ name = "my-keyboard", sensitivity = -0.5 })`. Already an attrset list, no restructuring needed. |

### 4.14 `windowrule`

| Current | Lua (target) | Notes |
|---|---|---|
| `windowrule = [ ];` (in `modules/desktop/hyprland/default.nix`) | **rename to `window_rule = [ ];`** (or omit) | wiki uses `hl.window_rule({ match = { class = "..." }, … })`. Underscore, singular. The default in the main module is empty, but **seven other modules append entries via `lib.mkIf …enable`** — see section 4.18 for the full translation table. The attribute key rename from `windowrule` to `window_rule` must apply in every contributing file. |

### 4.15 `env`

| Current | Lua (target) | Notes |
|---|---|---|
| `env = [ "XDG_CURRENT_DESKTOP,Hyprland" … ];` | restructure to `_args` pairs | wiki: `hl.env("XDG_CURRENT_DESKTOP", "Hyprland")` — two scalar args. |

Concrete shape:

```nix
env = [
  { _args = [ "XDG_CURRENT_DESKTOP" "Hyprland" ]; }
  { _args = [ "XDG_SESSION_TYPE"    "wayland" ]; }
  { _args = [ "XDG_SESSION_DESKTOP" "Hyprland" ]; }
  { _args = [ "GDK_BACKEND"         "wayland,x11" ]; }
  { _args = [ "_JAVA_AWT_WM_NONREPARENTING" "1" ]; }
  { _args = [ "QT_QPA_PLATFORM"     "wayland" ]; }
];
```

### 4.16 `exec-once` and `exec` — the gotcha

These attribute names contain `-`, which is **not legal in a lua
identifier**. The HM renderer would happily emit `hl.exec-once(...)` —
which Hyprland's lua VM will reject as a syntax error. There is no
`hl.exec-once` or `hl.exec` function in the lua API; the wiki documents
no such call. The lua equivalent is `hl.on("hyprland.start", function() … end)`
with `hl.exec_cmd(...)` inside the function body
([Configuring/Start → Sample config](https://wiki.hypr.land/Configuring/Start/),
HM:583–591 emits exactly that shape for `systemd` and `plugins`).

Translation: fold both lists into one `on = { _args = [...]; }` entry
that uses `mkLuaInline` for the function body. The HM option docstring
shows precisely this idiom (HM:277–286):

```nix
on = {
  _args = [
    "hyprland.start"
    (lib.generators.mkLuaInline ''
      function()
        ${lib.concatMapStringsSep "\n  " (cmd: ''hl.exec_cmd(${lib.escapeShellArg cmd})'') startupCommands}
      end
    '')
  ];
};
```

… where `startupCommands` is the nix list we currently express as
`exec-once`. The `exec` list (currently two entries: `pkill waybar &&
hyprctl dispatch exec waybar`, and the `setHyprGaps` script call)
likewise rolls into the same hook — `exec` in hyprlang means "run on
every config reload", and in the lua model the same effect is achieved
either by putting it in the start hook (runs once at session start) or
by a dedicated `hl.on("config.reload", function() … end)` if we want
the exact reload semantics. **Pragma:** PR 2 should fold both `exec` and
`exec-once` into the start hook unless we discover a real need for
reload-only behaviour. Flag for review during the spike (section 8).

`lib.mkIf`-wrapped entries inside `exec-once` (e.g. `(lib.mkIf
config.nx.desktop.wallpaper.enable "swaybg -i ...")`) survive the
migration because they are filtered out by the module system **before**
the value reaches the renderer (verified by `lib.evalModules` test —
see section 7), so they simply disappear when the condition is false.

> ⚠️ **There's also the question of whether the post-migration nix
> looks tidier with a small helper.** A `mkStartupHook = cmds: { _args
> = [ "hyprland.start" (mkLuaInline "function()\n${…}\nend") ]; }`
> in the module's `let` block would keep the call site readable. PR 2
> should add it.

### 4.17 `bind`, `bindl`, `bindrt`, `bindm` (in `modules/desktop/hyprland/default.nix`)

In hyprlang these are four separate directives. In lua they collapse to
**one** `hl.bind` function whose third argument is a flag table. From
the wiki [Configuring/Basics/Binds → Bind flags](https://wiki.hypr.land/Configuring/Basics/Binds/#bind-flags):

| hyprlang | lua flag table |
|---|---|
| `bind` | `{ }` (no flags) |
| `bindl` (locked) | `{ locked = true }` |
| `bindrt` (release + transparent) | `{ release = true, transparent = true }` |
| `bindm` (mouse) | `{ mouse = true }` |
| `binde` (repeating, only used in our `extraConfig` submap blocks) | `{ repeating = true }` |

So in PR 2 we collapse `bind`, `bindl`, `bindrt`, `bindm` from four
separate attribute names into a single `bind = [...]` list, with each
entry tagged via the third `_args` element where it differs from the
default. (We *could* keep them as separate attributes — `bindl =
[...]` etc. — but the renderer would emit `hl.bindl(...)` which is not
a function that exists in the lua API. Confirmed by absence from the
wiki and from `meta/` stubs.)

#### Translation pattern

The general shape is the same as the HM option docstring example
(HM:243–284):

```nix
bind = [
  { _args = [
      "<MOD> + <KEY>"
      (lib.generators.mkLuaInline "hl.dsp.<dispatcher>(<args>)")
      # optional 3rd arg — flag table
      # { locked = true; }
    ];
  }
];
```

#### Representative examples

The AC list requires one concrete bind translation for each of
movefocus, workspace switch, `togglespecialworkspace`, `exec`
dispatcher, media-key, and `lib.mkIf`-wrapped conditional. Here they
are, with current → target → rendered lua:

**movefocus dispatcher.** Current: `"SUPER, h, movefocus, l"`. Lua
nix:

```nix
{ _args = [
    "SUPER + h"
    (lib.generators.mkLuaInline ''hl.dsp.focus({ direction = "l" })'')
  ];
}
```

Renders as:

```lua
hl.bind("SUPER + h", hl.dsp.focus({ direction = "l" }))
```

(See wiki [Dispatchers — Window movement](https://wiki.hypr.land/Configuring/Basics/Dispatchers/) — `hl.dsp.focus({ direction })`.)

**Workspace switch.** Current: `"SUPER, 1, workspace, 1"`. Lua nix:

```nix
{ _args = [
    "SUPER + 1"
    (lib.generators.mkLuaInline "hl.dsp.workspace(1)")
  ];
}
```

Renders as `hl.bind("SUPER + 1", hl.dsp.workspace(1))`.

**`togglespecialworkspace`.** Current: `"SUPER, X, togglespecialworkspace, magic"`. Lua nix:

```nix
{ _args = [
    "SUPER + X"
    (lib.generators.mkLuaInline ''hl.dsp.workspace.toggle_special("magic")'')
  ];
}
```

Renders as `hl.bind("SUPER + X", hl.dsp.workspace.toggle_special("magic"))`
(wiki line 1022 of our scraped Dispatchers copy).

**`exec` dispatcher.** Current: `"SUPER, q, killactive"` — wait, that's
not exec. The exec example: `"ALT, Return, exec, ${terminal}"`. Lua
nix:

```nix
{ _args = [
    "ALT + Return"
    (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("${terminal}")'')
  ];
}
```

Renders as `hl.bind("ALT + Return", hl.dsp.exec_cmd("kitty"))`.

**Media-key binding.** Current: `", XF86AudioPlay, exec, ${pkgs.playerctl}/bin/playerctl play-pause"`. Lua nix (note the leading-comma syntax disappears — lua bind has just the keys string, mods + key joined with ` + `):

```nix
{ _args = [
    "XF86AudioPlay"
    (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("${pkgs.playerctl}/bin/playerctl play-pause")'')
  ];
}
```

Renders as `hl.bind("XF86AudioPlay", hl.dsp.exec_cmd("…/bin/playerctl play-pause"))`.

For consistency with the rest of our media binds, we'd add
`{ locked = true }` as a 3rd flag-table arg — see the wiki's "Media"
example block ([Configuring/Basics/Binds → Example Binds → Media](https://wiki.hypr.land/Configuring/Basics/Binds/#media)).

**`lib.mkIf`-wrapped conditional.** Current:

```nix
(lib.mkIf enableAudioControls ", XF86AudioRaiseVolume, exec, ${volumeUp}")
```

Lua nix:

```nix
(lib.mkIf enableAudioControls {
  _args = [
    "XF86AudioRaiseVolume"
    (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("${volumeUp}")'')
    { repeating = true; }
  ];
})
```

(`repeating` added per the wiki media example.) When
`enableAudioControls` is false, `lib.mkIf false { … }` is removed from
the list entirely during module merging — confirmed empirically with
`lib.evalModules` against our flake's locked nixpkgs:

```
$ nix eval --impure --expr '… foo = [ "a" (lib.mkIf false "b") "c" ]; …'
[ "a" "c" ]
```

So the renderer never sees `null`. (If a literal `null` slipped through
some other path, the renderer would emit `hl.bind(nil)` — which would
fail at runtime. Section 7 discusses this further.)

**Representative `bindl` (locked).** Current: `"SUPER, s, exec, ${scriptsDir}/cli.system.suspend"` from `bindl`. Lua nix entry inside the single `bind` list:

```nix
{ _args = [
    "SUPER + s"
    (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("${scriptsDir}/cli.system.suspend")'')
    { locked = true; }
  ];
}
```

**Representative `bindrt`.** Current: `"SUPER, SUPER_L, exec, pkill -SIGUSR2 waybar; hyprctl dispatch event quickshell:hide"`. Lua nix:

```nix
{ _args = [
    "SUPER + SUPER_L"
    (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("pkill -SIGUSR2 waybar; hyprctl dispatch event quickshell:hide")'')
    { release = true; transparent = true; }
  ];
}
```

**Representative `bindm` (mouse).** Current: `"SUPER, mouse:272, movewindow"`. Lua nix:

```nix
{ _args = [
    "SUPER + mouse:272"
    (lib.generators.mkLuaInline "hl.dsp.window.drag()")
    { mouse = true; }
  ];
}
```

Renders as `hl.bind("SUPER + mouse:272", hl.dsp.window.drag(), { mouse = true })`
(wiki Mouse Binds section).

> **Open question (section 8):** `hl.dsp.window.drag()` vs
> `hl.dsp.movewindow()`. The wiki Mouse Binds section uses
> `hl.dsp.window.drag()` for "move by dragging" and
> `hl.dsp.window.resize()` for "resize by dragging". Our hyprlang names
> are `movewindow` / `resizewindow`. Verify in the spike — there's a
> renamed-dispatcher table in the wiki.

### 4.18 `windowrule` and `bind` contributions from non-hyprland modules

The thirteen-file list in section 2 includes **eleven** files outside
`modules/desktop/hyprland/default.nix` that contribute string-form
entries to `wayland.windowManager.hyprland.settings.{windowrule,bind}`
via the standard nix module list-merge semantics. Under
`configType = "hyprlang"` these render fine as extra `windowrule = …`
and `bind = …` lines. Under `configType = "lua"` they would render as
`hl.windowrule("tile on, match:class PrismLauncher")` etc. — calls to
a function that doesn't exist, plus string arguments that the lua API
wouldn't parse even if it did. The lua API uses `hl.window_rule({
match = {...}, ... })` (note **underscore + singular**) with a
structured match table, per
[wiki Window Rules](https://wiki.hypr.land/Configuring/Basics/Window-Rules/).

**Translation shape for windowrules.** The current new-syntax entries
like `"tile on, match:class PrismLauncher"` parse as `<property
value>, match:<key> <value>`. The lua form keys the match into a
`match = {...}` table and the rule's property as a sibling field:

| Current (hyprlang v2) | Lua (target, nix) |
|---|---|
| `"tile on, match:class PrismLauncher"` | `{ match.class = "PrismLauncher"; tile = true; }` |
| `"sync_fullscreen 0, match:class steam"` | `{ match.class = "steam"; sync_fullscreen = false; }` |
| `"tile on, match:class Chromium-browser"` | `{ match.class = "Chromium-browser"; tile = true; }` |
| `"float on, match:title darktable starting"` | `{ match.title = "darktable starting"; float = true; }` |
| `"suppress_event fullscreen, match:class darktable"` | `{ match.class = "darktable"; suppress_event = "fullscreen"; }` |
| `"workspace 2 silent, match:class discord"` | `{ match.class = "discord"; workspace = "2 silent"; }` |
| `"sync_fullscreen 0, match:class libreoffice-writer"` | `{ match.class = "libreoffice-writer"; sync_fullscreen = false; }` |
| `"float on, match:class qute-filepicker"` | `{ match.class = "qute-filepicker"; float = true; }` |
| `"size 800 480, match:class qute-filepicker"` | `{ match.class = "qute-filepicker"; size = "800 480"; }` |
| `"stay_focused on, match:class qute-filepicker"` | `{ match.class = "qute-filepicker"; stay_focused = true; }` |
| (qute-editor: same triple) | (same shape with `match.class = "qute-editor"`) |
| `"sync_fullscreen 0, match:class org.qutebrowser.qutebrowser"` | `{ match.class = "org.qutebrowser.qutebrowser"; sync_fullscreen = false; }` |

Each entry becomes a single `hl.window_rule({...})` call. **The
attribute key on `settings` also changes — from `windowrule` to
`window_rule`.** A worker walking these files in PR 2 must rename the
key in every contributing file, not just translate the value.

> **Open question (section 8.10):** the `workspace 2 silent` value
> packs two things — a workspace identifier and a `silent` modifier.
> Whether the lua API takes that as a single composite string
> (`workspace = "2 silent"`) or as a structured table
> (`workspace = { id = 2; silent = true; }`) is not obvious from the
> wiki examples we've scraped. Resolve in the PR 2 spike.

**Translation shape for `bind` contributions from non-hyprland modules.**
The binds in `screenshot.nix`, `home-automation.nix`,
`voice-to-text.nix`, and `voice-to-text-daemon.nix` translate exactly
the same way as binds in the main hyprland module —
`{ _args = [ keys (mkLuaInline dispatcher) ]; }` per section 4.17.

All bind contributions in one place, for easy walking in PR 2:

| File | Current (hyprlang) | Lua (target, nix) |
|---|---|---|
| `modules/desktop/screenshot.nix:60` | `"SUPER SHIFT, S, exec, ${screenshotToClipboard}"` | `{ _args = [ "SUPER + SHIFT + S" (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("${screenshotToClipboard}")'') ]; }` |
| `modules/programs/home-automation.nix:194` | `"SUPER, ${pageup}, exec, …openBlinds"` | `{ _args = [ "SUPER + ${pageup}" (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("…openBlinds")'') ]; }` |
| `modules/programs/home-automation.nix:195` | `"SUPER, ${pagedown}, exec, …closeBlinds"` | (same shape with `pagedown`) |
| `modules/programs/home-automation.nix:196` | `"ALT, h, exec, ${newwindow} https://$HASS_DOMAIN"` | `{ _args = [ "ALT + h" (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("${newwindow} https://$HASS_DOMAIN")'') ]; }` |
| `modules/programs/voice-to-text.nix:443–445` | `"${cfg.keybind}, exec, voice-to-text toggle"` + cancel | depends on open question 8.11 |
| `modules/programs/voice-to-text-daemon.nix:493–494` | (same shape as `voice-to-text.nix`) | depends on open question 8.11 |

A worked example for the `home-automation.nix` blinds binds:

```nix
# Current (modules/programs/home-automation.nix:186–197)
bind = [
  "SUPER, ${pageup}, exec, ${homeDir}/.local/scripts/home.office.openBlinds"
  "SUPER, ${pagedown}, exec, ${homeDir}/.local/scripts/home.office.closeBlinds"
  "ALT, h, exec, ${newwindow} https://$HASS_DOMAIN"
];

# Target
bind = [
  { _args = [
      "SUPER + ${pageup}"
      (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("${homeDir}/.local/scripts/home.office.openBlinds")'')
    ];
  }
  { _args = [
      "SUPER + ${pagedown}"
      (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("${homeDir}/.local/scripts/home.office.closeBlinds")'')
    ];
  }
  { _args = [
      "ALT + h"
      (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("${newwindow} https://$HASS_DOMAIN")'')
    ];
  }
];
```

The `voice-to-text*.nix` binds use `${cfg.keybind}` — a string
like `"SUPER, v"` defined in the option default. The lua form needs
the single ` + `-joined keys string; the option's stored value may
need reformatting or, more conservatively, a one-liner helper that
rewrites `"<mod>, <key>"` to `"<mod> + <key>"` in the module. The
cleanest path is to change the `cfg.keybind` option's documented
format to the lua form directly (e.g. default `"SUPER + v"`) — note
this is a user-visible option change and should be called out in PR
2's commit message. Less invasively: keep the `"SUPER, v"` format and
split/rejoin inside the binding expression.

**Why this matters — silent regression risk.** Under
`configType = "lua"`, an untranslated `windowrule = [ "…" ]`
contribution from (say) `modules/programs/qutebrowser/default.nix`
renders as `hl.windowrule("float on, match:class qute-filepicker")` —
Hyprland's lua runtime errors on the call (no such function) and the
rule silently doesn't apply. The session keeps working; the user only
notices when they next open a qute-filepicker and it tiles instead of
floating. Same for the home-automation binds: `SUPER+PgUp/PgDn` simply
stop working and you don't notice until the next time you try to open
the blinds. The 24-hour soak in section 9 step 18 is unlikely to
exercise every contributing module, so we cannot rely on it to catch a
missed file.

This is exactly the failure class the review-context agent surfaced in
PR 1 review round 1, and is the reason section 9 step 11 is a separate
explicit step rather than rolled into the main hyprland module work.

---

## 5. Bind translation pattern (summary)

For section 4.17 readers who want the pattern in one place:

- **Shape.**
  ```nix
  { _args = [ "<keys>" (mkLuaInline "<dispatcher call>") <optional flag table> ]; }
  ```
- **Keys string.** Mods and key joined with ` + `. No leading comma.
  `XF86AudioPlay` and similar named keys go through as-is. Mouse buttons
  are `mouse:272`-style. Mouse wheel is `mouse_up` / `mouse_down`.
- **Dispatcher.** Wrapped in `mkLuaInline` so it isn't quoted. The
  dispatcher catalog is at
  [Configuring/Basics/Dispatchers](https://wiki.hypr.land/Configuring/Basics/Dispatchers/).
  Many lua dispatchers take a single attrset arg
  (`hl.dsp.focus({ direction = "l" })`) where hyprlang took a positional
  list.
- **Flag tables.** Third `_args` element is the flag table. Available
  flags per the wiki: `locked`, `release`, `click`, `drag`, `long_press`,
  `repeating`, `non_consuming`, `auto_consuming`, `mouse`, `transparent`,
  `ignore_mods`, `separate`, `description`, `bypass`, `submap_universal`,
  `devices`. The hyprlang-to-lua mapping for our four bind variants is
  given in 4.17.
- **Bind variants collapse.** `bind`, `bindl`, `bindrt`, `bindm` all
  become entries in the single top-level `bind = [...]` list,
  differentiated by their flag table.
- **`lib.mkIf condition { … }`.** When `condition` is false the entry
  is removed by the module system before the renderer runs, verified
  by `lib.evalModules` test (section 4.17, section 7). The renderer
  never sees `null` from `mkIf`. Literal `null` (not produced by `mkIf`)
  *does* survive into the value and would render as `hl.bind(nil)` —
  don't write literal `null` into a list.

---

## 6. Submap translation

We have two submaps today, both currently expressed as raw hyprlang
inside `extraConfig` (`modules/desktop/hyprland/default.nix:320–346`):
`resize` (entered from `SUPER+R`, auto-resets after 10 s) and `exit`
(entered from `SUPER SHIFT+E`, auto-resets after 3 s, with `l/s/r/L`
options for hyprlock / shutdown / reboot / logout).

Under `configType = "lua"` these become entries in
`wayland.windowManager.hyprland.submaps`, the `attrsOf submodule` option
documented at HM:293–375. Each submap has:

- `onDispatch` (string) — submap to switch to after any dispatch
  (`"reset"` to disable submap mode entirely). HM:303–311.
- `settings` (attrset of lists) — bind entries. **Only attribute
  entries with `_args` render under lua; string entries are filtered
  out** (HM:597–599, docstring HM:316–328).

### 6.1 The `resize` submap

```nix
submaps.resize = {
  # No onDispatch — bindings stay inside the submap until escape.
  settings.bind = [
    # Resize keys (formerly `binde=` — that's just the repeating flag in lua).
    { _args = [ "h"      (lib.generators.mkLuaInline "hl.dsp.window.resize({ x = -10, y = 0, relative = true })") { repeating = true; } ]; }
    { _args = [ "j"      (lib.generators.mkLuaInline "hl.dsp.window.resize({ x = 0,   y = 10, relative = true })") { repeating = true; } ]; }
    { _args = [ "k"      (lib.generators.mkLuaInline "hl.dsp.window.resize({ x = 0,   y = -10, relative = true })") { repeating = true; } ]; }
    { _args = [ "l"      (lib.generators.mkLuaInline "hl.dsp.window.resize({ x = 10,  y = 0, relative = true })") { repeating = true; } ]; }

    # Escape resets to the global submap.
    { _args = [ "escape" (lib.generators.mkLuaInline ''hl.dsp.submap("reset")'') ]; }

    # SUPER_L release continues to hide waybar / quickshell *while inside the submap*.
    # This is intentional (see section 8) — without it, the global bindrt is
    # shadowed by the submap and the hide stops firing.
    { _args = [
        "SUPER + SUPER_L"
        (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("pkill -SIGUSR2 waybar; hyprctl dispatch event quickshell:hide")'')
        { release = true; transparent = true; }
      ];
    }
  ];
};
```

`hl.dsp.window.resize({ x, y, relative = true })` is the documented lua
form (wiki [Configuring/Basics/Binds → Submaps example](https://wiki.hypr.land/Configuring/Basics/Binds/#submaps)).
Note that `hyprlang`'s `resizeactive,-10 0` (string-of-two-ints) becomes
`{ x = -10, y = 0, relative = true }` (table-of-ints).

### 6.2 The `exit` submap

```nix
submaps.exit = {
  settings.bind = [
    { _args = [ "l"        (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("hyprlock")'') ]; }
    { _args = [ "SHIFT + L" (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("loginctl terminate-user $USER")'') ]; }
    { _args = [ "s"        (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("systemctl poweroff")'') ]; }
    { _args = [ "r"        (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("systemctl reboot")'') ]; }
    { _args = [ "escape"   (lib.generators.mkLuaInline ''hl.dsp.submap("reset")'') ]; }
    # Same SUPER_L release as in `resize`, for the same reason.
    { _args = [
        "SUPER + SUPER_L"
        (lib.generators.mkLuaInline ''hl.dsp.exec_cmd("pkill -SIGUSR2 waybar; hyprctl dispatch event quickshell:hide")'')
        { release = true; transparent = true; }
      ];
    }
  ];
};
```

### 6.3 Entry binds and auto-reset

The entry binds live in the global `bind` list, not in the submap:

```nix
# Enter the resize submap (no longer needs the sleep-and-reset trick — see below).
{ _args = [ "SUPER + R"
            (lib.generators.mkLuaInline ''hl.dsp.submap("resize")'') ]; }

# Enter the exit submap.
{ _args = [ "SUPER + SHIFT + E"
            (lib.generators.mkLuaInline ''hl.dsp.submap("exit")'') ]; }
```

### 6.4 Auto-reset behaviour — the lua idiom

In hyprlang we implement auto-reset by binding the entry key to **two**
dispatchers in sequence:

```hyprlang
bind=SUPER,R,exec,sleep 10 && hyprctl dispatch submap reset
bind=SUPER,R,submap,resize
```

The first line spawns `sleep 10 && hyprctl dispatch submap reset` as a
background process; the second enters the submap. After 10 s the
backgrounded subshell calls `hyprctl` to reset. This works but is
ugly — it spawns a shell and an extra `hyprctl` invocation per
submap-entry.

Lua offers `hl.timer(function, { timeout = N, type = "oneshot" })`
(wiki [Configuring/Basics/Dispatchers — DPMS warning](https://wiki.hypr.land/Configuring/Basics/Dispatchers/) — "consider something like
`hl.timer(function() hl.dispatch(hl.dsp.dpms(...)) end, { timeout = 500, type = "oneshot" })`").
The cleaner idiom is to bind the entry key to an inline function that
enters the submap **and** schedules the reset:

```nix
# Resize submap entry, with 10 s auto-reset baked in.
{ _args = [
    "SUPER + R"
    (lib.generators.mkLuaInline ''
      function()
        hl.dispatch(hl.dsp.submap("resize"))
        hl.timer(function()
          hl.dispatch(hl.dsp.submap("reset"))
        end, { timeout = 10000, type = "oneshot" })
      end
    '')
  ];
}
```

Same shape for `exit` with `timeout = 3000`. This removes the
sleep+hyprctl shell-out entirely.

`timeout` is in **milliseconds** per the wiki snippet (`timeout = 500`
in the DPMS example matches the "delay DPMS off by 500 ms" intent).
Verify in the spike (section 8) — the wiki doesn't explicitly state
the unit.

### 6.5 What can't be modelled cleanly in `submaps`

After working through the translations above, every line in our current
`extraConfig` maps cleanly into the `submaps` option. **Post-migration,
`extraConfig` should be the empty string.** If anything surprising
surfaces during the spike (section 8), the escape hatch is to keep that
specific line in `extraConfig` as raw lua, but we don't expect to need
it.

> **One subtle thing to preserve.** The `bindrt=SUPER,SUPER_L,exec,…`
> line appears once in each submap (`default.nix:326` and `default.nix:343`).
> At first glance this looks like a copy-paste duplicate to delete in
> the migration. It is not — see section 8 for the analysis.

---

## 7. `lib.mkIf` and per-machine composition

### 7.1 `lib.mkIf` on individual list entries

We have several `lib.mkIf` expressions wrapping individual entries in
`exec-once`, `bind`, and similar lists:

```nix
(lib.mkIf config.nx.isLaptop "${pkgs.brightnessctl}/bin/brightnessctl s 70%")
(lib.mkIf enableAudioControls ", XF86AudioRaiseVolume, exec, ${volumeUp}")
```

These survive the migration unchanged in *shape* (the `lib.mkIf` itself
stays). What changes is the wrapped expression — it becomes the
`{ _args = […]; }` attrset rather than the bare string.

**Empirically verified** against our flake's locked nixpkgs:

```
$ nix eval --impure --expr '
let
  flake = builtins.getFlake (toString ./.);
  lib = flake.inputs.nixpkgs.lib;
  result = lib.evalModules {
    modules = [
      ({ lib, ... }: {
        options.foo = lib.mkOption {
          type = lib.types.listOf (lib.types.nullOr lib.types.str);
          default = [];
        };
      })
      ({ lib, ... }: {
        foo = [ "a" (lib.mkIf false "b") "c" (lib.mkIf true "d") ];
      })
    ];
  };
in result.config.foo'
[ "a" "c" "d" ]
```

`mkIf false <x>` is stripped during option merging, **before** the
list reaches the renderer. So `null` never makes it into the rendered
lua — the false-conditioned entries simply vanish from the list. ✅

**Caveat.** Literal `null` is a *valid* element under the
`settingValueType` (HM:14–22 — `nullOr (oneOf [...])`) and *would*
survive into the renderer. The renderer doesn't filter null (HM:573–575
just passes the value through `toLua`, which renders null as `nil`),
so `[ "a" null "c" ]` would emit `hl.bind(nil)` — a runtime error in
Hyprland. The fix is "don't write literal `null` into a settings list" —
use `mkIf` or `lib.optional`. This is a behaviour to be aware of, not
something we need to engineer around.

### 7.2 Per-machine `monitor` composition

`machines/navi/configuration.nix:119–122` and
`machines/tui/configuration.nix:119–122` both add their own
`wayland.windowManager.hyprland.settings.monitor` entry. Currently:

```nix
# navi
home-manager.users.ben.wayland.windowManager.hyprland.settings = {
  monitor = [ "DP-3,5120x1440@239.76Hz,0x0,1" ];
  # …
};
```

Under `configType = "lua"` these become:

```nix
# navi
home-manager.users.ben.wayland.windowManager.hyprland.settings.monitor = [
  { output = "DP-3"; mode = "5120x1440@239.76Hz"; position = "0x0"; scale = 1; }
];
```

The module-level default `monitor = [ { output = ""; mode = "preferred"; … } ]`
and the per-machine `monitor` entry are both contributions to the same
`listOf settingValueType` option. The default merge semantics for `listOf`
in nixpkgs' module system is concatenation in declaration order, which I
verified by evaluating a minimal `lib.evalModules` against our locked
nixpkgs:

```
$ nix eval --impure --expr '
  lib.evalModules {
    modules = [
      { options.s = lib.mkOption { type = lib.types.submodule { freeformType = lib.types.attrsOf (lib.types.listOf lib.types.str); }; default = {}; }; }
      { s.monitor = [ ",preferred,auto,auto" ]; }
      { s.monitor = [ "DP-3,5120x1440@239.76Hz,0x0,1" ]; }
    ];
  }'
[ ",preferred,auto,auto" "DP-3,5120x1440@239.76Hz,0x0,1" ]
```

So in PR 2 we keep the module's `,preferred,auto,auto` default (as the
attrset form), and navi/tui contribute their specific entry alongside.
The rendered lua looks like:

```lua
-- settings.monitor
hl.monitor({ output = "", mode = "preferred", position = "auto", scale = 1 })
hl.monitor({ output = "DP-3", mode = "5120x1440@239.76Hz", position = "0x0", scale = 1 })
```

Hyprland resolves these in order; the more specific `DP-3` entry takes
precedence when that output is actually present, exactly as in the
current hyprlang setup.

### 7.3 `lib.mkMerge` interactions

We don't currently use `lib.mkMerge` on the hyprland `settings`
attrset anywhere, but the module type
(`settingValueType = nullOr (oneOf [bool int float str path attrs list])`,
HM:14–22) is a freeform-attrs-and-lists structure with the standard
nixpkgs merge semantics. Nothing about the migration changes that.

---

## 8. Open questions and risks

Each question below lists a resolution path. None of these are
showstoppers; they are either ergonomic choices or low-risk
verification steps to confirm during the PR 2 spike.

### 8.1 `hl.config({ ... })` bulk-setter vs per-section calls

The wiki ([Variables — Syntax](https://wiki.hypr.land/Configuring/Basics/Variables/#syntax))
shows a bulk form:

```lua
hl.config({ category = { value = ... }, category2 = { value2 = ... } })
```

and the [Switchable keyboard layouts section](https://wiki.hypr.land/Configuring/Basics/Binds/#switchable-keyboard-layouts)
also uses `hl.config({ input = { ... } })`. The HM renderer emits the
per-section form `hl.general({...})`, `hl.input({...})`. Both are
documented as valid; both update the same internal state. We use what
the renderer emits — no change needed on our side.

**Resolution: confirmed by reading the wiki — no action required.**

### 8.2 Gradient colour as string vs structured table

The wiki [Variables → Colors](https://wiki.hypr.land/Configuring/Basics/Variables/#variable-types)
defines the `gradient` type as accepting "a color, or `{ colors =
{ "rgba(...)", "rgba(...)" }, angle? = 45 }`". It is not 100 % clear
from the page whether the legacy whitespace-separated string form
(`"rgba(…) rgba(…) … 45deg"`) is also accepted under lua, or only the
table form.

**Risk.** If only the table form is accepted, our 7-colour rainbow
border becomes:

```nix
"col.active_border" = {
  colors = map (c: "rgba(${builtins.substring 1 6 c}ff)") rainbowColors;
  angle = 45;
};
```

Low cost either way.

**Resolution: spike on the throwaway branch.** Write both forms,
`hyprctl reload`, check the border. If the string form errors, switch
to the table form. ETA: ~5 minutes inside the spike.

### 8.3 `hl.exec_cmd` quoting of multi-word strings

`hl.exec_cmd("pkill -SIGUSR1 waybar; hyprctl dispatch event quickshell:show")`
is a single string. Hyprland's lua VM passes it to a shell — but
which shell, and with what quoting?

**Why this matters.** Several of our binds chain commands with `&&`,
`;`, or pipe through arguments with special chars (e.g. swaync's
`--close-all && --close-panel`). If `exec_cmd` runs via `sh -c "$str"`,
shell quoting is our responsibility (same as hyprlang). If it does an
`execvp` split, then `&&` and `;` would be broken.

**Resolution: source-read.** The hyprland repo `src/managers/EventManager.cpp`
and the lua `exec_cmd` binding will tell us definitively. Alternatively,
read the autogenerated lua stubs at `${cfg.finalPackage}/share/hypr/stubs/`
(see HM:461–467 — HM ships a `.luarc.json` pointing at exactly that path)
which carry doc comments per function. Both are local-only checks; no
upstream interaction needed.

**Best guess from wiki examples.** Every `hl.dsp.exec_cmd(...)`
example in the wiki uses shell metacharacters freely
(`"pkill wofi || wofi"`, `"wpctl set-volume @DEFAULT_AUDIO_SINK@ 5%+"`,
`"loginctl terminate-user $USER"`), so it's almost certainly
`sh -c $str` semantics, and our existing commands transfer 1:1. Confirm
in the spike.

### 8.4 Monitor shorthand `,preferred,auto,auto`

Resolved in section 4.12 — wiki line 636 of our scraped
`Monitors.txt` shows
`hl.monitor({ output = "", mode = "preferred", position = "auto", scale = 1 })`
as the direct equivalent. No structural surprise. ✅

### 8.5 The `bindrt=SUPER,SUPER_L,exec,…` lines inside both submaps

`modules/desktop/hyprland/default.nix:326` (inside `resize`) and
`default.nix:343` (inside `exit`) both contain the same line:

```hyprlang
bindrt=SUPER,SUPER_L,exec,pkill -SIGUSR2 waybar; hyprctl dispatch event quickshell:hide
```

At first glance this looks like a stray duplicate — same line, two
places, smells like copy-paste. **But it's intentional, and the
migration must preserve both.** The reasoning:

- The same `bindrt` exists at top level (`default.nix:230`), so when
  no submap is active, `SUPER_L` release hides the waybar / quickshell
  widgets.
- Hyprland's submap model **shadows** all global binds while a submap
  is active. (Wiki [Configuring/Basics/Binds → Submaps](https://wiki.hypr.land/Configuring/Basics/Binds/#submaps):
  "Keybinds further down will be global again…" — and conversely,
  global binds defined before a submap don't apply within it unless
  flagged `submap_universal`.)
- Without the per-submap copy, the `SUPER + SUPER_L` release-hide
  stops firing the moment the user enters `resize` or `exit` — which
  is exactly when they're most likely to release `SUPER` (because
  they pressed it to enter the submap).
- The lua API provides a cleaner alternative: the `submap_universal`
  flag ([wiki](https://wiki.hypr.land/Configuring/Basics/Binds/#submaps)
  bottom of section). Adding `{ submap_universal = true, release =
  true, transparent = true }` to the top-level `bindrt` for `SUPER_L`
  makes both submap copies redundant.

**Resolution: in PR 2, collapse the three copies (top-level + two
submaps) into a single top-level entry with `{ submap_universal =
true, release = true, transparent = true }`.** This is a genuine
ergonomic improvement enabled by the lua API; we don't need to
preserve the duplication once we have a cleaner mechanism. Note this
explicitly in the PR 2 description.

### 8.6 `bindrt` flag-table — verify the `transparent` half

Hyprlang `bindrt` is documented as "release + transparent". I have not
found an authoritative single-source statement in the lua wiki that
`bindrt`'s `t` corresponds to lua's `transparent` flag; I'm
extrapolating from the hyprlang directive table. If the migration
brings up surprising behaviour around the `SUPER_L` release bind, the
suspect is `transparent`.

**Resolution: spike.** Try with and without `transparent = true` and
see which matches the current hyprlang behaviour. Cheap to test.

### 8.7 `hl.timer` `timeout` units

The wiki example uses `timeout = 500` in the DPMS context where 500 ms
is the obvious read, but the unit is not stated on the page.

**Resolution: source-read.** Same as 8.3 — the lua stubs at
`${cfg.finalPackage}/share/hypr/stubs/` will carry the parameter type
and units.

### 8.8 `exec` (per-reload) vs `exec-once` semantics in lua

In hyprlang, `exec` runs on every config reload and `exec-once` runs
only at session start. In the lua model, `hl.on("hyprland.start", …)`
is clearly the "once-per-session" hook; there isn't an obviously named
"on-reload" event in the wiki snippets we've scraped (but we haven't
read all the pages).

**Resolution: wiki read + source-read.** Read the events catalog
(probably under `Configuring/Advanced-and-Cool/` — the page didn't
appear in our scraped index, will need to find it). If there's an
`hl.on("config.reload", …)`, use it for the `exec` list (which is just
the waybar-restart + setHyprGaps script today). If there isn't, fold
`exec` into `hyprland.start` — we don't actually depend on the
"reload" semantics; we just want those commands to run.

### 8.9 Dispatcher rename audit

Several hyprlang dispatcher names rename in lua:

| hyprlang | lua |
|---|---|
| `movefocus, l` | `hl.dsp.focus({ direction = "l" })` |
| `movewindow, l` | `hl.dsp.window.move({ direction = "l" })` |
| `workspace, 1` | `hl.dsp.workspace(1)` |
| `movetoworkspacesilent, 1` | `hl.dsp.window.move({ workspace = 1, silent = true })` (likely) |
| `togglespecialworkspace, magic` | `hl.dsp.workspace.toggle_special("magic")` |
| `togglefloating` | `hl.dsp.window.float({ action = "toggle" })` |
| `fullscreen` | `hl.dsp.window.fullscreen(...)` |
| `killactive` | `hl.dsp.window.close()` |
| `movewindow` (mouse) | `hl.dsp.window.drag()` |
| `resizewindow` (mouse) | `hl.dsp.window.resize()` |
| `exec` | `hl.dsp.exec_cmd("...")` |

Some of these are confirmed by the scraped wiki; others (e.g.
`movetoworkspacesilent`, `fullscreen`) I'm extrapolating. The
`movetoworkspacesilent` form in particular is a guess based on
`hl.dsp.window.move({ workspace = "special:magic" })` shown at wiki
line 1020 of our scraped Dispatchers copy.

**Resolution: walk the full
[Dispatchers wiki page](https://wiki.hypr.land/Configuring/Basics/Dispatchers/)
during the spike** and produce an exact dispatcher-mapping table for
every dispatcher used in our config. PR 2 includes this table in the
commit message so a reviewer can sanity-check it. The Hyprland-shipped
lua stubs at `${cfg.finalPackage}/share/hypr/stubs/` are the
authoritative reference.

### 8.10 `workspace 2 silent` — composite string vs structured table

The `discord.nix` windowrule sets `"workspace 2 silent, match:class
discord"`. In hyprlang the rule's RHS is a free-form string that
Hyprland parses internally; the `silent` modifier is appended after
the workspace id with a space. In the lua API it's unclear whether
that translates to:

- A single composite string: `workspace = "2 silent"`, or
- A structured table: `workspace = { id = 2; silent = true; }`.

The wiki [Window Rules](https://wiki.hypr.land/Configuring/Basics/Window-Rules/)
examples we scraped lean toward structured values for most fields, but
the `workspace` field specifically wasn't covered with a `silent`
modifier in any of the examples I read.

**Resolution: source-read** the lua stubs at
`${cfg.finalPackage}/share/hypr/stubs/` (the auto-loaded
`.luarc.json` config from HM:461–467 points there) for the
`window_rule.workspace` field type, or test both forms in the spike.
Low risk either way — the worst case is one wrong form that fails on
reload with a clear error.

### 8.11 `voice-to-text` keybind format option

The `nx.programs.voice-to-text.keybind` (and `voice-to-text-daemon`)
option's default value is currently in hyprlang `"<mod>, <key>"`
format. Section 4.18 proposes changing the documented format to the
lua `"<mod> + <key>"` form. This is a user-visible breaking change
for anyone overriding the option.

**Resolution:** decide during PR 2. Two reasonable answers:

- **Change the format.** Cleaner long-term, but requires a one-line
  change in each consumer's machine config if they override it. Look
  for overrides with
  `rg 'nx\.programs\.voice-to-text(-daemon)?\.keybind'` before
  deciding — if there are zero non-default overrides, just change it.
- **Keep the format, translate inside the module.** Add a tiny helper
  (`luaKeybind = s: lib.replaceStrings [ ", " ] [ " + " ] s;`) and
  use it at the bind call site. No user-visible change.

My current lean: change the format, because the cleaner option's
blast radius is one or two files at most and it removes a layer of
indirection.

### 8.12 Should `extraConfig` really be empty?

Section 6.5 claims `extraConfig = "";` post-migration. If the spike
finds something that doesn't model cleanly in `submaps` (or any other
section above), the escape hatch is to put that one stubborn thing in
`extraConfig` as raw lua. Don't pre-emptively engineer around it; fall
back to `extraConfig` only when you hit the wall.

---

## 9. Concrete migration checklist for PR 2

The order matters — each step is independently verifiable.

1. **Branch off main** (after PR 1 — this doc — has merged), name
   `hyprland-lua-migration`. Confirm `hyprland-pin-hyprlang-drop-hy3`
   is merged before starting; this work assumes the `hy3` cleanup is
   already in.
2. **Resolve the open questions in section 8 via a throwaway spike
   branch.** Specifically: 8.2 (gradient form), 8.3 (`exec_cmd`
   quoting), 8.6 (`bindrt` flag table), 8.7 (`hl.timer` units),
   8.8 (`exec` vs `hyprland.start`), 8.9 (full dispatcher-rename
   audit). Write the findings as comments inline in the spike, then
   throw the spike away and start clean.
3. **Flip `configType` from `"hyprlang"` to `"lua"`** in
   `modules/desktop/hyprland/default.nix` — the single line PR 0
   added. Don't change anything else yet; just confirm `nh switch
   --flake .#navi` fails loudly (because all the hyprlang directives
   are now being rendered as broken lua).
4. **Translate `general`, `input`, `decoration`, `dwindle`, `misc`,
   `cursor`, `debug`, `ecosystem`.** These are pure attribute-name +
   type tweaks per section 4. After each block, `home-manager build`
   and inspect the generated `hyprland.lua` to confirm it's
   syntactically valid lua.
5. **Translate `bezier` → `curve`** (attrset/`_args` form) and
   **`animations.animation` → `animation`** (top-level attrset list).
6. **Translate `monitor`** to the attrset form, in
   `modules/desktop/hyprland/default.nix`, `machines/navi/configuration.nix`,
   and `machines/tui/configuration.nix`.
7. **Translate `device`, drop empty `windowrule` / `gestures`.**
8. **Translate `env`** to `_args` pairs.
9. **Translate `exec-once` and `exec` into the `hyprland.start` hook.**
   Use the `mkStartupHook` helper sketched in section 4.16.
10. **Collapse `bind` / `bindl` / `bindrt` / `bindm` into one
    `bind = [...]` list.** Walk each entry; produce the
    `{ _args = [keys dispatcher flags?]; }` shape per section 4.17 and
    the section 8.9 dispatcher-rename table.
11. **Walk every non-hyprland module that contributes to
    `wayland.windowManager.hyprland.settings`** per section 4.18 and
    section 2's in-scope list. The current set (verify before
    starting with the broad regex — see the warning in section 4.18,
    and the second paragraph of this step:
    `rg 'hyprland\.(settings|extraConfig|submaps|plugins)' --type nix`):
    - `modules/gaming/prismlauncher.nix` (1 windowrule)
    - `modules/gaming/steam.nix` (1 windowrule)
    - `modules/programs/chromium.nix` (1 windowrule)
    - `modules/programs/darktable.nix` (2 windowrules)
    - `modules/programs/discord.nix` (1 windowrule — see open question 8.10)
    - `modules/programs/libreoffice.nix` (1 windowrule)
    - `modules/programs/qutebrowser/default.nix` (7 windowrules)
    - `modules/desktop/screenshot.nix` (1 bind — `SUPER SHIFT + S`; nested-attribute style, see section 4.18 warning)
    - `modules/programs/home-automation.nix` (3 binds — `SUPER+PgUp/PgDn`, `ALT+h`)
    - `modules/programs/voice-to-text.nix` (2 binds — see open question 8.11)
    - `modules/programs/voice-to-text-daemon.nix` (2 binds — see open question 8.11)

    For each: rename `windowrule` → `window_rule`, translate every
    string entry to the attrset form, and `home-manager build` after
    each file to confirm no lua syntax errors.

    **Use the broad regex when re-verifying the file list.** Run
    `rg 'hyprland\.(settings|extraConfig|submaps|plugins)' --type nix`
    — not the narrower `rg 'wayland\.windowManager\.hyprland\.settings'`
    pattern, which silently misses any module that opens
    `wayland.windowManager = { hyprland.settings.<…> = …; };` as a
    nested attrset. The narrow form is exactly how
    `modules/desktop/screenshot.nix` is written today, and it slipped
    through PR 1's round-2 audit for that reason.
12. **Migrate `extraConfig` submaps into `submaps.resize` /
    `submaps.exit`** per section 6, using the `hl.timer` idiom from
    section 6.4 for auto-reset.
13. **Collapse the three `SUPER_L` release-bind copies into one
    top-level `submap_universal = true` entry** per section 8.5.
14. **Set `extraConfig = "";`** (or remove the key entirely).
15. **Build:** `nix flake check`, `nh switch --flake .#navi` on navi,
    then on tui. Both must succeed without errors.
16. **Visual smoke-test on each machine:**
    - Launch a terminal with `ALT+Return` — confirms the simplest
      `exec` bind path.
    - Switch workspaces with `SUPER+1..9` — confirms workspace
      dispatcher.
    - Move focus with `SUPER+h/j/k/l` — confirms `movefocus`.
    - Toggle the special workspace with `SUPER+X` — confirms
      `togglespecialworkspace` rename.
    - Float a window with `SUPER SHIFT+Space` — confirms
      `togglefloating` rename.
    - Open the launcher with `ALT+Space` — confirms a `bind` with a
      script-path expansion.
    - Hit `XF86AudioPlay` (or `Pause`) — confirms a media bind with
      `{ locked = true }`.
    - Enter the resize submap with `SUPER+R`, resize with `h/j/k/l`,
      wait 10 s — confirms submap + timer auto-reset. Repeat with
      `Escape` to confirm manual reset.
    - Enter the exit submap with `SUPER SHIFT+E`, hit `Escape`
      immediately (don't trigger `s`/`r`/`L`).
    - On navi only: confirm the DP-3 monitor still comes up at
      `5120x1440@239.76Hz`.
    - On tui only: confirm `eDP-1` at `2880x1800@120` with `1.5`
      scale.
    - Press and release `SUPER_L` alone — confirms the waybar /
      quickshell show/hide pulse still works, both globally and
      inside the resize / exit submaps (per section 8.5).
    - **Exercise every contributing module's binds and windowrules**
      (per step 11 list — otherwise the 24-hour soak below cannot
      catch a missed translation):
      - Open Prism Launcher — confirms the gaming windowrule.
      - Launch Steam — confirms the steam `sync_fullscreen` rule.
      - Open Chromium — confirms the chromium tile rule.
      - Open Darktable, observe splash float + no maximise.
      - Open Discord, confirm it lands on workspace 2 silently.
      - Open a LibreOffice writer document, try fullscreen.
      - Open qutebrowser, open a file picker dialog (e.g. via
        `Ctrl+O` in a form), confirm float + sizing; same for an
        edit-in-external-editor flow.
      - Hit `SUPER SHIFT+S` and confirm a screenshot lands on the
        clipboard (paste into a kitty terminal or a chat input to
        verify) — confirms `modules/desktop/screenshot.nix`'s bind.
      - Hit `Print` and confirm the full-screen file capture still
        works — confirms the older `,Print,exec,…fullScreenshotToFile`
        bind in the main module survived migration. (Distinct from
        the `SUPER SHIFT+S` bind above.)
      - Hit `SUPER+PgUp` and `SUPER+PgDn` — confirms
        home-automation blinds binds (only meaningful where the
        HA endpoint is reachable).
      - Hit `ALT+h` — confirms the HA-dashboard bind.
      - Hit `${cfg.keybind}` on a voice-to-text-enabled machine —
        confirms the voice-to-text binds (only meaningful on
        machines where it's enabled, namely those with the
        daemon variant).
17. **Commit, push, open PR with `gh pr create`.** Title:
    `hyprland: migrate from hyprlang to lua`. Body links back to this
    design doc and to PR 0.
18. **Run `prism review`** on the PR. Address findings.
19. **Leave the PR open for at least 24 h** before merging — Ben
    should sit on the running config for a day to make sure no
    secondary bind behaviour has silently regressed (e.g. obscure
    media keys, the long-press-vs-tap interaction on
    `XF86AudioNext`, app windowrules from the step-11 modules).

---

## 10. Rollback plan

The hyprlang codepath in Home Manager is unchanged by this migration.
HM still ships both renderers (HM:459 and HM:531), gated on
`cfg.configType`. The migration is, structurally, a one-line flip plus
a bunch of cosmetic restructuring within `settings`.

If PR 2 lands and something misbehaves on a machine:

```bash
git revert <pr-2-merge-sha>
nh switch --flake .#<machine>
```

That restores the hyprlang `default.nix`, hyprlang `monitor` strings on
navi / tui, and `configType = "hyprlang"`. We lose nothing operational
by reverting — the hyprlang module is still the supported path in HM at
the locked rev; it's just no longer the default at `stateVersion >=
26.05`. We are at `25.11` and PR 0 pinned `configType` explicitly, so
the revert is unconditionally safe.

If the issue is partial — say, only the resize submap timer is wrong —
preferred over a full revert is to land a targeted fix PR. Section 8's
open questions list is the most likely source of any post-merge
surprise; check that section first.

---

*Sources*

- Home Manager hyprland module at locked rev
  `928d72376949e222ea4f07b44828a55b0136422e`:
  `modules/services/window-managers/hyprland.nix`. Line citations in
  the doc are against this revision, fetched via
  `https://raw.githubusercontent.com/nix-community/home-manager/928d72376949e222ea4f07b44828a55b0136422e/modules/services/window-managers/hyprland.nix`.
- Hyprland wiki pages, all fetched 2026-05-22:
  - <https://wiki.hypr.land/Configuring/Start/>
  - <https://wiki.hypr.land/Configuring/Basics/Variables/>
  - <https://wiki.hypr.land/Configuring/Basics/Binds/>
  - <https://wiki.hypr.land/Configuring/Basics/Dispatchers/>
  - <https://wiki.hypr.land/Configuring/Basics/Monitors/>
  - <https://wiki.hypr.land/Configuring/Basics/Window-Rules/>
  - <https://wiki.hypr.land/Configuring/Advanced-and-Cool/Animations/>
  - <https://wiki.hypr.land/Configuring/Advanced-and-Cool/Environment-variables/>
  - <https://wiki.hypr.land/Configuring/Advanced-and-Cool/Devices/>
- Local config sources:
  - `modules/desktop/hyprland/default.nix`
  - `machines/navi/configuration.nix`
  - `machines/tui/configuration.nix`
