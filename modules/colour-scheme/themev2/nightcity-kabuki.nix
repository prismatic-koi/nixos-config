# themev2 scheme: nightcity-kabuki (dark). Source: the v1 scheme definition
# (../nightcity-kabuki.nix) — nightcity-kabuki has no widely-published
# canonical upstream palette.
#
# Plain hex values. Provenance in inline comments only where a slot deviates
# from the v1 source (rename / derived / adjusted); native colours get no
# comment.
{ colourLib }:
let
  inherit (colourLib) darken lighten;
in
rec {
  name = "nightcity-kabuki";
  type = "dark";

  # Neutrals: background_0 is the primary/default background; background_1..5
  # climbs lighter toward the foreground; foreground_dim / foreground are
  # named text anchors. No distinct shade sits below background_0 (it is
  # already v1's bg_dim), so background_dim is set equal.
  neutrals = {
    background_dim = "#1b1b1b"; # no lower shade below background_0 (bg_dim); equal
    background_0 = "#1b1b1b"; # bg_dim
    background_1 = "#282828"; # bg0
    background_2 = "#393633"; # bg2
    background_3 = "#4a4542"; # bg3
    background_4 = "#615b53"; # bg4
    background_5 = "#777064"; # bg5
    foreground_dim = "#a39b85"; # grey1
    foreground = "#f9efc5";
  };

  # Tailwind-inspired hues
  hues = rec {
    red = "#ff4b3b";
    orange = "#ff9457";
    amber = darken yellow 8; # derived from yellow
    yellow = "#ffbe32";
    lime = lighten green 15; # derived from green
    green = "#9ea32a";
    emerald = lighten teal 10; # derived from teal
    teal = "#8db885"; # v1 calls this aqua
    cyan = "#89c5bf"; # v1 calls this statusline1
    sky = lighten blue 20; # derived from blue
    blue = "#6e9685";
    indigo = darken purple 12; # derived from purple
    violet = "#d9869e"; # v1 calls this purple
    purple = violet; # nightcity-kabuki purple slot maps directly onto violet
    fuchsia = lighten violet 10; # derived from violet
    pink = lighten fuchsia 8; # derived from fuchsia — not in v1
    rose = darken red 6; # derived from red — not in v1
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

  # Tinted backgrounds — from v1's bg_* roles, else derived
  backgrounds = {
    bg_red = "#eb3040"; # v1 bg_red
    bg_green = "#988921"; # v1 bg_green
    bg_blue = "#5e8d6f"; # v1 bg_blue
    bg_yellow = "#eb8d27"; # v1 bg_yellow
    bg_visual = "#777064"; # v1 bg_visual
  };

  # Roles — aliases into hues and neutrals
  roles = {
    primary = hues.orange; # v1 primary
    secondary = hues.teal; # v1 secondary (aqua)
    error = hues.red;
    warning = hues.yellow;
    success = hues.green;
    info = hues.blue;
    selection = neutrals.background_3;
    cursor = neutrals.foreground;
    border = neutrals.background_5;
  };
}
