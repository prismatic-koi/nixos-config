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
  # themev2 (migration increment #1): a parallel schema — a base24 spine plus
  # an extended evocative-hue band. Purely additive; no consumer reads it yet.
  # See ./themev2/palette.nix for the sample data and ./themev2/register.md
  # for the divergence record.
  colourLib = import ./lib.nix;
  themev2Data = import ./themev2/palette.nix { inherit colourLib; };

  # One palette slot: its resolved hex plus provenance metadata.
  colourSlot = types.submodule {
    options = {
      value = mkOption { type = types.str; };
      provenance = mkOption {
        type = types.enum [
          "upstream"
          "derived"
          "adjusted"
        ];
      };
      source = mkOption {
        type = types.str;
        default = "";
      };
      method = mkOption {
        type = types.str;
        default = "";
      };
    };
  };

  mkSlots =
    names:
    listToAttrs (
      map (
        n:
        nameValuePair n (mkOption {
          type = colourSlot;
        })
      ) names
    );

  themev2Type = types.submodule {
    options = {
      name = mkOption { type = types.str; };
      type = mkOption { type = types.str; };
      palette = mkOption {
        type = types.submodule {
          options = mkSlots [
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
      hues = mkOption {
        type = types.submodule {
          options = mkSlots [
            "rosewater"
            "flamingo"
            "pink"
            "mauve"
            "maroon"
            "peach"
            "teal"
            "sky"
            "sapphire"
            "lavender"
          ];
        };
      };
      roles = mkOption {
        type = types.submodule {
          options = mkSlots [
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
      default = themev2Data.schemes.everforest;
    };
  };
}
