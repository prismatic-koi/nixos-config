# themev2 palette data — the single source of truth for both the NixOS module
# option (see ../schema.nix) and the truecolor swatch preview (see
# ./preview.nix). Pure data: takes colourLib, returns the three sample schemes
# plus the display grouping the preview walks.
#
# Structure:
#   - a NEUTRALS band with semantic names and no baseX codes
#     (background_darkest, background_dark, background, surface, overlay,
#     muted, foreground_dim, foreground);
#   - a BRIGHT band kept for ANSI correctness (kitty color9–color14 map to
#     bright_red/yellow/green/cyan/blue/magenta), plus bright_orange and
#     bright_brown;
#   - a TAILWIND-INSPIRED hue band that carries all the chromatic colour. It
#     follows Tailwind's 17 chromatic hue names (red, orange, amber, yellow,
#     lime, green, emerald, teal, cyan, sky, blue, indigo, violet, purple,
#     fuchsia, pink, rose) with two documented adaptations:
#       * `brown` is ADDED — Tailwind omits it, but base24/ANSI and terminals
#         need it.
#       * `maroon` is NOT a slot — it is reachable by luminance (darken red).
#     This band REPLACES the old base24 accent band (base08–base0F) and the
#     bespoke Catppuccin-named band; `magenta` maps onto Tailwind `fuchsia`.
#   - computed tinted backgrounds and a universal role core.
#
# Provenance model (three categories, one per slot):
#   upstream — literal hex present in the scheme's authoritative source. The
#              slot may now carry a Tailwind name that differs from the source
#              palette's own name for that hex; provenance tracks the hex, not
#              the name.
#   derived  — value produced by a Nix expression (a colourLib call, or an
#              alias). Self-documenting; carries a `method`. colourLib shifts
#              luminance only, so a derived hue is the nearest native colour
#              lightened or darkened.
#   adjusted — literal hex that does NOT match the source. Carries a `source`
#              note AND an inline comment.
#
# The divergence register (./register.md) is the authoritative provenance
# record; it lists every derived and adjusted slot.
{ colourLib }:
let
  inherit (colourLib) darken lighten;

  # Slot constructors — keep provenance and value together.
  up = source: value: {
    inherit value source;
    provenance = "upstream";
    method = "";
  };
  der = method: value: {
    inherit value method;
    provenance = "derived";
    source = "";
  };
  adj = source: value: {
    inherit value source;
    provenance = "adjusted";
    method = "";
  };

  # bright variant: lighten on dark themes, darken on light themes.
  # Mirrors the light-vs-dark conditional in modules/programs/prism/pi.nix.
  brightMethod =
    type: base: pct:
    "${if type == "light" then "darken" else "lighten"} ${base} ${toString pct}";
  brighten =
    type: c: pct:
    if type == "light" then darken c pct else lighten c pct;

  # tinted background: pull an accent toward the base background. Do NOT
  # darken on light themes (pi.nix pattern) — lighten toward white instead.
  tintMethod =
    type: base: pct:
    "${if type == "light" then "lighten" else "darken"} ${base} ${toString pct}";
  tint =
    type: c: pct:
    if type == "light" then lighten c pct else darken c pct;

  # ---- edge (dark, sainnhe/edge, style=default) ----
  edge =
    let
      t = "dark";
      src = "sainnhe/edge (dark, default)";
      fg = "#c5cdd9";
      # native edge accents (upstream literals)
      nred = "#ec7279";
      nyellow = "#deb974";
      ngreen = "#a0c980";
      ncyan = "#5dbbc1";
      nblue = "#6cb6eb";
      npurple = "#d38aea";
      # edge upstream has NO orange — carried from the v1 theme (adjusted).
      norange = "#e59676";

      # Tailwind-hue values. Upstream where a native colour lands nearest the
      # Tailwind hue; otherwise derived from the nearest native by luminance.
      red = nred;
      orange = norange;
      yellow = nyellow;
      green = ngreen;
      teal = ncyan; # edge cyan is a soft teal-cyan → Tailwind teal
      cyan = lighten teal 10;
      emerald = darken teal 10;
      lime = lighten green 15;
      amber = darken yellow 8;
      sky = nblue; # edge blue is a sky blue → Tailwind sky
      blue = darken sky 12;
      indigo = darken blue 15;
      violet = npurple; # edge purple → Tailwind violet
      purple = lighten violet 6;
      fuchsia = lighten violet 12;
      pink = lighten violet 18;
      rose = lighten red 6;
      brown = darken orange 25;
    in
    {
      name = "edge";
      type = t;
      neutrals = {
        background_darkest = up src "#202023"; # black
        background_dark = up src "#24262a"; # bg_dim
        background = up src "#2c2e34"; # bg0
        surface = up src "#33353f"; # bg1
        overlay = up src "#3b3e48"; # bg3
        muted = up src "#535c6a"; # grey_dim
        foreground_dim = up src "#758094"; # grey
        foreground = up src fg; # fg
      };
      brights = {
        bright_red = der (brightMethod t "red" 15) (brighten t red 15);
        bright_orange = der (brightMethod t "orange" 15) (brighten t orange 15);
        bright_yellow = der (brightMethod t "yellow" 15) (brighten t yellow 15);
        bright_green = der (brightMethod t "green" 15) (brighten t green 15);
        bright_cyan = der (brightMethod t "cyan" 15) (brighten t cyan 15);
        bright_blue = der (brightMethod t "blue" 15) (brighten t blue 15);
        bright_magenta = der (brightMethod t "fuchsia" 15) (brighten t fuchsia 15);
        bright_brown = der (brightMethod t "brown" 15) (brighten t brown 15);
      };
      hues = {
        red = up src red;
        orange = adj src orange; # ADJUSTED: edge upstream has no orange slot
        amber = der "darken yellow 8" amber;
        yellow = up src yellow;
        lime = der "lighten green 15" lime;
        green = up src green;
        emerald = der "darken teal 10" emerald;
        teal = up src teal;
        cyan = der "lighten teal 10" cyan;
        sky = up src sky;
        blue = der "darken sky 12" blue;
        indigo = der "darken blue 15" indigo;
        violet = up src violet;
        purple = der "lighten violet 6" purple;
        fuchsia = der "lighten violet 12" fuchsia;
        pink = der "lighten violet 18" pink;
        rose = der "lighten red 6" rose;
        brown = der "darken orange 25" brown; # ADAPTATION: not a Tailwind hue
      };
      backgrounds = {
        bg_red = der (tintMethod t "red" 62) (tint t red 62);
        bg_green = der (tintMethod t "green" 62) (tint t green 62);
        bg_blue = der (tintMethod t "blue" 62) (tint t blue 62);
        bg_yellow = der (tintMethod t "yellow" 62) (tint t yellow 62);
        bg_visual = der (tintMethod t "fuchsia" 62) (tint t fuchsia 62);
      };
      roles = {
        primary = der "alias -> green" green;
        secondary = der "alias -> blue" blue;
        error = der "alias -> red" red;
        warning = der "alias -> orange" orange;
        success = der "alias -> green" green;
        info = der "alias -> blue" blue;
        selection = der "alias -> overlay" "#3b3e48";
        cursor = der "alias -> foreground" fg;
        border = der "alias -> muted" "#535c6a";
      };
    };

  # ---- everforest (dark, sainnhe/everforest, background=medium) ----
  everforest =
    let
      t = "dark";
      src = "sainnhe/everforest (dark, medium)";
      fg = "#d3c6aa";
      bg0 = "#2d353b";
      # native everforest accents (upstream literals)
      nred = "#e67e80";
      norange = "#e69875";
      nyellow = "#dbbc7f";
      ngreen = "#a7c080";
      naqua = "#83c092";
      nblue = "#7fbbb3";
      npurple = "#d699b6";

      red = nred;
      orange = norange;
      yellow = nyellow;
      green = ngreen;
      emerald = naqua; # everforest aqua is a green-cyan → Tailwind emerald
      teal = darken emerald 12;
      blue = nblue; # everforest blue is a muted teal-blue → Tailwind blue
      cyan = lighten blue 12;
      sky = lighten blue 20;
      indigo = darken blue 18;
      fuchsia = npurple; # everforest purple is a dusty pink-purple → fuchsia
      violet = darken fuchsia 10;
      purple = lighten fuchsia 4;
      pink = lighten fuchsia 12;
      rose = lighten red 6;
      amber = darken yellow 8;
      lime = lighten green 15;
      brown = darken orange 25;
    in
    {
      name = "everforest";
      type = t;
      neutrals = {
        background_darkest = der "darken bg0 40" (darken bg0 40);
        background_dark = up src "#232a2e"; # bg_dim
        background = up src bg0; # bg0
        surface = up src "#343f44"; # bg1
        overlay = up src "#475258"; # bg3
        muted = up src "#7a8478"; # grey0
        foreground_dim = up src "#859289"; # grey1
        foreground = up src fg; # fg
      };
      brights = {
        bright_red = der (brightMethod t "red" 15) (brighten t red 15);
        bright_orange = der (brightMethod t "orange" 15) (brighten t orange 15);
        bright_yellow = der (brightMethod t "yellow" 15) (brighten t yellow 15);
        bright_green = der (brightMethod t "green" 15) (brighten t green 15);
        bright_cyan = der (brightMethod t "cyan" 15) (brighten t cyan 15);
        bright_blue = der (brightMethod t "blue" 15) (brighten t blue 15);
        bright_magenta = der (brightMethod t "fuchsia" 15) (brighten t fuchsia 15);
        bright_brown = der (brightMethod t "brown" 15) (brighten t brown 15);
      };
      hues = {
        red = up src red;
        orange = up src orange;
        amber = der "darken yellow 8" amber;
        yellow = up src yellow;
        lime = der "lighten green 15" lime;
        green = up src green;
        emerald = up src emerald;
        teal = der "darken emerald 12" teal;
        cyan = der "lighten blue 12" cyan;
        sky = der "lighten blue 20" sky;
        blue = up src blue;
        indigo = der "darken blue 18" indigo;
        violet = der "darken fuchsia 10" violet;
        purple = der "lighten fuchsia 4" purple;
        fuchsia = up src fuchsia;
        pink = der "lighten fuchsia 12" pink;
        rose = der "lighten red 6" rose;
        brown = der "darken orange 25" brown; # ADAPTATION: not a Tailwind hue
      };
      backgrounds = {
        bg_red = der (tintMethod t "red" 62) (tint t red 62);
        bg_green = der (tintMethod t "green" 62) (tint t green 62);
        bg_blue = der (tintMethod t "blue" 62) (tint t blue 62);
        bg_yellow = der (tintMethod t "yellow" 62) (tint t yellow 62);
        bg_visual = der (tintMethod t "fuchsia" 62) (tint t fuchsia 62);
      };
      roles = {
        primary = der "alias -> green" green;
        secondary = der "alias -> blue" blue;
        error = der "alias -> red" red;
        warning = der "alias -> orange" orange;
        success = der "alias -> green" green;
        info = der "alias -> blue" blue;
        selection = der "alias -> overlay" "#475258";
        cursor = der "alias -> foreground" fg;
        border = der "alias -> muted" "#7a8478";
      };
    };

  # ---- catppuccin-latte (light, catppuccin/palette palette.json) ----
  catppuccin-latte =
    let
      t = "light";
      src = "catppuccin/palette (latte)";
      # native latte colours (upstream literals). The comment names the source
      # palette's own name; the slot it fills carries the nearest Tailwind name.
      lred = "#d20f39"; # red
      lmaroon = "#e64553"; # maroon → rose
      lpeach = "#fe640b"; # peach → orange
      lyellow = "#df8e1d"; # yellow
      lgreen = "#40a02b"; # green
      lteal = "#179299"; # teal
      lsky = "#04a5e5"; # sky
      lsapphire = "#209fb5"; # sapphire → cyan
      lblue = "#1e66f5"; # blue
      llavender = "#7287fd"; # lavender → indigo
      lmauve = "#8839ef"; # mauve → violet
      lpink = "#ea76cb"; # pink → fuchsia
      lflamingo = "#dd7878"; # flamingo → pink

      red = lred;
      orange = lpeach;
      yellow = lyellow;
      green = lgreen;
      teal = lteal;
      cyan = lsapphire;
      sky = lsky;
      blue = lblue;
      indigo = llavender;
      violet = lmauve;
      fuchsia = lpink;
      pink = lflamingo;
      rose = lmaroon;
      # latte has no distinct colour in these regions → derived by luminance.
      amber = darken yellow 8;
      lime = lighten green 15;
      emerald = lighten teal 12;
      purple = lighten violet 12;
      brown = darken orange 30;
    in
    {
      name = "catppuccin-latte";
      type = t;
      neutrals = {
        background_darkest = up src "#bcc0cc"; # surface1
        background_dark = up src "#dce0e8"; # crust
        background = up src "#eff1f5"; # base
        surface = up src "#e6e9ef"; # mantle
        overlay = up src "#ccd0da"; # surface0
        muted = up src "#9ca0b0"; # overlay0
        foreground_dim = up src "#6c6f85"; # subtext0
        foreground = up src "#4c4f69"; # text
      };
      brights = {
        bright_red = der (brightMethod t "red" 12) (brighten t red 12);
        bright_orange = der (brightMethod t "orange" 12) (brighten t orange 12);
        bright_yellow = der (brightMethod t "yellow" 12) (brighten t yellow 12);
        bright_green = der (brightMethod t "green" 12) (brighten t green 12);
        bright_cyan = der (brightMethod t "cyan" 12) (brighten t cyan 12);
        bright_blue = der (brightMethod t "blue" 12) (brighten t blue 12);
        bright_magenta = der (brightMethod t "fuchsia" 12) (brighten t fuchsia 12);
        bright_brown = der (brightMethod t "brown" 12) (brighten t brown 12);
      };
      hues = {
        red = up src red;
        orange = up src orange;
        amber = der "darken yellow 8" amber;
        yellow = up src yellow;
        lime = der "lighten green 15" lime;
        green = up src green;
        emerald = der "lighten teal 12" emerald;
        teal = up src teal;
        cyan = up src cyan;
        sky = up src sky;
        blue = up src blue;
        indigo = up src indigo;
        violet = up src violet;
        purple = der "lighten violet 12" purple;
        fuchsia = up src fuchsia;
        pink = up src pink;
        rose = up src rose;
        brown = der "darken orange 30" brown; # ADAPTATION: not a Tailwind hue
      };
      backgrounds = {
        bg_red = der (tintMethod t "red" 82) (tint t red 82);
        bg_green = der (tintMethod t "green" 82) (tint t green 82);
        bg_blue = der (tintMethod t "blue" 82) (tint t blue 82);
        bg_yellow = der (tintMethod t "yellow" 82) (tint t yellow 82);
        bg_visual = der (tintMethod t "fuchsia" 82) (tint t fuchsia 82);
      };
      roles = {
        primary = der "alias -> green" green;
        secondary = der "alias -> blue" blue;
        error = der "alias -> red" red;
        warning = der "alias -> orange" orange;
        success = der "alias -> green" green;
        info = der "alias -> blue" blue;
        selection = der "alias -> overlay" "#ccd0da";
        cursor = der "alias -> foreground" "#4c4f69";
        border = der "alias -> muted" "#9ca0b0";
      };
    };

