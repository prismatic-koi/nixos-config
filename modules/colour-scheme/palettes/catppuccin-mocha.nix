# theme sample scheme: catppuccin-mocha (dark). Source: catppuccin/palette,
# mocha flavour.
#
# Plain hex values. Provenance in inline comments only where a slot deviates
# from upstream (rename / derived / adjusted); native colours get no comment.
# Mocha provides enough colours to fill most slots natively.
{ colourLib }:
let
  inherit (colourLib) darken lighten;
in
rec {
  name = "catppuccin-mocha";
  type = "dark";

  # Neutrals: background_0 is the primary/default background; background_1..5
  # climbs lighter toward the foreground; foreground_dim / foreground are
  # named text anchors. background_dim is set to mocha mantle (#181825), a
  # real recessed shade below background_0 (base #1e1e2e).
  neutrals = {
    background_dim = "#181825"; # mantle — a real recessed shade below background_0
    background_0 = "#1e1e2e"; # base
    background_1 = "#313244"; # surface0
    background_2 = "#45475a"; # surface1
    background_3 = "#585b70"; # surface2
    background_4 = "#6c7086"; # overlay0
    background_5 = "#7f849c"; # overlay1
    foreground_dim = "#a6adc8"; # subtext0
    foreground = "#cdd6f4"; # text
  };

  # Tailwind-inspired hues
  hues = rec {
    red = "#f38ba8";
    orange = "#fab387"; # upstream calls this peach
    amber = darken yellow 8; # derived from yellow
    yellow = "#f9e2af";
    lime = lighten green 15; # derived from green
    green = "#a6e3a1";
    emerald = lighten teal 12; # derived from teal
    teal = "#94e2d5";
    cyan = "#74c7ec"; # upstream calls this sapphire
    sky = "#89dceb";
    blue = "#89b4fa";
    indigo = "#b4befe"; # upstream calls this lavender
    violet = "#cba6f7"; # upstream calls this mauve
    purple = lighten violet 12; # derived from violet
    fuchsia = "#f5c2e7"; # upstream calls this pink
    pink = "#f2cdcd"; # upstream calls this flamingo
    rose = "#eba0ac"; # upstream calls this maroon
    brown = darken orange 25; # derived from orange (Tailwind omits brown)
  };

  # Brights (ANSI color9-14, plus orange/brown) — lighten the hue
  brights = {
    bright_red = lighten hues.red 15;
    bright_orange = lighten hues.orange 15;
    bright_yellow = lighten hues.yellow 15;
    bright_green = lighten hues.green 15;
    bright_cyan = lighten hues.cyan 15;
    bright_blue = lighten hues.blue 15;
    bright_magenta = lighten hues.fuchsia 15;
    bright_brown = lighten hues.brown 15;
  };

  # Tinted backgrounds — pull the hue toward the background (dark: darken)
  backgrounds = {
    bg_red = darken hues.red 62;
    bg_green = darken hues.green 62;
    bg_blue = darken hues.blue 62;
    bg_yellow = darken hues.yellow 62;
    bg_visual = darken hues.fuchsia 62;
  };

  # Roles — aliases into hues and neutrals
  roles = {
    primary = hues.green;
    secondary = hues.blue;
    error = hues.red;
    warning = hues.yellow;
    success = hues.green;
    info = hues.blue;
    selection = neutrals.background_3;
    cursor = neutrals.foreground;
    border = neutrals.background_5;
  };
}
