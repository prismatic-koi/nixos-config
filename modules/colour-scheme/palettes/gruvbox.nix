# theme scheme: gruvbox (light + dark). Source: morhetz/gruvbox.
#
# One file, two variants — mirrors how modules/colour-scheme/gruvbox.nix
# handles gruvbox-light and gruvbox-dark. Plain hex values. Provenance in
# inline comments only where a slot deviates from upstream (rename / derived
# / adjusted); native colours get no comment.
{ colourLib }:
let
  inherit (colourLib) darken lighten;
in
{
  light = rec {
    name = "gruvbox-light";
    type = "light";

    # Neutrals: background_0 is the primary/default background; background_1..5
    # is a strict luminance ramp climbing darker toward the foreground;
    # foreground_dim / foreground are named text anchors. No shade sits below
    # background_0 in this light scheme, so background_dim is set equal to it.
    neutrals = {
      background_dim = "#fbf1c7"; # no lower shade on this light scheme; equal to background_0
      background_0 = "#fbf1c7"; # bg0
      background_1 = "#ebdbb2"; # bg1
      background_2 = "#d5c4a1"; # bg2
      background_3 = "#bdae93"; # bg3
      background_4 = "#a89984"; # bg4
      background_5 = "#928374"; # gray
      foreground_dim = "#7c6f64"; # fg4
      foreground = "#3c3836"; # fg1
    };

    # Tailwind-inspired hues
    hues = rec {
      red = "#9d0006"; # faded red
      orange = "#af3a03"; # faded orange
      amber = darken yellow 8; # derived from yellow
      yellow = "#b57614"; # faded yellow
      lime = lighten green 15; # derived from green
      green = "#79740e"; # faded green
      emerald = lighten teal 12; # derived from teal
      teal = "#427b58"; # faded aqua
      cyan = lighten teal 15; # derived — gruvbox has no distinct cyan
      sky = lighten blue 15; # derived from blue
      blue = "#076678"; # faded blue
      indigo = darken purple 8; # derived from purple
      violet = "#8f3f71"; # faded purple
      purple = violet; # gruvbox purple slot maps directly onto violet
      fuchsia = lighten purple 10; # derived from purple
      pink = lighten fuchsia 8; # derived from fuchsia
      rose = darken red 6; # derived from red
      brown = darken orange 25; # derived from orange (Tailwind omits brown)
    };

    # Brights (ANSI color9-14, plus orange/brown) — darken the hue (light)
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
      bg_red = "#cc241d"; # neutral red used as tint source
      bg_green = "#98971a"; # neutral green used as tint source
      bg_blue = "#458588"; # neutral blue used as tint source
      bg_yellow = "#d79921"; # neutral yellow used as tint source
      bg_visual = lighten hues.green 55; # derived — gruvbox has no visual bg
    };

    # Roles — aliases into hues and neutrals
    roles = {
      primary = hues.green;
      secondary = hues.orange;
      error = hues.red;
      warning = hues.yellow;
      success = hues.green;
      info = hues.blue;
      selection = neutrals.background_2;
      cursor = neutrals.foreground;
      border = neutrals.background_5;
    };
  };

  dark = rec {
    name = "gruvbox-dark";
    type = "dark";

    # Neutrals: background_0 is the primary/default background; background_1..5
    # climbs lighter toward the foreground; foreground_dim / foreground are
    # named text anchors. v1's bg_dim equals bg0 for this scheme, so no
    # distinct shade sits below background_0 — background_dim is set equal.
    neutrals = {
      background_dim = "#282828"; # v1 bg_dim == bg0 for this scheme; equal to background_0
      background_0 = "#282828"; # bg0
      background_1 = "#3c3836"; # bg1
      background_2 = "#504945"; # bg2
      background_3 = "#665c54"; # bg3
      background_4 = "#7c6f64"; # bg4
      background_5 = "#928374"; # gray
      foreground_dim = "#a89984"; # fg4
      foreground = "#ebdbb2"; # fg1
    };

    # Tailwind-inspired hues
    hues = rec {
      red = "#cc241d";
      orange = "#d65d0e";
      amber = darken yellow 8; # derived from yellow
      yellow = "#d79921";
      lime = lighten green 15; # derived from green
      green = "#98971a";
      emerald = lighten teal 12; # derived from teal
      teal = "#689d6a"; # upstream calls this aqua
      cyan = lighten teal 15; # derived — gruvbox has no distinct cyan
      sky = lighten blue 15; # derived from blue
      blue = "#83a598";
      indigo = darken purple 8; # derived from purple
      violet = "#b16286"; # upstream calls this purple
      purple = violet; # gruvbox purple slot maps directly onto violet
      fuchsia = lighten purple 10; # derived from purple
      pink = lighten fuchsia 8; # derived from fuchsia
      rose = darken red 6; # derived from red
      brown = darken orange 25; # derived from orange (Tailwind omits brown)
    };

    # Brights (ANSI color9-14, plus orange/brown) — lighten the hue (dark)
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
      secondary = hues.orange;
      error = hues.red;
      warning = hues.yellow;
      success = hues.green;
      info = hues.blue;
      selection = neutrals.background_2;
      cursor = neutrals.foreground;
      border = neutrals.background_5;
    };
  };
}
