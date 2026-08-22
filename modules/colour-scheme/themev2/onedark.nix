# themev2 scheme: onedark (dark). Source: navarasu/onedark.nvim.
#
# Plain hex values. Provenance in inline comments only where a slot deviates
# from upstream (rename / derived / adjusted); native colours get no comment.
{ colourLib }:
let
  inherit (colourLib) darken lighten;
in
rec {
  name = "onedark";
  type = "dark";

  # Neutrals: background_0 is the primary/default background; background_1..5
  # climbs lighter toward the foreground; foreground_dim / foreground are
  # named text anchors. No distinct shade sits below background_0 (it is
  # already v1's bg_dim), so background_dim is set equal.
  neutrals = {
    background_dim = "#21252b"; # no lower shade below background_0 (bg_dim); equal
    background_0 = "#21252b"; # bg_dim
    background_1 = "#282c34"; # bg0
    background_2 = "#31353f"; # bg1
    background_3 = "#393f4a"; # bg2
    background_4 = "#3b3f4c"; # bg3
    background_5 = "#535965"; # grey0
    foreground_dim = "#5c6370"; # grey1
    foreground = "#abb2bf"; # fg
  };

  # Tailwind-inspired hues
  hues = rec {
    red = "#e86671";
    orange = "#e89a5e"; # adjusted — v1 diverges from upstream #d19a66
    amber = darken yellow 8; # derived from yellow
    yellow = "#e5c07b";
    lime = lighten green 15; # derived from green
    green = "#98c379";
    emerald = lighten cyan 10; # derived from cyan
    teal = darken cyan 10; # derived from cyan
    cyan = "#56b6c2";
    sky = lighten blue 15; # derived from blue
    blue = "#61afef";
    indigo = darken purple 12; # derived from purple
    violet = "#c678dd"; # upstream calls this purple
    purple = violet; # onedark purple slot maps directly onto violet
    fuchsia = lighten violet 10; # derived from violet
    pink = lighten fuchsia 8; # derived from fuchsia — onedark has no pink
    rose = darken red 6; # derived from red
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
