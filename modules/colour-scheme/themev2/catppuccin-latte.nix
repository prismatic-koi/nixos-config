# themev2 sample scheme: catppuccin-latte (light). Source: catppuccin/palette,
# latte flavour.
#
# Plain hex values. Provenance in inline comments only where a slot deviates
# from upstream (rename / derived / adjusted); native colours get no comment.
# Latte has enough colours to fill most slots natively.
{ colourLib }:
let
  inherit (colourLib) darken lighten;
in
rec {
  name = "catppuccin-latte";
  type = "light";

  # Neutrals (dark -> light)
  neutrals = {
    background_darkest = "#bcc0cc";
    background_dark = "#dce0e8";
    background = "#eff1f5";
    surface = "#e6e9ef";
    overlay = "#ccd0da";
    muted = "#9ca0b0";
    foreground_dim = "#6c6f85";
    foreground = "#4c4f69";
  };

  # Tailwind-inspired hues
  hues = rec {
    red = "#d20f39";
    orange = "#fe640b"; # upstream calls this peach
    amber = darken yellow 8; # derived from yellow
    yellow = "#df8e1d";
    lime = lighten green 15; # derived from green
    green = "#40a02b";
    emerald = lighten teal 12; # derived from teal
    teal = "#179299";
    cyan = "#209fb5"; # upstream calls this sapphire
    sky = "#04a5e5";
    blue = "#1e66f5";
    indigo = "#7287fd"; # upstream calls this lavender
    violet = "#8839ef"; # upstream calls this mauve
    purple = lighten violet 12; # derived from violet
    fuchsia = "#ea76cb"; # upstream calls this pink
    pink = "#dd7878"; # upstream calls this flamingo
    rose = "#e64553"; # upstream calls this maroon
    brown = darken orange 30; # derived from orange (Tailwind omits brown)
  };

  # Brights (ANSI color9-14, plus orange/brown) — darken the hue (light theme)
  brights = {
    bright_red = darken hues.red 12;
    bright_orange = darken hues.orange 12;
    bright_yellow = darken hues.yellow 12;
    bright_green = darken hues.green 12;
    bright_cyan = darken hues.cyan 12;
    bright_blue = darken hues.blue 12;
    bright_magenta = darken hues.fuchsia 12;
    bright_brown = darken hues.brown 12;
  };

  # Tinted backgrounds — pull the hue toward the background (light: lighten)
  backgrounds = {
    bg_red = lighten hues.red 82;
    bg_green = lighten hues.green 82;
    bg_blue = lighten hues.blue 82;
    bg_yellow = lighten hues.yellow 82;
    bg_visual = lighten hues.fuchsia 82;
  };

  # Roles — aliases into hues and neutrals
  roles = {
    primary = hues.green;
    secondary = hues.blue;
    error = hues.red;
    warning = hues.orange;
    success = hues.green;
    info = hues.blue;
    selection = neutrals.overlay;
    cursor = neutrals.foreground;
    border = neutrals.muted;
  };
}
