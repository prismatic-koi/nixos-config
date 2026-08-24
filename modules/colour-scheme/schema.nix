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
  # base26 colour schema: a numbered neutral ramp (no baseX codes), an ANSI
  # bright band, and a tailwind-inspired hue palette (Tailwind hue names,
  # with `brown` added and `maroon` reached via luminance). Each slot is a
  # plain hex string. The schemes live one per file in ./palettes/
  # (edge.nix, everforest.nix, catppuccin-latte.nix, github-light.nix,
  # gruvbox.nix, nightcity-kabuki.nix, onedark.nix); provenance is recorded
  # as inline comments in those files.
  colourLib = import ./lib.nix;
  defaultTheme = import ./palettes/everforest.nix { inherit colourLib; };

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

  themeType = types.submodule {
    options = {
      name = mkOption { type = types.str; };
      type = mkOption { type = types.str; };
      # Neutral band: background_0 is universally the primary/default
      # background (numeric order always reads background -> foreground, so
      # background_1..5 climb progressively closer to the foreground).
      # background_dim is the recessed shade below background_0 (or equal to
      # it, noted, on schemes with no distinct lower shade). foreground_dim
      # and foreground are named text anchors. Roles that need the default
      # text/background reference neutrals.foreground / a
      # neutrals.background_N slot.
      neutrals = mkOption {
        type = types.submodule {
          options = mkSlots [
            "background_dim"
            "background_0"
            "background_1"
            "background_2"
            "background_3"
            "background_4"
            "background_5"
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
      #
      # Caveat: these 18 slots are NOT guaranteed to be perceptually
      # independent colours. Per scheme, many slots are `darken` / `lighten`
      # derivations of roughly six upstream anchors, and which slots are
      # native (vs. derived) varies per scheme — the inline comments in each
      # palettes/*.nix file record the actual provenance. For example, in
      # palettes/everforest.nix, `cyan`, `sky`, and `indigo` are all
      # lighten/darken derivations of the same `blue` anchor. A consumer
      # that interpolates or ramps across hue slots (e.g. building a
      # gradient) must not assume even spacing — verify the actual spread
      # against each scheme's real values (e.g. after palette quantisation)
      # rather than assume the 18 names imply 18 independent colours.
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

in
{
  options = {
    # base26 colour schema. Defaults to everforest; each scheme module
    # overrides it via mkIf on nx.desktop.theme. edge, everforest,
    # catppuccin-latte, github-light, gruvbox (light + dark),
    # nightcity-kabuki and onedark populate it.
    theme = mkOption {
      type = themeType;
      default = defaultTheme;
    };
  };
}
