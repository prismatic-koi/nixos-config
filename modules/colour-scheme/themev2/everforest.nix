# themev2 sample scheme: everforest (dark). Source: sainnhe/everforest,
# background=medium.
#
# Plain hex values. Provenance in inline comments only where a slot deviates
# from upstream (rename / derived / adjusted); native colours get no comment.
{ colourLib }:
let
  inherit (colourLib) darken lighten;
  bg0 = "#2d353b";
in
rec {
  name = "everforest";
  type = "dark";

  # Neutrals: background_0..5 is a strict luminance ramp (dark theme ->
  # background_0 darkest, climbing lighter toward the foreground);
  # foreground_dim / foreground are named text anchors.
  neutrals = {
    background_0 = darken bg0 40; # derived — darkest
    background_1 = "#232a2e";
    background_2 = bg0; # primary/default background (main canvas)
    background_3 = "#343f44";
    background_4 = "#475258";
    background_5 = "#7a8478";
    foreground_dim = "#859289";
    foreground = "#d3c6aa";
  };

  # Tailwind-inspired hues
  hues = rec {
    red = "#e67e80";
    orange = "#e69875";
    amber = darken yellow 8; # derived from yellow
    yellow = "#dbbc7f";
    lime = lighten green 15; # derived from green
    green = "#a7c080";
    emerald = "#83c092"; # upstream calls this aqua
    teal = darken emerald 12; # derived from emerald
    cyan = lighten blue 12; # derived from blue
    sky = lighten blue 20; # derived from blue
    blue = "#7fbbb3";
    indigo = darken blue 18; # derived from blue
    violet = darken fuchsia 10; # derived from fuchsia
    purple = lighten fuchsia 4; # derived from fuchsia
    fuchsia = "#d699b6"; # upstream calls this purple
    pink = lighten fuchsia 12; # derived from fuchsia
    rose = lighten red 6; # derived from red
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
    warning = hues.orange;
    success = hues.green;
    info = hues.blue;
    selection = neutrals.background_4;
    cursor = neutrals.foreground;
    border = neutrals.background_5;
  };
}
