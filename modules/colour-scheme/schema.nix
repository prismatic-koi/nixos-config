{
  config,
  lib,
  pkgs,
  ...
}:
# This modules is used to define the schema for a colour scheme
# It is then free to be used by all other modules
with lib;
let
  # themev2 (migration increment #1): a parallel schema — a semantic neutrals
  # band (no baseX codes), an ANSI bright band, and a tailwind-inspired hue
  # palette (Tailwind hue names, with `brown` added and `maroon` reached via
  # luminance). Purely additive; no consumer reads it yet. Each slot is a
  # plain hex string, exactly like `theme` above. The sample schemes live one
  # per file in ./themev2/ (edge.nix, everforest.nix, catppuccin-latte.nix);
  # provenance is recorded as inline comments in those files.
  colourLib = import ./lib.nix;
  defaultThemev2 = import ./themev2/everforest.nix { inherit colourLib; };

  mkSlots =
    names:
    listToAttrs (
      map (
        n:
        nameValuePair n (mkOption {
          type = types.str;
        })
      ) names
    );

  themev2Type = types.submodule {
    options = {
      name = mkOption { type = types.str; };
      type = mkOption { type = types.str; };
      # Semantic neutrals band (dark -> light), no baseX codes. The default
      # text and background colours are first-class slots here; roles that
      # need them reference neutrals.foreground / neutrals.background.
      neutrals = mkOption {
        type = types.submodule {
          options = mkSlots [
            "background_darkest"
            "background_dark"
            "background"
            "surface"
            "overlay"
            "muted"
            "foreground_dim"
            "foreground"
          ];
        };
      };
      # ANSI bright band. bright_red/yellow/green/cyan/blue/magenta map to
      # kitty color9–color14; bright_orange and bright_brown are additions.
      brights = mkOption {
        type = types.submodule {
          options = mkSlots [
            "bright_red"
            "bright_orange"
            "bright_yellow"
            "bright_green"
            "bright_cyan"
            "bright_blue"
            "bright_magenta"
            "bright_brown"
          ];
        };
      };
      # Tailwind-inspired hue palette. 17 Tailwind hue names plus `brown`
      # (Tailwind omits it; base24/ANSI need it). `maroon` is not a slot — it
      # is reached via luminance (darken red).
      hues = mkOption {
        type = types.submodule {
          options = mkSlots [
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
        };
      };
      backgrounds = mkOption {
        type = types.submodule {
          options = mkSlots [
            "bg_red"
            "bg_green"
            "bg_blue"
            "bg_yellow"
            "bg_visual"
          ];
        };
      };
      # Universal role core. Does not duplicate neutrals — no foreground /
      # background roles; consumers reference neutrals for those.
      roles = mkOption {
        type = types.submodule {
          options = mkSlots [
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
        };
      };
    };
  };

  themeType = types.submodule {
    options = {
      name = mkOption { type = types.str; };
      type = mkOption { type = types.str; };
      foreground = mkOption { type = types.str; };
      primary = mkOption { type = types.str; };
      secondary = mkOption { type = types.str; };
      red = mkOption { type = types.str; };
      orange = mkOption { type = types.str; };
      yellow = mkOption { type = types.str; };
      green = mkOption { type = types.str; };
      aqua = mkOption { type = types.str; };
      blue = mkOption { type = types.str; };
      purple = mkOption { type = types.str; };
      grey0 = mkOption { type = types.str; };
      grey1 = mkOption { type = types.str; };
      grey2 = mkOption { type = types.str; };
      statusline1 = mkOption { type = types.str; };
      statusline2 = mkOption { type = types.str; };
      statusline3 = mkOption { type = types.str; };
      bg_dim = mkOption { type = types.str; };
      bg0 = mkOption { type = types.str; };
      bg1 = mkOption { type = types.str; };
      bg2 = mkOption { type = types.str; };
      bg3 = mkOption { type = types.str; };
      bg4 = mkOption { type = types.str; };
      bg5 = mkOption { type = types.str; };
      bg_visual = mkOption { type = types.str; };
      bg_red = mkOption { type = types.str; };
      bg_green = mkOption { type = types.str; };
      bg_blue = mkOption { type = types.str; };
      bg_yellow = mkOption { type = types.str; };
    };
  };
in
{
  options = {
    theme = mkOption {
      type = themeType;
      default = {
        name = "default";
        type = "light";
        foreground = "#24292f";
        primary = "#0366d6";
        secondary = "#116329";
        red = "#cf222e";
        orange = "#fb8f44";
        yellow = "#4d2d00";
        green = "#116329";
        aqua = "#1b7c83";
        blue = "#0969da";
        purple = "#8250df";
        grey0 = "#6e7781";
        grey1 = "#57606a";
        grey2 = "#424a53";
        statusline1 = "#0969da";
        statusline2 = "#6e7781";
        statusline3 = "#cf222e";
        bg_dim = "#ffffff";
        bg0 = "#ffffff";
        bg1 = "#f6f8fa";
        bg2 = "#eaeef2";
        bg3 = "#d0d7de";
        bg4 = "#afb8c1";
        bg5 = "#afb8c1";
        bg_visual = "#ddf4ff";
        bg_red = "#ffebe9";
        bg_green = "#dafbe1";
        bg_blue = "#ddf4ff";
        bg_yellow = "#fff8c5";
      };
    };

    # Parallel themev2 schema. Defaults to everforest; the sample scheme
    # modules override it via mkIf on nx.desktop.theme, exactly parallel to
    # `theme` above. Only edge, everforest and catppuccin-latte populate it
    # in this increment; every other scheme falls back to this default.
    themev2 = mkOption {
      type = themev2Type;
      default = defaultThemev2;
    };
  };
}
