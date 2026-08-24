{
  config,
  pkgs,
  lib,
  ...
}:
let
  colourLib = import ./lib.nix;
  mk = name: import ./palettes/${name}.nix { inherit colourLib; };
  gruvbox = mk "gruvbox";
  byTheme = {
    catppuccin-latte = mk "catppuccin-latte";
    edge = mk "edge";
    everforest = mk "everforest";
    github-light = mk "github-light";
    nightcity-kabuki = mk "nightcity-kabuki";
    onedark = mk "onedark";
    gruvbox-light = gruvbox.light;
    gruvbox-dark = gruvbox.dark;
  };
in
{
  imports = [
    ./schema.nix
    ./gradient.nix
  ];
  options = {
    nx.desktop.theme = lib.mkOption {
      default = "everforest";
      type = lib.types.enum (builtins.attrNames byTheme);
    };
  };
  config = {
    theme = byTheme.${config.nx.desktop.theme};
  };
}