in
{
  schemes = {
    inherit edge everforest catppuccin-latte;
  };

  # Display order the preview walks. Group titles are printed as headers.
  groups = [
    {
      title = "Neutrals";
      group = "neutrals";
      slots = [
        "background_darkest"
        "background_dark"
        "background"
        "surface"
        "overlay"
        "muted"
        "foreground_dim"
        "foreground"
      ];
    }
    {
      title = "Brights";
      group = "brights";
      slots = [
        "bright_red"
        "bright_orange"
        "bright_yellow"
        "bright_green"
        "bright_cyan"
        "bright_blue"
        "bright_magenta"
        "bright_brown"
      ];
    }
    {
      title = "Hues";
      group = "hues";
      slots = [
        "red"
        "orange"
        "amber"
        "yellow"
        "lime"
        "green"
        "emerald"
        "teal"
        "cyan"
        "sky"
        "blue"
        "indigo"
        "violet"
        "purple"
        "fuchsia"
        "pink"
        "rose"
        "brown"
      ];
    }
    {
      title = "Tinted backgrounds";
      group = "backgrounds";
      slots = [
        "bg_red"
        "bg_green"
        "bg_blue"
        "bg_yellow"
        "bg_visual"
      ];
    }
    {
      title = "Roles";
      group = "roles";
      slots = [
        "primary"
        "secondary"
        "error"
        "warning"
        "success"
        "info"
        "selection"
        "cursor"
        "border"
      ];
    }
  ];
}
