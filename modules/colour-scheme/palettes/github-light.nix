# theme scheme: github-light (light). Source: GitHub Primer light palette.
#
# Plain hex values. Provenance in inline comments only where a slot deviates
# from upstream (rename / derived / adjusted); native colours get no comment.
{ colourLib }:
let
  inherit (colourLib) darken lighten;
in
rec {
  name = "github-light";
  type = "light";

  # Neutrals: background_0 is the primary/default background; background_1..5
  # is a strict luminance ramp climbing darker toward the foreground;
  # foreground_dim / foreground are named text anchors. No shade sits below
  # background_0 in this light scheme, so background_dim is set equal to it.
  neutrals = {
    background_dim = "#ffffff"; # no lower shade on this light scheme; equal to background_0
    background_0 = "#ffffff"; # canvas.default
    background_1 = "#f6f8fa"; # canvas.subtle
    background_2 = "#eaeef2"; # border.muted
    background_3 = "#d0d7de"; # border.default
    background_4 = "#afb8c1"; # border.emphasis
    background_5 = "#8c959f"; # fg.subtle
    foreground_dim = "#6e7781"; # fg.muted
    foreground = "#24292f"; # fg.default
  };

  # Tailwind-inspired hues
  hues = rec {
    red = "#cf222e";
    orange = "#fb8f44";
    amber = darken yellow 8; # derived from yellow
    yellow = "#d4a72c";
    lime = lighten green 15; # derived from green
    green = "#116329";
    emerald = lighten teal 12; # derived from teal
    teal = "#1b7c83"; # upstream calls this open (done)
    cyan = "#3192aa"; # not in upstream — derived-adjacent primer accent
    sky = "#54aeff"; # upstream calls this accent.emphasis (lighter)
    blue = "#0969da";
    indigo = "#6639ba"; # upstream calls this done.emphasis (purple family)
    violet = "#8250df";
    purple = lighten violet 10; # derived from violet
    fuchsia = lighten purple 8; # derived from purple — primer has no fuchsia
    pink = "#bf3989";
    rose = darken pink 10; # derived from pink
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
    bg_red = "#ffebe9"; # danger.subtle
    bg_green = "#dafbe1"; # success.subtle
    bg_blue = "#ddf4ff"; # accent.subtle
    bg_yellow = "#fff8c5"; # attention.subtle
    bg_visual = "#ddf4ff"; # accent.subtle
  };

  # Roles — aliases into hues and neutrals
  roles = {
    primary = hues.blue;
    secondary = hues.green;
    error = hues.red;
    warning = hues.orange;
    success = hues.green;
    info = hues.blue;
    selection = neutrals.background_2;
    cursor = neutrals.foreground;
    border = neutrals.background_3;
  };
}
