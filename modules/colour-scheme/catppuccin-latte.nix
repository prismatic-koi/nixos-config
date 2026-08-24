{
  config,
  lib,
  pkgs,
  ...
}:
let
  latteTheme = import ./palettes/catppuccin-latte.nix { colourLib = import ./lib.nix; };
in
{
  theme = lib.mkIf (config.nx.desktop.theme == "catppuccin-latte") latteTheme;
}
