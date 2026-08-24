{
  config,
  lib,
  pkgs,
  ...
}:
let
  onedarkTheme = import ./palettes/onedark.nix { colourLib = import ./lib.nix; };
in
{
  theme = lib.mkIf (config.nx.desktop.theme == "onedark") onedarkTheme;
}
