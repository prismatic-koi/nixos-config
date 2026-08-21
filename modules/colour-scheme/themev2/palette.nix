# themev2 base26 palette data — the single source of truth for both the
# NixOS module option (see ../schema.nix) and the truecolor swatch preview
# (see ./preview.nix). Pure data: takes colourLib, returns the three sample
# schemes plus the display grouping the preview walks.
#
# Provenance model (three categories, one per slot):
#   upstream — literal hex equal to the scheme's authoritative source.
#   derived  — value produced by a Nix expression (a colourLib call, or an
#              alias to another slot). Self-documenting; carries a `method`.
#   adjusted — literal hex that does NOT match the source. Carries a `source`
#              note AND an inline comment, because the value form alone cannot
#              reveal the divergence.
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
      # upstream accent literals
      red = "#ec7279";
      yellow = "#deb974";
      green = "#a0c980";
      cyan = "#5dbbc1";
      blue = "#6cb6eb";
      magenta = "#d38aea"; # upstream calls it "purple"
      fg = "#c5cdd9";
      # edge has NO upstream orange — carried from the v1 theme (a divergence).
      orange = "#e59676";
      brown = darken orange 25;
    in
    {
      name = "edge";
      type = t;
      palette = {
        base00 = up src "#2c2e34"; # bg0
        base01 = up src "#33353f"; # bg1
        base02 = up src "#3b3e48"; # bg3 (selection)
        base03 = up src "#535c6a"; # grey_dim (comments)
        base04 = up src "#758094"; # grey
        base05 = up src fg; # fg
        base06 = up src "#828a98"; # bg_grey (light fg)
        base07 = der "lighten fg 20" (lighten fg 20);
        base10 = up src "#24262a"; # bg_dim (darker bg)
        base11 = up src "#202023"; # black (darkest bg)
        red = up src red;
        orange = adj src orange; # ADJUSTED: edge upstream has no orange slot
        yellow = up src yellow;
        green = up src green;
        cyan = up src cyan;
        blue = up src blue;
        magenta = up src magenta;
        brown = der "darken orange 25" brown;
        bright_red = der (brightMethod t "red" 15) (brighten t red 15);
        bright_orange = der (brightMethod t "orange" 15) (brighten t orange 15);
        bright_yellow = der (brightMethod t "yellow" 15) (brighten t yellow 15);
        bright_green = der (brightMethod t "green" 15) (brighten t green 15);
        bright_cyan = der (brightMethod t "cyan" 15) (brighten t cyan 15);
        bright_blue = der (brightMethod t "blue" 15) (brighten t blue 15);
        bright_magenta = der (brightMethod t "magenta" 15) (brighten t magenta 15);
        bright_brown = der (brightMethod t "brown" 15) (brighten t brown 15);
      };
      backgrounds = {
        bg_red = der (tintMethod t "red" 62) (tint t red 62);
        bg_green = der (tintMethod t "green" 62) (tint t green 62);
        bg_blue = der (tintMethod t "blue" 62) (tint t blue 62);
        bg_yellow = der (tintMethod t "yellow" 62) (tint t yellow 62);
        bg_visual = der (tintMethod t "magenta" 62) (tint t magenta 62);
      };
      roles = {
        error = der "alias -> red" red;
        warning = der "alias -> orange" orange;
        success = der "alias -> green" green;
        info = der "alias -> blue" blue;
        selection = der "alias -> base02" "#3b3e48";
        cursor = der "alias -> base05" fg;
        border = der "alias -> base03" "#535c6a";
      };
    };

  # ---- everforest (dark, sainnhe/everforest, background=medium) ----
  everforest =
    let
      t = "dark";
      src = "sainnhe/everforest (dark, medium)";
      red = "#e67e80";
      orange = "#e69875";
      yellow = "#dbbc7f";
      green = "#a7c080";
      cyan = "#83c092"; # upstream calls it "aqua"
      blue = "#7fbbb3";
      magenta = "#d699b6"; # upstream calls it "purple"
      fg = "#d3c6aa";
      bg0 = "#2d353b";
      brown = darken orange 25;
    in
    {
      name = "everforest";
      type = t;
      palette = {
        base00 = up src bg0; # bg0
        base01 = up src "#343f44"; # bg1
        base02 = up src "#475258"; # bg3 (selection)
        base03 = up src "#7a8478"; # grey0 (comments)
        base04 = up src "#859289"; # grey1
        base05 = up src fg; # fg
        base06 = der "lighten fg 12" (lighten fg 12);
        base07 = der "lighten fg 28" (lighten fg 28);
        base10 = up src "#232a2e"; # bg_dim (darker bg)
        base11 = der "darken bg0 40" (darken bg0 40);
        red = up src red;
        orange = up src orange;
        yellow = up src yellow;
        green = up src green;
        cyan = up src cyan;
        blue = up src blue;
        magenta = up src magenta;
        brown = der "darken orange 25" brown;
        bright_red = der (brightMethod t "red" 15) (brighten t red 15);
        bright_orange = der (brightMethod t "orange" 15) (brighten t orange 15);
        bright_yellow = der (brightMethod t "yellow" 15) (brighten t yellow 15);
        bright_green = der (brightMethod t "green" 15) (brighten t green 15);
        bright_cyan = der (brightMethod t "cyan" 15) (brighten t cyan 15);
        bright_blue = der (brightMethod t "blue" 15) (brighten t blue 15);
        bright_magenta = der (brightMethod t "magenta" 15) (brighten t magenta 15);
        bright_brown = der (brightMethod t "brown" 15) (brighten t brown 15);
      };
      backgrounds = {
        bg_red = der (tintMethod t "red" 62) (tint t red 62);
        bg_green = der (tintMethod t "green" 62) (tint t green 62);
        bg_blue = der (tintMethod t "blue" 62) (tint t blue 62);
        bg_yellow = der (tintMethod t "yellow" 62) (tint t yellow 62);
        bg_visual = der (tintMethod t "magenta" 62) (tint t magenta 62);
      };
      roles = {
        error = der "alias -> red" red;
        warning = der "alias -> orange" orange;
        success = der "alias -> green" green;
        info = der "alias -> blue" blue;
        selection = der "alias -> base02" "#475258";
        cursor = der "alias -> base05" fg;
        border = der "alias -> base03" "#7a8478";
      };
    };

  # ---- catppuccin-latte (light, catppuccin/palette palette.json) ----
  catppuccin-latte =
    let
      t = "light";
      src = "catppuccin/palette (latte)";
      red = "#d20f39"; # red
      orange = "#fe640b"; # peach
      yellow = "#df8e1d"; # yellow
      green = "#40a02b"; # green
      cyan = "#179299"; # teal
      blue = "#1e66f5"; # blue
      magenta = "#8839ef"; # mauve
      brown = darken orange 30;
    in
    {
      name = "catppuccin-latte";
      type = t;
      palette = {
        base00 = up src "#eff1f5"; # base (main bg)
        base01 = up src "#e6e9ef"; # mantle
        base02 = up src "#ccd0da"; # surface0 (selection)
        base03 = up src "#9ca0b0"; # overlay0 (comments)
        base04 = up src "#6c6f85"; # subtext0
        base05 = up src "#4c4f69"; # text (main fg)
        base06 = up src "#5c5f77"; # subtext1
        base07 = up src "#7c7f93"; # overlay2
        base10 = up src "#dce0e8"; # crust (extra bg)
        base11 = up src "#bcc0cc"; # surface1 (extra bg)
        red = up src red;
        orange = up src orange;
        yellow = up src yellow;
        green = up src green;
        cyan = up src cyan;
        blue = up src blue;
        magenta = up src magenta;
        brown = der "darken orange 30" brown;
        bright_red = der (brightMethod t "red" 12) (brighten t red 12);
        bright_orange = der (brightMethod t "orange" 12) (brighten t orange 12);
        bright_yellow = der (brightMethod t "yellow" 12) (brighten t yellow 12);
        bright_green = der (brightMethod t "green" 12) (brighten t green 12);
        bright_cyan = der (brightMethod t "cyan" 12) (brighten t cyan 12);
        bright_blue = der (brightMethod t "blue" 12) (brighten t blue 12);
        bright_magenta = der (brightMethod t "magenta" 12) (brighten t magenta 12);
        bright_brown = der (brightMethod t "brown" 12) (brighten t brown 12);
      };
      backgrounds = {
        bg_red = der (tintMethod t "red" 82) (tint t red 82);
        bg_green = der (tintMethod t "green" 82) (tint t green 82);
        bg_blue = der (tintMethod t "blue" 82) (tint t blue 82);
        bg_yellow = der (tintMethod t "yellow" 82) (tint t yellow 82);
        bg_visual = der (tintMethod t "magenta" 82) (tint t magenta 82);
      };
      roles = {
        error = der "alias -> red" red;
        warning = der "alias -> orange" orange;
        success = der "alias -> green" green;
        info = der "alias -> blue" blue;
        selection = der "alias -> base02" "#ccd0da";
        cursor = der "alias -> base05" "#4c4f69";
        border = der "alias -> base03" "#9ca0b0";
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
      title = "Palette (base26)";
      group = "palette";
      slots = [
        "base00"
        "base01"
        "base02"
        "base03"
        "base04"
        "base05"
        "base06"
        "base07"
        "base10"
        "base11"
        "red"
        "orange"
        "yellow"
        "green"
        "cyan"
        "blue"
        "magenta"
        "brown"
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
      title = "Role core";
      group = "roles";
      slots = [
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
