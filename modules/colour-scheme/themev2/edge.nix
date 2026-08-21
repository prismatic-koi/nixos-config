# themev2 sample scheme: edge (dark). Source: sainnhe/edge, style=default.
#
# Plain hex values, like the v1 scheme files. Provenance lives in inline
# comments, but only where a slot deviates from the upstream palette:
#   * a rename    — "# upstream calls this <name>"
#   * derived     — "# derived ..." (a colourLib expression)
#   * adjusted    — "# adjusted ..." (a hand-picked literal, not in upstream)
# A straightforward native colour gets no comment.
{ colourLib }:
let
  inherit (colourLib) darken lighten;
in
rec {
  name = "edge";
  type = "dark";

  # Neutrals (dark -> light)
  neutrals = {
    background_darkest = "#202023";
    background_dark = "#24262a";
    background = "#2c2e34";
    surface = "#33353f";
    overlay = "#3b3e48";
    muted = "#535c6a";
    foreground_dim = "#758094";
    foreground = "#c5cdd9";
  };

  # Tailwind-inspired hues
  hues = rec {
    red = "#ec7279";
    orange = "#e59676"; # adjusted — edge upstream has no orange
    amber = darken yellow 8; # derived from yellow
    yellow = "#deb974";
    lime = lighten green 15; # derived from green
    green = "#a0c980";
    emerald = darken teal 10; # derived from teal
    teal = "#5dbbc1"; # upstream calls this cyan
    cyan = lighten teal 10; # derived — native cyan fills teal
    sky = "#6cb6eb"; # upstream calls this blue
    blue = darken sky 12; # derived from sky
    indigo = darken blue 15; # derived from blue
    violet = "#d38aea"; # upstream calls this purple
    purple = lighten violet 6; # derived from violet
    fuchsia = lighten violet 12; # derived from violet
    pink = lighten violet 18; # derived from violet
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
    selection = neutrals.overlay;
    cursor = neutrals.foreground;
    border = neutrals.muted;
  };
}
