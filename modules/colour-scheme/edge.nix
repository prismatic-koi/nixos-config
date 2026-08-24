{
  config,
  lib,
  pkgs,
  ...
}:
let
  edgeTheme = import ./palettes/edge.nix { colourLib = import ./lib.nix; };
in
{
  theme = lib.mkIf (config.nx.desktop.theme == "edge") edgeTheme;
}
